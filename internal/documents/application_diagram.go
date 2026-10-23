package documents

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"gitlab-doc-platform/internal/evidence"
)

const maxApplicationNodes = 24

type applicationSummary struct {
	names  map[string]bool
	routes map[string]bool
	role   string
}

func architectureTestSource(file string) bool {
	for _, segment := range strings.Split(file, "/") {
		switch segment {
		case "test", "tests", "testdata", "__tests__":
			return true
		}
	}
	name := path.Base(file)
	return strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "Test.java") || strings.HasSuffix(name, "Tests.java") || strings.Contains(name, ".test.") || strings.Contains(name, ".spec.")
}

func applicationDiagram(s evidence.Snapshot) string {
	// This is a component overview, not a declaration inventory. Keep all
	// omitted details in the snapshot and API reference rather than adding nodes.
	filtered := s
	filtered.Evidence = nil
	for _, e := range s.Evidence {
		if e.Confidence == evidence.Verified && !architectureTestSource(e.SourceFile) {
			filtered.Evidence = append(filtered.Evidence, e)
		}
	}
	d := newDiagram()
	module, packages := goPackages(filtered, d)
	summaries := map[string]*applicationSummary{}
	javaPackages := map[string]string{}
	for _, e := range filtered.Evidence {
		if e.Type == "java_package" {
			javaPackages[e.SourceFile] = e.Value
		}
	}
	fileNodes := map[string]map[string]bool{}
	add := func(key, group, title, role, name string, e evidence.Item, route bool) {
		d.add(key, "", e)
		d.groups[group], d.nodes[key].group = title, group
		if summaries[key] == nil {
			summaries[key] = &applicationSummary{names: map[string]bool{}, routes: map[string]bool{}, role: role}
		}
		if route {
			summaries[key].routes[fmt.Sprintf("%s:%d:%s", e.SourceFile, e.LineStart, e.Value)] = true
		} else if name != "" {
			summaries[key].names[name] = true
		}
		if fileNodes[e.SourceFile] == nil {
			fileNodes[e.SourceFile] = map[string]bool{}
		}
		fileNodes[e.SourceFile][key] = true
	}
	for _, e := range filtered.Evidence {
		if strings.HasSuffix(e.SourceFile, ".java") {
			role, group := "", ""
			switch e.Type {
			case "controller", "route_annotation":
				role, group = "Controllers", "API"
			case "service":
				role, group = "Services", "Business Components"
			case "repository":
				role, group = "Repositories", "Persistence"
			case "entity":
				role, group = "Entities", "Domain Models"
			case "entrypoint":
				role, group = "Entrypoints", "Application"
			}
			if role == "" {
				continue
			}
			pkg := javaPackages[e.SourceFile]
			if pkg == "" {
				pkg = path.Dir(e.SourceFile)
			}
			key := "java:" + pkg + ":" + role
			name := strings.Split(e.Value, " @")[0]
			if len(e.RelatedComponents) == 1 {
				name = e.RelatedComponents[0]
			}
			if e.Type == "route_annotation" {
				name = apiComponent(e)
				add(key, group, group, role, name, e, false)
				add(key, group, group, role, "", e, true)
			} else {
				add(key, group, group, role, name, e, false)
			}
			continue
		}
		if e.Category == "api" && (e.Type == "route" || e.Type == "route_export") {
			group, title := apiGroup(e)
			key := "api:" + group
			add(key, group, title, "Endpoints", "", e, true)
		}
	}
	for key, summary := range summaries {
		names := make([]string, 0, len(summary.names))
		for name := range summary.names {
			names = append(names, name)
		}
		sort.Strings(names)
		label := summary.role
		if len(names) > 0 {
			shown := names
			if len(shown) > 3 {
				shown = shown[:3]
			}
			labels := make([]string, len(shown))
			for i, name := range shown {
				labels[i] = architectureLabel(name, 64)
			}
			label += ": " + strings.Join(labels, ", ")
			if len(names) > 3 {
				label += fmt.Sprintf(" and %d more", len(names)-3)
			}
		}
		if len(summary.routes) > 0 {
			unit := "endpoint declarations"
			if strings.HasPrefix(key, "java:") {
				unit = "mapping declarations"
			}
			label += fmt.Sprintf(" - %d %s", len(summary.routes), unit)
		}
		if strings.HasPrefix(key, "java:") {
			pkg := strings.TrimSuffix(strings.TrimPrefix(key, "java:"), ":"+summary.role)
			label = architectureLabel(pkg, 80) + " - " + label
		}
		d.nodes[key].label = label
	}
	addImports(filtered, d, module, packages, false)
	addJavaArchitectureImports(filtered, d, javaPackages, fileNodes)
	omitted := limitApplicationDiagram(d, maxApplicationNodes)
	result := d.render("LR")
	if omitted > 0 {
		result += fmt.Sprintf("  %%%% Overview omitted %d components; complete declarations are retained in evidence.json and references/api-reference.md.\n", omitted)
	}
	return result
}

func architectureLabel(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max]) + "..."
	}
	return value
}

func addJavaArchitectureImports(s evidence.Snapshot, d *diagram, packages map[string]string, fileNodes map[string]map[string]bool) {
	types := map[string]map[string]bool{}
	for _, e := range s.Evidence {
		if e.Type != "java_type" || packages[e.SourceFile] == "" {
			continue
		}
		qualified := packages[e.SourceFile] + "." + e.Value
		if types[qualified] == nil {
			types[qualified] = map[string]bool{}
		}
		types[qualified][e.SourceFile] = true
	}
	for _, e := range s.Evidence {
		if e.Type != "java_import" || len(types[e.Value]) != 1 {
			continue
		}
		for targetFile := range types[e.Value] {
			for from := range fileNodes[e.SourceFile] {
				for to := range fileNodes[targetFile] {
					if from != to {
						d.edges[[2]string{from, to}] = "imports"
						d.add(from, d.nodes[from].label, e)
					}
				}
			}
		}
	}
}

func limitApplicationDiagram(d *diagram, max int) int {
	if len(d.nodes) <= max {
		return 0
	}
	degree := map[string]int{}
	for edge := range d.edges {
		degree[edge[0]]++
		degree[edge[1]]++
	}
	keys := make([]string, 0, len(d.nodes))
	for key := range d.nodes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if degree[keys[i]] != degree[keys[j]] {
			return degree[keys[i]] > degree[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys[max:] {
		delete(d.nodes, key)
	}
	for edge := range d.edges {
		if d.nodes[edge[0]] == nil || d.nodes[edge[1]] == nil {
			delete(d.edges, edge)
		}
	}
	groups := map[string]string{}
	for _, node := range d.nodes {
		if node.group != "" {
			groups[node.group] = d.groups[node.group]
		}
	}
	d.groups = groups
	return len(keys) - max
}
