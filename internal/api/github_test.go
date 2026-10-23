package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab-doc-platform/internal/config"
	"gitlab-doc-platform/internal/jobs"
)

func TestGitHubUploadAndProviderIsolation(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "OKP", "crv": "Ed25519", "kid": "test", "x": base64.RawURLEncoding.EncodeToString(public),
		}}})
	}))
	defer jwks.Close()
	cfg := config.Load()
	cfg.StorageDir = t.TempDir()
	cfg.GitHubEnabled, cfg.GitHubIssuer, cfg.GitHubJWKSURL = true, "https://token.actions.githubusercontent.com", jwks.URL
	cfg.GitHubAudience, cfg.GitLabAudience = "docs", "docs"
	cfg.GitLabIssuer, cfg.GitLabJWKSURL = "https://gitlab.example.com", jwks.URL
	cfg.AllowedGitHubOwners, cfg.AllowedGroups = map[string]bool{"101": true}, map[string]bool{"101": true}
	cfg.AllowedGitHubRepos, cfg.AllowedProjects = nil, nil
	cfg.GitHubProtectedRef, cfg.RequireProtectedRef = false, false
	store := jobs.NewMemoryStore()
	router := NewRouter(Dependencies{Config: cfg, Jobs: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	sha := strings.Repeat("a", 40)
	token := func(provider, project string) string {
		claims := map[string]any{
			"iss": cfg.GitLabIssuer, "aud": "docs", "sub": "job", "exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
			"ref": "main", "ref_type": "branch", "sha": sha, "project_id": project, "namespace_id": "101",
		}
		if provider == "github" {
			claims["iss"], claims["ref"], claims["event_name"] = cfg.GitHubIssuer, "refs/heads/main", "push"
			claims["repository_id"], claims["repository"], claims["repository_owner_id"], claims["repository_owner"] = project, "backend/app", "101", "backend"
		}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "test"})
		payload, _ := json.Marshal(claims)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(signed)))
	}
	upload := func(path, bearerToken, project, commit, branch string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		_ = form.WriteField("project_id", project)
		_ = form.WriteField("commit_sha", commit)
		_ = form.WriteField("branch", branch)
		file, err := form.CreateFormFile("archive", "source.tar.gz")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write([]byte("archive content"))
		_ = form.Close()
		req := httptest.NewRequest(http.MethodPost, path, &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+bearerToken)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	githubToken, gitlabToken := token("github", "42"), token("gitlab", "42")
	var ids []string
	for _, test := range []struct{ path, token string }{{"/api/v1/github/jobs", githubToken}, {"/api/v1/jobs", gitlabToken}} {
		first := upload(test.path, test.token, "42", sha, "main")
		if first.Code != http.StatusAccepted {
			t.Fatalf("upload: %d %s", first.Code, first.Body)
		}
		var result struct {
			ID string `json:"job_id"`
		}
		if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, result.ID)
		retry := upload(test.path, test.token, "42", sha, "main")
		if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), result.ID) {
			t.Fatalf("idempotency: %d %s", retry.Code, retry.Body)
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("GitHub and GitLab collided")
	}
	job, err := store.Get(context.Background(), ids[0])
	if err != nil || job.ProjectID != "github:42" || job.GroupID != "github:101" || job.Branch != "main" {
		t.Fatalf("stored identity: %+v %v", job, err)
	}
	for _, mismatch := range []struct{ project, commit, branch string }{{"43", sha, "main"}, {"42", "different", "main"}, {"42", sha, "feature"}} {
		response := upload("/api/v1/github/jobs", githubToken, mismatch.project, mismatch.commit, mismatch.branch)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("metadata mismatch accepted: %d", response.Code)
		}
	}
	if response := upload("/api/v1/jobs", githubToken, "42", sha, "main"); response.Code != http.StatusUnauthorized {
		t.Fatal("GitHub token accepted by GitLab endpoint")
	}
	if response := upload("/api/v1/github/jobs", gitlabToken, "42", sha, "main"); response.Code != http.StatusUnauthorized {
		t.Fatal("GitLab token accepted by GitHub endpoint")
	}
	for _, suffix := range []string{"", "/artifacts", "/publications"} {
		for _, test := range []struct {
			path, token string
			status      int
		}{
			{"/api/v1/github/jobs/" + ids[0], token("github", "43"), http.StatusNotFound},
			{"/api/v1/github/jobs/" + ids[1], githubToken, http.StatusNotFound},
			{"/api/v1/jobs/" + ids[0], gitlabToken, http.StatusNotFound},
			{"/api/v1/github/jobs/" + ids[0], "", http.StatusUnauthorized},
		} {
			req := httptest.NewRequest(http.MethodGet, test.path+suffix, nil)
			req.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != test.status {
				t.Fatalf("ownership %s: %d %s", test.path+suffix, response.Code, response.Body)
			}
		}
	}
	if err := store.UpdateStatus(context.Background(), ids[0], jobs.Completed, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StorageDir, "artifacts"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StorageDir, "artifacts", ids[0]+".tar.gz"), []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/artifacts", "/publications"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/github/jobs/"+ids[0]+suffix, nil)
		req.Header.Set("Authorization", "Bearer "+githubToken)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("read %s: %d %s", suffix, response.Code, response.Body)
		}
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StorageDir, "archives"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("duplicate or rejected archives retained: %v %v", entries, err)
	}
	cfg.GitHubEnabled = false
	disabled := NewRouter(Dependencies{Config: cfg, Jobs: store})
	response := httptest.NewRecorder()
	disabled.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/github/jobs", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled GitHub endpoint exposed: %d", response.Code)
	}
}
