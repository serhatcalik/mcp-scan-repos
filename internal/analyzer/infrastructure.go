package analyzer

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"gitlab-doc-platform/internal/evidence"
)

func analyzeYAML(path, rel, sha string) ([]evidence.Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoder := yaml.NewDecoder(f)
	var out []evidence.Item
	add := func(category, typ, value string) { out = append(out, item(category, typ, value, rel, 0, 0, sha)) }
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if doc == nil {
			continue
		}
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		namespace, _ := meta["namespace"].(string)
		qualified := func(kind, name, ns string) string {
			if ns != "" {
				name = ns + "/" + name
			}
			return kind + " " + name
		}
		owner := qualified(kind, name, namespace)
		reference := func(service map[string]any) {
			targetName, _ := service["name"].(string)
			if targetName == "" || owner == " " {
				return
			}
			targetNS := namespace
			if ns, ok := service["namespace"].(string); ok {
				targetNS = ns
			}
			target := qualified("Service", targetName, targetNS)
			add("infrastructure", "resource_reference", owner+" references "+target)
			out[len(out)-1].RelatedComponents = []string{owner, target}
		}
		if kind != "" && name != "" {
			add("infrastructure", "resource", owner)
		}
		// Secret values and arbitrary ConfigMap contents never enter evidence.
		if kind == "Secret" {
			add("configuration", "secret_reference", name)
			continue
		}
		if kind == "ConfigMap" {
			if values, ok := doc["data"].(map[string]any); ok {
				for key := range values {
					add("configuration", "config_key", name+"/"+key)
				}
			}
			continue
		}
		if filepath.Base(rel) == "Chart.yaml" {
			if name, ok := doc["name"].(string); ok {
				add("infrastructure", "helm_chart", name)
			}
		}
		if strings.Contains(strings.ToLower(filepath.Base(rel)), "kustomization") {
			add("infrastructure", "kustomize", "kustomize configuration")
		}
		if rel == ".gitlab-ci.yml" || strings.Contains(rel, "gitlab-ci") {
			add("infrastructure", "ci", "gitlab ci")
			for key, value := range doc {
				if job, ok := value.(map[string]any); ok {
					if _, hasScript := job["script"]; hasScript {
						add("infrastructure", "ci_job", key)
						if stage, ok := job["stage"].(string); ok {
							add("infrastructure", "ci_stage", key+": "+stage)
						}
					}
				}
			}
			if includes, ok := doc["include"].([]any); ok {
				for _, inc := range includes {
					if entry, ok := inc.(map[string]any); ok {
						if local, ok := entry["local"].(string); ok {
							add("infrastructure", "ci_local_include", local)
						} else {
							add("infrastructure", "ci_external_include", "external include not resolved from archive")
						}
					}
				}
			}
		}
		if strings.HasPrefix(rel, ".github/workflows/") && (strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml")) {
			add("infrastructure", "ci", "github actions")
			if jobs, ok := doc["jobs"].(map[string]any); ok {
				for name := range jobs {
					add("infrastructure", "ci_job", name)
				}
			}
		}
		if services, ok := doc["services"].(map[string]any); ok {
			for service := range services {
				add("infrastructure", "compose_service", service)
			}
		}
		var walk func(any, string)
		walk = func(value any, key string) {
			switch node := value.(type) {
			case map[string]any:
				if kind == "Ingress" && key == "backend" {
					if service, ok := node["service"].(map[string]any); ok {
						reference(service)
					}
				}
				if kind == "HTTPRoute" && key == "backendRefs" {
					if targetKind, ok := node["kind"].(string); !ok || targetKind == "Service" {
						reference(node)
					}
				}
				if kind == "Route" && key == "to" {
					if targetKind, ok := node["kind"].(string); !ok || targetKind == "Service" {
						reference(node)
					}
				}
				if key == "secretKeyRef" || key == "configMapKeyRef" {
					name, _ := node["name"].(string)
					field, _ := node["key"].(string)
					add("configuration", key, name+"/"+field)
					return
				}
				if key == "secretRef" || key == "configMapRef" {
					name, _ := node["name"].(string)
					add("configuration", key, name)
					return
				}
				if key == "secret" {
					if name, ok := node["secretName"].(string); ok {
						add("configuration", "secret_reference", name)
					}
					return
				}
				if key == "env" {
					if name, ok := node["name"].(string); ok {
						add("configuration", "environment_variable", name)
						if from, ok := node["valueFrom"]; ok {
							walk(from, "valueFrom")
						}
						return
					}
				}
				if key == "environment" || key == "variables" {
					for name := range node {
						add("configuration", "environment_variable", name)
					}
					return
				}
				if key == "labels" || key == "annotations" || key == "data" || key == "stringData" || key == "args" || key == "command" {
					return
				}
				if key == "livenessProbe" || key == "readinessProbe" || key == "startupProbe" {
					add("infrastructure", "probe", key)
				}
				for child, v := range node {
					walk(v, child)
				}
			case []any:
				for _, v := range node {
					if key == "environment" {
						if text, ok := v.(string); ok {
							add("configuration", "environment_variable", strings.SplitN(text, "=", 2)[0])
						}
						continue
					}
					walk(v, key)
				}
			default:
				switch key {
				case "image", "containerPort", "port", "targetPort", "replicas", "cpu", "memory", "mountPath", "claimName", "storageClassName":
					add("infrastructure", key, fmt.Sprint(value))
				case "secretName":
					add("configuration", "secret_reference", fmt.Sprint(value))
				case "resources":
					if text, ok := value.(string); ok {
						add("infrastructure", "manifest_reference", text)
					}
				}
			}
		}
		walk(doc, "")
		if strings.HasPrefix(filepath.Base(rel), "application.") {
			configurationKeys(doc, "", add)
		}
	}
	return out, nil
}

