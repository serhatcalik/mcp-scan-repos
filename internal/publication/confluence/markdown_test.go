package confluence

import (
	"strings"
	"testing"
)

func TestMarkdownStorageNativeFormatting(t *testing.T) {
	md := "# Technical Paper\n\n**Bold** and *italic* with `code`, ~~old~~, [docs](https://example.com).\n\n| Group | Value |\n| --- | --- |\n| API | Gin |\n\n- One\n- Two\n\n1. First\n2. Second\n\n> Verified\n\n- [x] Done\n\n```go\nfmt.Println(\"ok\")\n```\n\nSource[^s]\n\n[^s]: main.go:10\n"
	body, err := MarkdownStorage(md, false, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h1>Technical Paper</h1>", "<strong>Bold</strong>", "<em>italic</em>", "<code>code</code>", "<del>old</del>", `<a href="https://example.com">docs</a>`, "<table>", "<th>Group</th>", "<td>Gin</td>", "<ul>", "<ol>", "<blockquote>", "[x] Done", `ac:name="code"`, `ac:name="language">go`, `fmt.Println("ok")`, "main.go:10"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	if !equalStorage(body, body) {
		t.Fatalf("invalid storage XML: %s", body)
	}
}

func TestMermaidRemainsOneCompleteMacro(t *testing.T) {
	source := "flowchart LR\n\n  A[API] --> B[Worker]\n  %% preserve ]]> safely\n"
	for _, diagram := range []bool{false, true} {
		md := source
		if !diagram {
			md = "```mermaid\n" + source + "```\n"
		}
		for _, parameter := range []string{"", "chart"} {
			body, err := MarkdownStorage(md, diagram, Settings{MermaidMacro: "mermaid-chart", MermaidBodyParameter: parameter})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(body, "<ac:structured-macro") != 1 || !strings.Contains(body, `ac:name="mermaid-chart"`) || !strings.Contains(body, "flowchart LR\n\n") {
				t.Fatalf("split or damaged Mermaid: %s", body)
			}
			if !equalStorage(body, body) {
				t.Fatalf("invalid macro XML: %s", body)
			}
		}
	}
	fallback, err := MarkdownStorage(source, true, Settings{})
	if err != nil || !strings.Contains(fallback, `ac:name="code"`) {
		t.Fatalf("missing code fallback: %s %v", fallback, err)
	}
}

func TestMarkdownStorageRejectsActiveContentAndLimits(t *testing.T) {
	body, err := MarkdownStorage("<script>alert(1)</script>\n\n[bad](javascript:alert(1)) [credentials](https://user:secret@example.com) ![alt](https://example.com/private.png)\n", false, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script", "<img", `href="javascript`, "user:secret", "private.png"} {
		if strings.Contains(body, bad) {
			t.Fatalf("active content leaked: %s", body)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("HTML was not preserved as literal text")
	}
	if _, err := MarkdownStorage(strings.Repeat("x", (2<<20)+1), false, Settings{}); err == nil {
		t.Fatal("oversized Markdown accepted")
	}
	if _, err := MarkdownStorage("flowchart LR", true, Settings{MermaidMacro: `evil"><script>`}); err == nil {
		t.Fatal("macro injection accepted")
	}
	if _, err := MarkdownStorage("invalid\x00XML", true, Settings{}); err == nil {
		t.Fatal("invalid XML character accepted")
	}
}

func TestStorageComparisonPreservesMeaningfulEdits(t *testing.T) {
	body := codeStorage("A ]]> B\n\n", "mermaid", Settings{MermaidMacro: "mermaid"})
	withMetadata := strings.Replace(body, `ac:name="mermaid"`, `ac:name="mermaid" ac:macro-id="abc" ac:schema-version="1"`, 1)
	if !equalStorage(body, withMetadata) {
		t.Fatal("Confluence macro metadata caused mismatch")
	}
	if !equalStorage(`<p>A &amp; B</p>`, `<p><![CDATA[A & B]]></p>`) {
		t.Fatal("equivalent XML character representations mismatched")
	}
	if equalStorage(body, strings.Replace(body, "A ", "C ", 1)) || equalStorage(`<p>Human edit</p>`, `<p>Generated</p>`) {
		t.Fatal("human edit ignored")
	}
	if equalStorage(body, strings.Replace(body, "\n\n", "\n", 1)) {
		t.Fatal("code whitespace edit ignored")
	}
	if equalStorage(`<p><strong>A</strong> <em>B</em></p>`, `<p><strong>A</strong><em>B</em></p>`) {
		t.Fatal("inline text whitespace edit ignored")
	}
}

func TestDangerousMarkdownURLsDoNotBecomeActiveContent(t *testing.T) {
	for _, markdown := range []string{
		`[click](&#106;avascript:alert(1))`,
		`[click](java&#x73;cript:alert(1))`,
		`[click](javascript&#58;alert(1))`,
		`<javascript:alert(document.domain)>`,
		`![alt](javascript:alert(document.domain))`,
	} {
		t.Run(markdown, func(t *testing.T) {
			body, err := MarkdownStorage(markdown, false, Settings{})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(body, "<a ") || strings.Contains(body, "<img ") {
				t.Fatalf("dangerous URL became active content: %s", body)
			}
		})
	}
}

func cloudFootnoteStorage() string {
	return `<p>Source<sup><a class="footnote-ref">1</a></sup> and again<sup><a class="footnote-ref">1</a></sup>.</p>
<div class="footnotes">
<hr />
<ol>
<li>
<p>main.go:10.&nbsp;<a class="footnote-backref">` + "\u21a9\ufe0e" + `</a>&nbsp;<a class="footnote-backref">` + "\u21a9\ufe0e" + `</a></p>
</li>
</ol>
</div>`
}

func TestCloudFootnoteStorageNormalization(t *testing.T) {
	body, err := MarkdownStorage("Source[^s] and again[^s].\n\n[^s]: main.go:10.\n", false, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	cloud := cloudFootnoteStorage()
	if !equalStorage(body, cloud) {
		t.Fatalf("Cloud footnote serialization mismatched: %s", body)
	}
	for _, changed := range []string{
		strings.Replace(cloud, "main.go:10", "main.go:20", 1),
		strings.Replace(cloud, `class="footnote-ref">1`, `class="footnote-ref">2`, 1),
		strings.Replace(cloud, `class="footnote-ref"`, `class="footnote-ref" href="https://example.com/changed"`, 1),
		strings.Replace(cloud, `class="footnote-ref"`, `class="footnote-ref" href="javascript:alert(1)"`, 1),
		strings.Replace(cloud, "main.go:10.&nbsp;", "main.go:10.", 1),
	} {
		if equalStorage(body, changed) {
			t.Fatalf("meaningful footnote edit accepted: %s", changed)
		}
	}
	if equalStorage(`<p><a href="#fn:1">ordinary link</a></p>`, `<p><a>ordinary link</a></p>`) {
		t.Fatal("ordinary fragment link change ignored")
	}
	if !equalStorage(`<p>A&#160;B</p>`, `<p>A&nbsp;B</p>`) {
		t.Fatal("equivalent non-breaking spaces mismatched")
	}
	code := codeStorage("literal &nbsp;", "mermaid", Settings{MermaidMacro: "mermaid"})
	if equalStorage(code, strings.Replace(code, "&nbsp;", "\u00a0", 1)) {
		t.Fatal("CDATA diagram content was HTML-decoded")
	}
}
