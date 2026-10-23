package documents

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"gitlab-doc-platform/internal/ai"
	"gitlab-doc-platform/internal/analyzer"
	"gitlab-doc-platform/internal/evidence"
)

type mockProvider struct{}

func (mockProvider) Generate(context.Context, ai.Request) (ai.Response, error) {
	return ai.Response{Text: `{"summaries":[],"recommendations":[]}`}, nil
}
func (mockProvider) Name() string  { return "mock" }
func (mockProvider) Model() string { return "mock-model" }

func TestGeneratorCreatesRequiredArtifacts(t *testing.T) {
	snap := evidence.Snapshot{ProjectID: "42", CommitSHA: "abc", Language: "go", Framework: "gin"}
	artifacts, err := NewGenerator(mockProvider{}).Generate(context.Background(), snap, Metadata{ProjectID: "42", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(artifacts.TechnicalPaper, "## Verified Facts") {
		t.Fatalf("technical paper missing required section")
	}
	if artifacts.Diagrams["application-architecture.mmd"] == "" || len(artifacts.EvidenceJSON) == 0 || len(artifacts.MetadataJSON) == 0 {
		t.Fatalf("missing generated artifacts")
	}
}

func TestDiagramsUseTargetEvidenceAndOnlyVerifiedEdges(t *testing.T) {
	snap := evidence.Snapshot{Evidence: []evidence.Item{
		{Type: "go_module", Value: "example.com/diet", Confidence: evidence.Verified, SourceFile: "go.mod"},
		{Type: "go_package", Value: "cmd/api", Confidence: evidence.Verified, SourceFile: "cmd/api/main.go"},
		{Type: "go_package", Value: "internal/appointments", Confidence: evidence.Verified, SourceFile: "internal/appointments/service.go"},
		{Type: "go_import", Value: "example.com/diet/internal/appointments", Confidence: evidence.Verified, SourceFile: "cmd/api/main.go", RelatedComponents: []string{"cmd/api"}},
		{Type: "go_import", Value: "net/http", Confidence: evidence.Verified, SourceFile: "internal/appointments/service.go", RelatedComponents: []string{"internal/appointments"}},
		{Category: "infrastructure", Type: "resource", Value: "Deployment diet-api", Confidence: evidence.Verified, SourceFile: "deploy/app.yaml"},
		{Category: "infrastructure", Type: "resource", Value: "Ollama", Confidence: evidence.Inferred, SourceFile: "guess.yaml"},
	}}
	app, infra, deps := appDiagram(snap), infraDiagram(snap), dependencyDiagram(snap)
	if !strings.Contains(app, "internal/appointments") || strings.Count(app, "-->|imports|") != 1 {
		t.Fatalf("incorrect application graph: %s", app)
	}
	if !strings.Contains(deps, "net/http") || strings.Count(deps, "-->|imports|") != 2 {
		t.Fatalf("incorrect dependency graph: %s", deps)
	}
	if !strings.Contains(infra, "Deployment diet-api") || strings.Contains(infra, "-->") {
		t.Fatalf("invented infrastructure relationship: %s", infra)
	}
	for _, diagram := range []string{app, infra, deps, appDiagram(evidence.Snapshot{}), infraDiagram(evidence.Snapshot{}), dependencyDiagram(evidence.Snapshot{})} {
		for _, placeholder := range []string{"Ollama", "Documentation API", "GitLab Pipeline", "Static Analyzer", "Source Archive", "Traefik"} {
			if strings.Contains(diagram, placeholder) {
				t.Errorf("platform placeholder %q in target diagram: %s", placeholder, diagram)
			}
		}
	}
	if !strings.Contains(infraDiagram(evidence.Snapshot{}), "No supported repository evidence") {
		t.Fatal("missing unavailable fallback")
	}
	for i, j := 0, len(snap.Evidence)-1; i < j; i, j = i+1, j-1 {
		snap.Evidence[i], snap.Evidence[j] = snap.Evidence[j], snap.Evidence[i]
	}
	if appDiagram(snap) != app || infraDiagram(snap) != infra || dependencyDiagram(snap) != deps {
		t.Fatal("diagram depends on evidence order")
	}
}

func TestMermaidEscapesRepositoryLabels(t *testing.T) {
	label := mermaidLabel("name\"]\nInjected[evil] %%")
	if strings.ContainsAny(label, "\"[]\n%") {
		t.Fatalf("unsafe label: %s", label)
	}
}

func TestSampleRepositoriesProduceTargetArtifacts(t *testing.T) {
	for _, sample := range []string{"go-app", "spring-app"} {
		t.Run(sample, func(t *testing.T) {
			snap, err := analyzer.New().Analyze(context.Background(), filepath.Join("..", "analyzer", "testdata", sample), analyzer.Metadata{ProjectID: "42", CommitSHA: "abc"})
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Warnings) != 0 {
				t.Fatalf("sample analysis warnings: %v", snap.Warnings)
			}
			artifacts, err := NewGenerator(mockProvider{}).Generate(context.Background(), snap, Metadata{ProjectID: "42", ProjectName: sample})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(artifacts.TechnicalPaper, "/appointments") || !strings.Contains(artifacts.Diagrams["application-architecture.mmd"], "Appointment") {
				t.Fatalf("missing target details: %+v", artifacts.Diagrams)
			}
			if sample == "go-app" && !strings.Contains(artifacts.Diagrams["infrastructure-architecture.mmd"], "-->|references|") {
				t.Fatal("verified ingress/service reference missing")
			}
		})
	}
}
