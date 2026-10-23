package notion

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestHumanReadableTitlesUseProjectPathAndCompletionTime(t *testing.T) {
	p := publication{ProjectID: "51703321", ProjectName: "serhatcalik/diyetapi", CreatedAt: time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC), CompletedAt: time.Date(2026, 10, 12, 22, 8, 0, 0, time.FixedZone("Istanbul", 3*60*60))}
	if projectTitle(p) != "serhatcalik/diyetapi" || snapshotTitle(p) != "Documentation - 2026-10-12 19:08:00 UTC" {
		t.Fatalf("unexpected titles: %s / %s", projectTitle(p), snapshotTitle(p))
	}
	p.ProjectName = ""
	if projectTitle(p) != "GitLab Project 51703321" {
		t.Fatal("missing project name fallback")
	}
	p.CompletedAt = time.Time{}
	if snapshotTitle(p) != "Documentation - 2026-10-12 18:00:00 UTC" {
		t.Fatal("missing timestamp fallback")
	}
	p.ProjectName = "path\n glpat-abcdefghijklmnopqrst"
	if strings.Contains(projectTitle(p), "glpat-") || strings.Contains(projectTitle(p), "\n") {
		t.Fatal("unsafe title")
	}
}

func TestSnapshotVersionLabel(t *testing.T) {
	p := publication{SnapshotLabel: "Documentation - 1.0.0"}
	if snapshotTitle(p) != "Documentation - 1.0.0" {
		t.Fatal("version label ignored")
	}
	p.SnapshotLabel = "Documentation - 1.0.0 - Revision 2"
	if snapshotTitle(p) != p.SnapshotLabel {
		t.Fatal("version revision ignored")
	}
}

func TestSameDateTitlesKeepDistinctIdentityAndRecoverCommittedCreate(t *testing.T) {
	f, c := newFakeNotion(t)
	ctx := context.Background()
	title := "Documentation - 2026-10-12 19:08:00 UTC"
	f.failCreate = true
	if _, err := c.ensurePage(ctx, testParent, title, "Documentation job ID: a", ""); err == nil {
		t.Fatal("expected ambiguous create")
	}
	a, err := c.ensurePage(ctx, testParent, title, "Documentation job ID: a", "")
	if err != nil || f.creates != 1 {
		t.Fatalf("duplicate create: %v", err)
	}
	b, err := c.ensurePage(ctx, testParent, title, "Documentation job ID: b", "")
	if err != nil || a == b || f.creates != 2 {
		t.Fatalf("same-second jobs conflated: %s %s %v", a, b, err)
	}
	for _, test := range []struct{ identity, id string }{{"Documentation job ID: a", a}, {"Documentation job ID: b", b}} {
		got, err := c.ensurePage(ctx, testParent, title, test.identity, "")
		if err != nil || got != test.id || f.creates != 2 {
			t.Fatalf("identity recovery: %s %v", got, err)
		}
	}
}

func TestLegacyTitleRenamePreservesPageIdentityContentAndCustomNames(t *testing.T) {
	f, c := newFakeNotion(t)
	ctx := context.Background()
	id, err := c.EnsurePage(ctx, testParent, "GitLab Project 42")
	if err != nil {
		t.Fatal(err)
	}
	f.pages[id] = []block{textBlock("paragraph", "Human notes")}
	if err := c.renameLegacyPage(ctx, id, "GitLab Project 42", "backend/service"); err != nil {
		t.Fatal(err)
	}
	if f.pages[testParent][0].ID != id || f.pages[testParent][0].ChildPage.Title != "backend/service" || f.pages[id][0].text() != "Human notes" {
		t.Fatal("rename changed URL or content")
	}
	f.pages[testParent][0].ChildPage.Title = "Team Documentation"
	if err := c.renameLegacyPage(ctx, id, "GitLab Project 42", "backend/renamed"); err != nil || f.pages[testParent][0].ChildPage.Title != "Team Documentation" {
		t.Fatal("custom title overwritten")
	}
}
