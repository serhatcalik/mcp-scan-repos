package notion

import (
	"context"
	"strings"
	"testing"
)

func TestMermaidIsOneCompleteLanguageTaggedBlock(t *testing.T) {
	input := "flowchart LR\n\n  subgraph g0[\"Appointments API\"]\n    n0[\"GET /appointments\"]\n  end\n"
	for _, raw := range []bool{true, false} {
		value := input
		if !raw {
			value = "```mermaid\n" + input + "```\n"
		}
		blocks, err := MarkdownBlocks(value, raw)
		if err != nil || len(blocks) != 1 || blocks[0].Type != "code" || blocks[0].Code.Language != "mermaid" || blocks[0].text() != input {
			t.Fatalf("diagram split or damaged: %+v %v", blocks, err)
		}
	}
	long := "flowchart LR\n" + strings.Repeat("  %% source provenance\n", 200)
	blocks, err := MarkdownBlocks(long, true)
	if err != nil || len(blocks) != 1 || len(blocks[0].Code.RichText) < 2 || blocks[0].text() != long {
		t.Fatalf("long diagram must remain one block: %v", err)
	}
}

func TestDangerousMarkdownURLsDoNotBecomeRichTextLinks(t *testing.T) {
	for _, markdown := range []string{
		`[click](&#106;avascript:alert(1))`,
		`[click](java&#x73;cript:alert(1))`,
		`[click](javascript&#58;alert(1))`,
		`<javascript:alert(document.domain)>`,
		`![alt](javascript:alert(document.domain))`,
	} {
		t.Run(markdown, func(t *testing.T) {
			blocks, err := MarkdownBlocks(markdown, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range blocks {
				if content := b.content(); content != nil {
					for _, item := range content.RichText {
						if linkURL(item) != "" {
							t.Fatalf("dangerous URL became an active link: %s", linkURL(item))
						}
					}
				}
			}
		})
	}
}

func TestNativeTablesInlineFormattingAndFootnotes(t *testing.T) {
	input := "# Paper\n\nA **bold** and *italic* paragraph with `DATABASE_URL`, ~~old~~, [Notion](https://www.notion.so), and source[^S1].\n\n| Group | Value |\n| --- | --- |\n| Users | GET /user\\_me |\n\n[^S1]: internal/userapi/routes.go:42\n"
	blocks, err := MarkdownBlocks(input, false)
	if err != nil {
		t.Fatal(err)
	}
	var table *tableContent
	bold, italic, code, strike, link, source := false, false, false, false, false, false
	for _, b := range blocks {
		if b.Table != nil {
			table = b.Table
			continue
		}
		if strings.Contains(b.text(), "[1] internal/userapi/routes.go:42") {
			source = true
		}
		if c := b.content(); c != nil {
			for _, item := range c.RichText {
				bold = bold || item.Annotations.Bold
				italic = italic || item.Annotations.Italic
				code = code || item.Annotations.Code
				strike = strike || item.Annotations.Strikethrough
				link = link || linkURL(item) == "https://www.notion.so"
			}
		}
	}
	if !bold || !italic || !code || !strike || !link || !source {
		t.Fatalf("formatting/citations lost: bold=%v italic=%v code=%v strike=%v link=%v source=%v", bold, italic, code, strike, link, source)
	}
	if table == nil || table.TableWidth != 2 || !table.HasColumnHeader || len(table.Children) != 2 || table.Children[1].TableRow.Cells[1][0].Text.Content != "GET /user_me" {
		t.Fatalf("native table missing: %+v", table)
	}
}

func TestNotionListsQuotesAndLanguageAliases(t *testing.T) {
	blocks, err := MarkdownBlocks("1. First\n2. Second\n\n- [x] Done\n- [ ] Pending\n\n> Quoted **text**\n\n---\n\n```yml\nkey: value\n```\n", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"numbered_list_item", "numbered_list_item", "to_do", "to_do", "quote", "divider", "code"}
	if len(blocks) != len(want) {
		t.Fatalf("blocks: %+v", blocks)
	}
	for i, kind := range want {
		if blocks[i].Type != kind {
			t.Fatalf("block %d: got %s want %s", i, blocks[i].Type, kind)
		}
	}
	if !blocks[2].ToDo.Checked || blocks[3].ToDo.Checked || blocks[6].Code.Language != "yaml" {
		t.Fatal("task state or language lost")
	}
}

func TestUnsafeLinksAndImagesAreNotActiveBlocks(t *testing.T) {
	blocks, err := MarkdownBlocks("[unsafe](javascript:alert) ![image](https://example.com/asset.png) [local](evidence.json)", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if b.Type != "paragraph" {
			t.Fatal("untrusted asset became active block")
		}
		for _, item := range b.Paragraph.RichText {
			if item.Text.Link != nil {
				t.Fatal("unsafe or relative link became active")
			}
		}
	}
	if !strings.Contains(blocks[0].text(), "javascript:alert") || !strings.Contains(blocks[0].text(), "evidence.json") {
		t.Fatal("literal targets lost")
	}
}

func TestNativeTableRetriesReadNestedRowsAndRejectEdits(t *testing.T) {
	f, c := newFakeNotion(t)
	page, err := c.EnsurePage(context.Background(), testParent, "Paper")
	if err != nil {
		t.Fatal(err)
	}
	desired, err := MarkdownBlocks("| Group | Value |\n| --- | --- |\n| API | **Gin** |\n\n```mermaid\nflowchart LR\n  A --> B\n```\n", false)
	if err != nil {
		t.Fatal(err)
	}
	f.failAppend = true
	if err := c.EnsureContent(context.Background(), page, desired); err == nil {
		t.Fatal("expected ambiguous committed write")
	}
	if err := c.EnsureContent(context.Background(), page, desired); err != nil || f.appends != 1 {
		t.Fatalf("duplicated native content: %v", err)
	}
	tableID := f.pages[page][0].ID
	f.pages[tableID][1].TableRow.Cells[1][0].Text.Content = "Human edit"
	if err := c.EnsureContent(context.Background(), page, desired); err == nil || f.appends != 1 {
		t.Fatal("table edit overwritten")
	}
}

func TestReconciliationChecksFormattingLanguageAndTextSegmentation(t *testing.T) {
	f, c := newFakeNotion(t)
	page, err := c.EnsurePage(context.Background(), testParent, "Diagram")
	if err != nil {
		t.Fatal(err)
	}
	desired, _ := MarkdownBlocks("flowchart LR\n  A --> B\n", true)
	if err := c.EnsureContent(context.Background(), page, desired); err != nil {
		t.Fatal(err)
	}
	f.pages[page][0].Code.RichText = []richText{textItem("flowchart LR\n"), textItem("  A --> B\n")}
	for i := range f.pages[page][0].Code.RichText {
		f.pages[page][0].Code.RichText[i].Annotations.Color = "default"
	}
	if err := c.EnsureContent(context.Background(), page, desired); err != nil {
		t.Fatalf("response segmentation is not an edit: %v", err)
	}
	f.pages[page][0].Code.Language = "plain text"
	if err := c.EnsureContent(context.Background(), page, desired); err == nil {
		t.Fatal("diagram language change ignored")
	}
	f.pages[page][0].Code.Language = "mermaid"
	f.pages[page][0].Code.RichText[0].Annotations.Bold = true
	if err := c.EnsureContent(context.Background(), page, desired); err == nil {
		t.Fatal("formatting edit ignored")
	}
}
