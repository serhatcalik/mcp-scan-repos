package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab-doc-platform/internal/ai"
)

var testRequest = ai.Request{
	SystemPrompt: "system", UserPrompt: "redacted evidence", MaxTokens: 128, ContextLength: 8192,
	JSONSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["text"],"properties":{"text":{"type":"string","maxLength":200}}}`),
}

func success(provider string) string {
	if provider == "openai" {
		return `{"status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"message","content":[{"type":"output_text","text":"{\"text\":\"ok\"}"}]}],"usage":{"input_tokens":50,"output_tokens":20}}`
	}
	return `{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"{\"text\":\"ok\"}"}],"usage":{"input_tokens":50,"output_tokens":20}}`
}

func testClient(t *testing.T, provider string, handler http.HandlerFunc, retries int) *Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(Options{Provider: provider, Endpoint: server.URL + "/v1", Model: "test-model", APIKey: "test-key", HTTPClient: server.Client(), Retries: retries, RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestProviderRequestsAndUsage(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			client := testClient(t, provider, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("unexpected request")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body["model"] != "test-model" || body["stream"] != false {
					t.Errorf("unexpected model/stream: %v", body)
				}
				if _, exists := body["temperature"]; exists {
					t.Error("model-dependent temperature must be omitted")
				}
				if provider == "openai" {
					if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("x-api-key") != "" {
						t.Error("invalid OpenAI path/auth")
					}
					if body["store"] != false || body["instructions"] != "system" || body["input"] != testRequest.UserPrompt || body["max_output_tokens"] != float64(128) {
						t.Error("invalid Responses payload")
					}
					format := body["text"].(map[string]any)["format"].(map[string]any)
					if format["strict"] != true || format["type"] != "json_schema" {
						t.Error("structured output not enabled")
					}
				} else {
					if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
						t.Error("invalid Anthropic path/auth")
					}
					if body["system"] != "system" || body["max_tokens"] != float64(128) {
						t.Error("invalid Messages payload")
					}
					message := body["messages"].([]any)[0].(map[string]any)
					if message["role"] != "user" || message["content"] != testRequest.UserPrompt {
						t.Error("lost user evidence")
					}
					format := body["output_config"].(map[string]any)["format"].(map[string]any)
					data, _ := json.Marshal(format)
					if format["type"] != "json_schema" || strings.Contains(string(data), "maxLength") {
						t.Error("unsupported Claude schema")
					}
				}
				fmt.Fprint(w, success(provider))
			}, 0)
			out, err := client.Generate(context.Background(), testRequest)
			if err != nil || out.Text != `{"text":"ok"}` || out.PromptTokens != 50 || out.GeneratedTokens != 20 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if client.Name() != provider || client.Model() != "test-model" {
				t.Fatal("lost provider identity")
			}
		})
	}
}

func TestHTTPFailuresRetriesAndSafeErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500, 503, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			client := testClient(t, "openai", func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				fmt.Fprint(w, "test-key private repository prompt")
			}, 2)
			_, err := client.Generate(context.Background(), testRequest)
			want := 1
			if status == 429 || status >= 500 {
				want = 3
			}
			if err == nil || calls != want || strings.Contains(err.Error(), "test-key") || strings.Contains(err.Error(), "private") {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRetriesRecoverAndRespectCancellation(t *testing.T) {
	calls := 0
	client := testClient(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, success("anthropic"))
	}, 1)
	if _, err := client.Generate(context.Background(), testRequest); err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	client = testClient(t, "openai", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "60"); w.WriteHeader(429) }, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.Generate(ctx, testRequest); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestRedirectDoesNotForwardCredentialsOrEvidence(t *testing.T) {
	reached := false
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer destination.Close()
	client := testClient(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}, 0)
	if _, err := client.Generate(context.Background(), testRequest); err == nil {
		t.Fatal("redirect accepted")
	}
	if reached {
		t.Fatal("credentials forwarded to redirect")
	}
}

func TestRejectsUnusableResponses(t *testing.T) {
	for _, tc := range []struct{ provider, body string }{
		{"openai", `{"status":"incomplete","output":[]}`},
		{"openai", `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"private"}]}]}`},
		{"openai", `{"status":"completed","output":[]}`},
		{"anthropic", `{"type":"message","stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`},
		{"anthropic", `{"type":"message","stop_reason":"refusal"}`},
		{"anthropic", `{"type":"error","error":{"message":"test-key"}}`},
		{"openai", `not json test-key`},
		{"anthropic", strings.Repeat("x", (4<<20)+1)},
		{"openai", strings.Replace(success("openai"), `"output_tokens":20`, `"output_tokens":129`, 1)},
		{"anthropic", strings.Replace(success("anthropic"), `"input_tokens":50`, `"input_tokens":8192`, 1)},
	} {
		client := testClient(t, tc.provider, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }, 0)
		if _, err := client.Generate(context.Background(), testRequest); err == nil || strings.Contains(err.Error(), "test-key") {
			t.Fatalf("provider=%s err=%v", tc.provider, err)
		}
	}
}

func TestEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"http://api.example.com/v1", "https://user:secret@api.example.com/v1", "https://api.example.com/v1?key=secret", "https://api.example.com/v1#fragment", "/v1"} {
		if _, err := NewClient(Options{Provider: "openai", Endpoint: endpoint, Model: "test", APIKey: "key"}); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	if _, err := NewClient(Options{Provider: "unknown", Endpoint: "https://api.example.com/v1", Model: "test", APIKey: "key"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestReadAPIKey(t *testing.T) {
	for _, value := range []string{"test-key\n", "", " \n", "key\nextra", strings.Repeat("a", 8193)} {
		filename := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(filename, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		key, err := ReadAPIKey(filename)
		if value == "test-key\n" {
			if key != "test-key" || err != nil {
				t.Fatalf("key=%q err=%v", key, err)
			}
		} else if err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
