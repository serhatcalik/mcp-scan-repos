package documents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gitlab-doc-platform/internal/ai"
	"gitlab-doc-platform/internal/evidence"
)

type providerFunc func(context.Context, ai.Request) (ai.Response, error)

func (p providerFunc) Generate(ctx context.Context, req ai.Request) (ai.Response, error) {
	return p(ctx, req)
}
func (providerFunc) Name() string  { return "mock" }
func (providerFunc) Model() string { return "test" }

func promptRecords(t *testing.T, req ai.Request) []promptRecord {
	t.Helper()
	parts := strings.Split(req.UserPrompt, "\nRepository evidence JSON:\n")
	if len(parts) != 2 {
		t.Fatal("missing evidence input")
	}
	var records []promptRecord
	if err := json.Unmarshal([]byte(parts[1]), &records); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestPromptBudgetIsBoundedGroupedAndReproducible(t *testing.T) {
	snap := evidence.Snapshot{}
	for i := 0; i < 1000; i++ {
		snap.Evidence = append(snap.Evidence, evidence.Item{ID: fmt.Sprintf("id-%d", i), Category: "api", Type: "route", Value: "GET /appointments/" + fmt.Sprint(i), SourceFile: fmt.Sprintf("internal/component%d/routes.go", i%4), Confidence: evidence.Verified})
	}
	opts := Options{ContextLength: 8192, MaxTokens: 1800, Temperature: 0.1}
	first, selected, err := buildPrompt(snap, "technical", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) == 0 || len(selected) >= len(snap.Evidence) || len(first.SystemPrompt)+len(first.UserPrompt) > opts.ContextLength-opts.MaxTokens-1024 {
		t.Fatalf("unbounded or empty prompt: items=%d bytes=%d", len(selected), len(first.SystemPrompt)+len(first.UserPrompt))
	}
	if !json.Valid(first.JSONSchema) {
		t.Fatal("invalid response schema")
	}
	groups := map[string]bool{}
	for _, record := range promptRecords(t, first) {
		groups[record.Component] = true
	}
	if len(groups) != 4 {
		t.Fatalf("component starvation: %v", groups)
	}
	for i, j := 0, len(snap.Evidence)-1; i < j; i, j = i+1, j-1 {
		snap.Evidence[i], snap.Evidence[j] = snap.Evidence[j], snap.Evidence[i]
	}
	second, _, err := buildPrompt(snap, "technical", opts)
	if err != nil || first.UserPrompt != second.UserPrompt {
		t.Fatal("prompt depends on input order")
	}
}

func TestModelInterpretationNeverBecomesVerifiedFact(t *testing.T) {
	snap := evidence.Snapshot{ProjectID: "42", CommitSHA: "abc", Evidence: []evidence.Item{{ID: "route", Category: "api", Type: "route", Value: "GET /appointments", SourceFile: "routes.go", LineStart: 10, Confidence: evidence.Verified}}}
	provider := providerFunc(func(ctx context.Context, req ai.Request) (ai.Response, error) {
		record := promptRecords(t, req)[0]
		data, _ := json.Marshal(paperResponse{Summaries: []interpretation{{Component: record.Component, Text: "This component may organize appointment access", EvidenceIDs: []string{record.ID}}}, Recommendations: []interpretation{}})
		return ai.Response{Text: string(data), GeneratedTokens: 42}, nil
	})
	artifacts, err := NewGenerator(provider).Generate(context.Background(), snap, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	verified := strings.Split(artifacts.TechnicalPaper, "## Inferred Information")[0]
	if strings.Contains(verified, "may organize") || !strings.Contains(verified, "GET /appointments") || !strings.Contains(artifacts.TechnicalPaper, "routes.go:10") {
		t.Fatalf("incorrect confidence/source handling: %s", artifacts.TechnicalPaper)
	}
	if !strings.Contains(artifacts.TechnicalPaper, "may organize") || !json.Valid(artifacts.ValidationJSON) {
		t.Fatal("missing interpretation or validation report")
	}
}

func TestStructuredOutputRejectsBadCitationsAndSensitiveContent(t *testing.T) {
	selected := []evidence.Item{{ID: "valid", Category: "api", SourceFile: "routes.go"}}
	for _, text := range []string{
		`plain Markdown`,
		`{"summaries":[],"recommendations":[]} trailing`,
		`{"summaries":[],"recommendations":[],"unknown":true}`,
		`{"summaries":[{"component":"repository","text":"claim","evidence_ids":["invented"]}],"recommendations":[]}`,
		`{"summaries":[{"component":"other","text":"claim","evidence_ids":["valid"]}],"recommendations":[]}`,
		`{"summaries":[{"component":"repository","text":"password=private-value","evidence_ids":["valid"]}],"recommendations":[]}`,
		`{"summaries":[{"component":"repository","text":"claim","evidence_ids":[]}],"recommendations":[]}`,
	} {
		if _, err := validateResponse(text, selected); err == nil {
			t.Errorf("accepted invalid output %s", text)
		}
	}
}

func TestRedactionBeforePromptAndArtifacts(t *testing.T) {
	secret := "glpat-abcdefghijklmnopqrst"
	snap := evidence.Snapshot{Evidence: []evidence.Item{{ID: "secret", Category: "configuration", Type: "configuration", Value: secret, SourceFile: "config.yml", Confidence: evidence.Verified}}}
	provider := providerFunc(func(ctx context.Context, req ai.Request) (ai.Response, error) {
		if strings.Contains(req.UserPrompt, secret) {
			t.Fatal("secret sent to provider")
		}
		return ai.Response{Text: `{"summaries":[],"recommendations":[]}`}, nil
	})
	artifacts, err := NewGenerator(provider).Generate(context.Background(), snap, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(artifacts.EvidenceJSON), secret) || strings.Contains(artifacts.TechnicalPaper, secret) {
		t.Fatal("secret leaked into output")
	}
	if snap.Evidence[0].Value != secret {
		t.Fatal("sanitizer mutated caller snapshot")
	}
}

func TestInvalidGenerationBudgetAndBrokenMermaidFailClosed(t *testing.T) {
	if _, err := NewGeneratorWithOptions(mockProvider{}, Options{ContextLength: 2048, MaxTokens: 1800}); err == nil {
		t.Fatal("accepted impossible context budget")
	}
	artifacts, err := NewGenerator(mockProvider{}).Generate(context.Background(), evidence.Snapshot{}, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	artifacts.Diagrams["application-architecture.mmd"] = "flowchart LR\n  n1 -->|imports| n2\n"
	if _, err := ValidateArtifacts(artifacts); err == nil {
		t.Fatal("accepted unresolved Mermaid edges")
	}
}
