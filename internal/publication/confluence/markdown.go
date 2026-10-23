package confluence

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	htmlrenderer "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

const rendererVersion = "storage-v2"

// Render only generated Markdown; raw HTML and remote images are never imported.
func MarkdownStorage(markdown string, diagram bool, settings Settings) (string, error) {
	if len(markdown) > 2<<20 {
		return "", errors.New("document exceeds Confluence input limit")
	}
	if err := validateMermaid(settings); err != nil {
		return "", err
	}
	if diagram {
		return checkedStorage(codeStorage(markdown, "mermaid", settings))
	}
	r := &storageRenderer{settings: settings}
	md := goldmark.New(goldmark.WithExtensions(extension.GFM, extension.Footnote),
		goldmark.WithRendererOptions(htmlrenderer.WithXHTML(), renderer.WithNodeRenderers(util.Prioritized(r, 100))))
	source := []byte(markdown)
	root := md.Parser().Parse(text.NewReader(source))
	count, depth := 0, 0
	if err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			count++
			depth++
		} else {
			depth--
		}
		if count > 10000 || depth > 64 {
			return ast.WalkStop, errors.New("document exceeds Confluence structure limit")
		}
		return ast.WalkContinue, nil
	}); err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := md.Renderer().Render(&out, source, root); err != nil {
		return "", errors.New("render Confluence storage failed")
	}
	return checkedStorage(out.String())
}

func checkedStorage(body string) (string, error) {
	if len(body) > 4<<20 {
		return "", errors.New("document exceeds Confluence output limit")
	}
	if !equalStorage(body, body) {
		return "", errors.New("document cannot be represented as Confluence storage XML")
	}
	return body, nil
}

func codeStorage(code, language string, settings Settings) string {
	return codeStorageAt(code, language, settings, 0)
}

func codeStorageAt(code, language string, settings Settings, index int) string {
	if language == "mermaid" && settings.MermaidApp == "narva" {
		return narvaStorage(code, settings.MermaidExtensionKey, index)
	}
	name := "code"
	parameter := ""
	if language == "mermaid" && settings.MermaidMacro != "" {
		name = settings.MermaidMacro
		if settings.MermaidBodyParameter != "" {
			return fmt.Sprintf(`<ac:structured-macro ac:name="%s"><ac:parameter ac:name="%s">%s</ac:parameter></ac:structured-macro>`, name, settings.MermaidBodyParameter, html.EscapeString(code))
		}
	} else {
		// Unknown languages (including Mermaid without an app) remain a complete code block.
		aliases := map[string]string{"go": "go", "java": "java", "json": "json", "javascript": "javascript", "js": "javascript", "python": "python", "py": "python", "sql": "sql", "bash": "bash", "sh": "bash", "xml": "xml", "html": "html", "css": "css"}
		language = aliases[language]
		if language == "" {
			language = "none"
		}
		parameter = `<ac:parameter ac:name="language">` + language + `</ac:parameter>`
	}
	return `<ac:structured-macro ac:name="` + name + `">` + parameter + `<ac:plain-text-body><![CDATA[` + strings.ReplaceAll(code, "]]>", "]]]]><![CDATA[>") + `]]></ac:plain-text-body></ac:structured-macro>`
}

type storageRenderer struct {
	settings  Settings
	codeIndex int
}

func (r *storageRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, r.code)
	reg.Register(ast.KindCodeBlock, r.code)
	reg.Register(ast.KindHTMLBlock, r.raw)
	reg.Register(ast.KindRawHTML, r.raw)
	reg.Register(ast.KindImage, r.image)
	reg.Register(ast.KindLink, r.link)
	reg.Register(ast.KindAutoLink, r.autoLink)
	reg.Register(extast.KindTaskCheckBox, r.checkbox)
}

func (r *storageRenderer) code(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	var b strings.Builder
	for i := 0; i < n.Lines().Len(); i++ {
		line := n.Lines().At(i)
		b.Write(line.Value(source))
	}
	language := ""
	if fenced, ok := n.(*ast.FencedCodeBlock); ok {
		language = string(fenced.Language(source))
	}
	_, err := w.WriteString(codeStorageAt(b.String(), language, r.settings, r.codeIndex))
	r.codeIndex++
	return ast.WalkSkipChildren, err
}

func (r *storageRenderer) raw(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	var b strings.Builder
	if raw, ok := n.(*ast.RawHTML); ok {
		for i := 0; i < raw.Segments.Len(); i++ {
			seg := raw.Segments.At(i)
			b.Write(seg.Value(source))
		}
	} else {
		for i := 0; i < n.Lines().Len(); i++ {
			seg := n.Lines().At(i)
			b.Write(seg.Value(source))
		}
		if block, ok := n.(*ast.HTMLBlock); ok && block.HasClosure() {
			b.Write(block.ClosureLine.Value(source))
		}
	}
	_, err := w.WriteString(html.EscapeString(b.String()))
	return ast.WalkSkipChildren, err
}

func (r *storageRenderer) image(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, err := w.WriteString(html.EscapeString(string(n.Text(source))))
		return ast.WalkSkipChildren, err
	}
	return ast.WalkContinue, nil
}

func safeLink(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.User == nil && !strings.ContainsAny(value, "\r\n") && ((u.Scheme == "https" || u.Scheme == "http") && u.Host != "" || u.Scheme == "mailto")
}

func (r *storageRenderer) link(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	link := n.(*ast.Link)
	if !safeLink(string(link.Destination)) {
		return ast.WalkContinue, nil
	}
	value := "</a>"
	if entering {
		value = `<a href="` + html.EscapeString(string(link.Destination)) + `">`
	}
	_, err := w.WriteString(value)
	return ast.WalkContinue, err
}

func (r *storageRenderer) autoLink(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	link := n.(*ast.AutoLink)
	dest := string(link.URL(source))
	label := html.EscapeString(string(link.Label(source)))
	if safeLink(dest) {
		label = `<a href="` + html.EscapeString(dest) + `">` + label + `</a>`
	}
	_, err := w.WriteString(label)
	return ast.WalkSkipChildren, err
}

func (r *storageRenderer) checkbox(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		value := "[ ] "
		if n.(*extast.TaskCheckBox).IsChecked {
			value = "[x] "
		}
		_, err := w.WriteString(value)
		return ast.WalkSkipChildren, err
	}
	return ast.WalkContinue, nil
}
