package storage

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gitlab-doc-platform/internal/documents"
)

func TestBundleIsClosedAndHasValidationReport(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bundle.tar.gz")
	if err := WriteArtifactBundle(path, documents.ArtifactSet{TechnicalPaper: "facts", InfrastructurePaper: "infra", ValidationJSON: []byte(`{"status":"passed"}`), References: map[string]string{"api-reference.md": "api details", "configuration-reference.md": "config details"}}); err != nil {
		t.Fatal(err)
	}
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
	found := false
	references := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "generated-docs/validation.json" {
			found = true
		}
		if header.Name == "generated-docs/references/api-reference.md" || header.Name == "generated-docs/references/configuration-reference.md" {
			references++
		}
	}
	if !found {
		t.Fatal("missing validation report")
	}
	if references != 2 {
		t.Fatalf("missing reference files: %d", references)
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		t.Fatalf("invalid gzip footer: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary artifacts left behind: %v %v", entries, err)
	}
}

func TestUnsafeArtifactNameDoesNotPublishBundle(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bundle.tar.gz")
	if err := WriteArtifactBundle(path, documents.ArtifactSet{Diagrams: map[string]string{"../escape.mmd": "bad"}}); err == nil {
		t.Fatal("unsafe artifact name accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("partial artifact published: %v", err)
	}
}

func TestUnsafeReferenceNameDoesNotPublishBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := WriteArtifactBundle(path, documents.ArtifactSet{References: map[string]string{"../escape.md": "bad"}}); err == nil {
		t.Fatal("unsafe reference name accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("partial artifact published: %v", err)
	}
}
