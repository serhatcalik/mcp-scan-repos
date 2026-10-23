package ai

import (
	"context"
	"errors"
	"testing"
)

type testGate struct {
	acquire func(context.Context) (Admission, error)
}

func (g testGate) Acquire(ctx context.Context) (Admission, error) { return g.acquire(ctx) }

type testProvider struct {
	generate func(context.Context, Request) (Response, error)
}

func (p testProvider) Generate(ctx context.Context, r Request) (Response, error) {
	return p.generate(ctx, r)
}
func (testProvider) Name() string  { return "test" }
func (testProvider) Model() string { return "model" }

func TestQueuedProviderReleasesOnFailureAndPreservesLeaseCause(t *testing.T) {
	released := false
	leaseError := errors.New("lease lost")
	ctx, cancel := context.WithCancelCause(context.Background())
	provider := QueuedProvider{Gate: testGate{acquire: func(context.Context) (Admission, error) {
		return Admission{Context: ctx, Release: func() { released = true }}, nil
	}}, Provider: testProvider{generate: func(context.Context, Request) (Response, error) {
		cancel(leaseError)
		return Response{}, context.Canceled
	}}}
	if _, err := provider.Generate(context.Background(), Request{}); !errors.Is(err, leaseError) || !released {
		t.Fatalf("lease error=%v release=%v", err, released)
	}
}
