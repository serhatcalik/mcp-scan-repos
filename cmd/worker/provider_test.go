package main

import (
	"os"
	"path/filepath"
	"testing"

	"gitlab-doc-platform/internal/config"
)

func TestProviderSelection(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(filename, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ollama", "openai", "anthropic", "unknown"} {
		t.Setenv("LLM_PROVIDER", name)
		t.Setenv("LLM_MODEL", "test-model")
		t.Setenv("LLM_ENDPOINT", "")
		cfg := config.Load()
		cfg.LLMAPIKeyFile = filename
		provider, err := newProvider(cfg, nil, nil)
		if name == "unknown" {
			if err == nil {
				t.Fatal("unknown provider accepted")
			}
			continue
		}
		if err != nil || provider.Name() != name || provider.Model() != "test-model" {
			t.Fatalf("provider=%s err=%v", name, err)
		}
		cfg.LLMAPIKeyFile = "missing"
		_, err = newProvider(cfg, nil, nil)
		if name == "ollama" && err != nil || name != "ollama" && err == nil {
			t.Fatalf("key requirement for %s: %v", name, err)
		}
	}
}
