package confluence

import (
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"
)

const testNarvaKey = "336440d6-2d95-4275-833f-6351e86634e3/e5743d37-53d6-4daa-a3a1-acca68790e69/static/mermaid-diagram"

func narvaSettings() Settings {
	s := validSettings()
	s.MermaidApp, s.MermaidExtensionKey = "narva", testNarvaKey
	return s
}

func TestNarvaConfigurationValidation(t *testing.T) {
	s := narvaSettings()
	data, _ := json.Marshal(s)
	if _, err := ParseSettings(data); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Settings){
		func(s *Settings) { s.MermaidExtensionKey = "" },
		func(s *Settings) { s.MermaidExtensionKey = `bad"><script>` },
		func(s *Settings) { s.MermaidExtensionKey = strings.Replace(testNarvaKey, "336440d6", "00000000", 1) },
		func(s *Settings) { s.MermaidMacro = "mermaid" },
		func(s *Settings) { s.MermaidBodyParameter = "code" },
		func(s *Settings) { s.MermaidApp = "other" },
		func(s *Settings) { s.MermaidApp = "" },
		func(s *Settings) { s.Edition = "data-center" },
	} {
		s := narvaSettings()
		mutate(&s)
		data, _ := json.Marshal(s)
		if _, err := ParseSettings(data); err == nil {
			t.Fatal("invalid Narva settings accepted")
		}
		if _, err := MarkdownStorage("flowchart LR", true, s); err == nil {
			t.Fatal("invalid Narva renderer settings accepted")
		}
	}
}

func TestNarvaForgeStoragePreservesDiagram(t *testing.T) {
	code := "flowchart LR\n\n A[\"<script> & ]]>\"] --> B\n"
	for _, raw := range []bool{true, false} {
		md := code
		if !raw {
			md = "```mermaid\n" + code + "```\n"
		}
		body, err := MarkdownStorage(md, raw, narvaSettings())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(body, "<ac:adf-extension>") != 1 || strings.Count(body, `key="guest-params"`) != 2 || strings.Contains(body, `ac:name="code"`) || strings.Contains(body, "<script>") {
			t.Fatalf("bad Forge diagram: %s", body)
		}
		for _, forbidden := range []string{"account-id", "cloud-id", "context-ids", "embedded-macro-context", "1015809"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("copied user/page context: %s", forbidden)
			}
		}
		d := xml.NewDecoder(strings.NewReader(`<root xmlns:ac="` + acNamespace + `">` + body + `</root>`))
		codes := []string{}
		for {
			token, err := d.Token()
			if err != nil {
				break
			}
			if start, ok := token.(xml.StartElement); ok && start.Name.Local == "adf-parameter" {
				for _, attr := range start.Attr {
					if attr.Name.Local == "key" && attr.Value == "code" {
						var value string
						if err := d.DecodeElement(&value, &start); err != nil {
							t.Fatal(err)
						}
						codes = append(codes, value)
					}
				}
			}
		}
		if len(codes) != 2 || codes[0] != code || codes[1] != code {
			t.Fatalf("Forge guest code damaged: %v", codes)
		}
		again, err := MarkdownStorage(md, raw, narvaSettings())
		if err != nil || body != again {
			t.Fatal("Narva render is unstable across retries")
		}
	}
}

func TestNarvaBlocksHaveDistinctStableIDs(t *testing.T) {
	block := "```mermaid\nflowchart LR\n A --> B\n```\n\n"
	body, err := MarkdownStorage(block+block, false, narvaSettings())
	if err != nil {
		t.Fatal(err)
	}
	first := narvaStorage("flowchart LR\n A --> B\n", testNarvaKey, 0)
	second := narvaStorage("flowchart LR\n A --> B\n", testNarvaKey, 1)
	if first == second || !strings.Contains(body, first) || !strings.Contains(body, second) {
		t.Fatal("repeated diagrams share a local ID")
	}
}

func TestNarvaContextNormalizationDoesNotIgnoreDiagramEdits(t *testing.T) {
	body, err := MarkdownStorage("flowchart LR\n A --> B\n", true, narvaSettings())
	if err != nil {
		t.Fatal(err)
	}
	context := `<ac:adf-parameter key="embedded-macro-context"><ac:adf-parameter key="extension-data"><ac:adf-parameter key="content"><ac:adf-parameter key="id">999999</ac:adf-parameter></ac:adf-parameter></ac:adf-parameter><ac:adf-parameter key="account-id">test-user</ac:adf-parameter></ac:adf-parameter>`
	cloud := strings.ReplaceAll(body, `<ac:adf-parameter key="guest-params">`, context+`<ac:adf-parameter key="guest-params">`)
	// The host can replace local IDs while keeping the diagram and app identity.
	cloud = strings.ReplaceAll(cloud, `key="local-id">`, `key="local-id">host-`)
	if !equalStorage(body, cloud) {
		t.Fatal("host-supplied Narva context caused mismatch")
	}
	for _, changed := range []string{
		strings.Replace(cloud, "A --&gt; B", "A --&gt; C", 1),
		strings.Replace(cloud, "A --&gt; B", "A --&gt; C", 2),
		strings.Replace(cloud, testNarvaKey, strings.Replace(testNarvaKey, "e5743d37", "00000000", 1), 1),
		strings.Replace(cloud, `key="guest-params"`, `key="changed-params"`, 1),
		cloud + `<p>Human notes</p>`,
	} {
		if equalStorage(body, changed) {
			t.Fatal("meaningful Narva edit ignored")
		}
	}
	foreign := strings.ReplaceAll(body, "336440d6", "00000000")
	foreignContext := strings.ReplaceAll(cloud, "336440d6", "00000000")
	if equalStorage(foreign, foreignContext) {
		t.Fatal("context normalization affected an unknown Forge app")
	}
}
