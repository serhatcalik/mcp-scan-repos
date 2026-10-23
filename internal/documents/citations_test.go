package documents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gitlab-doc-platform/internal/ai"
	"gitlab-doc-platform/internal/evidence"
)

func citationEvidence() []evidence.Item {
	return []evidence.Item{
		{ID: "users-1", Category: "api", Type: "route", Value: "GET /users", SourceFile: "internal/users/routes.go", Confidence: evidence.Verified},
		{ID: "users-2", Category: "api", Type: "route", Value: "POST /users", SourceFile: "internal/users/routes.go", Confidence: evidence.Verified},
		{ID: "diets-1", Category: "api", Type: "route", Value: "GET /diets", SourceFile: "internal/diets/routes.go", Confidence: evidence.Verified},
	}
}

func TestScopedSchemaBindsCitationsToComponent(t *testing.T) {
	var schema jsonschema.Schema
	if err := json.Unmarshal(scopedSchema(citationEvidence()), &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"summaries", "recommendations"} {
		for _, test := range []struct {
			name      string
			component string
			ids       []string
			valid     bool
		}{
			{"same component", "internal/users", []string{"users-1", "users-2"}, true},
			{"different component", "internal/users", []string{"diets-1"}, false},
			{"mixed components", "internal/users", []string{"users-1", "diets-1"}, false},
			{"unknown ID", "internal/users", []string{"invented"}, false},
			{"unknown component", "invented", []string{"users-1"}, false},
		} {
			t.Run(section+"/"+test.name, func(t *testing.T) {
				value := map[string]any{"summaries": []any{}, "recommendations": []any{}}
				value[section] = []any{map[string]any{"component": test.component, "text": "Interpretation", "evidence_ids": test.ids}}
				err := resolved.Validate(value)
				if (err == nil) != test.valid {
					t.Fatalf("valid=%v error=%v", test.valid, err)
				}
			})
		}
	}
}

func TestPaperRetriesInvalidCitationsWithoutPublishingThem(t *testing.T) {
	calls := 0
	var first ai.Request
	provider := providerFunc(func(ctx context.Context, req ai.Request) (ai.Response, error) {
		calls++
		record := promptRecords(t, req)[0]
		if calls == 1 {
			first = req
			return ai.Response{Text: `{"summaries":[{"component":"invalid","text":"unsupported marker","evidence_ids":["invented"]}],"recommendations":[]}`, GeneratedTokens: 7}, nil
		}
		if req.Temperature != 0 || req.UserPrompt != first.UserPrompt || strings.Contains(req.UserPrompt, "unsupported marker") || req.TraceID != "job:technical:validation-retry" {
			t.Fatalf("unexpected retry request: %+v", req)
		}
		data, _ := json.Marshal(paperResponse{Summaries: []interpretation{{Component: record.Component, Text: "Supported interpretation", EvidenceIDs: []string{record.ID}}}, Recommendations: []interpretation{}})
		return ai.Response{Text: string(data), GeneratedTokens: 11}, nil
	})
	paper, _, tokens, err := NewGenerator(provider).generatePaper(context.Background(), evidence.Snapshot{Evidence: citationEvidence()}, "technical", "job")
	if err != nil || calls != 2 || tokens != 18 || !strings.Contains(paper, "Supported interpretation") || strings.Contains(paper, "unsupported marker") {
		t.Fatalf("calls=%d tokens=%d error=%v paper=%s", calls, tokens, err, paper)
	}
}

func TestPaperRejectsRepeatedInvalidCitations(t *testing.T) {
	calls := 0
	provider := providerFunc(func(context.Context, ai.Request) (ai.Response, error) {
		calls++
		return ai.Response{Text: `{"summaries":[{"component":"internal/users","text":"Invalid","evidence_ids":["diets-1"]}],"recommendations":[]}`}, nil
	})
	paper, _, _, err := NewGenerator(provider).generatePaper(context.Background(), evidence.Snapshot{Evidence: citationEvidence()}, "technical", "job")
	if err == nil || calls != 2 || paper != "" || !strings.Contains(err.Error(), "after two attempts") {
		t.Fatalf("calls=%d paper=%q error=%v", calls, paper, err)
	}
}

func TestPaperDoesNotRetryAfterCancellationOrProviderFailure(t *testing.T) {
	for _, cancelJob := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		provider := providerFunc(func(context.Context, ai.Request) (ai.Response, error) {
			calls++
			if cancelJob {
				cancel()
				return ai.Response{Text: "invalid"}, nil
			}
			return ai.Response{}, errors.New("provider unavailable")
		})
		_, _, _, err := NewGenerator(provider).generatePaper(ctx, evidence.Snapshot{Evidence: citationEvidence()}, "technical", "job")
		cancel()
		if err == nil || calls != 1 {
			t.Fatalf("cancel=%v calls=%d error=%v", cancelJob, calls, err)
		}
		if cancelJob && !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	}
}
