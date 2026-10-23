package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestHealthz(t *testing.T) {
	r := NewRouter(Dependencies{Config: config.Load(), Jobs: jobs.NewMemoryStore()})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestReadinessFailsWhenDatabaseIsUnavailable(t *testing.T) {
	router := NewRouter(Dependencies{Config: config.Load(), Jobs: jobs.NewMemoryStore(), ReadyCheck: func(context.Context) error { return errors.New("database unavailable") }})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestUploadLogsStorageFailureWithoutLeakingToken(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "OKP", "crv": "Ed25519", "kid": "test", "x": base64.RawURLEncoding.EncodeToString(publicKey),
		}}})
	}))
	defer jwks.Close()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "test"})
	claims, _ := json.Marshal(map[string]any{
		"iss": "https://gitlab.example.com", "aud": "gitlab-doc-platform",
		"exp": time.Now().Add(time.Hour).Unix(), "project_id": "42", "namespace_id": "101",
		"ref": "main", "ref_type": "branch", "sha": "abc123",
	})
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	token := signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(signed)))

	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable storage", true: "invalid storage path"}[blocked], func(t *testing.T) {
			cfg := config.Load()
			cfg.GitLabIssuer = "https://gitlab.example.com"
			cfg.GitLabAudience = "gitlab-doc-platform"
			cfg.GitLabJWKSURL = jwks.URL
			cfg.AllowedGroups = map[string]bool{"101": true}
			cfg.AllowedProjects = nil
			cfg.RequireProtectedRef = false
			cfg.StorageDir = t.TempDir()
			if blocked {
				cfg.StorageDir = filepath.Join(cfg.StorageDir, "not-a-directory")
				if err := os.WriteFile(cfg.StorageDir, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var logs, body bytes.Buffer
			writer := multipart.NewWriter(&body)
			archive, err := writer.CreateFormFile("archive", "source.tar.gz")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = archive.Write([]byte("uploaded archive"))
			_ = writer.WriteField("commit_sha", "abc123")
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			store := jobs.NewMemoryStore()
			router := NewRouter(Dependencies{Config: cfg, Jobs: store, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			payload := append([]byte(nil), body.Bytes()...)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs?private=must-not-log", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			wantStatus := http.StatusAccepted
			if blocked {
				wantStatus = http.StatusInternalServerError
				var failure struct {
					Message string `json:"msg"`
					Error   string `json:"error"`
				}
				if err := json.NewDecoder(strings.NewReader(logs.String())).Decode(&failure); err != nil {
					t.Fatal(err)
				}
				if failure.Message != "failed to prepare archive storage" || !strings.Contains(failure.Error, cfg.StorageDir) {
					t.Fatalf("missing underlying storage error: %s", &logs)
				}
			} else {
				entries, err := os.ReadDir(filepath.Join(cfg.StorageDir, "archives"))
				if err != nil || len(entries) != 1 {
					t.Fatalf("archive not persisted: entries=%v, error=%v", entries, err)
				}
			}
			if response.Code != wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			requestID := response.Header().Get("X-Request-ID")
			if requestID == "" || !strings.Contains(logs.String(), requestID) || !strings.Contains(logs.String(), "http request") {
				t.Fatalf("missing correlated request log: %s", &logs)
			}
			if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "must-not-log") {
				t.Fatal("logs include sensitive request data")
			}
			if !blocked {
				var accepted struct {
					ID string `json:"job_id"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
					t.Fatal(err)
				}
				if err := store.UpdateStatus(context.Background(), accepted.ID, jobs.Failed, "ollama timed out"); err != nil {
					t.Fatal(err)
				}
				retry := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(payload))
				retry.Header.Set("Content-Type", writer.FormDataContentType())
				retry.Header.Set("Authorization", "Bearer "+token)
				retried := httptest.NewRecorder()
				router.ServeHTTP(retried, retry)
				var result struct {
					ID      string      `json:"job_id"`
					Status  jobs.Status `json:"status"`
					Created bool        `json:"created"`
				}
				if err := json.Unmarshal(retried.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if retried.Code != http.StatusOK || result.ID != accepted.ID || result.Status != jobs.Queued || result.Created {
					t.Fatalf("retry did not requeue: status=%d body=%s", retried.Code, retried.Body)
				}
				job, err := store.Get(context.Background(), accepted.ID)
				if err != nil || job.Error != "" {
					t.Fatalf("old failure retained: job=%+v error=%v", job, err)
				}
				if _, err := os.Stat(job.ArchivePath); err != nil {
					t.Fatalf("retry removed original archive: %v", err)
				}
				entries, err := os.ReadDir(filepath.Join(cfg.StorageDir, "archives"))
				if err != nil || len(entries) != 1 {
					t.Fatalf("duplicate archive not cleaned up: entries=%v error=%v", entries, err)
				}
				var otherClaims map[string]any
				_ = json.Unmarshal(claims, &otherClaims)
				otherClaims["project_id"] = "43"
				otherJSON, _ := json.Marshal(otherClaims)
				otherSigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(otherJSON)
				otherToken := otherSigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(otherSigned)))
				for _, path := range []string{"/api/v1/jobs/" + accepted.ID, "/api/v1/jobs/" + accepted.ID + "/artifacts", "/api/v1/jobs/" + accepted.ID + "/publications"} {
					for _, test := range []struct {
						token  string
						status int
					}{{"", http.StatusUnauthorized}, {otherToken, http.StatusNotFound}, {token, map[bool]int{true: http.StatusConflict, false: http.StatusOK}[strings.HasSuffix(path, "/artifacts")]}} {
						request := httptest.NewRequest(http.MethodGet, path, nil)
						if test.token != "" {
							request.Header.Set("Authorization", "Bearer "+test.token)
						}
						result := httptest.NewRecorder()
						router.ServeHTTP(result, request)
						if result.Code != test.status {
							t.Fatalf("read authorization: path=%s status=%d want=%d", path, result.Code, test.status)
						}
					}
				}
			}
		})
	}
}
