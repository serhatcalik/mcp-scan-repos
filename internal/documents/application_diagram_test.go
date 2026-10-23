package documents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab-doc-platform/internal/analyzer"
	"gitlab-doc-platform/internal/evidence"
)

func TestSpringArchitectureIsAComponentOverview(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"pom.xml": `<project><artifactId>demo</artifactId><version>0.0.1-SNAPSHOT</version><parent><artifactId>spring-boot-starter-parent</artifactId></parent></project>`,
		"src/main/java/example/DemoApplication.java":                  "package example;\n@SpringBootApplication public class DemoApplication {}",
		"src/main/java/example/api/CategoryRestController.java":       "package example.api;\nimport example.service.CategoryService;\n@RestController public class CategoryRestController {\n@RequestMapping(\"/categories\") void root() {}\n@GetMapping(\"/{id}\") void get() {}\n}",
		"src/main/java/example/service/CategoryService.java":          "package example.service;\nimport example.data.CategoryRepository;\n@Service public class CategoryService {}",
		"src/main/java/example/data/CategoryRepository.java":          "package example.data;\nimport org.springframework.data.jpa.repository.JpaRepository;\nimport example.model.Category;\npublic interface CategoryRepository extends JpaRepository<Category, Long> {}",
		"src/main/java/example/model/Category.java":                   "package example.model;\n@Entity public class Category {}",
		"src/main/java/example/model/CategoryDTO.java":                "package example.model;\npublic class CategoryDTO {}",
		"src/main/java/example/errors/CategoryNotFoundException.java": "package example.errors;\npublic class CategoryNotFoundException {}",
		"src/test/java/example/service/CategoryServiceImplTest.java":  "package example.service;\n@Service public class CategoryServiceImplTest {}",
	}
	for file, body := range files {
		filename := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := analyzer.New().Analyze(context.Background(), root, analyzer.Metadata{ProjectID: "42", CommitSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	d := appDiagram(snap)
	for _, noise := range []string{"0.0.1-SNAPSHOT", "spring boot", "CategoryDTO", "CategoryNotFoundException", "CategoryServiceImplTest", "@RestController", "@Service", "JpaRepository"} {
		if strings.Contains(d, noise) {
			t.Errorf("declaration noise %q in diagram:\n%s", noise, d)
		}
	}
	for _, component := range []string{"DemoApplication", "CategoryRestController", "CategoryService", "CategoryRepository", "Category", "2 mapping declarations"} {
		if !strings.Contains(d, component) {
			t.Errorf("missing component %q:\n%s", component, d)
		}
	}
	if strings.Count(d, "[\"") != 10 || strings.Count(d, "-->|imports|") != 3 {
		t.Fatalf("want five groups/nodes and three explicit imports:\n%s", d)
	}
	if strings.Contains(d, "/categories/{id}") {
		t.Fatal("invented effective route")
	}
	artifacts, err := NewGenerator(mockProvider{}).Generate(context.Background(), snap, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(artifacts.EvidenceJSON), "CategoryDTO") || !strings.Contains(string(artifacts.EvidenceJSON), "CategoryServiceImplTest") {
		t.Fatal("overview discarded original evidence")
	}
}

func TestArchitectureRollupIsBoundedAndDeterministic(t *testing.T) {
	s := evidence.Snapshot{}
	for i := 0; i < 100; i++ {
		file := fmt.Sprintf("src/main/java/example/services/Service%d.java", i)
		s.Evidence = append(s.Evidence,
			evidence.Item{Type: "java_package", Value: "example.services", SourceFile: file, Confidence: evidence.Verified},
			evidence.Item{Category: "application", Type: "service", Value: fmt.Sprintf("Service%d @Service", i), SourceFile: file, Confidence: evidence.Verified},
		)
	}
	d := appDiagram(s)
	if strings.Count(d, "[\"") != 2 || !strings.Contains(d, "and 97 more") {
		t.Fatalf("large package must be summarized into one node:\n%s", d)
	}
	for i, j := 0, len(s.Evidence)-1; i < j; i, j = i+1, j-1 {
		s.Evidence[i], s.Evidence[j] = s.Evidence[j], s.Evidence[i]
	}
	if appDiagram(s) != d {
		t.Fatal("overview depends on evidence order")
	}
	for i := 0; i < 100; i++ {
		s.Evidence = append(s.Evidence, evidence.Item{Type: "go_package", Value: fmt.Sprintf("internal/p%d", i), SourceFile: fmt.Sprintf("internal/p%d/main.go", i), Confidence: evidence.Verified})
	}
	d = appDiagram(s)
	nodes := 0
	for _, line := range strings.Split(d, "\n") {
		if mermaidNode.MatchString("  " + strings.TrimSpace(line)) {
			nodes++
		}
	}
	if nodes != maxApplicationNodes || !strings.Contains(d, "Overview omitted 77 components") {
		t.Fatalf("overview must enforce component limit:\n%s", d)
	}
	for i := range s.Evidence {
		s.Evidence[i].ID = fmt.Sprintf("e%d", i)
	}
	if _, err := NewGenerator(mockProvider{}).Generate(context.Background(), s, Metadata{}); err != nil {
		t.Fatalf("bounded overview failed artifact validation: %v", err)
	}
}

func TestJavaArchitectureImportsRequireUniqueExplicitTargets(t *testing.T) {
	var items []evidence.Item
	for _, file := range []string{"src/one/Service.java", "other/Service.java"} {
		items = append(items,
			evidence.Item{Type: "java_package", Value: "example.services", SourceFile: file, Confidence: evidence.Verified},
			evidence.Item{Type: "java_type", Value: "Service", SourceFile: file, Confidence: evidence.Verified},
			evidence.Item{Type: "service", Value: "Service @Service", SourceFile: file, Confidence: evidence.Verified})
	}
	items = append(items,
		evidence.Item{Type: "java_package", Value: "example.api", SourceFile: "Controller.java", Confidence: evidence.Verified},
		evidence.Item{Type: "controller", Value: "Controller @RestController", SourceFile: "Controller.java", Confidence: evidence.Verified},
		evidence.Item{Type: "java_import", Value: "example.services.Service", SourceFile: "Controller.java", Confidence: evidence.Verified})
	if d := appDiagram(evidence.Snapshot{Evidence: items}); strings.Contains(d, "-->") {
		t.Fatalf("ambiguous Java type resolution produced an edge:\n%s", d)
	}
	items = items[3:]
	d := appDiagram(evidence.Snapshot{Evidence: items})
	if strings.Count(d, "-->|imports|") != 1 {
		t.Fatalf("unique explicit local import missing:\n%s", d)
	}
	items[len(items)-1].Confidence = evidence.Inferred
	if strings.Contains(appDiagram(evidence.Snapshot{Evidence: items}), "-->") {
		t.Fatal("inferred relationship used as verified edge")
	}
}
