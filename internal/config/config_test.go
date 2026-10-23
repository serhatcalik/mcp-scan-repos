package config

import (
	"testing"
	"time"
)

func TestGitHubConfiguration(t *testing.T) {
	for _, key := range []string{"GITHUB_ENABLED", "GITHUB_ISSUER", "GITHUB_OIDC_AUDIENCE", "GITHUB_JWKS_URL", "ALLOWED_GITHUB_OWNERS", "ALLOWED_GITHUB_REPOSITORIES", "GITHUB_REQUIRE_PROTECTED_REF"} {
		t.Setenv(key, "")
	}
	cfg := Load()
	if cfg.GitHubEnabled || cfg.GitHubIssuer != "https://token.actions.githubusercontent.com" || cfg.GitHubJWKSURL != "https://token.actions.githubusercontent.com/.well-known/jwks" || cfg.GitHubAudience != "gitlab-doc-platform" || len(cfg.AllowedGitHubOwners) != 0 || len(cfg.AllowedGitHubRepos) != 0 || cfg.GitHubProtectedRef {
		t.Fatal("unexpected GitHub defaults")
	}
	t.Setenv("GITHUB_ENABLED", "true")
	t.Setenv("GITHUB_ISSUER", "https://actions.example.com/")
	t.Setenv("GITHUB_OIDC_AUDIENCE", "docs")
	t.Setenv("ALLOWED_GITHUB_OWNERS", "101, 102")
	t.Setenv("ALLOWED_GITHUB_REPOSITORIES", "42")
	t.Setenv("GITHUB_REQUIRE_PROTECTED_REF", "true")
	cfg = Load()
	if !cfg.GitHubEnabled || cfg.GitHubJWKSURL != "https://actions.example.com/.well-known/jwks" || cfg.GitHubAudience != "docs" || !cfg.AllowedGitHubOwners["102"] || !cfg.AllowedGitHubRepos["42"] || !cfg.GitHubProtectedRef {
		t.Fatal("GitHub overrides ignored")
	}
	t.Setenv("GITHUB_JWKS_URL", "https://actions.example.com/keys")
	if Load().GitHubJWKSURL != "https://actions.example.com/keys" {
		t.Fatal("explicit JWKS URL ignored")
	}
}

func TestGenerationTimeoutConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, request, job   string
		wantRequest, wantJob time.Duration
	}{
		{"defaults", "", "", 30 * time.Minute, 75 * time.Minute},
		{"overrides", "45m", "120m", 45 * time.Minute, 120 * time.Minute},
		{"invalid values", "bad", "-1m", 30 * time.Minute, 75 * time.Minute},
		{"zero values", "0s", "0s", 30 * time.Minute, 75 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("LLM_REQUEST_TIMEOUT", test.request)
			t.Setenv("WORKER_JOB_TIMEOUT", test.job)
			cfg := Load()
			if cfg.LLMRequestTimeout != test.wantRequest || cfg.WorkerJobTimeout != test.wantJob {
				t.Fatalf("request timeout=%v, job timeout=%v", cfg.LLMRequestTimeout, cfg.WorkerJobTimeout)
			}
		})
	}
}

func TestHostedProviderDefaultsAndCacheIdentity(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Setenv("LLM_PROVIDER", provider)
		t.Setenv("LLM_ENDPOINT", "")
		t.Setenv("LLM_MODEL", "")
		cfg := Load()
		endpoint := "https://api.openai.com/v1"
		if provider == "anthropic" {
			endpoint = "https://api.anthropic.com/v1"
		}
		if cfg.LLMEndpoint != endpoint || cfg.LLMModel != "" {
			t.Fatal("hosted providers must not inherit Ollama settings")
		}
		other := cfg
		other.LLMAPIKeyFile = "/other/key"
		if cfg.GenerationVersion() != other.GenerationVersion() {
			t.Fatal("key rotation changed content identity")
		}
		other.LLMEndpoint = "https://gateway.example.com/v1"
		if cfg.GenerationVersion() == other.GenerationVersion() {
			t.Fatal("endpoint change did not invalidate cache")
		}
	}
}

func TestGenerationProfileConfigurationAndCacheIdentity(t *testing.T) {
	t.Setenv("LLM_CONTEXT_LENGTH", "16384")
	t.Setenv("LLM_MAX_TOKENS", "3000")
	t.Setenv("LLM_TEMPERATURE", "0")
	t.Setenv("LLM_RETRIES", "0")
	t.Setenv("LLM_MAX_CONCURRENT_REQUESTS", "2")
	t.Setenv("LLM_MAX_QUEUED_REQUESTS", "8")
	first := Load()
	if first.LLMContextLength != 16384 || first.LLMMaxTokens != 3000 || first.LLMTemperature != 0 || first.LLMRetries != 0 || first.LLMMaxConcurrent != 2 || first.LLMMaxQueued != 8 {
		t.Fatalf("unexpected generation settings: %+v", first)
	}
	second := first
	second.LLMModel = "different-model"
	if first.GenerationVersion() == second.GenerationVersion() {
		t.Fatal("model change did not invalidate cache")
	}
	second = first
	second.LLMMaxConcurrent++
	if first.GenerationVersion() != second.GenerationVersion() {
		t.Fatal("scheduling-only change unnecessarily invalidates content cache")
	}
}

func TestConfluenceConfigurationDefaultsAndOverrides(t *testing.T) {
	t.Setenv("CONFLUENCE_ENABLED", "")
	t.Setenv("CONFLUENCE_CONFIG_FILE", "")
	t.Setenv("CONFLUENCE_TOKEN_FILE", "")
	cfg := Load()
	if cfg.ConfluenceEnabled || cfg.ConfluenceConfigFile != "/etc/confluence/config.json" || cfg.ConfluenceTokenFile != "/etc/confluence-secret/token" {
		t.Fatal("unsafe Confluence defaults")
	}
	t.Setenv("CONFLUENCE_ENABLED", "true")
	t.Setenv("CONFLUENCE_CONFIG_FILE", "/config/confluence.json")
	t.Setenv("CONFLUENCE_TOKEN_FILE", "/secrets/confluence-token")
	cfg = Load()
	if !cfg.ConfluenceEnabled || cfg.ConfluenceConfigFile != "/config/confluence.json" || cfg.ConfluenceTokenFile != "/secrets/confluence-token" {
		t.Fatal("Confluence overrides ignored")
	}
}
