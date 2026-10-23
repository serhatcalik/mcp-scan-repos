package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"gitlab-doc-platform/internal/documents"
)

func WriteArtifactBundle(path string, artifacts documents.ArtifactSet) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".artifact-*.tar.gz")
	if err != nil {
		return err
	}
	defer f.Close()
	tempPath := f.Name()
	defer os.Remove(tempPath)
	if err := f.Chmod(0o640); err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	files := map[string][]byte{
		"generated-docs/technical-paper.md":      []byte(artifacts.TechnicalPaper),
		"generated-docs/infrastructure-paper.md": []byte(artifacts.InfrastructurePaper),
		"generated-docs/evidence.json":           artifacts.EvidenceJSON,
		"generated-docs/metadata.json":           artifacts.MetadataJSON,
		"generated-docs/validation.json":         artifacts.ValidationJSON,
	}
	for name, body := range artifacts.Diagrams {
		if name == "" || filepath.Base(name) != name || filepath.Ext(name) != ".mmd" {
			return errors.New("unsafe diagram artifact name")
		}
		files["generated-docs/diagrams/"+name] = []byte(body)
	}
	for name, body := range artifacts.References {
		if name != "api-reference.md" && name != "configuration-reference.md" {
			return errors.New("unsafe reference artifact name")
		}
		files["generated-docs/references/"+name] = []byte(body)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		if err := writeTarFile(tw, name, body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func writeTarFile(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))}); err != nil {
		return err
	}
	_, err := io.Copy(tw, bytes.NewReader(body))
	return err
}
