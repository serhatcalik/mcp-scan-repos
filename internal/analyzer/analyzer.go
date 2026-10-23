package analyzer

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"gitlab-doc-platform/internal/evidence"
)

type Analyzer struct{}

type Metadata struct {
	ProjectID string
	CommitSHA string
}

func New() Analyzer {
	return Analyzer{}
}

func (Analyzer) Analyze(ctx context.Context, root string, meta Metadata) (evidence.Snapshot, error) {
	snap := evidence.Snapshot{ProjectID: meta.ProjectID, CommitSHA: meta.CommitSHA}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build", "target", "generated-docs":
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			snap.Warnings = append(snap.Warnings, rel+": symlink skipped")
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			snap.Warnings = append(snap.Warnings, rel+": nonregular file skipped")
			return nil
		}
		snap.Inventory = append(snap.Inventory, evidence.File{Path: rel, Size: info.Size(), Language: fileLanguage(rel)})
		if info.Size() > 2<<20 {
			snap.Warnings = append(snap.Warnings, rel+": exceeds 2 MiB analysis limit")
			return nil
		}
		items, err := analyzeFile(path, rel, meta.CommitSHA)
		if rel == ".gitlab-docs-release.json" {
			items, err = analyzeReleaseMetadata(path, meta)
		}
		if err != nil {
			snap.Warnings = append(snap.Warnings, rel+": analysis failed; unsupported or malformed syntax")
			return nil
		}
		for _, item := range items {
			snap.Evidence = append(snap.Evidence, item)
		}
		return nil
	})
	snap.Evidence = deduplicate(snap.Evidence)
	for _, e := range snap.Evidence {
		applySummary(&snap, e)
	}
	return snap, err
}

func analyzeFile(path, rel, sha string) ([]evidence.Item, error) {
	switch {
	case rel == "VERSION" || rel == "version.txt":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return applicationVersionItems(strings.TrimSpace(string(data)), rel, 1, sha), nil
	case rel == "go.mod":
		var out []evidence.Item
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		mod, err := modfile.Parse(rel, data, nil)
		if err != nil {
			return nil, err
		}
		if mod.Module != nil {
			out = append(out, item("application", "go_module", mod.Module.Mod.Path, rel, mod.Module.Syntax.Start.Line, mod.Module.Syntax.End.Line, sha))
		}
		for _, dep := range mod.Require {
			out = append(out, item("dependency", "go_dependency", dep.Mod.Path, rel, dep.Syntax.Start.Line, dep.Syntax.End.Line, sha))
			category, typ, value := "", "", ""
			switch {
			case dep.Mod.Path == "github.com/gin-gonic/gin":
				category, typ, value = "application", "framework", "gin"
			case dep.Mod.Path == "gorm.io/gorm":
				category, typ, value = "data", "orm", "gorm"
			case strings.HasPrefix(dep.Mod.Path, "github.com/jackc/pgx"):
				category, typ, value = "data", "database_client", "postgresql"
			}
			if value != "" {
				out = append(out, item(category, typ, value, rel, dep.Syntax.Start.Line, dep.Syntax.End.Line, sha))
			}
		}
		return out, nil
	case filepath.Base(rel) == "go.sum":
		return analyzeGoSum(path, rel, sha)
	case filepath.Base(rel) == "package.json":
		return analyzePackageJSON(path, rel, sha)
	case filepath.Base(rel) == "pom.xml":
		return analyzePOM(path, rel, sha)
	case strings.HasSuffix(rel, "build.gradle") || strings.HasSuffix(rel, "build.gradle.kts"):
		items, err := scanContains(path, rel, sha, map[string]fileSignal{
			"spring-boot":                  {Category: "application", Type: "framework", Value: "spring boot"},
			"spring-boot-starter-web":      {Category: "application", Type: "web", Value: "spring mvc"},
			"spring-boot-starter-data-jpa": {Category: "data", Type: "orm", Value: "jpa"},
		})
		for i := range items {
			items[i].Confidence = evidence.Inferred
			items[i].Verification = "keyword detection; Gradle execution and dependency resolution unavailable"
		}
		return items, err
	case strings.HasSuffix(rel, ".go"):
		if strings.HasSuffix(rel, "_test.go") {
			return nil, nil
		}
		return analyzeGoSource(path, rel, sha)
	case strings.HasSuffix(rel, ".java"):
		return analyzeJavaSource(path, rel, sha)
	case strings.HasSuffix(rel, ".ts") || strings.HasSuffix(rel, ".js") || strings.HasSuffix(rel, ".tsx") || strings.HasSuffix(rel, ".jsx"):
		return analyzeNodeSource(path, rel, sha)
	case filepath.Base(rel) == "Dockerfile":
		return analyzeDockerfile(path, rel, sha)
	case rel == ".gitlab-ci.yml" || strings.Contains(rel, "gitlab-ci"):
		return analyzeYAML(path, rel, sha)
	case strings.Contains(rel, "Chart.yaml"):
		return analyzeYAML(path, rel, sha)
	case strings.HasSuffix(rel, ".tf"):
		return analyzeTerraform(path, rel, sha)
	case strings.HasSuffix(rel, ".properties") || strings.HasPrefix(filepath.Base(rel), ".env"):
		return analyzeProperties(path, rel, sha)
	case strings.HasSuffix(rel, ".yaml") || strings.HasSuffix(rel, ".yml"):
		resources, err := analyzeYAML(path, rel, sha)
		if err == nil {
			return resources, nil
		}
		if !strings.Contains(rel, "/templates/") {
			return nil, err
		}
		return scanContains(path, rel, sha, map[string]fileSignal{
			"kind: Deployment": {Category: "infrastructure", Type: "kubernetes", Value: "deployment"},
			"kind: Service":    {Category: "infrastructure", Type: "kubernetes", Value: "service"},
			"kind: Ingress":    {Category: "infrastructure", Type: "kubernetes", Value: "ingress"},
			"kind: Route":      {Category: "infrastructure", Type: "openshift", Value: "route"},
			"kind: HTTPRoute":  {Category: "infrastructure", Type: "gateway", Value: "gateway api httproute"},
		})
	default:
		return nil, nil
	}
}

