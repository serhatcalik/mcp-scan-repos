package analyzer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubWorkflowEvidenceOmitsScriptsAndSecrets(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".github", "workflows", "ci.yml"), `name: CI
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      ACCESS_TOKEN: must-not-enter-evidence
    steps:
      - run: echo must-not-execute-or-store
  publish:
    uses: example/private/.github/workflows/publish.yml@main
`)
	snap, err := New().Analyze(context.Background(), root, Metadata{ProjectID: "github:42", CommitSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"github actions": false, "test": false, "publish": false}
	for _, e := range snap.Evidence {
		if _, ok := want[e.Value]; ok {
			want[e.Value] = true
		}
	}
	for value, found := range want {
		if !found {
			t.Errorf("missing GitHub CI evidence: %s", value)
		}
	}
	data, _ := json.Marshal(snap)
	if strings.Contains(string(data), "must-not-") {
		t.Fatalf("script or secret entered snapshot: %s", data)
	}
}
