package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlab-doc-platform/internal/jobs"
	"gitlab-doc-platform/internal/observability"
)

func TestStatusToolReadsCurrentJobStateOverHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := jobs.NewMemoryStore()
	if _, _, err := store.CreateOrGet(ctx, jobs.Job{ID: "shared", ProjectID: "42", IdempotencyKey: "shared"}); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	hash := sha256.Sum256([]byte(token))
	server := httptest.NewServer(NewHandler(store, Options{Credentials: []Credential{{ID: "reader", TokenSHA256: hex.EncodeToString(hash[:]), ProjectIDs: []string{"42"}}}}))
	defer server.Close()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token}}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, want := range []jobs.Status{jobs.Queued, jobs.Completed} {
		if err := store.UpdateStatus(ctx, "shared", want, ""); err != nil {
			t.Fatal(err)
		}
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "get_documentation_status", Arguments: map[string]any{"job_id": "shared"}})
		if err != nil || result.IsError {
			t.Fatalf("tool result=%+v error=%v", result, err)
		}
		body, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var status statusOutput
		if err := json.Unmarshal(body, &status); err != nil {
			t.Fatal(err)
		}
		if status.JobID != "shared" || status.Status != string(want) {
			t.Fatalf("status=%+v want=%s", status, want)
		}
	}
}

type bearerTransport struct{ token string }

func TestPublicListenerDoesNotExposeManagement(t *testing.T) {
	token := strings.Repeat("a", 64)
	hash := sha256.Sum256([]byte(token))
	options := Options{
		Credentials: []Credential{{ID: "reader", TokenSHA256: hex.EncodeToString(hash[:]), ProjectIDs: []string{"42"}}},
		Metrics:     observability.New(nil),
	}
	public := NewHandler(jobs.NewMemoryStore(), options)
	for _, path := range []string{"/", "/metrics", "/healthz", "/readyz", "/mcp/metrics", "/mcp/"} {
		for _, authorization := range []string{"", "Bearer " + token} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", authorization)
			w := httptest.NewRecorder()
			public.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("public %s = %d, want 404", path, w.Code)
			}
		}
	}
	w := httptest.NewRecorder()
	public.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated MCP = %d, want 401", w.Code)
	}
	management := NewManagementHandler(options)
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		management.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("management %s = %d, want 200", path, w.Code)
		}
	}
	w = httptest.NewRecorder()
	management.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("management MCP = %d, want 404", w.Code)
	}
}

type publishedStore struct{ *jobs.MemoryStore }

func (s publishedStore) ListPublications(ctx context.Context, jobID string) ([]jobs.Publication, error) {
	return []jobs.Publication{{Status: "published", ProjectURL: "https://www.notion.so/11111111111111111111111111111111"}}, nil
}

func TestPublicationURLsRespectMCPProjectScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := publishedStore{jobs.NewMemoryStore()}
	for _, item := range []jobs.Job{{ID: "own", ProjectID: "42", IdempotencyKey: "own"}, {ID: "other", ProjectID: "43", IdempotencyKey: "other"}} {
		if _, _, err := store.CreateOrGet(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	token := strings.Repeat("p", 64)
	hash := sha256.Sum256([]byte(token))
	server := httptest.NewServer(NewHandler(store, Options{Credentials: []Credential{{ID: "reader", TokenSHA256: hex.EncodeToString(hash[:]), ProjectIDs: []string{"42"}}}}))
	defer server.Close()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token}}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, id := range []string{"own", "other"} {
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "get_documentation_status", Arguments: map[string]any{"job_id": id}})
		if err != nil || result.IsError {
			t.Fatalf("status tool failed: %v %+v", err, result)
		}
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		containsURL := strings.Contains(string(data), "www.notion.so")
		if containsURL != (id == "own") {
			t.Fatalf("publication scope failed: %s", data)
		}
	}
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone)
}
