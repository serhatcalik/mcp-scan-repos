package notion

import (
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

const richTextRunes = 1500
const rendererVersion = "native-blocks-v1"

// Parse Markdown without rendering HTML, fetching assets, or executing diagrams.
func MarkdownBlocks(markdown string, codeOnly bool) ([]block, error) {
	if len(markdown) > 2<<20 {
		return nil, errors.New("document exceeds Notion input limit")
	}
	if codeOnly {
		b := textBlock("code", "")
		b.Code.Language = "mermaid"
		b.Code.RichText = richItems(markdown, annotations{}, "")
		return checkedBlocks([]block{b})
	}
	source := []byte(markdown)
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM, extension.Footnote))
	root := parser.Parser().Parse(text.NewReader(source))
	count, depth := 0, 0
	if err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			count++
			depth++
		} else {
			depth--
		}
		if count > 10000 || depth > 64 {
			return ast.WalkStop, errors.New("document exceeds Notion structure limit")
		}
		return ast.WalkContinue, nil
	}); err != nil {
		return nil, err
	}
	r := markdownRenderer{source: source}
	return checkedBlocks(r.children(root))
}

func richItems(value string, style annotations, link string) []richText {
	chars := []rune(value)
	var out []richText
	for len(chars) > 0 {
		n := min(richTextRunes, len(chars))
		item := textItem(string(chars[:n]))
		item.Annotations = style
		if link != "" {
			item.Text.Link = &textLink{URL: link}
		}
		out = append(out, item)
		chars = chars[n:]
	}
	return out
}

