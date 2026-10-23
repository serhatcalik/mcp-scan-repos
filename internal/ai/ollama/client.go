package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gitlab-doc-platform/internal/ai"
	"gitlab-doc-platform/internal/observability"
)

type Client struct {
	endpoint string
	model    string
	http     *http.Client
	options  Options
}

type Options struct {
	Retries          int
	RetryBackoff     time.Duration
	MaxResponseBytes int64
	Logger           *slog.Logger
	KeepAlive        string
	Metrics          *observability.Metrics
}

func NewClient(endpoint, model string, client *http.Client) *Client {
	return NewClientWithOptions(endpoint, model, client, Options{})
}

func NewClientWithOptions(endpoint, model string, client *http.Client, options Options) *Client {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	if options.RetryBackoff <= 0 {
		options.RetryBackoff = 2 * time.Second
	}
	if options.MaxResponseBytes <= 0 {
		options.MaxResponseBytes = 4 << 20
	}
	if options.Retries < 0 {
		options.Retries = 0
	}
	if options.Retries > 5 {
		options.Retries = 5
	}
	if options.KeepAlive == "" {
		options.KeepAlive = "30m"
	}
	return &Client{endpoint: strings.TrimRight(endpoint, "/"), model: model, http: client, options: options}
}

func (c *Client) Name() string  { return "ollama" }
func (c *Client) Model() string { return c.model }

func (c *Client) Generate(ctx context.Context, req ai.Request) (ai.Response, error) {
	started := time.Now()
	for attempt := 0; ; attempt++ {
		attemptStarted := time.Now()
		response, retry, err := c.generateOnce(ctx, req)
		c.options.Metrics.LLM(time.Since(attemptStarted), response.GeneratedTokens, err)
		if c.options.Logger != nil {
			c.options.Logger.InfoContext(ctx, "ollama request finished", "request_id", req.TraceID, "model", c.model, "attempt", attempt+1, "duration", time.Since(attemptStarted), "elapsed", time.Since(started), "prompt_tokens", response.PromptTokens, "generated_tokens", response.GeneratedTokens, "success", err == nil)
		}
		if err == nil || !retry || attempt >= c.options.Retries || ctx.Err() != nil {
			return response, err
		}
		delay := c.options.RetryBackoff * time.Duration(1<<attempt)
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ai.Response{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) generateOnce(ctx context.Context, req ai.Request) (ai.Response, bool, error) {
	if req.ContextLength <= 0 {
		req.ContextLength = 8192
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1800
	}
	if req.MaxTokens >= req.ContextLength {
		return ai.Response{}, false, errors.New("generation token budget must be below context length")
	}
	body := map[string]any{
		"model":      c.model,
		"prompt":     req.UserPrompt,
		"system":     req.SystemPrompt,
		"keep_alive": c.options.KeepAlive,
		"stream":     false,
		"options": map[string]any{
			"temperature": req.Temperature,
			"num_predict": req.MaxTokens,
			"num_ctx":     req.ContextLength,
		},
	}
	if len(req.JSONSchema) > 0 {
		body["format"] = req.JSONSchema
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ai.Response{}, false, errors.New("invalid Ollama request format")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/generate", bytes.NewReader(payload))
	if err != nil {
		return ai.Response{}, false, errors.New("invalid Ollama endpoint")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return ai.Response{}, false, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return ai.Response{}, false, fmt.Errorf("ollama request timeout: %w", context.DeadlineExceeded)
		}
		return ai.Response{}, true, errors.New("ollama transport failed; check service connectivity")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		status := fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		retry := resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504
		var failure struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&failure); err == nil && failure.Error != "" {
			// Only expose the useful missing-model error, never arbitrary upstream text.
			if resp.StatusCode == 404 && strings.Contains(failure.Error, "not found") {
				return ai.Response{}, retry, fmt.Errorf("ollama returned %s for model %q: model %q not found; pull it on the Ollama service", status, c.model, c.model)
			}
		}
		return ai.Response{}, retry, fmt.Errorf("ollama returned %s for model %q (check endpoint, model availability, and proxy routing)", status, c.model)
	}
	var out struct {
		Response    string `json:"response"`
		EvalCount   int    `json:"eval_count"`
		PromptCount int    `json:"prompt_eval_count"`
		Done        bool   `json:"done"`
		DoneReason  string `json:"done_reason"`
		Error       string `json:"error"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.options.MaxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ai.Response{}, false, ctx.Err()
		}
		return ai.Response{}, false, errors.New("could not read Ollama response")
	}
	if int64(len(data)) > c.options.MaxResponseBytes {
		return ai.Response{}, false, errors.New("Ollama response exceeded size limit")
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ai.Response{}, false, errors.New("invalid Ollama JSON response")
	}
	if out.Error != "" || !out.Done || strings.TrimSpace(out.Response) == "" {
		return ai.Response{}, false, errors.New("Ollama did not return a completed nonempty response")
	}
	if out.DoneReason == "length" {
		return ai.Response{}, false, errors.New("Ollama output reached its token limit; increase LLM_MAX_TOKENS or reduce the response scope")
	}
	if out.PromptCount+req.MaxTokens > req.ContextLength {
		return ai.Response{}, false, errors.New("Ollama prompt and reserved output exceed context budget")
	}
	if out.EvalCount > req.MaxTokens {
		return ai.Response{}, false, errors.New("Ollama response exceeded generation token budget")
	}
	if out.EvalCount < 0 || out.PromptCount < 0 {
		return ai.Response{}, false, errors.New("invalid Ollama token counters")
	}
	return ai.Response{Text: out.Response, GeneratedTokens: out.EvalCount, PromptTokens: out.PromptCount, ProviderMetadata: map[string]string{"done_reason": out.DoneReason}}, false, nil
}
