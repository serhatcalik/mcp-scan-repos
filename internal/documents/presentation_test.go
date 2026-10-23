package documents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gitlab-doc-platform/internal/evidence"
)

func paperFixture() evidence.Snapshot {
	s := evidence.Snapshot{ProjectID: "42", CommitSHA: "abc"}
	add := func(category, typ, value, file string) {
		s.Evidence = append(s.Evidence, evidence.Item{ID: fmt.Sprintf("fact-%04d", len(s.Evidence)), Category: category, Type: typ, Value: value, SourceFile: file, LineStart: len(s.Evidence) + 1, Confidence: evidence.Verified})
	}
	add("application", "framework", "gin", "go.mod")
	add("application", "go_package", "internal/users", "internal/users/a.go")
	add("application", "go_package", "internal/users", "internal/users/b.go")
	add("application", "struct", "Service", "internal/users/a.go")
	add("data", "model", "User", "models.go")
	add("data", "database_client", "postgresql", "go.mod")
	add("configuration", "config_key", "SMTP_HOST", ".env")
	add("configuration", "environment_variable", "SMTP_HOST", "mail.go")
	add("configuration", "environment_variable", "DOCUMENTATION_API_URL", ".gitlab-ci.yml")
	add("dependency", "go_import", "net/http", "main.go")
	add("dependency", "go_import", "go.opentelemetry.io/otel", "main.go")
	add("infrastructure", "container_base_image", "golang:1.25", "Dockerfile")
	add("infrastructure", "container_base_image", "alpine:3.22", "Dockerfile")
	add("infrastructure", "compose_service", "api", "docker-compose.yml")
	add("infrastructure", "image", "postgres:16", "docker-compose.yml")
	add("infrastructure", "ci_job", "test", ".gitlab-ci.yml")
	add("infrastructure", "image", "docker:dind", ".gitlab-ci.yml")
	for i := 0; i < 400; i++ {
		add("dependency", "go_dependency", fmt.Sprintf("example.com/transitive/lib%d", i), "go.mod")
		add("data", "model_field", fmt.Sprintf("User.Field%d", i), "models.go")
		add("api", "route", fmt.Sprintf("GET /appointments/%d", i), "main.go")
	}
	return s
}

func TestBothPapersCondenseNoiseAndKeepFullReferences(t *testing.T) {
	s := paperFixture()
	a, err := NewGenerator(mockProvider{}).Generate(context.Background(), s, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	for kind, paper := range map[string]string{"technical": a.TechnicalPaper, "infrastructure": a.InfrastructurePaper} {
		if strings.Count(paper, "\n") > 130 {
			t.Fatalf("%s paper is an inventory again: %d lines", kind, strings.Count(paper, "\n"))
		}
		for _, noise := range []string{"User.Field", "example.com/transitive/", "net/http", "Risk Hypotheses", "fact-0000", "No verified evidence available"} {
			if strings.Contains(paper, noise) {
				t.Errorf("%s contains noise %q", kind, noise)
			}
		}
		if strings.Count(paper, "SMTP\\_HOST") != 1 {
			t.Errorf("%s did not deduplicate configuration", kind)
		}
		if !strings.Contains(paper, "Telemetry References") || !strings.Contains(paper, "[^S") {
			t.Errorf("%s missing telemetry/citations", kind)
		}
	}
	if strings.Contains(a.TechnicalPaper, "DOCUMENTATION\\_API\\_URL") {
		t.Fatal("CI settings leaked into application paper")
	}
	for _, section := range []string{"Container Build Definitions", "Compose Definitions", "CI and Workflow Definitions", "CI Configuration"} {
		if !strings.Contains(a.InfrastructurePaper, "### "+section) {
			t.Errorf("missing infrastructure section %s", section)
		}
	}
	if !strings.Contains(a.TechnicalPaper, "Appointments API") || !strings.Contains(a.References["api-reference.md"], "GET /appointments/399") || !strings.Contains(a.References["configuration-reference.md"], "DOCUMENTATION\\_API\\_URL") {
		t.Fatal("missing grouping or full references")
	}
	var persisted evidence.Snapshot
	if err := json.Unmarshal(a.EvidenceJSON, &persisted); err != nil || len(persisted.Evidence) != len(s.Evidence) {
		t.Fatalf("lost full evidence: %v", err)
	}
	for i, j := 0, len(s.Evidence)-1; i < j; i, j = i+1, j-1 {
		s.Evidence[i], s.Evidence[j] = s.Evidence[j], s.Evidence[i]
	}
	b, err := NewGenerator(mockProvider{}).Generate(context.Background(), s, Metadata{})
	if err != nil || a.TechnicalPaper != b.TechnicalPaper || a.InfrastructurePaper != b.InfrastructurePaper || a.References["api-reference.md"] != b.References["api-reference.md"] {
		t.Fatalf("presentation depends on evidence order: %v", err)
	}
}

func TestInfrastructurePromptPrioritizesDeploymentOverApplicationKeys(t *testing.T) {
	s := evidence.Snapshot{}
	for i := 0; i < 200; i++ {
		s.Evidence = append(s.Evidence, evidence.Item{ID: fmt.Sprint(i), Category: "configuration", Type: "environment_variable", Value: fmt.Sprintf("APP_KEY_%d", i), SourceFile: fmt.Sprintf("internal/app%d/config.go", i), Confidence: evidence.Verified})
	}
	s.Evidence = append(s.Evidence, evidence.Item{ID: "deployment", Category: "infrastructure", Type: "resource", Value: "Deployment api", SourceFile: "deploy/app.yaml", Confidence: evidence.Verified})
	req, selected, err := buildPrompt(s, "infrastructure", Options{ContextLength: 8192, MaxTokens: 1800})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) == 0 || selected[0].ID != "deployment" {
		t.Fatal("deployment starved by unrelated configuration")
	}
	if len(req.SystemPrompt)+len(req.UserPrompt) > 8192-1800-1024 {
		t.Fatal("prompt exceeded existing context budget")
	}
}

func TestModelCannotNameUncitedSettingOrPublishRecommendations(t *testing.T) {
	selected := []evidence.Item{{ID: "ttl", Category: "configuration", Type: "environment_variable", Value: "USER_CACHE_TTL_SECONDS", SourceFile: "internal/auth/cache.go", Confidence: evidence.Verified}}
	text := `{"summaries":[{"component":"internal/auth","text":"Rotate JWT_SECRET periodically","evidence_ids":["ttl"]}],"recommendations":[]}`
	if _, err := validateResponse(text, selected); err == nil {
		t.Fatal("accepted JWT_SECRET advice citing only a cache TTL")
	}
	paper, err := renderPaper(evidence.Snapshot{Evidence: selected}, "infrastructure", paperResponse{Recommendations: []interpretation{{Component: "internal/auth", Text: "Rotate cache TTL", EvidenceIDs: []string{"ttl"}}}}, 1)
	if err != nil || strings.Contains(paper, "Rotate cache TTL") {
		t.Fatalf("published generic model recommendation: %v", err)
	}
}
