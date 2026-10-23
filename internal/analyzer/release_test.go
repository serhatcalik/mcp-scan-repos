package analyzer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCIReleaseMetadataIsBoundToProjectAndCommit(t *testing.T) {
	for _, test := range []struct {
		name, project, commit string
		tags                  []string
		version               string
		malformed             bool
	}{
		{"exact tag", "42", "abc", []string{"v1.2.3"}, "v1.2.3", false},
		{"non-release names ignored", "42", "abc", []string{"latest", "v1.2.3"}, "v1.2.3", false},
		{"other project", "99", "abc", []string{"v1.2.3"}, "", false},
		{"other commit", "42", "older", []string{"v1.2.3"}, "", false},
		{"untagged", "42", "abc", nil, "", false},
		{"malformed", "42", "abc", nil, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			data, err := json.Marshal(map[string]any{"schema_version": 1, "project_id": test.project, "commit_sha": test.commit, "git_tags": test.tags})
			if err != nil {
				t.Fatal(err)
			}
			if test.malformed {
				data = []byte(`{"secret":"do-not-execute"}`)
			}
			if err := os.WriteFile(filepath.Join(root, ".gitlab-docs-release.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			snap, err := New().Analyze(context.Background(), root, Metadata{ProjectID: "42", CommitSHA: "abc"})
			if err != nil {
				t.Fatal(err)
			}
			version := ""
			for _, e := range snap.Evidence {
				if e.Type == "application_version" {
					version = e.Value
					if e.SourceFile != ".gitlab-docs-release.json" || !strings.Contains(e.Verification, "CI-declared") {
						t.Fatal("tag provenance misrepresented")
					}
				}
			}
			if version != test.version {
				t.Fatalf("version=%q want=%q", version, test.version)
			}
		})
	}
}
