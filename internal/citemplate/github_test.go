package citemplate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"gitlab-doc-platform/internal/archive"
)

func githubScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "github-actions", "documentation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["documentation"]
	if len(workflow.Permissions) != 0 || len(job.Permissions) != 2 || job.Permissions["contents"] != "read" || job.Permissions["id-token"] != "write" {
		t.Fatal("unexpected workflow permissions")
	}
	for _, step := range job.Steps {
		if step.Run != "" {
			return step.Run
		}
	}
	t.Fatal("workflow script missing")
	return ""
}

func TestGitHubWorkflowRefreshesTokensAndArchivesExactCommit(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is required for GitHub workflow archive tests")
	}
	script := githubScript(t)
	if output, err := exec.Command(shell(t), "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("syntax: %v %s", err, output)
	}
	for _, test := range []struct {
		name, status, timeout string
		code, requests        int
	}{
		{"completed", "progress", "5400", 0, 4},
		{"failed", "failed", "5400", 4, 2},
		{"cancelled", "cancelled", "5400", 4, 2},
		{"timeout", "queued", "0", 5, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			for name, content := range map[string]string{
				"go.mod":                    "module example.com/app\n",
				".gitlab-docs-release.json": "repository-supplied-metadata",
				".gitattributes":            "private.txt export-ignore\n",
				"private.txt":               "must-not-upload",
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			git := func(args ...string) string {
				cmd := exec.Command(gitPath, args...)
				cmd.Dir = root
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			git("init")
			git("config", "core.autocrlf", "false")
			git("add", ".")
			git("-c", "user.name=CI test", "-c", "user.email=ci@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
			git("tag", "v1.2.3")
			sha := git("rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("must-not-upload"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("uncommitted-modification"), 0600); err != nil {
				t.Fatal(err)
			}
			harness := `
sleep() { :; }
jq() {
  case "$1" in
    -rn) printf 'docs\n' ;;
    -R) actual=$(cat); test "$actual" = 'v1.2.3' || return 20; printf '["v1.2.3"]\n' ;;
    -n) printf '{"schema_version":1,"project_id":"github:42","commit_sha":"%s","git_tags":["v1.2.3"]}\n' "$GITHUB_SHA" ;;
    -er)
      case "$2" in
        .value*) cat ;;
        .job_id*) printf 'job-1\n' ;;
        .status) cat "$3" ;;
        *) return 21 ;;
      esac ;;
    *) return 22 ;;
  esac
}
curl() {
  case "$*" in
    *https://oidc.example/*)
      count=0
      if [ -f token-count ]; then count=$(cat token-count); fi
      count=$((count + 1))
      printf '%s\n' "$count" > token-count
      printf 'token-%s\n' "$count"
      return ;;
  esac
  authenticated=false
  output=''
  archive=''
  endpoint=''
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --header) shift; if [ "$1" = "Authorization: Bearer token-$(cat token-count)" ]; then authenticated=true; fi ;;
      --output) shift; output=$1 ;;
      --form) shift; case "$1" in archive=@*) archive=${1#archive=@} ;; esac ;;
      https://platform.example/*) endpoint=$1 ;;
    esac
    shift
  done
  test "$authenticated" = true || return 23
  test -n "$output" || return 24
  case "$endpoint" in
    */github/jobs)
      cp "$archive" uploaded.tar.gz
      printf '{"job_id":"job-1"}\n' > "$output" ;;
    */github/jobs/job-1)
      status=$MOCK_STATUS
      if [ "$status" = progress ]; then
        if [ -f polled ]; then status=completed; else status=queued; touch polled; fi
      fi
      printf '%s\n' "$status" > "$output" ;;
    */github/jobs/job-1/artifacts) printf 'artifact-bundle' > "$output" ;;
    *) return 25 ;;
  esac
}
`
			cmd := exec.Command(shell(t), "-c", harness+"\n"+script)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "DOCUMENTATION_API_URL=https://platform.example", "DOCUMENTATION_AUDIENCE=docs", "DOCUMENTATION_PROJECT_ID=42", "DOCUMENTATION_TIMEOUT_SECONDS="+test.timeout, "DOCUMENTATION_POLL_SECONDS=0", "GITHUB_SHA="+sha, "ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-secret", "ACTIONS_ID_TOKEN_REQUEST_URL=https://oidc.example/token?request=1", "MOCK_STATUS="+test.status)
			output, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != test.code {
				t.Fatalf("exit=%d want=%d output=%s", code, test.code, output)
			}
			count, err := os.ReadFile(filepath.Join(root, "token-count"))
			if err != nil || strings.TrimSpace(string(count)) != strconv.Itoa(test.requests) {
				t.Fatalf("tokens not refreshed: %s %v", count, err)
			}
			if test.code != 0 {
				return
			}
			data, err := os.ReadFile(filepath.Join(root, "uploaded.tar.gz"))
			if err != nil {
				t.Fatal(err)
			}
			if err := archive.ExtractTarGz(bytes.NewReader(data), filepath.Join(root, "extracted"), archive.Limits{MaxArchiveBytes: 1 << 20, MaxExtractedBytes: 1 << 20, MaxFiles: 100}); err != nil {
				t.Fatalf("workflow archive rejected by platform extractor: %v", err)
			}
			gz, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			tr := tar.NewReader(gz)
			metadata, source := 0, 0
			for {
				header, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
				switch header.Name {
				case ".gitlab-docs-release.json":
					metadata++
					var declaration struct {
						ProjectID string `json:"project_id"`
						CommitSHA string `json:"commit_sha"`
					}
					if err := json.Unmarshal(body, &declaration); err != nil || declaration.ProjectID != "github:42" || declaration.CommitSHA != sha {
						t.Fatalf("release identity: %s %v", body, err)
					}
				case "go.mod":
					source++
					if string(body) != "module example.com/app\n" {
						t.Fatalf("wrong commit content: %s", body)
					}
				case "private.txt", "untracked.txt":
					t.Fatalf("excluded file uploaded: %s", header.Name)
				}
				if strings.HasPrefix(header.Name, ".git/") {
					t.Fatal("git credentials uploaded")
				}
			}
			if metadata != 1 || source != 1 {
				t.Fatalf("metadata=%d source=%d", metadata, source)
			}
			bundle, err := os.ReadFile(filepath.Join(root, "generated-docs", "artifacts.tar.gz"))
			if err != nil || string(bundle) != "artifact-bundle" {
				t.Fatalf("artifact download: %s %v", bundle, err)
			}
		})
	}
}
