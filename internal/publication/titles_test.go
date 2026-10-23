package publication

import (
	"gitlab-doc-platform/internal/evidence"
	"testing"
)

func versionItem(source, value string) evidence.Item {
	return evidence.Item{Category: "application", Type: "application_version", Value: value, SourceFile: source, Confidence: evidence.Verified}
}

func TestVerifiedApplicationVersionSelection(t *testing.T) {
	for _, test := range []struct {
		name  string
		items []evidence.Item
		want  string
	}{
		{"absent", nil, ""},
		{"root manifest", []evidence.Item{versionItem("pom.xml", "1.0.0")}, "1.0.0"},
		{"Go declaration", []evidence.Item{versionItem("version.go", "v1.2.3")}, "v1.2.3"},
		{"Git tag", []evidence.Item{versionItem(".gitlab-docs-release.json", "v1.2.3")}, "v1.2.3"},
		{"Git tag beats source fallback", []evidence.Item{versionItem(".gitlab-docs-release.json", "v1.2.3"), versionItem("version.go", "1.0.0")}, "v1.2.3"},
		{"conflicting Git tags", []evidence.Item{versionItem(".gitlab-docs-release.json", "v1.2.3"), versionItem(".gitlab-docs-release.json", "v2.0.0")}, ""},
		{"Go command declaration", []evidence.Item{versionItem("cmd/api/main.go", "1.2.3")}, "1.2.3"},
		{"Go conflicting commands", []evidence.Item{versionItem("cmd/api/main.go", "1.2.3"), versionItem("cmd/worker/main.go", "2.0.0")}, ""},
		{"Go nested dependency", []evidence.Item{versionItem("internal/other/version.go", "9.9.9")}, ""},
		{"Go explicit precedence", []evidence.Item{versionItem("VERSION", "2.0.0"), versionItem("version.go", "1.2.3")}, "2.0.0"},
		{"explicit precedence", []evidence.Item{versionItem("VERSION", "v2.0.0"), versionItem("pom.xml", "1.0.0")}, "v2.0.0"},
		{"agreeing manifests", []evidence.Item{versionItem("pom.xml", "1.0.0"), versionItem("package.json", "1.0.0")}, "1.0.0"},
		{"conflicting manifests", []evidence.Item{versionItem("pom.xml", "1.0.0"), versionItem("package.json", "2.0.0")}, ""},
		{"conflicting explicit", []evidence.Item{versionItem("VERSION", "1.0.0"), versionItem("version.txt", "2.0.0")}, ""},
		{"nested", []evidence.Item{versionItem("frontend/package.json", "3.0.0")}, ""},
		{"unsafe", []evidence.Item{versionItem("VERSION", "1.0.0\nsecret")}, ""},
		{"secret", []evidence.Item{versionItem("VERSION", "1.0.0-glpat-abcdefghijklmnopqrst")}, ""},
		{"inferred", []evidence.Item{{Category: "application", Type: "application_version", Value: "1.0.0", SourceFile: "pom.xml", Confidence: evidence.Inferred}}, ""},
		{"dependency", []evidence.Item{{Category: "dependency", Type: "application_version", Value: "1.0.0", SourceFile: "pom.xml", Confidence: evidence.Verified}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ApplicationVersion(test.items); got != test.want {
				t.Fatalf("version=%q want=%q", got, test.want)
			}
		})
	}
}

func TestRepeatedVersionsGetDistinctRevisionLabels(t *testing.T) {
	base := "Documentation - 1.0.0"
	used := map[string]bool{}
	for _, want := range []string{base, base + " - Revision 2", base + " - Revision 3"} {
		got := revisionLabel(base, used)
		if got != want {
			t.Fatalf("label=%q want=%q", got, want)
		}
		used[got] = true
	}
}
