package analyzer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"gitlab-doc-platform/internal/evidence"
)

func analyzeReleaseMetadata(path string, meta Metadata) ([]evidence.Item, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<10 {
		return nil, errors.New("release metadata exceeds limit")
	}
	var declaration struct {
		Schema    int      `json:"schema_version"`
		ProjectID string   `json:"project_id"`
		CommitSHA string   `json:"commit_sha"`
		Tags      []string `json:"git_tags"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&declaration) != nil {
		return nil, errors.New("invalid release metadata")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || declaration.Schema != 1 || declaration.ProjectID != meta.ProjectID || declaration.CommitSHA != meta.CommitSHA || len(declaration.Tags) > 256 {
		return nil, errors.New("release metadata identity or schema mismatch")
	}
	var out []evidence.Item
	for _, tag := range declaration.Tags {
		items := applicationVersionItems(tag, ".gitlab-docs-release.json", 0, meta.CommitSHA)
		for i := range items {
			items[i].Verification = "parsed CI-declared exact-commit Git tag; remote tag mapping not independently verified by the worker"
		}
		out = append(out, items...)
	}
	return out, nil
}
