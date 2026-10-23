package analyzer

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gitlab-doc-platform/internal/evidence"
)

func fileLanguage(path string) string {
	switch filepath.Ext(path) {
	case ".go":
		return "go"
	case ".java":
		return "java"
	case ".js", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".yaml", ".yml":
		return "yaml"
	case ".tf":
		return "terraform"
	}
	return ""
}

func analyzePOM(path, rel, sha string) ([]evidence.Item, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pom struct {
		Version      string `xml:"version"`
		Artifact     string `xml:"artifactId"`
		Dependencies []struct {
			Group    string `xml:"groupId"`
			Artifact string `xml:"artifactId"`
		} `xml:"dependencies>dependency"`
		Parent struct {
			Group    string `xml:"groupId"`
			Artifact string `xml:"artifactId"`
		} `xml:"parent"`
	}
	if err := xml.Unmarshal(data, &pom); err != nil {
		return nil, err
	}
	var out []evidence.Item
	if rel == "pom.xml" {
		out = append(out, applicationVersionItems(strings.TrimSpace(pom.Version), rel, 0, sha)...)
	}
	if pom.Artifact != "" {
		out = append(out, item("application", "java_module", pom.Artifact, rel, 0, 0, sha))
	}
	deps := pom.Dependencies
	deps = append(deps, pom.Parent)
	for _, dep := range deps {
		if dep.Artifact == "" {
			continue
		}
		out = append(out, item("dependency", "maven_dependency", dep.Group+":"+dep.Artifact, rel, 0, 0, sha))
		if strings.Contains(dep.Artifact, "spring-boot") {
			out = append(out, item("application", "framework", "spring boot", rel, 0, 0, sha))
		}
		if strings.Contains(dep.Artifact, "data-jpa") {
			out = append(out, item("data", "orm", "jpa", rel, 0, 0, sha))
		}
		if strings.Contains(dep.Artifact, "security") {
			out = append(out, item("security", "dependency", dep.Artifact, rel, 0, 0, sha))
		}
		if strings.Contains(dep.Artifact, "actuator") {
			out = append(out, item("observability", "dependency", dep.Artifact, rel, 0, 0, sha))
		}
	}
	return out, nil
}

var javaClass = regexp.MustCompile(`\b(?:class|interface|record)\s+([A-Za-z_][A-Za-z0-9_]*)`)
var javaAnnotation = regexp.MustCompile(`@(RestController|Controller|Service|Repository|Entity|EnableWebSecurity|SpringBootApplication)\b`)
var javaPackage = regexp.MustCompile(`(?m)^\s*package\s+([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*;`)
var javaImport = regexp.MustCompile(`(?m)^\s*import\s+(?:static\s+)?([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*|\.\*)+)\s*;`)
var javaRepository = regexp.MustCompile(`\binterface\s+([A-Za-z_][A-Za-z0-9_]*)\s+extends\s+((?:[A-Za-z_][A-Za-z0-9_]*\.)*(?:JpaRepository|CrudRepository|PagingAndSortingRepository))\s*[<{,]`)
var javaMapping = regexp.MustCompile(`@(GetMapping|PostMapping|PutMapping|PatchMapping|DeleteMapping|RequestMapping)\s*(?:\(\s*(?:(?:value|path)\s*=\s*)?"([^"\r\n]*)")?`)
var expressRoute = regexp.MustCompile(`\b([A-Za-z_$][A-Za-z0-9_$]*)\.(get|post|put|patch|delete|head|options|use)\s*\(\s*['"]([^'"\r\n]*)['"]`)
var nextMethod = regexp.MustCompile(`\bexport\s+(?:(?:async\s+)?function\s+|const\s+)(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\b`)
var envReference = regexp.MustCompile(`\bprocess\.env\.([A-Za-z_][A-Za-z0-9_]*)`)

// Conservative lexical extraction for Java/JS: remove comments, retain line numbers,
// and mark unresolved routing relationships as inferred instead of claiming AST resolution.
func uncomment(data string) string {
	b := []byte(data)
	quote := byte(0)
	for i := 0; i < len(b); i++ {
		if quote != 0 {
			if b[i] == '\\' {
				i++
				continue
			}
			if b[i] == quote {
				quote = 0
			}
			continue
		}
		if b[i] == '"' || b[i] == '\'' || b[i] == '`' {
			quote = b[i]
			continue
		}
		if i+1 < len(b) && b[i] == '/' && b[i+1] == '/' {
			for i < len(b) && b[i] != '\n' {
				b[i] = ' '
				i++
			}
			continue
		}
		if i+1 < len(b) && b[i] == '/' && b[i+1] == '*' {
			b[i], b[i+1] = ' ', ' '
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				if b[i] != '\n' {
					b[i] = ' '
				}
				i++
			}
			if i+1 < len(b) {
				b[i], b[i+1] = ' ', ' '
				i++
			}
		}
	}
	return string(b)
}

