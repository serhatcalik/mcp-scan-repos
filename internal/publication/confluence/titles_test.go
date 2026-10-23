package confluence

import (
	"testing"
	"time"
)

func TestSnapshotVersionOrDateTitle(t *testing.T) {
	p := publication{ProjectName: "test4644041/java-app", CompletedAt: time.Date(2026, 10, 13, 8, 30, 55, 0, time.UTC)}
	if snapshotTitle(p) != "Documentation - 2026-10-13 08:30:55 UTC" {
		t.Fatal("date fallback changed")
	}
	p.SnapshotLabel = "Documentation - 1.0.0"
	if snapshotTitle(p) != "Documentation - 1.0.0" {
		t.Fatal("version label ignored")
	}
	p.SnapshotLabel = "Documentation - 1.0.0 - Revision 2"
	if snapshotTitle(p) != "Documentation - 1.0.0 - Revision 2" {
		t.Fatal("version revision ignored")
	}
	p.Title = "test4644041/java-app - Documentation - 1.0.0"
	if snapshotTitle(p) != p.Title {
		t.Fatal("frozen legacy title ignored")
	}
}

func TestDocumentTitlesLeadWithDocumentKind(t *testing.T) {
	p := publication{ProjectName: "test4644041/java-app", SnapshotLabel: "Documentation - 1.0.0", DocumentTitleStyle: "kind-first"}
	if got := documentTitle(p, "Technical Paper"); got != "Technical Paper - 1.0.0" {
		t.Fatalf("unexpected child title: %s", got)
	}
	p.SnapshotLabel = "Documentation - 1.0.0 - Revision 2"
	if got := documentTitle(p, "Application Architecture"); got != "Application Architecture - 1.0.0 - Revision 2" {
		t.Fatalf("revision lost: %s", got)
	}
	p.SnapshotLabel = ""
	p.CompletedAt = time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	if got := documentTitle(p, "Technical Paper"); got != "Technical Paper - 2026-10-14 08:00:00 UTC" {
		t.Fatalf("date fallback lost: %s", got)
	}
	p.DocumentTitleStyle = ""
	p.Title = "test4644041/java-app - Documentation - 1.0.0"
	if got := documentTitle(p, "Technical Paper"); got != p.Title+" - Technical Paper" {
		t.Fatal("old child page title changed during retry")
	}
}
