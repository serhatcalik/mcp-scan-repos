package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"gitlab-doc-platform/internal/ai"
	"gitlab-doc-platform/internal/ai/hosted"
	"gitlab-doc-platform/internal/ai/ollama"
	"gitlab-doc-platform/internal/config"
	"gitlab-doc-platform/internal/observability"
)

func newProvider(cfg config.Config, logger *slog.Logger, metrics *observability.Metrics) (ai.Provider, error) {
	endpoint, err := url.Parse(cfg.LLMEndpoint)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || strings.TrimSpace(cfg.LLMModel) == "" || cfg.LLMRetries < 0 || cfg.LLMRetries > 5 || cfg.LLMMaxConcurrent < 1 || cfg.LLMMaxConcurrent > 16 || cfg.LLMMaxQueued < 1 || cfg.LLMMaxQueued > 1024 {
		return nil, errors.New("invalid LLM provider, endpoint, model, retry, or queue configuration")
	}
	client := &http.Client{Timeout: cfg.LLMRequestTimeout}
	switch cfg.LLMProvider {
	case "ollama":
		return ollama.NewClientWithOptions(cfg.LLMEndpoint, cfg.LLMModel, client, ollama.Options{Retries: cfg.LLMRetries, RetryBackoff: cfg.LLMRetryBackoff, Logger: logger, Metrics: metrics}), nil
	case "openai", "anthropic":
		key, err := hosted.ReadAPIKey(cfg.LLMAPIKeyFile)
		if err != nil {
			return nil, err
		}
		return hosted.NewClient(hosted.Options{Provider: cfg.LLMProvider, Endpoint: cfg.LLMEndpoint, Model: cfg.LLMModel, APIKey: key, HTTPClient: client, Retries: cfg.LLMRetries, RetryBackoff: cfg.LLMRetryBackoff, Logger: logger, Metrics: metrics})
	default:
		return nil, errors.New("LLM_PROVIDER must be ollama, openai, or anthropic")
	}
}