func checkedBlocks(blocks []block) ([]block, error) {
	count := 0
	var check func([]block) error
	check = func(nodes []block) error {
		for _, b := range nodes {
			count++
			if count > 10000 {
				return errors.New("document exceeds Notion block limit")
			}
			if c := b.content(); c != nil {
				if len(c.RichText) > 100 || len(c.Children) > 100 || len(c.Cells) > 100 {
					return errors.New("document exceeds Notion rich text or child limit")
				}
				for _, cell := range c.Cells {
					if len(cell) > 100 {
						return errors.New("table cell exceeds Notion rich text limit")
					}
				}
				if err := check(c.Children); err != nil {
					return err
				}
			}
			if b.Table != nil {
				if err := check(b.Table.Children); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return blocks, check(blocks)
}

type markdownRenderer struct{ source []byte }

func (r markdownRenderer) children(parent ast.Node) []block {
	var blocks []block
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		switch node := n.(type) {
		case *ast.Heading:
			blocks = append(blocks, r.inlineBlock(fmt.Sprintf("heading_%d", min(node.Level, 3)), node))
		case *ast.Paragraph, *ast.TextBlock:
			blocks = append(blocks, r.inlineBlock("paragraph", node))
		case *ast.FencedCodeBlock:
			b := textBlock("code", "")
			b.Code.Language = codeLanguage(string(node.Language(r.source)))
			b.Code.RichText = richItems(string(node.Lines().Value(r.source)), annotations{}, "")
			blocks = append(blocks, b)
		case *ast.CodeBlock:
			b := textBlock("code", "")
			b.Code.RichText = richItems(string(node.Lines().Value(r.source)), annotations{}, "")
			blocks = append(blocks, b)
		case *ast.List:
			// Flat list blocks retain text and order without exceeding API nesting limits.
			for item := node.FirstChild(); item != nil; item = item.NextSibling() {
				parts := r.children(item)
				for i := range parts {
					if i == 0 && parts[i].Type == "paragraph" {
						kind := "bulleted_list_item"
						if node.IsOrdered() {
							kind = "numbered_list_item"
						}
						content := parts[i].Paragraph
						b := textBlock(kind, "")
						b.content().RichText = content.RichText
						if p := item.FirstChild(); p != nil {
							if task, ok := p.FirstChild().(*extast.TaskCheckBox); ok {
								b = textBlock("to_do", "")
								b.ToDo.RichText, b.ToDo.Checked = content.RichText, task.IsChecked
							}
						}
						parts[i] = b
					}
				}
				blocks = append(blocks, parts...)
			}
		case *ast.Blockquote:
			parts := r.children(node)
			for i := range parts {
				if parts[i].Type == "paragraph" {
					parts[i].Type, parts[i].Quote, parts[i].Paragraph = "quote", parts[i].Paragraph, nil
				}
			}
			blocks = append(blocks, parts...)
		case *ast.ThematicBreak:
			blocks = append(blocks, block{Object: "block", Type: "divider", Divider: &struct{}{}})
		case *extast.Table:
			blocks = append(blocks, r.table(node)...)
		case *extast.FootnoteList:
			for note := node.FirstChild(); note != nil; note = note.NextSibling() {
				parts := r.children(note)
				if len(parts) > 0 && parts[0].content() != nil {
					index := note.(*extast.Footnote).Index
					parts[0].content().RichText = append(richItems(fmt.Sprintf("[%d] ", index), annotations{}, ""), parts[0].content().RichText...)
				}
				blocks = append(blocks, parts...)
			}
		case *ast.HTMLBlock:
			value := string(node.Lines().Value(r.source))
			if node.HasClosure() {
				value += string(node.ClosureLine.Value(r.source))
			}
			b := textBlock("paragraph", "")
			b.Paragraph.RichText = richItems(strings.TrimSuffix(value, "\n"), annotations{}, "")
			blocks = append(blocks, b)
		default:
			blocks = append(blocks, r.children(node)...)
		}
	}
	return blocks
}

func (r markdownRenderer) inlineBlock(kind string, node ast.Node) block {
	b := textBlock(kind, "")
	b.content().RichText = r.inline(node, annotations{}, "")
	return b
}

func (r markdownRenderer) inline(parent ast.Node, style annotations, link string) []richText {
	var out []richText
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		switch node := n.(type) {
		case *ast.Text:
			value := string(node.Segment.Value(r.source))
			if !node.IsRaw() {
				value = html.UnescapeString(string(util.UnescapePunctuations([]byte(value))))
			}
			if node.SoftLineBreak() || node.HardLineBreak() {
				value += "\n"
			}
			out = append(out, richItems(value, style, link)...)
		case *ast.String:
			out = append(out, richItems(string(node.Value), style, link)...)
		case *ast.Emphasis:
			childStyle := style
			if node.Level == 2 {
				childStyle.Bold = true
			} else {
				childStyle.Italic = true
			}
			out = append(out, r.inline(node, childStyle, link)...)
		case *extast.Strikethrough:
			childStyle := style
			childStyle.Strikethrough = true
			out = append(out, r.inline(node, childStyle, link)...)
		case *ast.CodeSpan:
			childStyle := style
			childStyle.Code = true
			out = append(out, r.inline(node, childStyle, link)...)
		case *ast.Link:
			target := safeLink(string(node.Destination))
			out = append(out, r.inline(node, style, target)...)
			if target == "" {
				out = append(out, richItems(" ("+string(node.Destination)+")", style, "")...)
			}
		case *ast.AutoLink:
			out = append(out, richItems(string(node.Label(r.source)), style, safeLink(string(node.URL(r.source))))...)
		case *ast.Image:
			// Uploaded Markdown must not cause external assets to be fetched.
			out = append(out, r.inline(node, style, "")...)
			out = append(out, richItems(" (image: "+string(node.Destination)+")", style, "")...)
		case *ast.RawHTML:
			out = append(out, richItems(string(node.Segments.Value(r.source)), style, "")...)
		case *extast.FootnoteLink:
			out = append(out, richItems(fmt.Sprintf("[%d]", node.Index), style, "")...)
		case *extast.FootnoteBacklink, *extast.TaskCheckBox:
		default:
			out = append(out, r.inline(node, style, link)...)
		}
	}
	return coalesceRichText(out)
}

func safeLink(value string) string {
	u, err := url.Parse(value)
	if err != nil || len(value) > 2000 || u.User != nil {
		return ""
	}
	if (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" || u.Scheme == "mailto" && u.Opaque != "" {
		return value
	}
	return ""
}

func (r markdownRenderer) table(node *extast.Table) []block {
	var rows []block
	for row := node.FirstChild(); row != nil; row = row.NextSibling() {
		cells := [][]richText{}
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, r.inline(cell, annotations{}, ""))
		}
		rows = append(rows, block{Object: "block", Type: "table_row", TableRow: &blockContent{Cells: cells}})
	}
	if len(rows) == 0 {
		return nil
	}
	var out []block
	for start := 1; start < len(rows) || start == 1; {
		end := min(start+19, len(rows))
		children := append([]block{rows[0]}, rows[start:end]...)
		out = append(out, block{Object: "block", Type: "table", Table: &tableContent{TableWidth: len(rows[0].TableRow.Cells), HasColumnHeader: true, Children: children}})
		start = end
		if start >= len(rows) {
			break
		}
	}
	return out
}

func codeLanguage(info string) string {
	info = strings.ToLower(strings.TrimSpace(info))
	aliases := map[string]string{"js": "javascript", "ts": "typescript", "py": "python", "sh": "shell", "yml": "yaml", "md": "markdown", "text": "plain text", "csharp": "c#", "c++": "c++"}
	if mapped, ok := aliases[info]; ok {
		return mapped
	}
	for _, language := range strings.Split("abap,arduino,bash,basic,c,clojure,coffeescript,c++,c#,css,dart,diff,docker,elixir,elm,erlang,flow,fortran,f#,gherkin,glsl,go,graphql,groovy,haskell,html,java,javascript,json,julia,kotlin,latex,less,lisp,livescript,lua,makefile,markdown,markup,matlab,mermaid,nix,objective-c,ocaml,pascal,perl,php,plain text,powershell,prolog,protobuf,python,r,reason,ruby,rust,sass,scala,scheme,scss,shell,sql,swift,typescript,vb.net,verilog,vhdl,visual basic,webassembly,xml,yaml", ",") {
		if info == language {
			return info
		}
	}
	return "plain text"
}
