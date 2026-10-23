package confluence

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gitlab-doc-platform/internal/database"
	"gitlab-doc-platform/internal/documents"
	"gitlab-doc-platform/internal/evidence"
	"gitlab-doc-platform/internal/jobs"
	publicationmeta "gitlab-doc-platform/internal/publication"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set TEST_DATABASE_URL to run Confluence publication integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("confluence_test_%d", time.Now().UnixNano())
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

func completeJob(t *testing.T, db *pgxpool.Pool, id, group string) {
	t.Helper()
	ctx := context.Background()
	store := jobs.NewPostgresStore(db)
	_, _, err := store.CreateOrGet(ctx, jobs.Job{ID: id, ProjectID: group, ProjectName: "backend/service-" + group, GroupID: group, CommitSHA: "abcdef123456", Branch: "main", IdempotencyKey: id})
	if err != nil {
		t.Fatal(err)
	}
	a := documents.ArtifactSet{TechnicalPaper: "# Technical\n\n| Component | Library |\n| --- | --- |\n| API | Gin |\n", InfrastructurePaper: "# Infrastructure\n\nKubernetes", Diagrams: map[string]string{
		"application-architecture.mmd": "flowchart LR\n A --> B\n", "infrastructure-architecture.mmd": "flowchart LR\n C --> D\n", "dependency-diagram.mmd": "flowchart LR\n E --> F\n",
	}}
	if err := store.SaveDocuments(ctx, id, a); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(ctx, id, jobs.Completed, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDurableConfluencePublicationAndIsolation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	f, c := newFakeConfluence(t, "cloud")
	c.settings.MermaidMacro = "mermaid"
	completeJob(t, db, "historical", "101")
	if err := Configure(ctx, db, c.settings); err != nil {
		t.Fatal(err)
	}
	completeJob(t, db, "new-job", "101")
	completeJob(t, db, "other-group", "102")
	r := &Runner{DB: db, Client: c, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f.failCreate = true
	if err := r.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	store := jobs.NewPostgresStore(db)
	pubs, err := store.ListPublications(ctx, "new-job")
	if err != nil || len(pubs) != 1 || pubs[0].Status != "retrying" || pubs[0].Attempts != 1 {
		t.Fatalf("durable failure: %+v %v", pubs, err)
	}
	for _, id := range []string{"historical", "other-group"} {
		pubs, err := store.ListPublications(ctx, id)
		if err != nil || len(pubs) != 0 {
			t.Fatalf("unexpected group/history publication: %s %+v %v", id, pubs, err)
		}
	}
	if _, err := db.Exec(ctx, `update publication_records set next_run_at=now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `update confluence_publication_control set next_request_at=now()`); err != nil {
		t.Fatal(err)
	}
	locked, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locked.Exec(ctx, `select pg_advisory_lock($1)`, publicationLock); err != nil {
		locked.Release()
		t.Fatal(err)
	}
	calls := f.calls
	if err := r.ProcessOne(ctx); err != nil || f.calls != calls {
		t.Fatalf("replica lock ignored: %v", err)
	}
	if _, err := locked.Exec(ctx, `select pg_advisory_unlock($1)`, publicationLock); err != nil {
		locked.Release()
		t.Fatal(err)
	}
	locked.Release()
	if err := r.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	pubs, err = store.ListPublications(ctx, "new-job")
	if err != nil || len(pubs) != 1 || pubs[0].Status != "published" || pubs[0].ProjectURL == "" || pubs[0].SnapshotURL == "" || pubs[0].Error != "" {
		t.Fatalf("incomplete publication: %+v %v", pubs, err)
	}
	if f.creates != 7 {
		t.Fatalf("ambiguous create duplicated or pages missing: %d", f.creates)
	}
	if err := r.ProcessOne(ctx); err != nil || f.creates != 7 {
		t.Fatalf("published job repeated: %v", err)
	}
	var activation time.Time
	if err := db.QueryRow(ctx, `select activated_at from confluence_targets where group_id='101'`).Scan(&activation); err != nil {
		t.Fatal(err)
	}
	disabled := c.settings
	disabled.Targets = append([]Target(nil), c.settings.Targets...)
	disabled.Targets[0].Enabled = false
	if err := Configure(ctx, db, disabled); err != nil {
		t.Fatal(err)
	}
	if err := Configure(ctx, db, c.settings); err != nil {
		t.Fatal(err)
	}
	var retained time.Time
	if err := db.QueryRow(ctx, `select activated_at from confluence_targets where group_id='101'`).Scan(&retained); err != nil || !retained.Equal(activation) {
		t.Fatalf("reenabling reset history boundary: %v", err)
	}
	changed := c.settings
	changed.Targets = append([]Target(nil), c.settings.Targets...)
	changed.Targets[0].ParentPageID = "301"
	if err := Configure(ctx, db, changed); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := db.QueryRow(ctx, `select destination_key from confluence_targets where group_id='101'`).Scan(&key); err != nil || key == c.settings.destination(c.settings.Targets[0]) {
		t.Fatalf("changed parent reused destination: %v", err)
	}
}

func TestVersionSnapshotLabelsAreDurableAndScoped(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	store := jobs.NewPostgresStore(db)
	for _, id := range []string{"version-a", "version-b", "legacy-version", "no-version"} {
		completeJob(t, db, id, "101")
		if id == "no-version" {
			continue
		}
		if err := store.SaveAnalysis(ctx, id, evidence.Snapshot{ProjectID: "101", CommitSHA: "abcdef123456", Evidence: []evidence.Item{{ID: id, Category: "application", Type: "application_version", Value: "1.0.0", SourceFile: "pom.xml", Confidence: evidence.Verified}}}); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	const target = "confluence:test-destination"
	const dateLabel = "Documentation - 2026-10-13 08:30:55 UTC"
	var firstID int64
	for _, test := range []struct{ job, want string }{
		{"version-a", "Documentation - 1.0.0"},
		{"version-b", "Documentation - 1.0.0 - Revision 2"},
		{"legacy-version", dateLabel},
		{"no-version", dateLabel},
	} {
		var id int64
		if err := conn.QueryRow(ctx, `insert into publication_records(job_id,target,status) values($1,$2,'queued') returning id`, test.job, target).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if test.job == "version-a" {
			firstID = id
		}
		if test.job == "legacy-version" {
			if _, err := conn.Exec(ctx, `insert into confluence_pages(destination_key,project_id,job_id,kind,page_id) values('test-destination','101',$1,'snapshot','999')`, test.job); err != nil {
				t.Fatal(err)
			}
		}
		got, err := publicationmeta.SnapshotLabel(ctx, conn, id, test.job, "101", target, dateLabel, "test-space")
		if err != nil || got != test.want {
			t.Fatalf("%s label=%q want=%q err=%v", test.job, got, test.want, err)
		}
		title, err := freezeSnapshotTitle(ctx, conn, publication{ID: id, ProjectName: "backend/service-101", SnapshotLabel: got})
		wantTitle := test.want
		if test.job == "legacy-version" {
			wantTitle = "backend/service-101 - " + test.want
		}
		if err != nil || title != wantTitle {
			t.Fatalf("unexpected frozen title: %q want=%q err=%v", title, wantTitle, err)
		}
		var style string
		if err := conn.QueryRow(ctx, `select details->>'document_title_style' from publication_records where id=$1`, id).Scan(&style); err != nil {
			t.Fatal(err)
		}
		wantStyle := "kind-first"
		if test.job == "legacy-version" {
			wantStyle = "snapshot-first"
		}
		if style != wantStyle {
			t.Fatalf("title style=%q want=%q", style, wantStyle)
		}
	}
	if _, err := conn.Exec(ctx, `update evidence_items set value='2.0.0' where job_id='version-a'`); err != nil {
		t.Fatal(err)
	}
	got, err := publicationmeta.SnapshotLabel(ctx, conn, firstID, "version-a", "101", target, "Documentation - changed fallback", "test-space")
	if err != nil || got != "Documentation - 1.0.0" {
		t.Fatalf("retry renamed snapshot: %q %v", got, err)
	}
	var notionID int64
	if err := conn.QueryRow(ctx, `insert into publication_records(job_id,target,status) values('version-b','notion:test-parent','queued') returning id`).Scan(&notionID); err != nil {
		t.Fatal(err)
	}
	got, err = publicationmeta.SnapshotLabel(ctx, conn, notionID, "version-b", "101", "notion:test-parent", dateLabel, "")
	if err != nil || got != "Documentation - 1.0.0" {
		t.Fatalf("destinations share revision counters: %q %v", got, err)
	}
	completeJob(t, db, "other-project", "102")
	if err := store.SaveAnalysis(ctx, "other-project", evidence.Snapshot{ProjectID: "102", CommitSHA: "abcdef123456", Evidence: []evidence.Item{{ID: "other", Category: "application", Type: "application_version", Value: "1.0.0", SourceFile: "pom.xml", Confidence: evidence.Verified}}}); err != nil {
		t.Fatal(err)
	}
	var otherID int64
	if err := conn.QueryRow(ctx, `insert into publication_records(job_id,target,status) values('other-project','confluence:other-destination','queued') returning id`).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	got, err = publicationmeta.SnapshotLabel(ctx, conn, otherID, "other-project", "102", "confluence:other-destination", dateLabel, "test-space")
	if err != nil || got != "Documentation - 1.0.0 - Revision 3" {
		t.Fatalf("same-space projects collide: %q %v", got, err)
	}
}
