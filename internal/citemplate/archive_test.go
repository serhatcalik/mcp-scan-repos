package citemplate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveCapturesExactCommitTagsWithoutChangingWorkspace(t *testing.T) {
	archive := ""
	for _, script := range templateScripts(t) {
		if strings.Contains(script, "archive_dir=$(mktemp") {
			archive = script
		}
	}
	if archive == "" {
		t.Fatal("archive script missing")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/app\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitlab-docs-release.json"), []byte("repository-owned-file"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []string{"true", "false"} {
		t.Run(enabled, func(t *testing.T) {
			metadata := `{"schema_version":1,"project_id":"42","commit_sha":"abc","git_tags":["latest","v1.2.3"]}`
			harness := `set -eu
CI_PROJECT_DIR="$PWD"
CI_PROJECT_ID=42
CI_COMMIT_SHA=abc
DOCUMENTATION_ARCHIVE=source.tar.gz
git() { printf '%s\n' 'tag-object refs/tags/v1.2.3' 'abc refs/tags/v1.2.3^{}' 'old-commit refs/tags/v0.9.0^{}' 'abc refs/tags/latest'; }
timeout() { shift; "$@"; }
jq() {
  if [ "$1" = '-R' ]; then
    actual=$(cat)
    expected=$(printf 'latest\nv1.2.3')
    test "$actual" = "$expected" || { echo 'wrong exact-commit tags' >&2; exit 11; }
    printf '["latest","v1.2.3"]\n'
  else
    printf '%s\n' "$MOCK_METADATA"
  fi
}
`
			inspect := `tar -tzf "$archive_path" > "$archive_dir/entries"
if [ "$DOCUMENTATION_GIT_TAGS_ENABLED" = true ]; then
  tar -xOf "$archive_path" .gitlab-docs-release.json
else
  if grep -q 'gitlab-docs-release.json' "$archive_dir/entries"; then exit 12; fi
  echo '{}'
fi
rm -f "$archive_dir/entries"
`
			cmd := exec.Command(shell(t), "-c", harness+"\n"+archive+"\n"+inspect)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "DOCUMENTATION_GIT_TAGS_ENABLED="+enabled, "MOCK_METADATA="+metadata)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("packaging failed: %v %s", err, out)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("invalid packaged metadata: %s %v", out, err)
			}
			if enabled == "true" && (got["project_id"] != "42" || got["commit_sha"] != "abc") {
				t.Fatal("release metadata identity lost")
			}
			data, err := os.ReadFile(filepath.Join(root, ".gitlab-docs-release.json"))
			if err != nil || string(data) != "repository-owned-file" {
				t.Fatal("pipeline changed source workspace")
			}
		})
	}
}
