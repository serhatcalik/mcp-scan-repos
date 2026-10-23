// Package hosted implements the OpenAI Responses and Anthropic Messages APIs.
package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gitlab-doc-platform/internal/ai"
	"gitlab-doc-platform/internal/observability"
)

type Options struct {
	Provider, Endpoint, Model, APIKey string
	HTTPClient                        *http.Client
	Retries                           int
	RetryBackoff                      time.Duration
	Logger                            *slog.Logger
	Metrics                           *observability.Metrics
}

type Client struct{ options Options }

func ReadAPIKey(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", errors.New("LLM API key file unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	key := strings.TrimSpace(string(data))
	if err != nil || len(data) > 8192 || key == "" || strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("LLM API key file empty, invalid, or oversized")
	}
	return key, nil
}

func NewClient(options Options) (*Client, error) {
	u, err := url.Parse(options.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("hosted LLM endpoint must be an HTTPS base URL without credentials, query, or fragment")
	}
	if options.Provider != "openai" && options.Provider != "anthropic" || strings.TrimSpace(options.Model) == "" || options.APIKey == "" || strings.ContainsAny(options.APIKey, " \t\r\n") || options.Retries < 0 || options.Retries > 5 {
		return nil, errors.New("invalid hosted LLM provider, model, key, or retry configuration")
	}
	options.Endpoint = strings.TrimRight(options.Endpoint, "/")
	if options.RetryBackoff <= 0 {
		options.RetryBackoff = 2 * time.Second
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	copyClient := *client
	// Never forward API keys or repository evidence to a redirect destination.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	options.HTTPClient = &copyClient
	return &Client{options: options}, nil
}

func (c *Client) Name() string  { return c.options.Provider }
func (c *Client) Model() string { return c.options.Model }

func (c *Client) Generate(ctx context.Context, req ai.Request) (ai.Response, error) {
	if req.MaxTokens <= 0 || req.ContextLength <= req.MaxTokens {
		return ai.Response{}, errors.New("invalid hosted LLM token budget")
	}
	payload, err := c.payload(req)
	if err != nil {
		return ai.Response{}, err
	}
	for attempt := 0; ; attempt++ {
		started := time.Now()
		out, retry, delay, err := c.generateOnce(ctx, payload, req)
		c.options.Metrics.LLM(time.Since(started), out.GeneratedTokens, err)
		if c.options.Logger != nil {
			c.options.Logger.InfoContext(ctx, "hosted LLM request finished", "provider", c.Name(), "model", c.Model(), "request_id", req.TraceID, "attempt", attempt+1, "duration", time.Since(started), "success", err == nil, "generated_tokens", out.GeneratedTokens)
		}
		if err == nil || !retry || attempt >= c.options.Retries || ctx.Err() != nil {
			return out, err
		}
		backoff := c.options.RetryBackoff * time.Duration(1<<attempt)
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		if delay > backoff {
			backoff = delay
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ai.Response{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) payload(req ai.Request) ([]byte, error) {
	body := map[string]any{"model": c.Model(), "stream": false}
	if c.Name() == "openai" {
		body["instructions"], body["input"], body["max_output_tokens"], body["store"] = req.SystemPrompt, req.UserPrompt, req.MaxTokens, false
		// Temperature is intentionally omitted: not every Responses model accepts it.
		if len(req.JSONSchema) > 0 {
			body["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "documentation", "strict": true, "schema": req.JSONSchema}}
		}
	} else {
		body["system"], body["max_tokens"] = req.SystemPrompt, req.MaxTokens
		body["messages"] = []map[string]string{{"role": "user", "content": req.UserPrompt}}
		if len(req.JSONSchema) > 0 {
			// Claude's grammar does not support these length constraints. The
			// original schema remains in the prompt and local validation enforces it.
			var schema map[string]any
			if json.Unmarshal(req.JSONSchema, &schema) != nil {
				return nil, errors.New("invalid hosted LLM request schema")
			}
			claudeSchema(schema)
			body["output_config"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": schema}}
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("invalid hosted LLM request format")
	}
	return data, nil
}

func claudeSchema(schema map[string]any) {
	for _, key := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
		delete(schema, key)
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for _, property := range properties {
			if child, ok := property.(map[string]any); ok {
				claudeSchema(child)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		claudeSchema(items)
	}
}

func (c *Client) generateOnce(ctx context.Context, payload []byte, input ai.Request) (ai.Response, bool, time.Duration, error) {
	path := "/responses"
	if c.Name() == "anthropic" {
		path = "/messages"
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.options.Endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return ai.Response{}, false, 0, errors.New("invalid hosted LLM request")
	}
	r.Header.Set("Content-Type", "application/json")
	if c.Name() == "openai" {
		r.Header.Set("Authorization", "Bearer "+c.options.APIKey)
	} else {
		r.Header.Set("x-api-key", c.options.APIKey)
		r.Header.Set("anthropic-version", "2023-06-01")
	}
	response, err := c.options.HTTPClient.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return ai.Response{}, false, 0, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return ai.Response{}, false, 0, fmt.Errorf("hosted LLM request timeout: %w", context.DeadlineExceeded)
		}
		return ai.Response{}, true, 0, errors.New("hosted LLM transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		retry := response.StatusCode == 429 || response.StatusCode >= 500 && response.StatusCode <= 599
		delay := retryDelay(response.Header.Get("Retry-After"))
		if delay > time.Minute {
			retry = false
		}
		return ai.Response{}, retry, delay, fmt.Errorf("%s API returned HTTP %d", c.Name(), response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if ctx.Err() != nil {
		return ai.Response{}, false, 0, ctx.Err()
	}
	if err != nil || len(data) > 4<<20 {
		return ai.Response{}, false, 0, errors.New("hosted LLM response unreadable or oversized")
	}
	out, err := c.decode(data)
	if err == nil && (out.GeneratedTokens < 0 || out.PromptTokens < 0 || out.GeneratedTokens > input.MaxTokens || out.PromptTokens > input.ContextLength-input.MaxTokens) {
		err = errors.New("hosted LLM response exceeds token budget or has invalid usage")
	}
	if err != nil {
		return ai.Response{}, false, 0, err
	}
	return out, false, 0, nil
}

func retryDelay(value string) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		if seconds > 60 {
			return 61 * time.Second
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && time.Until(date) > 0 {
		return time.Until(date)
	}
	return 0
}

func (c *Client) decode(data []byte) (ai.Response, error) {
	var text strings.Builder
	out := ai.Response{}
	if c.Name() == "openai" {
		var wire struct {
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			Usage struct {
				Input  int `json:"input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &wire) != nil {
			return out, errors.New("invalid OpenAI JSON response")
		}
		if wire.Status != "completed" || len(wire.Error) > 0 && string(wire.Error) != "null" {
			return out, errors.New("OpenAI response not completed; check output token budget or model availability")
		}
		for _, item := range wire.Output {
			for _, block := range item.Content {
				if block.Type == "refusal" {
					return out, errors.New("OpenAI refused generation")
				}
				if item.Type == "message" && block.Type == "output_text" {
					text.WriteString(block.Text)
				}
			}
		}
		out.PromptTokens, out.GeneratedTokens = wire.Usage.Input, wire.Usage.Output
	} else {
		var wire struct {
			Type       string `json:"type"`
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				Input  int `json:"input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &wire) != nil {
			return out, errors.New("invalid Anthropic JSON response")
		}
		if wire.Type != "message" || wire.StopReason != "end_turn" {
			return out, errors.New("Anthropic response not completed; check output token budget or refusal")
		}
		for _, block := range wire.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		out.PromptTokens, out.GeneratedTokens = wire.Usage.Input, wire.Usage.Output
	}
	if strings.TrimSpace(text.String()) == "" {
		return out, errors.New("hosted LLM returned empty output")
	}
	out.Text = text.String()
	return out, nil
}
