package documents

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"gitlab-doc-platform/internal/evidence"
)

type diagramNode struct {
	label   string
	group   string
	sources map[string]bool
}

type diagram struct {
	nodes  map[string]*diagramNode
	edges  map[[2]string]string
	groups map[string]string
}

func newDiagram() *diagram {
	return &diagram{nodes: map[string]*diagramNode{}, edges: map[[2]string]string{}, groups: map[string]string{}}
}

func (d *diagram) add(key, label string, e evidence.Item) {
	if d.nodes[key] == nil {
		d.nodes[key] = &diagramNode{label: label, sources: map[string]bool{}}
	}
	source := e.SourceFile
	if e.LineStart > 0 {
		source += fmt.Sprintf(":%d", e.LineStart)
	}
	d.nodes[key].sources[source] = true
}

func (d *diagram) render(direction string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "flowchart %s\n", direction)
	b.WriteString("  %% Nodes are repository evidence; arrows are verified imports or explicit resource references, not runtime traffic.\n")
	keys := make([]string, 0, len(d.nodes))
	for key := range d.nodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ids := map[string]int{}
	for i, key := range keys {
		ids[key] = i
	}
	writeNode := func(key, indent string) {
		i := ids[key]
		node := d.nodes[key]
		sources := make([]string, 0, len(node.sources))
		for source := range node.sources {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		for _, source := range sources {
			fmt.Fprintf(&b, "%s%%%% n%d source: %s\n", indent, i, strings.Join(strings.Fields(source), " "))
		}
		fmt.Fprintf(&b, "%sn%d[\"%s\"]\n", indent, i, mermaidLabel(node.label))
	}
	for _, key := range keys {
		if d.nodes[key].group == "" {
			writeNode(key, "  ")
		}
	}
	groups := make([]string, 0, len(d.groups))
	for key := range d.groups {
		groups = append(groups, key)
	}
	sort.Strings(groups)
	for i, group := range groups {
		fmt.Fprintf(&b, "  subgraph g%d[\"%s\"]\n    direction TB\n", i, mermaidLabel(d.groups[group]))
		for _, key := range keys {
			if d.nodes[key].group == group {
				writeNode(key, "    ")
			}
		}
		b.WriteString("  end\n")
	}
	if len(keys) == 0 {
		b.WriteString("  unavailable[\"No supported repository evidence available\"]\n")
	}
	edges := make([][2]string, 0, len(d.edges))
	for edge := range d.edges {
		edges = append(edges, edge)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i][0] != edges[j][0] {
			return edges[i][0] < edges[j][0]
		}
		return edges[i][1] < edges[j][1]
	})
	for _, edge := range edges {
		fmt.Fprintf(&b, "  n%d -->|%s| n%d\n", ids[edge[0]], d.edges[edge], ids[edge[1]])
	}
	return b.String()
}

// Encode repository-controlled text so it cannot introduce Mermaid syntax.
func mermaidLabel(text string) string {
	var b strings.Builder
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(" ./_:-", r) {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "#%d;", r)
		}
	}
	return b.String()
}

func goPackages(s evidence.Snapshot, d *diagram) (string, map[string]string) {
	module := ""
	for _, e := range s.Evidence {
		if e.Confidence == evidence.Verified && e.Type == "go_module" {
			module = e.Value
		}
	}
	packages := map[string]string{}
	for _, e := range s.Evidence {
		if e.Confidence != evidence.Verified || e.Type != "go_package" {
			continue
		}
		key := "package:" + e.Value
		packages[e.Value] = key
		d.add(key, "Go package: "+e.Value, e)
	}
	return module, packages
}

func addImports(s evidence.Snapshot, d *diagram, module string, packages map[string]string, external bool) {
	for _, e := range s.Evidence {
		if e.Confidence != evidence.Verified || e.Type != "go_import" {
			continue
		}
		path := e.Value
		local := ""
		if module != "" {
			if path == module {
				local = "."
			} else if strings.HasPrefix(path, module+"/") {
				local = strings.TrimPrefix(path, module+"/")
			}
		}
		target := packages[local]
		if target == "" {
			if !external {
				continue
			}
			target = "dependency:" + path
			d.add(target, path, e)
		}
		for _, source := range e.RelatedComponents {
			if key := packages[source]; key != "" && key != target {
				d.edges[[2]string{key, target}] = "imports"
			}
		}
	}
}

func appDiagram(s evidence.Snapshot) string {
	return applicationDiagram(s)
}

func apiComponent(e evidence.Item) string {
	if len(e.RelatedComponents) > 0 {
		return e.RelatedComponents[0]
	}
	return path.Dir(e.SourceFile)
}

// Group labels describe source organization, not inferred runtime service boundaries.
func apiGroup(e evidence.Item) (string, string) {
	fields := strings.Fields(e.Value)
	// Spring class/method annotations are not composed into effective route paths.
	if e.Type == "route_annotation" {
		fields = nil
	}
	for _, field := range fields {
		if !strings.HasPrefix(field, "/") {
			continue
		}
		segments := strings.Split(strings.Trim(field, "/"), "/")
		for i, segment := range segments {
			if segment == "" || segment == "api" || isRouteVersion(segment) || strings.HasPrefix(segment, ":") || strings.HasPrefix(segment, "{") || strings.HasPrefix(segment, "*") {
				continue
			}
			prefix := "/" + strings.Join(segments[:i+1], "/")
			return "route:" + prefix, apiTitle(segment) + " API - " + prefix
		}
	}
	component := apiComponent(e)
	name := path.Base(component)
	name = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "Controller"), "controller"), "api")
	if name == "" || name == "." {
		name = "Application"
	}
	return "component:" + component, apiTitle(name) + " API"
}

func isRouteVersion(segment string) bool {
	if len(segment) < 2 || segment[0] != 'v' {
		return false
	}
	for _, r := range segment[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func apiTitle(name string) string {
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(name))
	for i, word := range words {
		if word[0] >= 'a' && word[0] <= 'z' {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ")
}

func infraDiagram(s evidence.Snapshot) string {
	d := newDiagram()
	resources := map[string][]string{}
	for _, e := range s.Evidence {
		if e.Confidence == evidence.Verified && e.Category == "infrastructure" && e.Type != "resource_reference" {
			key := e.Type + ":" + e.Value + ":" + e.SourceFile
			d.add(key, e.Value+" - "+e.SourceFile, e)
			if e.Type == "resource" {
				resources[e.Value] = append(resources[e.Value], key)
			}
		}
	}
	for _, e := range s.Evidence {
		if e.Confidence != evidence.Verified || e.Type != "resource_reference" || len(e.RelatedComponents) != 2 {
			continue
		}
		from, to := resources[e.RelatedComponents[0]], resources[e.RelatedComponents[1]]
		if len(from) == 1 && len(to) == 1 {
			d.edges[[2]string{from[0], to[0]}] = "references"
		}
	}
	return d.render("TB")
}

func dependencyDiagram(s evidence.Snapshot) string {
	d := newDiagram()
	module, packages := goPackages(s, d)
	for _, e := range s.Evidence {
		if e.Confidence == evidence.Verified && e.Category == "dependency" && e.Type != "go_import" {
			d.add("declared:"+e.Value, "Declared dependency: "+e.Value, e)
		}
	}
	addImports(s, d, module, packages, true)
	return d.render("LR")
}
