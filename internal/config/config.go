package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	APIAddr              string
	MCPAddr              string
	MCPManagementAddr    string
	MCPAuthFile          string
	WorkerHTTPAddr       string
	DatabaseURL          string
	StorageDir           string
	GitLabIssuer         string
	GitLabAudience       string
	GitLabJWKSURL        string
	AllowedGroups        map[string]bool
	AllowedProjects      map[string]bool
	RequireProtectedRef  bool
	GitHubEnabled        bool
	GitHubIssuer         string
	GitHubAudience       string
	GitHubJWKSURL        string
	AllowedGitHubOwners  map[string]bool
	AllowedGitHubRepos   map[string]bool
	GitHubProtectedRef   bool
	MaxArchiveBytes      int64
	MaxExtractedBytes    int64
	HTTPReadTimeout      time.Duration
	HTTPWriteTimeout     time.Duration
	APIRequestsPerMinute int64
	WorkerPollInterval   time.Duration
	WorkerJobTimeout     time.Duration
	LLMEndpoint          string
	LLMModel             string
	LLMRequestTimeout    time.Duration
	LLMProvider          string
	LLMAPIKeyFile        string
	LLMContextLength     int
	LLMMaxTokens         int
	LLMTemperature       float64
	LLMRetries           int
	LLMRetryBackoff      time.Duration
	LLMMaxConcurrent     int
	LLMMaxQueued         int
	NotionEnabled        bool
	NotionTargetsFile    string
	NotionTokenFile      string
	ConfluenceEnabled    bool
	ConfluenceConfigFile string
	ConfluenceTokenFile  string
}

func Load() Config {
	issuer := env("GITLAB_ISSUER", "https://gitlab.example.com")
	githubIssuer := env("GITHUB_ISSUER", "https://token.actions.githubusercontent.com")
	provider := env("LLM_PROVIDER", "ollama")
	endpoint, model := "http://ollama.gitlab-docs.svc.cluster.local:11434", "qwen2.5-coder:7b"
	switch provider {
	case "openai":
		endpoint, model = "https://api.openai.com/v1", ""
	case "anthropic":
		endpoint, model = "https://api.anthropic.com/v1", ""
	}
	return Config{
		APIAddr:              env("API_ADDR", ":8080"),
		MCPAddr:              env("MCP_ADDR", ":8081"),
		MCPManagementAddr:    env("MCP_MANAGEMENT_ADDR", ":9091"),
		MCPAuthFile:          env("MCP_AUTH_FILE", "/etc/mcp-auth/clients.json"),
		WorkerHTTPAddr:       env("WORKER_HTTP_ADDR", ":9090"),
		DatabaseURL:          env("DATABASE_URL", "postgres://gitlab_docs:gitlab_docs@postgres:5432/gitlab_docs?sslmode=disable"),
		StorageDir:           env("STORAGE_DIR", "/var/lib/gitlab-docs"),
		GitLabIssuer:         issuer,
		GitLabAudience:       env("GITLAB_OIDC_AUDIENCE", "gitlab-doc-platform"),
		GitLabJWKSURL:        env("GITLAB_JWKS_URL", strings.TrimRight(issuer, "/")+"/oauth/discovery/keys"),
		AllowedGroups:        csvSet(os.Getenv("ALLOWED_GITLAB_GROUPS")),
		AllowedProjects:      csvSet(os.Getenv("ALLOWED_GITLAB_PROJECTS")),
		RequireProtectedRef:  boolEnv("REQUIRE_PROTECTED_REF", false),
		GitHubEnabled:        boolEnv("GITHUB_ENABLED", false),
		GitHubIssuer:         githubIssuer,
		GitHubAudience:       env("GITHUB_OIDC_AUDIENCE", "gitlab-doc-platform"),
		GitHubJWKSURL:        env("GITHUB_JWKS_URL", strings.TrimRight(githubIssuer, "/")+"/.well-known/jwks"),
		AllowedGitHubOwners:  csvSet(os.Getenv("ALLOWED_GITHUB_OWNERS")),
		AllowedGitHubRepos:   csvSet(os.Getenv("ALLOWED_GITHUB_REPOSITORIES")),
		GitHubProtectedRef:   boolEnv("GITHUB_REQUIRE_PROTECTED_REF", false),
		MaxArchiveBytes:      int64Env("MAX_ARCHIVE_BYTES", 100<<20),
		MaxExtractedBytes:    int64Env("MAX_EXTRACTED_BYTES", 500<<20),
		HTTPReadTimeout:      durationEnv("HTTP_READ_TIMEOUT", 30*time.Second),
		HTTPWriteTimeout:     durationEnv("HTTP_WRITE_TIMEOUT", 30*time.Second),
		APIRequestsPerMinute: int64Env("API_REQUESTS_PER_MINUTE", 120),
		WorkerPollInterval:   durationEnv("WORKER_POLL_INTERVAL", 5*time.Second),
		WorkerJobTimeout:     positiveDurationEnv("WORKER_JOB_TIMEOUT", 75*time.Minute),
		LLMEndpoint:          env("LLM_ENDPOINT", endpoint),
		LLMModel:             env("LLM_MODEL", model),
		LLMRequestTimeout:    positiveDurationEnv("LLM_REQUEST_TIMEOUT", 30*time.Minute),
		LLMProvider:          provider,
		LLMAPIKeyFile:        env("LLM_API_KEY_FILE", "/etc/llm-secret/api-key"),
		LLMContextLength:     int(int64Env("LLM_CONTEXT_LENGTH", 8192)),
		LLMMaxTokens:         int(int64Env("LLM_MAX_TOKENS", 1800)),
		LLMTemperature:       floatEnv("LLM_TEMPERATURE", 0.1),
		LLMRetries:           int(int64Env("LLM_RETRIES", 2)),
		LLMRetryBackoff:      positiveDurationEnv("LLM_RETRY_BACKOFF", 2*time.Second),
		LLMMaxConcurrent:     int(int64Env("LLM_MAX_CONCURRENT_REQUESTS", 1)),
		LLMMaxQueued:         int(int64Env("LLM_MAX_QUEUED_REQUESTS", 16)),
		NotionEnabled:        boolEnv("NOTION_ENABLED", false),
		NotionTargetsFile:    env("NOTION_TARGETS_FILE", "/etc/notion/targets.json"),
		NotionTokenFile:      env("NOTION_TOKEN_FILE", "/etc/notion-secret/token"),
		ConfluenceEnabled:    boolEnv("CONFLUENCE_ENABLED", false),
		ConfluenceConfigFile: env("CONFLUENCE_CONFIG_FILE", "/etc/confluence/config.json"),
		ConfluenceTokenFile:  env("CONFLUENCE_TOKEN_FILE", "/etc/confluence-secret/token"),
	}
}

func floatEnv(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return fallback
}

func (c Config) GenerationVersion() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%g", c.LLMProvider, strings.TrimRight(c.LLMEndpoint, "/"), c.LLMModel, c.LLMContextLength, c.LLMMaxTokens, c.LLMTemperature)))
	return hex.EncodeToString(sum[:8])
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func csvSet(value string) map[string]bool {
	out := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out[item] = true
		}
	}
	return out
}

func boolEnv(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func int64Env(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		parsed, err := time.ParseDuration(v)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func positiveDurationEnv(key string, fallback time.Duration) time.Duration {
	value := durationEnv(key, fallback)
	if value <= 0 {
		return fallback
	}
	return value
}
