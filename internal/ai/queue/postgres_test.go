package queue

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gitlab-doc-platform/internal/ai"
	"gitlab-doc-platform/internal/database"
)

func queueDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL inference queue tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	for admin.Ping(ctx) != nil {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(100 * time.Millisecond)
	}
	schema := fmt.Sprintf("queue_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema+",public")
	u.RawQuery = query.Encode()
	db, err := database.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func waitCount(t *testing.T, db *pgxpool.Pool, status string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int
		if err := db.QueryRow(context.Background(), `select count(*) from inference_requests where status=$1`, status).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %s count=%d want=%d", status, count, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPostgresQueueCapacityCancellationAndLeaseRecovery(t *testing.T) {
	db := queueDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := NewPostgres(ctx, db, "ollama", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewPostgres(ctx, db, "ollama", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	first.poll, second.poll = 5*time.Millisecond, 5*time.Millisecond
	first.heartbeat, second.heartbeat = 10*time.Millisecond, 10*time.Millisecond
	active, err := first.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Release()
	type result struct {
		permit ai.Admission
		err    error
	}
	results := make(chan result, 1)
	go func() { permit, err := second.Acquire(ctx); results <- result{permit, err} }()
	waitCount(t, db, "waiting", 1)
	if _, err := first.Acquire(ctx); !errors.Is(err, ErrFull) {
		t.Fatalf("queue capacity not enforced: %v", err)
	}
	active.Release()
	var next result
	select {
	case next = <-results:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if next.err != nil {
		t.Fatal(next.err)
	}
	defer next.permit.Release()
	waitCount(t, db, "running", 1)
	waitCtx, stop := context.WithCancel(ctx)
	go func() { permit, err := first.Acquire(waitCtx); results <- result{permit, err} }()
	waitCount(t, db, "waiting", 1)
	stop()
	select {
	case result := <-results:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("waiting cancellation: %v", result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitCount(t, db, "waiting", 0)
	if _, err := db.Exec(ctx, `update inference_requests set lease_expires_at=now()-interval '1 second' where status='running'`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-next.permit.Context.Done():
		if !errors.Is(context.Cause(next.permit.Context), ErrLeaseLost) {
			t.Fatal(context.Cause(next.permit.Context))
		}
	case <-ctx.Done():
		t.Fatal("lease loss did not cancel generation")
	}
	recovered, err := first.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recovered.Release()
	next.permit.Release()
	waitCount(t, db, "running", 0)
}

func TestPostgresQueueLimitsChangeOnlyWhenDrained(t *testing.T) {
	db := queueDB(t)
	ctx := context.Background()
	gate, err := NewPostgres(ctx, db, "pool", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	permit, err := gate.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()
	if _, err := NewPostgres(ctx, db, "pool", 2, 2); err == nil {
		t.Fatal("changed limits while a request was active")
	}
	permit.Release()
	if _, err := NewPostgres(ctx, db, "pool", 2, 2); err != nil {
		t.Fatal(err)
	}
}