type fileSignal struct {
	Category string
	Type     string
	Value    string
}

func scanContains(path, rel, sha string, signals map[string]fileSignal) ([]evidence.Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []evidence.Item
	scanner := bufio.NewScanner(f)
	line := 0
	seen := map[string]bool{}
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(text), "#") || strings.HasPrefix(strings.TrimSpace(text), "//") {
			continue
		}
		for needle, sig := range signals {
			matches := strings.Contains(text, needle)
			if strings.HasPrefix(needle, "kind: ") {
				matches = strings.TrimSpace(strings.SplitN(text, "#", 2)[0]) == needle
			}
			if matches && !seen[needle] {
				out = append(out, item(sig.Category, sig.Type, sig.Value, rel, line, line, sha))
				seen[needle] = true
			}
		}
	}
	return out, scanner.Err()
}

func analyzePackageJSON(path, rel, sha string) ([]evidence.Item, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pkg struct {
		Version         string            `json:"version"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return nil, err
	}
	var out []evidence.Item
	if rel == "package.json" {
		out = append(out, applicationVersionItems(pkg.Version, rel, 0, sha)...)
	}
	for dep := range pkg.Dependencies {
		out = append(out, item("dependency", "npm_dependency", dep, rel, 0, 0, sha))
		switch dep {
		case "next":
			out = append(out, item("application", "framework", "next.js", rel, 1, 1, sha))
		case "express":
			out = append(out, item("application", "framework", "express", rel, 1, 1, sha))
		case "prisma", "@prisma/client":
			out = append(out, item("data", "orm", "prisma", rel, 1, 1, sha))
		}
	}
	for dep := range pkg.DevDependencies {
		out = append(out, item("dependency", "npm_dev_dependency", dep, rel, 0, 0, sha))
	}
	return out, nil
}

func applicationVersionItems(value, source string, line int, sha string) []evidence.Item {
	if !evidence.ValidApplicationVersion(value) {
		return nil
	}
	return []evidence.Item{item("application", "application_version", value, source, line, line, sha)}
}

func item(category, typ, value, source string, start, end int, sha string) evidence.Item {
	sum := sha1.Sum([]byte(strings.Join([]string{category, typ, value, source}, "\x00")))
	return evidence.Item{
		ID: hex.EncodeToString(sum[:8]), Category: category, Type: typ, Value: value,
		SourceFile: source, LineStart: start, LineEnd: end, CommitSHA: sha,
		Verification: "source", Confidence: evidence.Verified,
	}
}

func applySummary(s *evidence.Snapshot, item evidence.Item) {
	if item.Type == "go_module" && s.Language == "" {
		s.Language = "go"
	}
	if item.Type != "framework" && item.Type != "database_client" {
		return
	}
	if item.Confidence != evidence.Verified {
		return
	}
	switch item.Value {
	case "gin":
		s.Language, s.Framework = "go", "gin"
	case "spring boot":
		s.Language, s.Framework = "java", "spring boot"
	case "next.js", "express":
		s.Language, s.Framework = "javascript/typescript", item.Value
	case "postgresql":
		s.Database = "postgresql"
	}
}
