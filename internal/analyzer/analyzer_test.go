package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAnalyzerFindsGoGinEvidence(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module app\nrequire github.com/gin-gonic/gin v1.0.0\nrequire gorm.io/gorm v1.0.0\n")
	write(t, filepath.Join(root, "main.go"), "package main\nfunc routes(r *gin.Engine){ r.GET(\"/health\", h) }\n")
	snap, err := New().Analyze(context.Background(), root, Metadata{ProjectID: "42", CommitSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Language != "go" || snap.Framework != "gin" {
		t.Fatalf("summary = %#v", snap)
	}
	if len(snap.Evidence) < 3 {
		t.Fatalf("evidence count = %d", len(snap.Evidence))
	}
}

func TestAnalyzerFindsSpringEvidence(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "pom.xml"), "<project><dependencies><dependency><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-web</artifactId></dependency></dependencies></project>")
	write(t, filepath.Join(root, "src", "App.java"), "@RestController class App {}")
	snap, err := New().Analyze(context.Background(), root, Metadata{ProjectID: "42", CommitSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Language != "java" {
		t.Fatalf("language = %q", snap.Language)
	}
}

func TestAnalyzerExtractsPackagesImportsAndNamedResources(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/diet\n\ngo 1.24\n\nrequire github.com/gin-gonic/gin v1.11.0\n")
	write(t, filepath.Join(root, "cmd", "api", "main.go"), "package main\nimport \"example.com/diet/internal/appointments\"\n")
	write(t, filepath.Join(root, "internal", "appointments", "service.go"), "package appointments\nimport \"net/http\"\n")
	write(t, filepath.Join(root, "internal", "appointments", "service_test.go"), "package appointments\nimport \"fake/ollama\"\n")
	write(t, filepath.Join(root, "deploy", "app.yaml"), "kind: Deployment\nmetadata:\n  name: diet-api\n---\nkind: Service\nmetadata:\n  name: diet-api-service\n")
	snap, err := New().Analyze(context.Background(), root, Metadata{ProjectID: "42", CommitSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"go_module:example.com/diet": false, "go_package:internal/appointments": false, "go_import:example.com/diet/internal/appointments": false, "resource:Deployment diet-api": false, "resource:Service diet-api-service": false}
	for _, e := range snap.Evidence {
		if e.Value == "fake/ollama" {
			t.Fatal("test dependency included in runtime architecture")
		}
		key := e.Type + ":" + e.Value
		if _, ok := want[key]; ok {
			want[key] = true
		}
		if e.Type == "go_import" && e.Value == "example.com/diet/internal/appointments" {
			if len(e.RelatedComponents) != 1 || e.RelatedComponents[0] != "cmd/api" || e.LineStart != 2 {
				t.Fatalf("incorrect import provenance: %+v", e)
			}
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing evidence %s", key)
		}
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}
