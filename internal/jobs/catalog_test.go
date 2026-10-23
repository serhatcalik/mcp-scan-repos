package jobs

import (
	"context"
	"errors"
	"testing"

	"gitlab-doc-platform/internal/documents"
)

func TestPostgresCatalogScopeAndDocumentPersistence(t *testing.T) {
	store, reader, db := postgresStores(t)
	ctx := context.Background()
	for _, j := range []Job{
		{ID: "a", ProjectID: "42", ProjectName: "group/first", GroupID: "101", IdempotencyKey: "a"},
		{ID: "b", ProjectID: "42", ProjectName: "group/renamed", GroupID: "101", IdempotencyKey: "b"},
		{ID: "private", ProjectID: "43", ProjectName: "group/private", GroupID: "101", IdempotencyKey: "private"},
	} {
		if _, _, err := store.CreateOrGet(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `update documentation_jobs set created_at=now()-interval '1 day' where id='a'`); err != nil {
		t.Fatal(err)
	}
	projects, err := reader.ListProjects(ctx, []string{"42"}, 100, 0)
	if err != nil || len(projects) != 1 || projects[0].ID != "42" || projects[0].Name != "group/renamed" {
		t.Fatalf("project scope/latest identity: %+v %v", projects, err)
	}
	list, err := reader.ListJobs(ctx, []string{"42"}, "", 1, 1)
	if err != nil || len(list) != 1 || list[0].ID != "a" {
		t.Fatalf("job scope and pagination: %+v %v", list, err)
	}
	list, err = reader.ListJobs(ctx, []string{"42"}, "43", 100, 0)
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-project filter: %+v %v", list, err)
	}
	a := documents.ArtifactSet{TechnicalPaper: "technical", InfrastructurePaper: "infrastructure", Diagrams: map[string]string{
		"application-architecture.mmd": "app", "infrastructure-architecture.mmd": "infra", "dependency-diagram.mmd": "deps",
	}}
	if err := store.SaveDocuments(ctx, "a", a); err != nil {
		t.Fatal(err)
	}
	for kind, expected := range documentContents(a) {
		got, err := reader.GetDocument(ctx, "a", kind)
		if err != nil || got != expected {
			t.Fatalf("persisted %s: %q %v", kind, got, err)
		}
	}
	a.TechnicalPaper = "replaced"
	if err := store.SaveDocuments(ctx, "a", a); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(ctx, `select count(*) from generated_documents where job_id='a'`).Scan(&count); err != nil || count != 5 {
		t.Fatalf("replacement must not duplicate documents: %d %v", count, err)
	}
	if got, err := reader.GetDocument(ctx, "a", "technical"); err != nil || got != "replaced" {
		t.Fatalf("replacement: %q %v", got, err)
	}
	if _, err := reader.GetDocument(ctx, "b", "technical"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("documents leaked across jobs: %v", err)
	}
	if err := store.SaveDocuments(ctx, "missing", a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job: %v", err)
	}
}