func maskStrings(text string) string {
	b := []byte(text)
	quote := byte(0)
	for i := 0; i < len(b); i++ {
		if quote == 0 {
			if b[i] == '"' || b[i] == '\'' || b[i] == '`' {
				quote = b[i]
				b[i] = ' '
			}
			continue
		}
		if b[i] == '\\' {
			b[i] = ' '
			if i+1 < len(b) {
				i++
				if b[i] != '\n' {
					b[i] = ' '
				}
			}
			continue
		}
		if b[i] == quote {
			quote = 0
		}
		if b[i] != '\n' {
			b[i] = ' '
		}
	}
	return string(b)
}

func analyzeJavaSource(path, rel, sha string) ([]evidence.Item, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := uncomment(string(data))
	code := maskStrings(text)
	var out []evidence.Item
	class := filepath.Base(rel)
	packageName := ""
	imports := map[string]bool{}
	if match := javaPackage.FindStringSubmatchIndex(code); match != nil {
		packageName = code[match[2]:match[3]]
		out = append(out, item("application", "java_package", packageName, rel, strings.Count(code[:match[2]], "\n")+1, 0, sha))
	}
	for _, match := range javaImport.FindAllStringSubmatchIndex(code, -1) {
		e := item("dependency", "java_import", code[match[2]:match[3]], rel, strings.Count(code[:match[2]], "\n")+1, 0, sha)
		imports[e.Value] = true
		e.RelatedComponents = []string{packageName}
		out = append(out, e)
	}
	for _, match := range javaRepository.FindAllStringSubmatchIndex(code, -1) {
		name, parent := code[match[2]:match[3]], code[match[4]:match[5]]
		qualified := parent
		switch parent {
		case "JpaRepository":
			qualified = "org.springframework.data.jpa.repository.JpaRepository"
		case "CrudRepository":
			qualified = "org.springframework.data.repository.CrudRepository"
		case "PagingAndSortingRepository":
			qualified = "org.springframework.data.repository.PagingAndSortingRepository"
		}
		if (qualified != "org.springframework.data.jpa.repository.JpaRepository" && qualified != "org.springframework.data.repository.CrudRepository" && qualified != "org.springframework.data.repository.PagingAndSortingRepository") || parent != qualified && !imports[qualified] {
			continue
		}
		e := item("data", "repository", name+" extends "+parent, rel, strings.Count(code[:match[2]], "\n")+1, 0, sha)
		e.RelatedComponents = []string{name}
		out = append(out, e)
	}
	if match := javaClass.FindStringSubmatch(code); len(match) > 1 {
		class = match[1]
		out = append(out, item("application", "java_type", class, rel, strings.Count(text[:javaClass.FindStringIndex(code)[0]], "\n")+1, 0, sha))
	}
	for _, m := range javaAnnotation.FindAllStringSubmatchIndex(text, -1) {
		if code[m[0]] != '@' {
			continue
		}
		name := text[m[2]:m[3]]
		category, typ := "application", strings.ToLower(name)
		switch name {
		case "RestController", "Controller":
			category, typ = "api", "controller"
		case "Repository", "Entity":
			category, typ = "data", strings.ToLower(name)
		case "EnableWebSecurity":
			category, typ = "security", "configuration"
		case "SpringBootApplication":
			typ = "entrypoint"
		}
		e := item(category, typ, class+" @"+name, rel, strings.Count(text[:m[0]], "\n")+1, 0, sha)
		e.RelatedComponents = []string{class}
		out = append(out, e)
	}
	for _, m := range javaMapping.FindAllStringSubmatchIndex(text, -1) {
		if code[m[0]] != '@' {
			continue
		}
		name := text[m[2]:m[3]]
		value := name
		if m[4] >= 0 {
			value += " " + text[m[4]:m[5]]
		}
		e := item("api", "route_annotation", value, rel, strings.Count(text[:m[0]], "\n")+1, 0, sha)
		e.RelatedComponents = []string{class}
		out = append(out, e)
	}
	return out, nil
}

func analyzeNodeSource(path, rel, sha string) ([]evidence.Item, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := uncomment(string(data))
	code := maskStrings(text)
	var out []evidence.Item
	for _, m := range expressRoute.FindAllStringSubmatchIndex(text, -1) {
		if code[m[0]] == ' ' {
			continue
		}
		receiver, method, route := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]]
		e := item("api", "route_candidate", strings.ToUpper(method)+" "+route, rel, strings.Count(text[:m[0]], "\n")+1, 0, sha)
		e.Confidence, e.Verification = evidence.Inferred, "lexical; receiver and router mounts unresolved"
		e.RelatedComponents = []string{receiver}
		out = append(out, e)
	}
	if strings.HasSuffix(rel, "/route.ts") || strings.HasSuffix(rel, "/route.js") {
		for _, m := range nextMethod.FindAllStringSubmatchIndex(code, -1) {
			out = append(out, item("api", "route_export", text[m[2]:m[3]]+" "+rel, rel, strings.Count(text[:m[0]], "\n")+1, 0, sha))
		}
	}
	for _, m := range envReference.FindAllStringSubmatchIndex(code, -1) {
		out = append(out, item("configuration", "environment_variable", text[m[2]:m[3]], rel, strings.Count(text[:m[0]], "\n")+1, 0, sha))
	}
	return out, nil
}
