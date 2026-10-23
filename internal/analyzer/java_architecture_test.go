package analyzer

import (
	"context"
	"path/filepath"
	"testing"
)

func TestJavaArchitecturalDeclarationsIgnoreCommentsAndStrings(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Main.java"), `package example.app;
import example.service.CatalogService;
// import fake.Comment;
/* package fake.comment; */
@SpringBootApplication
public class Main {
  String decoy = "import fake.String; @SpringBootApplication";
}`)
	s, err := New().Analyze(context.Background(), root, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range s.Evidence {
		counts[e.Type]++
		if e.Type == "java_package" && e.Value != "example.app" || e.Type == "java_import" && e.Value != "example.service.CatalogService" {
			t.Fatalf("false declaration: %+v", e)
		}
		if e.Type == "java_import" && e.LineStart != 2 || e.Type == "entrypoint" && e.LineStart != 5 {
			t.Fatalf("wrong source location: %+v", e)
		}
	}
	if counts["java_package"] != 1 || counts["java_import"] != 1 || counts["entrypoint"] != 1 {
		t.Fatalf("counts=%v", counts)
	}
}

func TestSpringRepositoryRequiresExplicitKnownSupertype(t *testing.T) {
	for _, source := range []struct {
		text string
		want bool
	}{
		{"import org.springframework.data.jpa.repository.JpaRepository;\ninterface CategoryRepository extends JpaRepository<Category, Long> {}", true},
		{"interface CategoryRepository extends org.springframework.data.repository.CrudRepository<Category, Long> {}", true},
		{"interface CategoryRepository extends JpaRepository<Category, Long> {}", false},
		{"import other.JpaRepository;\ninterface CategoryRepository extends JpaRepository<Category, Long> {}", false},
		{"import org.springframework.data.fake.JpaRepository;\ninterface CategoryRepository extends org.springframework.data.fake.JpaRepository<Category, Long> {}", false},
	} {
		root := t.TempDir()
		write(t, filepath.Join(root, "CategoryRepository.java"), source.text)
		s, err := New().Analyze(context.Background(), root, Metadata{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range s.Evidence {
			found = found || e.Type == "repository"
		}
		if found != source.want {
			t.Fatalf("repository=%v want=%v for %s", found, source.want, source.text)
		}
	}
}
