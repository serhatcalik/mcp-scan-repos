package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab-doc-platform/internal/ai/ollama"
	"gitlab-doc-platform/internal/analyzer"
	"gitlab-doc-platform/internal/database"
	"gitlab-doc-platform/internal/documents"
	"gitlab-doc-platform/internal/evidence"
)

func postgresStores(t *testing.T) (*PostgresStore, *PostgresStore, *pgxpool.Pool) {
	t.Helper()
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	for {
		if err = admin.Ping(ctx); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	schema := fmt.Sprintf("docs_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
			t.Errorf("clean up isolated test schema: %v", err)
		}
	})
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	first, err := database.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	second, err := database.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	return NewPostgresStore(first), NewPostgresStore(second), first
}

func TestPostgresSharedStoreProcessesWorkerJob(t *testing.T) {
	apiStore, workerStore, db := postgresStores(t)
	ctx := context.Background()
	root := t.TempDir()
	job := Job{ID: "shared-job", ProjectID: "42", GroupID: "101", GroupName: "backend", CommitSHA: "abc123", Branch: "main", ArchivePath: writeSourceArchive(t, root), IdempotencyKey: "shared-job"}
	created, inserted, err := apiStore.CreateOrGet(ctx, job)
	if err != nil || !inserted || created.Status != Queued {
		t.Fatalf("create: job=%+v inserted=%v error=%v", created, inserted, err)
	}
	duplicate, inserted, err := apiStore.CreateOrGet(ctx, job)
	if err != nil || inserted || duplicate.ID != created.ID {
		t.Fatalf("duplicate: job=%+v inserted=%v error=%v", duplicate, inserted, err)
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"response": `{"summaries":[],"recommendations":[]}`, "done": true, "done_reason": "stop"})
	}))
	defer model.Close()
	worker := Worker{Store: workerStore, Analyzer: analyzer.New(), Generator: documents.NewGenerator(ollama.NewClient(model.URL, "test", nil)), Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), StorageDir: root, ExtractedLimit: 1 << 20}
	if err := worker.processOne(ctx); err != nil {
		t.Fatal(err)
	}
	completed, err := apiStore.Get(ctx, job.ID)
	if err != nil || completed.Status != Completed {
		t.Fatalf("completed: job=%+v error=%v", completed, err)
	}
	assertArtifact(t, filepath.Join(root, "artifacts", job.ID+".tar.gz"))
	snapshot, err := apiStore.GetAnalysis(ctx, job.ID)
	if err != nil || len(snapshot.Inventory) == 0 || len(snapshot.Evidence) == 0 {
		t.Fatalf("stored snapshot: %+v error=%v", snapshot, err)
	}
	var count int
	if err := db.QueryRow(ctx, `select count(*) from evidence_items where job_id=$1`, job.ID).Scan(&count); err != nil || count != len(snapshot.Evidence) {
		t.Fatalf("normalized evidence count=%d want=%d error=%v", count, len(snapshot.Evidence), err)
	}
	if _, found, err := workerStore.LeaseNext(ctx, nil, time.Minute); err != nil || found {
		t.Fatalf("idle queue: found=%v error=%v", found, err)
	}
	if _, err := apiStore.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
}

