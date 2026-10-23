package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitlab-doc-platform/internal/ai"
)

func TestGenerateExplainsModelNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"model 'qwen2.5-coder:7b' not found"}`))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "qwen2.5-coder:7b", nil).Generate(context.Background(), ai.Request{})
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "qwen2.5-coder:7b") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error=%v", err)
	}
}

func TestGenerateDoesNotDumpProxyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>proxy-private-information</html>"))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "test-model", nil).Generate(context.Background(), ai.Request{})
	if err == nil || !strings.Contains(err.Error(), "check endpoint") || strings.Contains(err.Error(), "proxy-private-information") {
		t.Fatalf("error=%v", err)
	}
}

func TestNewClientGenerationTimeout(t *testing.T) {
	client := NewClient("http://localhost:11434", "test-model", nil)
	if client.http.Timeout != 30*time.Minute {
		t.Fatalf("timeout=%v, want 30m", client.http.Timeout)
	}
	custom := &http.Client{Timeout: 45 * time.Minute}
	if got := NewClient("http://localhost:11434", "test-model", custom); got.http != custom {
		t.Fatal("custom HTTP client was replaced")
	}
}

func TestGenerateRespectsJobCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := NewClient(server.URL, "test-model", nil).Generate(ctx, ai.Request{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want context deadline exceeded", err)
	}
}

func TestGenerateUsesStructuredFormatAndRetriesTransientFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System  string         `json:"system"`
			Prompt  string         `json:"prompt"`
			Format  map[string]any `json:"format"`
			Stream  bool           `json:"stream"`
			Options struct {
				Context int `json:"num_ctx"`
				Tokens  int `json:"num_predict"`
			} `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.System != "guard" || body.Prompt != "evidence" || body.Format["type"] != "object" || body.Stream || body.Options.Context != 4096 || body.Options.Tokens != 256 {
			t.Errorf("bad Ollama request: %+v", body)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"private-password=do-not-log"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"response": `{"summaries":[],"recommendations":[]}`, "done": true, "done_reason": "stop", "eval_count": 20, "prompt_eval_count": 100})
	}))
	defer server.Close()
	client := NewClientWithOptions(server.URL, "test", nil, Options{Retries: 2, RetryBackoff: time.Millisecond})
	response, err := client.Generate(context.Background(), ai.Request{SystemPrompt: "guard", UserPrompt: "evidence", JSONSchema: json.RawMessage(`{"type":"object"}`), ContextLength: 4096, MaxTokens: 256})
	if err != nil || calls.Load() != 2 || response.GeneratedTokens != 20 || response.PromptTokens != 100 {
		t.Fatalf("response=%+v calls=%d error=%v", response, calls.Load(), err)
	}
}

func TestGenerateRejectsIncompleteOversizedAndTruncatedResponses(t *testing.T) {
	for _, body := range []string{`{"response":"content","done":false}`, `{"response":"content","done":true,"done_reason":"length"}`, `{"response":"content","done":true,"eval_count":1000}`, `not-json`, strings.Repeat("x", 300)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		client := NewClientWithOptions(server.URL, "test", nil, Options{MaxResponseBytes: 256})
		if _, err := client.Generate(context.Background(), ai.Request{MaxTokens: 128}); err == nil {
			t.Errorf("accepted bad response: %s", body)
		}
		server.Close()
	}
}

func TestGenerateNeverRetriesMissingModel(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(404) }))
	defer server.Close()
	_, err := NewClientWithOptions(server.URL, "test", nil, Options{Retries: 5, RetryBackoff: time.Millisecond}).Generate(context.Background(), ai.Request{})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}
