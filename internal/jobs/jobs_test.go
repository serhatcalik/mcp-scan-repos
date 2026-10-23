package jobs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab-doc-platform/internal/ai/ollama"
	"gitlab-doc-platform/internal/analyzer"
	"gitlab-doc-platform/internal/documents"
	"gitlab-doc-platform/internal/evidence"
)

func TestMemoryResubmissionOnlyRequeuesFailedJobs(t *testing.T) {
	for _, status := range []Status{Queued, Analyzing, Generating, Validating, Completed, Failed, Cancelled} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryStore()
			original := Job{ID: "original", IdempotencyKey: "key", ArchivePath: "original.tar.gz"}
			if _, _, err := store.CreateOrGet(ctx, original); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateStatus(ctx, original.ID, status, "old error"); err != nil {
				t.Fatal(err)
			}
			before, _ := store.Get(ctx, original.ID)
			got, created, err := store.CreateOrGet(ctx, Job{ID: "duplicate", IdempotencyKey: "key", ArchivePath: "new.tar.gz"})
			if err != nil || created || got.ID != original.ID || got.ArchivePath != original.ArchivePath {
				t.Fatalf("resubmission: job=%+v created=%v error=%v", got, created, err)
			}
			if status == Failed {
				if got.Status != Queued || got.Error != "" {
					t.Fatalf("failed job not reset: %+v", got)
				}
			} else if got != before {
				t.Fatalf("nonfailed job changed: before=%+v after=%+v", before, got)
			}
		})
	}
}

func TestMemoryAnalysisIdentityAndCopyIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	_, _, _ = store.CreateOrGet(ctx, Job{ID: "job", ProjectID: "42", CommitSHA: "abc", IdempotencyKey: "job"})
	snap := evidence.Snapshot{ProjectID: "42", CommitSHA: "abc", Evidence: []evidence.Item{{ID: "same", Value: "gin"}}}
	if err := store.SaveAnalysis(ctx, "job", snap); err != nil {
		t.Fatal(err)
	}
	snap.Evidence[0].Value = "changed"
	stored, err := store.GetAnalysis(ctx, "job")
	if err != nil || stored.Evidence[0].Value != "gin" {
		t.Fatalf("snapshot mutated: %+v error=%v", stored, err)
	}
	stored.ProjectID = "43"
	if err := store.SaveAnalysis(ctx, "job", stored); err != ErrSnapshotIdentity {
		t.Fatalf("identity mismatch accepted: %v", err)
	}
}

func TestWorkerProcessesArchiveWithOllamaAndLogsProgress(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "model failure"}[fail], func(t *testing.T) {
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/generate" {
					t.Errorf("unexpected model path %s", r.URL.Path)
				}
				if fail {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"response": `{"summaries":[],"recommendations":[]}`, "eval_count": 10, "done": true, "done_reason": "stop"})
			}))
			defer model.Close()
			root := t.TempDir()
			archive := writeSourceArchive(t, root)
			store := NewMemoryStore()
			_, _, err := store.CreateOrGet(context.Background(), Job{ID: "worker-job", ProjectID: "42", GroupID: "101", ArchivePath: archive, CommitSHA: "abc123", IdempotencyKey: "worker-job"})
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			worker := Worker{Store: store, Analyzer: analyzer.New(), Generator: documents.NewGenerator(ollama.NewClient(model.URL, "test-model", nil)), Logger: slog.New(slog.NewJSONHandler(&logs, nil)), StorageDir: root, ExtractedLimit: 1 << 20, JobTimeout: time.Second}
			err = worker.processOne(context.Background())
			job, getErr := store.Get(context.Background(), "worker-job")
			if getErr != nil {
				t.Fatal(getErr)
			}
			snapshot, errSnapshot := store.GetAnalysis(context.Background(), job.ID)
			if errSnapshot != nil || len(snapshot.Inventory) == 0 || len(snapshot.Evidence) == 0 {
				t.Fatalf("analysis not retained: snapshot=%+v error=%v", snapshot, errSnapshot)
			}
			if fail {
				if err == nil || job.Status != Failed || !strings.Contains(job.Error, "503") || !strings.Contains(logs.String(), "job failed") {
					t.Fatalf("failure not persisted: job=%+v error=%v logs=%s", job, err, &logs)
				}
				return
			}
			if err != nil || job.Status != Completed {
				t.Fatalf("job=%+v error=%v", job, err)
			}
			assertArtifact(t, filepath.Join(root, "artifacts", "worker-job.tar.gz"))
			for _, message := range []string{"job leased", "extracting archive", "analyzing repository", "generating documents", "writing artifacts", "job completed"} {
				if !strings.Contains(logs.String(), message) {
					t.Errorf("missing log %q", message)
				}
			}
		})
	}
}

func writeSourceArchive(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "source.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte("module example\n\ngo 1.24\n\nrequire github.com/gin-gonic/gin v1.11.0\n")
	if err := tw.WriteHeader(&tar.Header{Name: "go.mod", Mode: 0600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	for _, close := range []func() error{tw.Close, gz.Close, f.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func assertArtifact(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	files := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = true
	}
	for _, name := range []string{"technical-paper.md", "infrastructure-paper.md", "evidence.json", "metadata.json"} {
		if !files["generated-docs/"+name] {
			t.Errorf("missing artifact %s", name)
		}
	}
}