func TestPostgresConcurrencyAndLeaseRecovery(t *testing.T) {
	first, second, db := postgresStores(t)
	ctx := context.Background()
	for _, id := range []string{"job-a", "job-b"} {
		if _, _, err := first.CreateOrGet(ctx, Job{ID: id, ProjectID: "42", GroupID: "101", IdempotencyKey: id}); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		job   Job
		found bool
		err   error
	}
	results := make(chan result, 2)
	for _, store := range []*PostgresStore{first, second} {
		go func(s *PostgresStore) {
			job, found, err := s.LeaseNext(ctx, nil, time.Minute)
			results <- result{job, found, err}
		}(store)
	}
	var leased Job
	count := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.found {
			leased = result.job
			count++
		}
	}
	if count != 1 {
		t.Fatalf("leased %d jobs with group concurrency=1", count)
	}
	if _, err := db.Exec(ctx, `update documentation_jobs set lease_expires_at=now()-interval '1 second' where id=$1`, leased.ID); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := second.LeaseNext(ctx, nil, time.Minute)
	if err != nil || !found || recovered.ID != leased.ID {
		t.Fatalf("recovery: job=%+v found=%v error=%v", recovered, found, err)
	}
	if _, err := db.Exec(ctx, `update documentation_jobs set attempt_count=max_attempts, lease_expires_at=now()-interval '1 second' where id=$1`, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.LeaseNext(ctx, nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	exhausted, err := first.Get(ctx, leased.ID)
	if err != nil || exhausted.Status != Failed {
		t.Fatalf("expired exhausted job: %+v, %v", exhausted, err)
	}
}

func TestPostgresResubmissionOnlyRequeuesFailedJobs(t *testing.T) {
	store, worker, db := postgresStores(t)
	ctx := context.Background()
	for _, status := range []Status{Queued, Analyzing, Generating, Validating, Completed, Failed, Cancelled} {
		t.Run(string(status), func(t *testing.T) {
			original := Job{ID: string(status), ProjectID: "42", GroupID: "101", IdempotencyKey: string(status), ArchivePath: "original.tar.gz"}
			if _, _, err := store.CreateOrGet(ctx, original); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ctx, `update documentation_jobs set status=$2, error='old error', attempt_count=max_attempts,
next_run_at=now()+interval '1 day', lease_expires_at=now()+interval '1 day' where id=$1`, original.ID, status); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get(ctx, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, created, err := store.CreateOrGet(ctx, Job{ID: "duplicate-" + string(status), ProjectID: "42", GroupID: "101", IdempotencyKey: original.IdempotencyKey, ArchivePath: "new.tar.gz"})
			if err != nil || created || got.ID != original.ID || got.ArchivePath != original.ArchivePath || !got.CreatedAt.Equal(before.CreatedAt) {
				t.Fatalf("resubmission: job=%+v created=%v error=%v", got, created, err)
			}
			var attempts int
			var delayCleared, leaseCleared bool
			if err := db.QueryRow(ctx, `select attempt_count, next_run_at is null, lease_expires_at is null from documentation_jobs where id=$1`, original.ID).Scan(&attempts, &delayCleared, &leaseCleared); err != nil {
				t.Fatal(err)
			}
			if status == Failed {
				if got.Status != Queued || got.Error != "" || attempts != 0 || !delayCleared || !leaseCleared {
					t.Fatalf("failed job not reset: job=%+v attempts=%d delayCleared=%v leaseCleared=%v", got, attempts, delayCleared, leaseCleared)
				}
			} else if got != before || attempts == 0 || delayCleared || leaseCleared {
				t.Fatalf("nonfailed job changed: before=%+v after=%+v", before, got)
			}
			// Keep previous subtests from occupying group capacity.
			if status != Failed {
				if err := store.UpdateStatus(ctx, original.ID, Cancelled, ""); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	leased, found, err := worker.LeaseNext(ctx, nil, time.Minute)
	if err != nil || !found || leased.ID != string(Failed) {
		t.Fatalf("requeued job not eligible: job=%+v found=%v error=%v", leased, found, err)
	}
}

func TestPostgresPreservesGroupRestrictions(t *testing.T) {
	store, _, db := postgresStores(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `insert into gitlab_groups(id,name,enabled) values('disabled','disabled',false)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(ctx, Job{ID: "blocked", GroupID: "disabled", ProjectID: "42", IdempotencyKey: "blocked"}); !errors.Is(err, ErrGroupDisabled) {
		t.Fatalf("disabled group: %v", err)
	}
	if _, err := db.Exec(ctx, `insert into gitlab_groups(id,name,allowed_projects) values('restricted','restricted',array['99'])`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(ctx, Job{ID: "blocked-project", GroupID: "restricted", ProjectID: "42", IdempotencyKey: "blocked-project"}); !errors.Is(err, ErrProjectNotAllowed) {
		t.Fatalf("restricted project: %v", err)
	}
}

func TestPostgresGitHubIdentityAndSchedulingIsolation(t *testing.T) {
	store, worker, db := postgresStores(t)
	ctx := context.Background()
	for _, job := range []Job{
		{ID: "gitlab", ProjectID: "42", GroupID: "101", IdempotencyKey: "42:abc"},
		{ID: "github", ProjectID: "github:42", GroupID: "github:101", GroupName: "backend", IdempotencyKey: "github:42:abc"},
	} {
		got, inserted, err := store.CreateOrGet(ctx, job)
		if err != nil || !inserted || got.ProjectID != job.ProjectID {
			t.Fatalf("create: %+v %v %v", got, inserted, err)
		}
		got, inserted, err = worker.CreateOrGet(ctx, job)
		if err != nil || inserted || got.ID != job.ID {
			t.Fatalf("duplicate: %+v %v %v", got, inserted, err)
		}
	}
	if _, err := db.Exec(ctx, `update gitlab_groups set enabled=false where id='101'`); err != nil {
		t.Fatal(err)
	}
	leased, found, err := worker.LeaseNext(ctx, []string{"github:101"}, time.Minute)
	if err != nil || !found || leased.ID != "github" {
		t.Fatalf("GitHub lease: %+v %v %v", leased, found, err)
	}
	if _, _, err := store.CreateOrGet(ctx, Job{ID: "blocked-gitlab", ProjectID: "42", GroupID: "101", IdempotencyKey: "blocked"}); !errors.Is(err, ErrGroupDisabled) {
		t.Fatalf("GitLab group restriction lost: %v", err)
	}
	if _, err := db.Exec(ctx, `update gitlab_groups set allowed_projects=array['github:42'] where id='github:101'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(ctx, Job{ID: "blocked-github", ProjectID: "github:43", GroupID: "github:101", IdempotencyKey: "blocked-github"}); !errors.Is(err, ErrProjectNotAllowed) {
		t.Fatalf("GitHub project policy ignored: %v", err)
	}
}

func TestPostgresAnalysisIsAtomicAndJobScoped(t *testing.T) {
	store, reader, _ := postgresStores(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, _, err := store.CreateOrGet(ctx, Job{ID: id, ProjectID: "42", GroupID: "101", CommitSHA: "abc", IdempotencyKey: id}); err != nil {
			t.Fatal(err)
		}
		snap := evidence.Snapshot{ProjectID: "42", CommitSHA: "abc", Evidence: []evidence.Item{{ID: "same", Category: "application", Type: "framework", Value: "gin", SourceFile: "go.mod", CommitSHA: "abc", Confidence: evidence.Verified, Verification: "source"}}}
		if err := store.SaveAnalysis(ctx, id, snap); err != nil {
			t.Fatalf("job-scoped evidence collision: %v", err)
		}
	}
	snap, err := reader.GetAnalysis(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	snap.ProjectID = "43"
	if err := store.SaveAnalysis(ctx, "a", snap); !errors.Is(err, ErrSnapshotIdentity) {
		t.Fatalf("identity: %v", err)
	}
	snap.ProjectID = "42"
	snap.Evidence = append(snap.Evidence, snap.Evidence[0])
	if err := store.SaveAnalysis(ctx, "a", snap); err == nil {
		t.Fatal("expected duplicate evidence transaction failure")
	}
	retained, err := reader.GetAnalysis(ctx, "a")
	if err != nil || len(retained.Evidence) != 1 {
		t.Fatalf("rollback lost snapshot: %+v error=%v", retained, err)
	}
}
