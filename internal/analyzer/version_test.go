package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gitlab-doc-platform/internal/evidence"
)

func TestApplicationVersionDeclarations(t *testing.T) {
	for _, test := range []struct{ name, file, content, version string }{
		{"version file", "VERSION", "v2.3.4\n", "v2.3.4"},
		{"version text", "version.txt", "1.2.3-SNAPSHOT\n", "1.2.3-SNAPSHOT"},
		{"package", "package.json", `{"version":"2.1.0","dependencies":{"express":"9.9.9"}}`, "2.1.0"},
		{"pom", "pom.xml", `<project><parent><version>9.9.9</version></parent><version>1.0.0</version></project>`, "1.0.0"},
		{"parent not application", "pom.xml", `<project><parent><version>9.9.9</version></parent></project>`, ""},
		{"unresolved Maven property", "pom.xml", `<project><version>${revision}</version></project>`, ""},
		{"dependency not application", "package.json", `{"dependencies":{"express":"9.9.9"}}`, ""},
		{"nested package", "frontend/package.json", `{"version":"9.9.9"}`, ""},
		{"nested version", "vendor-app/VERSION", "9.9.9", ""},
		{"executable version", "VERSION", "$(touch file)", ""},
		{"Go constant", "version.go", `package main; const Version = "v1.2.3"`, "v1.2.3"},
		{"Go variable declaration", "main.go", `package main; var appVersion = "1.2.3"`, "1.2.3"},
		{"Go command", "cmd/api/main.go", `package main; const AppVersion = "1.2.3"`, "1.2.3"},
		{"Go version package", "internal/version/version.go", `package version; const Version = "1.2.3"`, "1.2.3"},
		{"Go function local", "main.go", `package main; func main() { version := "1.2.3"; _ = version }`, ""},
		{"Go computed expression", "version.go", `package main; const Version = "1." + "2.3"`, ""},
		{"Go build default", "version.go", `package main; var Version = "dev"`, ""},
		{"Go schema version", "version.go", `package main; const SchemaVersion = "1.2.3"`, ""},
		{"Go dependency constant", "internal/other/version.go", `package other; const Version = "9.9.9"`, ""},
		{"Go test constant", "version_test.go", `package main; const Version = "1.2.3"`, ""},
		{"Go toolchain not application", "go.mod", "module example.com/app\n\ngo 1.26.0\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, filepath.FromSlash(test.file))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.content), 0600); err != nil {
				t.Fatal(err)
			}
			snapshot, err := New().Analyze(context.Background(), root, Metadata{ProjectID: "42", CommitSHA: "abc"})
			if err != nil {
				t.Fatal(err)
			}
			version := ""
			for _, e := range snapshot.Evidence {
				if e.Type == "application_version" {
					if e.Confidence != evidence.Verified || e.SourceFile != test.file || e.CommitSHA != "abc" {
						t.Fatalf("missing version provenance: %+v", e)
					}
					version = e.Value
				}
			}
			if version != test.version {
				t.Fatalf("version=%q want=%q", version, test.version)
			}
		})
	}
}
