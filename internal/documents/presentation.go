package documents

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"gitlab-doc-platform/internal/evidence"
)

const paperRows = 12

type paperRow struct {
	name  string
	items []evidence.Item
}

type sourceNotes struct {
	ids  map[string]int
	used map[string]map[int]bool
}

func newSourceNotes(s evidence.Snapshot) *sourceNotes {
	files := map[string]bool{}
	for _, e := range s.Evidence {
		files[e.SourceFile] = true
	}
	keys := sortedKeys(files)
	n := &sourceNotes{ids: map[string]int{}, used: map[string]map[int]bool{}}
	for i, file := range keys {
		n.ids[file] = i + 1
	}
	return n
}

func (n *sourceNotes) cite(items []evidence.Item) string {
	files := map[string]bool{}
	for _, e := range items {
		if e.SourceFile == "" {
			continue
		}
		files[e.SourceFile] = true
		if n.used[e.SourceFile] == nil {
			n.used[e.SourceFile] = map[int]bool{}
		}
		n.used[e.SourceFile][e.LineStart] = true
	}
	var refs []string
	for _, file := range sortedKeys(files) {
		refs = append(refs, fmt.Sprintf("[^S%d]", n.ids[file]))
	}
	return strings.Join(refs, " ")
}

func (n *sourceNotes) render() string {
	var b strings.Builder
	for _, file := range sortedKeys(n.used) {
		var lines []int
		for line := range n.used[file] {
			if line > 0 {
				lines = append(lines, line)
			}
		}
		sort.Ints(lines)
		locations := make([]string, 0, len(lines))
		for _, line := range lines {
			locations = append(locations, fmt.Sprint(line))
		}
		fmt.Fprintf(&b, "[^S%d]: %s", n.ids[file], plain(file))
		if len(locations) > 0 {
			fmt.Fprintf(&b, ":%s", locations[0])
			if len(locations) > 1 {
				fmt.Fprintf(&b, " (also lines %s)", strings.Join(locations[1:], ", "))
			}
		}
		b.WriteString(".\n")
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func workflowFile(file string) bool {
	return strings.Contains(file, "gitlab-ci") || strings.Contains(file, ".github/workflows/") || strings.Contains(file, ".gitea/workflows/")
}

func testEvidence(e evidence.Item) bool {
	file := strings.ToLower(e.SourceFile)
	return strings.Contains(file, "/testutil/") || strings.Contains(file, "/testdata/") || strings.HasSuffix(file, "_test.go") || strings.HasPrefix(e.Value, "TEST_") || strings.HasPrefix(e.Value, "RUN_INTEGRATION_TESTS") || strings.HasPrefix(e.Value, "RUN_API_TESTS")
}

func configurationGroup(key string) string {
	prefix := strings.SplitN(strings.ToUpper(key), "_", 2)[0]
	switch prefix {
	case "DB", "DATABASE", "POSTGRES":
		return "Database"
	case "SMTP", "EMAIL":
		return "Email"
	case "MINIO", "S3", "MAX":
		return "Object storage and uploads"
	case "REDIS", "CACHE":
		return "Cache"
	case "OTEL", "SENTRY", "DATADOG", "PROM", "TELEMETRY":
		return "Telemetry"
	case "JWT", "AUTH", "PASSWORD", "RESET", "PHONE":
		return "Authentication and verification"
	case "OPENWA", "INFOBIP", "MESSAGE":
		return "Messaging"
	default:
		return "Application and other settings"
	}
}

func deploymentSection(file string) string {
	base := strings.ToLower(path.Base(file))
	switch {
	case workflowFile(file):
		return "CI and Workflow Definitions"
	case strings.HasPrefix(base, "dockerfile"):
		return "Container Build Definitions"
	case strings.Contains(base, "compose"):
		return "Compose Definitions"
	default:
		return "Manifest and Deployment Definitions"
	}
}

func observedTelemetry(e evidence.Item) bool {
	if e.Category == "observability" {
		return true
	}
	if e.Type != "go_import" && e.Type != "middleware" {
		return false
	}
	v := strings.ToLower(e.Value)
	return strings.Contains(v, "opentelemetry") || strings.Contains(v, "sentry") || strings.Contains(v, "dd-trace") || strings.Contains(v, "prometheus") || strings.Contains(v, "metrics.middleware") || strings.Contains(v, "telemetry") || strings.Contains(v, "gintrace")
}

func groupedFacts(s evidence.Snapshot, kind string, notes *sourceNotes) string {
	sections := map[string]map[string]*paperRow{}
	add := func(section, name string, e evidence.Item) {
		if sections[section] == nil {
			sections[section] = map[string]*paperRow{}
		}
		if sections[section][name] == nil {
			sections[section][name] = &paperRow{name: name}
		}
		sections[section][name].items = append(sections[section][name].items, e)
	}
	for _, e := range s.Evidence {
		if e.Confidence != evidence.Verified || testEvidence(e) {
			continue
		}
		if observedTelemetry(e) {
			add("Telemetry References", component(e), e)
			continue
		}
		if e.Category == "configuration" {
			section, group := "Application Configuration", configurationGroup(e.Value)
			if workflowFile(e.SourceFile) {
				if kind == "technical" {
					continue
				}
				section, group = "CI Configuration", e.SourceFile
			} else if e.Type != "config_key" && e.Type != "environment_variable" {
				section, group = "Deployment Configuration References", e.SourceFile
			}
			add(section, group, e)
			continue
		}
		if kind == "infrastructure" {
			if e.Category == "infrastructure" {
				add(deploymentSection(e.SourceFile), e.SourceFile, e)
			}
			continue
		}
		switch e.Category {
		case "application":
			if e.Type == "framework" || e.Type == "go_module" || e.Type == "java_module" {
				add("Technology Overview", e.Type, e)
			} else if e.Type != "struct" || e.Value != "Service" {
				add("Application Components", component(e), e)
			}
		case "api":
			if strings.HasPrefix(e.Type, "route") {
				_, title := apiGroup(e)
				add("API Groups", title, e)
			}
		case "data":
			if e.Type == "model_field" {
				continue
			}
			section := "Data Models"
			if e.Type == "database_client" || e.Type == "orm" {
				section = "Technology Overview"
			}
			add(section, e.Type, e)
		case "dependency":
			if e.Type == "go_import" && strings.Contains(strings.SplitN(e.Value, "/", 2)[0], ".") {
				// An import is a source reference, not proof of an active integration.
				add("External Library References", component(e), e)
			} else if e.Type != "go_import" && e.Type != "go_checksum_module" && e.Type != "go_dependency" && e.Type != "java_import" {
				add("Declared Dependencies", e.Type, e)
			}
		case "security":
			add("Security References", component(e), e)
		}
	}
	order := []string{"Technology Overview", "Application Components", "API Groups", "Data Models", "External Library References", "Declared Dependencies", "Security References", "Container Build Definitions", "Compose Definitions", "Manifest and Deployment Definitions", "CI and Workflow Definitions", "Application Configuration", "Deployment Configuration References", "CI Configuration", "Telemetry References"}
	var b strings.Builder
	b.WriteString("This is a condensed static-source overview, not a description of live deployed state. Complete facts are retained in evidence.json and the reference files.\n\n")
	for _, section := range order {
		rows := sections[section]
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", section)
		if section == "Compose Definitions" {
			b.WriteString("Compose declarations do not establish the production environment. Images and services below are file-level facts; their association is not inferred.\n\n")
		}
		if section == "Telemetry References" {
			b.WriteString("Source references do not establish telemetry enablement or exporter behavior.\n\n")
		}
		if section == "Application Configuration" {
			b.WriteString("Key names only; requiredness, defaults, and secret values are not inferred.\n\n")
		}
		b.WriteString("| Group | Recorded declarations | Sources |\n| --- | --- | --- |\n")
		keys := sortedKeys(rows)
		for i, key := range keys {
			if i >= paperRows {
				break
			}
			row := rows[key]
			byType := map[string]map[string]evidence.Item{}
			sort.Slice(row.items, func(i, j int) bool { return row.items[i].ID < row.items[j].ID })
			for _, e := range row.items {
				typ := e.Type
				if e.Category == "configuration" && (typ == "config_key" || typ == "environment_variable") {
					typ = "configuration keys"
				}
				if byType[typ] == nil {
					byType[typ] = map[string]evidence.Item{}
				}
				if _, ok := byType[typ][e.Value]; !ok {
					byType[typ][e.Value] = e
				}
			}
			var descriptions []string
			var cited []evidence.Item
			for j, typ := range sortedKeys(byType) {
				if j >= 6 {
					descriptions = append(descriptions, "Additional declaration types in evidence.json")
					break
				}
				values := sortedKeys(byType[typ])
				var shown []string
				for k, value := range values {
					if k >= 6 {
						break
					}
					shown = append(shown, concise(value))
					cited = append(cited, byType[typ][value])
				}
				text := strings.Join(shown, ", ")
				if len(values) > 6 {
					text += fmt.Sprintf(" (+%d more in evidence.json)", len(values)-6)
				}
				descriptions = append(descriptions, plain(strings.ReplaceAll(typ, "_", " "))+": "+text)
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", concise(row.name), strings.Join(descriptions, "; "), notes.cite(cited))
		}
		if len(keys) > paperRows {
			fmt.Fprintf(&b, "\n%d additional groups retained in evidence.json.\n", len(keys)-paperRows)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func concise(value string) string {
	runes := []rune(value)
	if len(runes) > 160 {
		value = string(runes[:160]) + "... (full value in evidence.json)"
	}
	return plain(value)
}

func referenceDocuments(s evidence.Snapshot) map[string]string {
	out := map[string]string{}
	for _, kind := range []string{"api", "configuration"} {
		var b strings.Builder
		fmt.Fprintf(&b, "# %s Reference\n\nStatic declarations only; confidence and sources are retained per row.\n\n| Group | Declaration | Confidence | Source | Evidence ID |\n| --- | --- | --- | --- | --- |\n", strings.ToUpper(kind[:1])+kind[1:])
		items := append([]evidence.Item(nil), s.Evidence...)
		sort.Slice(items, func(i, j int) bool {
			if kind == "api" {
				_, left := apiGroup(items[i])
				_, right := apiGroup(items[j])
				if left != right {
					return left < right
				}
			}
			if items[i].Value != items[j].Value {
				return items[i].Value < items[j].Value
			}
			return items[i].ID < items[j].ID
		})
		for _, e := range items {
			if e.Category != kind {
				continue
			}
			group := component(e)
			if kind == "api" {
				_, group = apiGroup(e)
			}
			fmt.Fprintf(&b, "| %s | %s: %s | %s | %s | %s |\n", plain(group), plain(e.Type), plain(e.Value), plain(string(e.Confidence)), plain(source(e)), plain(e.ID))
		}
		out[kind+"-reference.md"] = b.String()
	}
	return out
}
