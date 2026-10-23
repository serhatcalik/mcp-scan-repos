package confluence

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"sort"
	"strings"
)

const acNamespace = "http://atlassian.com/content"

// Narva stores Mermaid in Forge guest parameters, not a code-macro body.
// IDs are stable across retries and distinct for repeated blocks in one page.
func narvaStorage(code, extensionKey string, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("narva-mermaid\x00%s\x00%d\x00%s", extensionKey, index, code)))
	sum[6] = (sum[6] & 0x0f) | 0x80
	sum[8] = (sum[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	attribute := func(key, value string) string {
		return `<ac:adf-attribute key="` + key + `">` + html.EscapeString(value) + `</ac:adf-attribute>`
	}
	parameter := func(key, value string) string {
		return `<ac:adf-parameter key="` + key + `">` + html.EscapeString(value) + `</ac:adf-parameter>`
	}
	node := `<ac:adf-node type="extension">` + attribute("extension-key", extensionKey) + attribute("extension-type", "com.atlassian.ecosystem") +
		`<ac:adf-attribute key="parameters">` + parameter("local-id", id) +
		parameter("extension-id", "ari:cloud:ecosystem::extension/"+extensionKey) + parameter("extension-title", "Mermaid Diagrams") +
		parameter("layout", "extension") + parameter("forge-environment", "PRODUCTION") +
		`<ac:adf-parameter key="guest-params">` + parameter("code", code) + `</ac:adf-parameter></ac:adf-attribute>` +
		attribute("text", "Mermaid Diagrams") + attribute("layout", "default") + attribute("local-id", id) + `</ac:adf-node>`
	return `<ac:adf-extension>` + node + `<ac:adf-fallback>` + node + `</ac:adf-fallback></ac:adf-extension>`
}

type storageNode struct {
	Name     xml.Name
	Attrs    []xml.Attr
	Text     string
	Children []*storageNode
}

// ADF is element-only except for text-valued leaves. Parse with explicit bounds
// before normalizing the known Narva context that Confluence can inject.
func readStorageNode(d *xml.Decoder, start xml.StartElement, count *int, depth int) (*storageNode, error) {
	if depth > 100 {
		return nil, errors.New("ADF nesting exceeds limit")
	}
	n := &storageNode{Name: start.Name, Attrs: start.Attr}
	var text strings.Builder
	for {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		(*count)++
		if *count > 100000 {
			return nil, errors.New("ADF structure exceeds limit")
		}
		switch token := token.(type) {
		case xml.StartElement:
			child, err := readStorageNode(d, token, count, depth+1)
			if err != nil {
				return nil, err
			}
			n.Children = append(n.Children, child)
		case xml.CharData:
			text.Write(token)
		case xml.EndElement:
			n.Text = text.String()
			if len(n.Children) > 0 {
				if strings.TrimSpace(n.Text) != "" {
					return nil, errors.New("unexpected mixed ADF content")
				}
				n.Text = ""
			}
			return n, nil
		default:
			return nil, errors.New("unexpected ADF content")
		}
	}
}

func (n *storageNode) attr(key string) string {
	for _, a := range n.Attrs {
		if a.Name.Space == "" && a.Name.Local == key {
			return a.Value
		}
	}
	return ""
}

func (n *storageNode) named(local string) bool {
	return n.Name.Space == acNamespace && n.Name.Local == local
}

func normalizeNarvaNode(n *storageNode) {
	if n.named("adf-node") && n.attr("type") == "extension" {
		key, ecosystem := "", ""
		for _, child := range n.Children {
			if child.named("adf-attribute") {
				switch child.attr("key") {
				case "extension-key":
					key = child.Text
				case "extension-type":
					ecosystem = child.Text
				}
			}
		}
		if narvaExtensionKey.MatchString(key) && ecosystem == "com.atlassian.ecosystem" {
			children := n.Children[:0]
			for _, child := range n.Children {
				if child.named("adf-attribute") && child.attr("key") == "local-id" {
					continue
				}
				if child.named("adf-attribute") && child.attr("key") == "parameters" {
					params := child.Children[:0]
					for _, param := range child.Children {
						if param.named("adf-parameter") && (param.attr("key") == "local-id" || param.attr("key") == "embedded-macro-context") {
							continue
						}
						params = append(params, param)
					}
					child.Children = params
				}
				children = append(children, child)
			}
			n.Children = children
		}
	}
	for _, child := range n.Children {
		normalizeNarvaNode(child)
	}
	// Attribute order and namespace declarations do not change ADF meaning.
	attrs := n.Attrs[:0]
	for _, a := range n.Attrs {
		if a.Name.Space != "xmlns" && a.Name.Local != "xmlns" {
			attrs = append(attrs, a)
		}
	}
	n.Attrs = attrs
	if len(n.Attrs) == 0 {
		n.Attrs = nil
	}
	sort.Slice(n.Attrs, func(i, j int) bool {
		a, b := n.Attrs[i], n.Attrs[j]
		if a.Name.Space != b.Name.Space {
			return a.Name.Space < b.Name.Space
		}
		if a.Name.Local != b.Name.Local {
			return a.Name.Local < b.Name.Local
		}
		return a.Value < b.Value
	})
}

func canonicalADF(d *xml.Decoder, start xml.StartElement, count *int, depth int) (string, error) {
	n, err := readStorageNode(d, start, count, depth)
	if err != nil {
		return "", err
	}
	normalizeNarvaNode(n)
	data, err := json.Marshal(n)
	return string(data), err
}