func configurationKeys(doc map[string]any, prefix string, add func(string, string, string)) {
	for key, value := range doc {
		name := key
		if prefix != "" {
			name = prefix + "." + key
		}
		if nested, ok := value.(map[string]any); ok {
			configurationKeys(nested, name, add)
		} else {
			add("configuration", "config_key", name)
		}
	}
}

func analyzeDockerfile(path, rel, sha string) ([]evidence.Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []evidence.Item{item("infrastructure", "container", "dockerfile", rel, 1, 1, sha)}
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) < 2 {
			continue
		}
		typ, value := "", ""
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			typ = "container_base_image"
			idx := 1
			if strings.HasPrefix(fields[idx], "--") && len(fields) > 2 {
				idx++
			}
			value = fields[idx]
		case "EXPOSE":
			typ, value = "container_port", strings.Join(fields[1:], " ")
		case "USER":
			typ, value = "container_user", fields[1]
		case "HEALTHCHECK":
			typ, value = "probe", "Docker HEALTHCHECK declared"
		case "ENV", "ARG":
			typ, value = "environment_variable", strings.SplitN(fields[1], "=", 2)[0]
		}
		if typ != "" {
			out = append(out, item("infrastructure", typ, value, rel, line, line, sha))
		}
	}
	return out, scanner.Err()
}

func analyzeProperties(path, rel, sha string) ([]evidence.Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []evidence.Item
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, "!") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		idx := strings.IndexAny(text, "=:")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(text[:idx])
		out = append(out, item("configuration", "config_key", key, rel, line, line, sha))
	}
	return out, scanner.Err()
}

var terraformBlock = regexp.MustCompile(`\b(resource|data|module)\s+"([A-Za-z0-9_-]+)"(?:\s+"([A-Za-z0-9_-]+)")?`)

func analyzeTerraform(path, rel, sha string) ([]evidence.Item, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := uncomment(string(data))
	var out []evidence.Item
	for _, m := range terraformBlock.FindAllStringSubmatchIndex(text, -1) {
		value := text[m[2]:m[3]] + " " + text[m[4]:m[5]]
		if m[6] >= 0 {
			value += " " + text[m[6]:m[7]]
		}
		out = append(out, item("infrastructure", "terraform_declaration", value, rel, strings.Count(text[:m[0]], "\n")+1, 0, sha))
	}
	return out, nil
}

func deduplicate(items []evidence.Item) []evidence.Item {
	seen := map[string]bool{}
	out := make([]evidence.Item, 0, len(items))
	for _, e := range items {
		if !seen[e.ID] {
			out = append(out, e)
			seen[e.ID] = true
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SourceFile != out[j].SourceFile {
			return out[i].SourceFile < out[j].SourceFile
		}
		if out[i].LineStart != out[j].LineStart {
			return out[i].LineStart < out[j].LineStart
		}
		return out[i].ID < out[j].ID
	})
	return out
}
