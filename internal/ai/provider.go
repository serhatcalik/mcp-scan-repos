package ai

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

type Request struct {
	SystemPrompt  string
	UserPrompt    string
	Temperature   float64
	MaxTokens     int
	ContextLength int
	JSONSchema    json.RawMessage
	TraceID       string
}

type Response struct {
	Text             string
	GeneratedTokens  int
	PromptTokens     int
	ProviderMetadata map[string]string
}

type Admission struct {
	Context context.Context
	Release func()
}

type Gate interface {
	Acquire(context.Context) (Admission, error)
}

type QueuedProvider struct {
	Provider Provider
	Gate     Gate
	Logger   *slog.Logger
}

func (p QueuedProvider) Name() string  { return p.Provider.Name() }
func (p QueuedProvider) Model() string { return p.Provider.Model() }
func (p QueuedProvider) Generate(ctx context.Context, req Request) (Response, error) {
	started := time.Now()
	if p.Logger != nil {
		p.Logger.InfoContext(ctx, "waiting for inference admission", "request_id", req.TraceID, "model", p.Model())
	}
	permit, err := p.Gate.Acquire(ctx)
	if err != nil {
		return Response{}, err
	}
	if p.Logger != nil {
		p.Logger.InfoContext(ctx, "inference admitted", "request_id", req.TraceID, "wait_duration", time.Since(started))
	}
	defer permit.Release()
	response, err := p.Provider.Generate(permit.Context, req)
	if cause := context.Cause(permit.Context); cause != nil {
		return Response{}, cause
	}
	return response, err
}

type Provider interface {
	Generate(ctx context.Context, req Request) (Response, error)
	Name() string
	Model() string
}
