package citemplate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func templateScripts(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "gitlab-ci", "documentation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var template struct {
		Job struct {
			Script []string `yaml:"script"`
		} `yaml:"generate_documentation"`
	}
	if err := yaml.Unmarshal(data, &template); err != nil {
		t.Fatal(err)
	}
	return template.Job.Script
}

func shell(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("bash")
	if err == nil {
		return path
	}
	if runtime.GOOS == "windows" {
		path = `C:\Program Files\Git\bin\bash.exe`
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	path, err = exec.LookPath("sh")
	if err != nil {
		t.Skip("shell not available for CI script tests")
	}
	return path
}

func TestTemplateShellSyntax(t *testing.T) {
	script := strings.Join(templateScripts(t), "\n")
	if output, err := exec.Command(shell(t), "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("invalid template shell syntax: %v %s", err, output)
	}
}

func TestPollingAuthenticatesAndHandlesTerminalStates(t *testing.T) {
	poll := ""
	for _, script := range templateScripts(t) {
		if strings.Contains(script, "while true") {
			poll = script
		}
	}
	if poll == "" {
		t.Fatal("poll script missing")
	}
	for _, test := range []struct {
		status   string
		deadline string
		exit     int
	}{{"completed", "4102444800", 0}, {"failed", "4102444800", 4}, {"queued", "0", 5}} {
		t.Run(test.status, func(t *testing.T) {
			harness := `set -eu
DOCUMENTATION_API_URL=http://mock
DOCUMENTATION_HTTP_RETRIES=3
DOCUMENTATION_POLL_SECONDS=10
GITLAB_DOCS_ID_TOKEN=test-token
job_id=job
curl() {
  authenticated=false
  for arg in "$@"; do
    if [ "$arg" = 'Authorization: Bearer test-token' ]; then authenticated=true; fi
  done
  if [ "$authenticated" != true ]; then echo 'missing authentication' >&2; return 22; fi
  printf '{"status":"%s"}\n' "$MOCK_STATUS"
}

jq() { read -r payload; printf '%s\n' "$MOCK_STATUS"; }
`
			cmd := exec.Command(shell(t), "-c", harness+"\n"+poll)
			cmd.Env = append(os.Environ(), "MOCK_STATUS="+test.status, "deadline="+test.deadline)
			output, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != test.exit {
				t.Fatalf("poll exit=%d want=%d output=%s", code, test.exit, output)
			}
		})
	}
}
