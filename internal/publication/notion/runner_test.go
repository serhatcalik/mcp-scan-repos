package notion

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gitlab-doc-platform/internal/database"
	"gitlab-doc-platform/internal/documents"
	"gitlab-doc-platform/internal/jobs"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set TEST_DATABASE_URL to run Notion publication integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("notion_test_%d", time.Now().UnixNano())
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
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	db, err := database.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestPublicationRateLimitsAndPermissionFailures(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			db := testDB(t)
			ctx := context.Background()
			if err := Configure(ctx, db, []Target{{GroupID: "101", ParentPageID: testParent, Enabled: true}}); err != nil {
				t.Fatal(err)
			}
			completeJob(t, db, "rate-job", "101")
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(code)
				fmt.Fprint(w, `{"message":"must-not-log-secret"}`)
			}))
			defer server.Close()
			client := NewClient("secret-test-token")
			client.endpoint = server.URL
			runner := &Runner{DB: db, Client: client}
			if err := runner.ProcessOne(ctx); err != nil {
				t.Fatal(err)
			}
			publications, err := jobs.NewPostgresStore(db).ListPublications(ctx, "rate-job")
			if err != nil || len(publications) != 1 {
				t.Fatalf("result: %v %+v", err, publications)
			}
			want := "failed"
			if code == http.StatusTooManyRequests {
				want = "retrying"
				var cooldown bool
				if err := db.QueryRow(ctx, `select next_request_at>now()+interval '110 seconds' from notion_publication_control where id=true`).Scan(&cooldown); err != nil || !cooldown {
					t.Fatalf("shared cooldown missing: %v", err)
				}
			}
			if publications[0].Status != want || publications[0].Error != fmt.Sprintf("Notion API returned HTTP %d; operation=list_blocks", code) {
				t.Fatalf("classification/secret exposure: %+v", publications[0])
			}
			if err := runner.ProcessOne(ctx); err != nil || calls != 1 {
				t.Fatalf("blocked or permanent error retried: calls=%d err=%v", calls, err)
			}
			// Explicitly test the durable retry bound without making more HTTP calls.
			if _, err := db.Exec(ctx, `update publication_records set status='publishing',attempt_count=10,next_run_at=now() where job_id='rate-job'`); err != nil {
				t.Fatal(err)
			}
			if err := runner.ProcessOne(ctx); err != nil {
				t.Fatal(err)
			}
			publications, err = jobs.NewPostgresStore(db).ListPublications(ctx, "rate-job")
			if err != nil || len(publications) != 1 || publications[0].Status != "failed" || calls != 1 {
				t.Fatalf("retry limit ignored: %v %+v", err, publications)
			}
		})
	}
}

func completeJob(t *testing.T, db *pgxpool.Pool, id, group string) {
	t.Helper()
	ctx := context.Background()
	store := jobs.NewPostgresStore(db)
	_, _, err := store.CreateOrGet(ctx, jobs.Job{ID: id, ProjectID: "42", ProjectName: "backend/service", GroupID: group, CommitSHA: "abcdef123456", Branch: "main", IdempotencyKey: id})
	if err != nil {
		t.Fatal(err)
	}
	a := documents.ArtifactSet{TechnicalPaper: "# Technical\n## Verified Facts\nGo application", InfrastructurePaper: "# Infrastructure\n## Verified Facts\nKubernetes", Diagrams: map[string]string{"application-architecture.mmd": "flowchart LR\n  n0[\"Application\"]\n", "infrastructure-architecture.mmd": "flowchart LR\n  n0[\"Deployment\"]\n", "dependency-diagram.mmd": "flowchart LR\n  n0[\"Dependencies\"]\n"}}
	if err := store.SaveDocuments(ctx, id, a); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(ctx, id, jobs.Completed, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDurablePublicationRetriesIsolationAndReplicaCoordination(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	// Old jobs are not automatically exported when a target is first enabled.
	completeJob(t, db, "historical", "101")
	targets := []Target{{GroupID: "101", ParentPageID: testParent, Enabled: true}}
	if err := Configure(ctx, db, targets); err != nil {
		t.Fatal(err)
	}
	completeJob(t, db, "new-job", "101")
	completeJob(t, db, "other-group", "102")
	fake, client := newFakeNotion(t)
	runner := &Runner{DB: db, Client: client, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	fake.failCreate = true
	if err := runner.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	pubs, err := jobs.NewPostgresStore(db).ListPublications(ctx, "new-job")
	if err != nil || len(pubs) != 1 || pubs[0].Status != "retrying" || pubs[0].Attempts != 1 {
		t.Fatalf("durable failure: %+v %v", pubs, err)
	}
	if _, err := db.Exec(ctx, `update publication_records set next_run_at=now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `update notion_publication_control set next_request_at=now()`); err != nil {
		t.Fatal(err)
	}
	// Another worker cannot publish while the first session owns coordination.
	locked, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locked.Exec(ctx, `select pg_advisory_lock($1)`, publicationLock); err != nil {
		t.Fatal(err)
	}
	calls := fake.calls
	if err := runner.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.calls != calls {
		t.Fatal("replica ignored advisory lock")
	}
	if _, err := locked.Exec(ctx, `select pg_advisory_unlock($1)`, publicationLock); err != nil {
		t.Fatal(err)
	}
	locked.Release()
	if err := runner.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	pubs, err = jobs.NewPostgresStore(db).ListPublications(ctx, "new-job")
	if err != nil || len(pubs) != 1 || pubs[0].Status != "published" || pubs[0].ProjectURL == "" || pubs[0].SnapshotURL == "" || pubs[0].Error != "" {
		t.Fatalf("publication incomplete: %+v %v", pubs, err)
	}
	if fake.creates != 7 {
		t.Fatalf("duplicate or missing hierarchy: %d", fake.creates)
	}
	for _, id := range []string{"historical", "other-group"} {
		pubs, err := jobs.NewPostgresStore(db).ListPublications(ctx, id)
		if err != nil || len(pubs) != 0 {
			t.Fatalf("unauthorized/backfilled job published: %s %+v %v", id, pubs, err)
		}
	}
	calls = fake.calls
	if err := runner.ProcessOne(ctx); err != nil || fake.calls != calls {
		t.Fatal("completed publication replayed")
	}
	// A second commit receives its own snapshot under the same stable project URL.
	completeJob(t, db, "second-job", "101")
	if _, err := db.Exec(ctx, `update notion_publication_control set next_request_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err := runner.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := jobs.NewPostgresStore(db).ListPublications(ctx, "second-job")
	if err != nil || len(second) != 1 || second[0].Status != "published" || second[0].ProjectURL == "" {
		t.Fatalf("second publication: %+v %v", second, err)
	}
	first, _ := jobs.NewPostgresStore(db).ListPublications(ctx, "new-job")
	if second[0].ProjectURL != first[0].ProjectURL || second[0].SnapshotURL == first[0].SnapshotURL || fake.creates != 13 {
		t.Fatal("project/snapshot hierarchy not stable")
	}
	// Removal from trusted config disables the target without deleting past pages.
	if err := Configure(ctx, db, []Target{}); err != nil {
		t.Fatal(err)
	}
	completeJob(t, db, "disabled-job", "101")
	calls = fake.calls
	if err := runner.ProcessOne(ctx); err != nil || fake.calls != calls {
		t.Fatal("disabled target published")
	}
}
