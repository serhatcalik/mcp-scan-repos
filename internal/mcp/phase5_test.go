package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gitlab-doc-platform/internal/documents"
	"gitlab-doc-platform/internal/evidence"
	"gitlab-doc-platform/internal/jobs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPToolsScopePersistenceAndRegeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := jobs.NewMemoryStore()
	for _, project := range []string{"42", "43"} {
		id := "job-" + project
		_, _, err := store.CreateOrGet(ctx, jobs.Job{ID: id, ProjectID: project, ProjectName: "project-" + project, GroupID: "101", CommitSHA: "abc", IdempotencyKey: id, ArchivePath: "source.tar.gz"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveAnalysis(ctx, id, evidence.Snapshot{ProjectID: project, CommitSHA: "abc", Language: "go", Evidence: []evidence.Item{{ID: "fact", Value: "private-" + project, SourceFile: "app.go"}}}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveDocuments(ctx, id, documents.ArtifactSet{TechnicalPaper: "technical-private-" + project, InfrastructurePaper: "infra-private-" + project, Diagrams: map[string]string{"application-architecture.mmd": "flowchart LR"}}); err != nil {
			t.Fatal(err)
		}
		_ = store.UpdateStatus(ctx, id, jobs.Completed, "")
	}
	reader := strings.Repeat("r", 64)
	writer := strings.Repeat("w", 64)
	credentials := []Credential{}
	for _, item := range []struct {
		token, id string
		write     bool
	}{{reader, "reader", false}, {writer, "writer", true}} {
		hash := sha256.Sum256([]byte(item.token))
		credentials = append(credentials, Credential{ID: item.id, TokenSHA256: hex.EncodeToString(hash[:]), ProjectIDs: []string{"42"}, CanRegenerate: item.write})
	}
	server := httptest.NewServer(NewHandler(store, Options{Credentials: credentials, GenerationVersion: "config"}))
	defer server.Close()
	connect := func(token string) *sdk.ClientSession {
		client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token}}, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	session := connect(reader)
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 9 {
		t.Fatalf("tools=%+v error=%v", tools, err)
	}
	for _, name := range []string{"list_projects", "list_documentation_jobs", "get_evidence", "get_application_summary", "get_technical_paper", "get_infrastructure_paper", "get_application_architecture"} {
		args := map[string]any{}
		if strings.HasPrefix(name, "get_") {
			args["job_id"] = "job-42"
		}
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("%s result=%+v error=%v", name, result, err)
		}
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), "private-43") || strings.Contains(string(encoded), "project-43") {
			t.Fatalf("cross-project leak in %s", name)
		}
	}
	for _, name := range []string{"get_evidence", "get_application_summary", "get_technical_paper", "get_infrastructure_paper", "get_application_architecture"} {
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: map[string]any{"job_id": "job-43"}})
		if err != nil {
			t.Fatal(err)
		}
		if !result.IsError {
			t.Fatalf("unauthorized access via %s", name)
		}
	}
	denied, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "regenerate_documentation", Arguments: map[string]any{"job_id": "job-42", "request_id": "retry-1"}})
	if err != nil || !denied.IsError {
		t.Fatalf("reader can regenerate: %+v %v", denied, err)
	}
	writable := connect(writer)
	id := ""
	for i := 0; i < 2; i++ {
		result, err := writable.CallTool(ctx, &sdk.CallToolParams{Name: "regenerate_documentation", Arguments: map[string]any{"job_id": "job-42", "request_id": "retry-1"}})
		if err != nil || result.IsError {
			t.Fatalf("regeneration: %+v %v", result, err)
		}
		data, _ := json.Marshal(result.StructuredContent)
		var out struct {
			JobID string `json:"job_id"`
		}
		_ = json.Unmarshal(data, &out)
		if i == 0 {
			id = out.JobID
		} else if out.JobID != id {
			t.Fatal("regeneration is not idempotent")
		}
	}
}

func TestMCPRejectsUnauthenticatedAndExpiredAccess(t *testing.T) {
	token := strings.Repeat("a", 64)
	hash := sha256.Sum256([]byte(token))
	for _, credentials := range [][]Credential{nil, {{ID: "expired", TokenSHA256: hex.EncodeToString(hash[:]), ProjectIDs: []string{"42"}, ExpiresAt: time.Now().Add(-time.Hour)}}} {
		handler := NewHandler(jobs.NewMemoryStore(), Options{Credentials: credentials})
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 401 {
			t.Fatalf("status=%d", response.Code)
		}
	}
	if _, err := ParseCredentials([]byte(`{"clients":[]}`)); err == nil {
		t.Fatal("empty configuration accepted")
	}
	if _, err := ParseCredentials([]byte(`{"clients":[{"id":"bad","token_sha256":"123","project_ids":["*"]}]}`)); err == nil {
		t.Fatal("unscoped credential accepted")
	}
}
