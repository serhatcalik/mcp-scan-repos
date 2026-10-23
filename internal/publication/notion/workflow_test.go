package notion

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"testing"
	"time"

	"gitlab-doc-platform/internal/ai/ollama"
	"gitlab-doc-platform/internal/analyzer"
	"gitlab-doc-platform/internal/api"
	"gitlab-doc-platform/internal/config"
	"gitlab-doc-platform/internal/documents"
	"gitlab-doc-platform/internal/jobs"
)

func fixtureUpload(t *testing.T, sample string) ([]byte, string) {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	root := filepath.Join("..", "..", "analyzer", "testdata", sample)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o600, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	part, err := form.CreateFormFile("archive", "source.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"project_id": "42", "commit_sha": "abcdef123456", "branch": "main"} {
		if err := form.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return upload.Bytes(), form.FormDataContentType()
}

func TestOIDCUploadToArtifactsAndNotionWithPostgres(t *testing.T) {
	for _, sample := range []string{"go-app", "spring-app"} {
		t.Run(sample, func(t *testing.T) {
			db := testDB(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := Configure(ctx, db, []Target{{GroupID: "101", ParentPageID: testParent, Enabled: true}}); err != nil {
				t.Fatal(err)
			}
			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "workflow", "x": base64.RawURLEncoding.EncodeToString(public)}}})
			}))
			defer jwks.Close()
			tokenFor := func(project string) string {
				header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "workflow"})
				claims, _ := json.Marshal(map[string]any{"iss": "https://gitlab.example.com", "aud": "gitlab-doc-platform", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "namespace_id": "101", "namespace_path": "backend", "project_id": project, "project_path": "backend/" + sample, "sha": "abcdef123456", "ref": "main", "ref_type": "branch"})
				signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
				return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(signed)))
			}
			cfg := config.Load()
			cfg.GitLabIssuer, cfg.GitLabAudience, cfg.GitLabJWKSURL = "https://gitlab.example.com", "gitlab-doc-platform", jwks.URL
			cfg.AllowedGroups, cfg.AllowedProjects, cfg.RequireProtectedRef = map[string]bool{"101": true}, nil, false
			cfg.StorageDir, cfg.MaxArchiveBytes = t.TempDir(), 1<<20
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			store := jobs.NewPostgresStore(db)
			router := api.NewRouter(api.Dependencies{Config: cfg, Jobs: store, Logger: logger, ReadyCheck: db.Ping})
			upload, contentType := fixtureUpload(t, sample)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(upload))
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("Authorization", "Bearer "+tokenFor("42"))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusAccepted {
				t.Fatalf("upload: %d %s", response.Code, response.Body)
			}
			var accepted struct {
				JobID string `json:"job_id"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil || accepted.JobID == "" {
				t.Fatalf("upload response: %v %s", err, response.Body)
			}
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("credentials forwarded to model")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"response": `{"summaries":[],"recommendations":[]}`, "done": true, "done_reason": "stop"})
			}))
			defer model.Close()
			worker := jobs.Worker{Store: store, Analyzer: analyzer.New(), Generator: documents.NewGenerator(ollama.NewClient(model.URL, "mock", nil)), Logger: logger, Interval: 10 * time.Millisecond, StorageDir: cfg.StorageDir, ExtractedLimit: 1 << 20, JobTimeout: time.Minute}
			workerCtx, stopWorker := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- worker.Run(workerCtx) }()
			fake, client := newFakeNotion(t)
			publisher := &Runner{DB: db, Client: client, Logger: logger, Interval: 20 * time.Millisecond}
			publicationDone := make(chan struct{})
			go func() { defer close(publicationDone); publisher.Run(workerCtx) }()
			defer func() {
				stopWorker()
				if err := <-done; err != nil {
					t.Error(err)
				}
				<-publicationDone
			}()
			for {
				job, err := store.Get(ctx, accepted.JobID)
				if err != nil {
					t.Fatal(err)
				}
				if job.Status == jobs.Failed {
					t.Fatalf("generation failed: %s", job.Error)
				}
				if job.Status == jobs.Completed {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			snapshot, err := store.GetAnalysis(ctx, accepted.JobID)
			if err != nil || len(snapshot.Evidence) == 0 || snapshot.Framework == "" {
				t.Fatalf("analysis: %+v %v", snapshot, err)
			}
			for {
				publications, err := store.ListPublications(ctx, accepted.JobID)
				if err != nil {
					t.Fatal(err)
				}
				if len(publications) > 0 {
					if publications[0].Status == "failed" || publications[0].Status == "retrying" {
						t.Fatalf("publication failed: %+v", publications[0])
					}
					if publications[0].Status == "published" {
						break
					}
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			fake.mu.Lock()
			creates := fake.creates
			diagrams, tables := 0, 0
			for _, blocks := range fake.pages {
				for _, b := range blocks {
					if b.Type == "code" {
						if b.Code.Language != "mermaid" {
							t.Error("diagram did not use Mermaid language")
						}
						diagrams++
					}
					if b.Type == "table" {
						tables++
					}
				}
			}
			fake.mu.Unlock()
			if creates != 7 {
				t.Fatalf("Notion hierarchy pages=%d", creates)
			}
			if diagrams != 3 || tables == 0 {
				t.Fatalf("native publication missing: diagrams=%d tables=%d", diagrams, tables)
			}
			for _, suffix := range []string{"/artifacts", "/publications"} {
				for _, identity := range []string{"42", "43"} {
					request := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+accepted.JobID+suffix, nil)
					request.Header.Set("Authorization", "Bearer "+tokenFor(identity))
					result := httptest.NewRecorder()
					router.ServeHTTP(result, request)
					if identity == "43" {
						if result.Code != http.StatusNotFound {
							t.Fatal("cross-project content exposed")
						}
						continue
					}
					if result.Code != http.StatusOK {
						t.Fatalf("download: %d %s", result.Code, result.Body)
					}
					if suffix == "/publications" {
						var out struct {
							Publications []jobs.Publication `json:"publications"`
						}
						if err := json.Unmarshal(result.Body.Bytes(), &out); err != nil || len(out.Publications) != 1 || out.Publications[0].Status != "published" || out.Publications[0].ProjectURL == "" {
							t.Fatalf("publication status: %v %s", err, result.Body)
						}
					} else {
						gz, err := gzip.NewReader(bytes.NewReader(result.Body.Bytes()))
						if err != nil {
							t.Fatal(err)
						}
						tr := tar.NewReader(gz)
						files := map[string]bool{}
						for {
							header, err := tr.Next()
							if err == io.EOF {
								break
							}
							if err != nil {
								t.Fatal(err)
							}
							files[header.Name] = true
						}
						_ = gz.Close()
						for _, name := range []string{"technical-paper.md", "infrastructure-paper.md", "evidence.json", "metadata.json"} {
							if !files["generated-docs/"+name] {
								t.Fatalf("artifact missing %s: %v", name, files)
							}
						}
					}
				}
			}
		})
	}
}
