package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testParent = "11111111111111111111111111111111"

type fakeNotion struct {
	mu                     sync.Mutex
	pages                  map[string][]block
	creates, appends       int
	failCreate, failAppend bool
	calls                  int
	blocks                 int
}

func newFakeNotion(t *testing.T) (*fakeNotion, *Client) {
	t.Helper()
	f := &fakeNotion{pages: map[string][]block{testParent: {}}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	c := NewClient("secret-test-token")
	c.endpoint = server.URL
	return f, c
}

func (f *fakeNotion) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer secret-test-token" || r.Header.Get("Notion-Version") != apiVersion {
		http.Error(w, "bad credentials/version", 401)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/pages" {
		var input struct {
			Parent struct {
				PageID string `json:"page_id"`
			} `json:"parent"`
			Properties struct {
				Title struct {
					Title []richText `json:"title"`
				} `json:"title"`
			} `json:"properties"`
			Children []block `json:"children"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		f.creates++
		id := fmt.Sprintf("%032x", f.creates+100)
		f.pages[id] = []block{}
		for _, child := range input.Children {
			f.pages[id] = append(f.pages[id], f.materialize(child))
		}
		f.pages[input.Parent.PageID] = append(f.pages[input.Parent.PageID], block{ID: id, Type: "child_page", ChildPage: &blockContent{Title: input.Properties.Title.Title[0].Text.Content}})
		if f.failCreate {
			f.failCreate = false
			w.WriteHeader(503)
			fmt.Fprint(w, `{"message":"remote write committed secret-test-token"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) == 3 && parts[1] == "pages" {
		var page *block
		for _, entries := range f.pages {
			for i := range entries {
				if entries[i].Type == "child_page" && entries[i].ID == normalizeID(parts[2]) {
					page = &entries[i]
				}
			}
		}
		if page == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPatch {
			var input struct {
				Properties map[string]struct {
					Title []richText `json:"title"`
				} `json:"properties"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				http.Error(w, "invalid title", 400)
				return
			}
			for _, property := range input.Properties {
				var title strings.Builder
				for _, item := range property.Title {
					title.WriteString(item.Text.Content)
				}
				page.ChildPage.Title = title.String()
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": page.ID, "properties": map[string]any{"title": map[string]any{"type": "title", "title": []richText{textItem(page.ChildPage.Title)}}}})
		return
	}
	if len(parts) != 4 || parts[1] != "blocks" || parts[3] != "children" {
		http.NotFound(w, r)
		return
	}
	id := normalizeID(parts[2])
	entries, ok := f.pages[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": entries, "has_more": false, "next_cursor": nil})
		return
	}
	if r.Method == http.MethodPatch {
		var input struct {
			Children []block `json:"children"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Children) > 20 {
			http.Error(w, "bad blocks", 400)
			return
		}
		f.appends++
		for i := range input.Children {
			input.Children[i] = f.materialize(input.Children[i])
		}
		f.pages[id] = append(entries, input.Children...)
		if f.failAppend {
			f.failAppend = false
			w.WriteHeader(503)
			fmt.Fprint(w, `{"message":"saved but unavailable"}`)
			return
		}
		fmt.Fprint(w, `{"results":[]}`)
		return
	}
	http.Error(w, "bad method", 405)
}

// Block children are retrieved separately by the real API, including table rows.
func (f *fakeNotion) materialize(b block) block {
	f.blocks++
	b.ID = fmt.Sprintf("%032x", f.blocks+10000)
	if b.Table != nil && len(b.Table.Children) > 0 {
		var children []block
		for _, child := range b.Table.Children {
			children = append(children, f.materialize(child))
		}
		f.pages[b.ID] = children
		b.Table.Children, b.HasChildren = nil, true
	}
	return b
}

func TestCreateAndAppendReconcileAmbiguousWrites(t *testing.T) {
	f, c := newFakeNotion(t)
	ctx := context.Background()
	f.failCreate = true
	if _, err := c.EnsurePage(ctx, testParent, "GitLab Project 42"); err == nil || strings.Contains(err.Error(), "secret-test-token") {
		t.Fatalf("unsafe or missing error: %v", err)
	}
	page, err := c.EnsurePage(ctx, testParent, "GitLab Project 42")
	if err != nil || f.creates != 1 {
		t.Fatalf("duplicate create: %d %v", f.creates, err)
	}
	desired, err := MarkdownBlocks("# Paper\n\n"+strings.Repeat("text\n\n", 45), false)
	if err != nil {
		t.Fatal(err)
	}
	f.failAppend = true
	if err := c.EnsureContent(ctx, page, desired); err == nil {
		t.Fatal("expected ambiguous append failure")
	}
	if err := c.EnsureContent(ctx, page, desired); err != nil {
		t.Fatal(err)
	}
	if len(f.pages[page]) != len(desired) || f.appends != 3 {
		t.Fatalf("duplicated blocks or replay: blocks=%d appends=%d", len(f.pages[page]), f.appends)
	}
	if err := c.EnsureContent(ctx, page, desired); err != nil || f.appends != 3 {
		t.Fatalf("non-idempotent publication: %v", err)
	}
	f.pages[page][0] = textBlock("paragraph", "human edit")
	if err := c.EnsureContent(ctx, page, desired); err == nil || f.appends != 3 {
		t.Fatal("human edit overwritten")
	}
}

func TestMarkdownUnicodeBoundsAndLiteralUntrustedContent(t *testing.T) {
	text := strings.Repeat("\u754c", 4000)
	blocks, err := MarkdownBlocks("## Heading\n- Item\n```mermaid\nflowchart LR\n```\n"+text+"\n<script>doBadThings()</script>", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 5 || blocks[0].Type != "heading_2" || blocks[1].Type != "bulleted_list_item" || blocks[2].Type != "code" || blocks[2].Code.Language != "mermaid" {
		t.Fatal("native Markdown conversion failed")
	}
	for _, item := range blocks[3].Paragraph.RichText {
		if len([]rune(item.Text.Content)) > richTextRunes {
			t.Fatal("oversized rich text segment")
		}
	}
	if blocks[3].text() != text || blocks[4].text() != "<script>doBadThings()</script>" {
		t.Fatal("content lost or interpreted")
	}
	if _, err := MarkdownBlocks(strings.Repeat("line\n", 10001), false); err == nil {
		t.Fatal("unbounded conversion")
	}
}

func TestRateLimitAndRedirectErrorsDoNotLeakToken(t *testing.T) {
	leaked := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, destination.URL, 302)
			return
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"message":"secret-test-token"}`)
	}))
	defer server.Close()
	c := NewClient("secret-test-token")
	c.endpoint = server.URL
	err := c.request(context.Background(), http.MethodGet, "/limited", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != 120*time.Second || !apiErr.Retryable() || strings.Contains(err.Error(), "secret-test-token") {
		t.Fatalf("invalid safe rate error: %v", err)
	}
	if err := c.request(context.Background(), http.MethodGet, "/redirect", nil, nil); err == nil || leaked {
		t.Fatal("redirect forwarded authorization")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.request(ctx, http.MethodGet, "/limited", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestTargetAndSecretConfiguration(t *testing.T) {
	good := `{"targets":[{"group_id":"101","parent_page_id":"11111111-1111-1111-1111-111111111111","enabled":true}]}`
	targets, err := ParseTargets([]byte(good))
	if err != nil || targets[0].ParentPageID != testParent {
		t.Fatal("valid target rejected")
	}
	for _, bad := range []string{`{"targets":[]}`, good + `{}`, strings.Replace(good, `"101"`, `"../other"`, 1), strings.Replace(good, `"enabled":true`, `"token":"secret"`, 1), `{"targets":[{"group_id":"101","parent_page_id":"invalid","enabled":true}]}`} {
		if _, err := ParseTargets([]byte(bad)); err == nil {
			t.Fatalf("invalid config accepted: %s", bad)
		}
	}
	root := t.TempDir()
	configFile := filepath.Join(root, "targets.json")
	tokenFile := filepath.Join(root, "token")
	if err := os.WriteFile(configFile, []byte(good), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("secret-test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, token, err := LoadFiles(configFile, tokenFile); err != nil || token != "secret-test-token" {
		t.Fatalf("file config failed: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("secret\r\nHeader: injection"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadFiles(configFile, tokenFile); err == nil || strings.Contains(err.Error(), "injection") {
		t.Fatal("unsafe token config accepted/exposed")
	}
}

func TestPaginationRejectsCyclesAndMissingResults(t *testing.T) {
	for _, mode := range []string{"cycle", "missing", "valid"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				switch mode {
				case "missing":
					fmt.Fprint(w, `{}`)
				case "cycle":
					cursor := "one"
					if calls == 2 {
						cursor = "two"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"results": []block{}, "has_more": true, "next_cursor": cursor})
				case "valid":
					if calls == 1 {
						fmt.Fprint(w, `{"results":[],"has_more":true,"next_cursor":"next"}`)
					} else {
						if r.URL.Query().Get("start_cursor") != "next" {
							t.Error("cursor not propagated")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"results": []block{textBlock("paragraph", "last")}, "has_more": false})
					}
				}
			}))
			defer server.Close()
			c := NewClient("secret-test-token")
			c.endpoint = server.URL
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			blocks, err := c.children(ctx, testParent)
			if mode == "valid" {
				if err != nil || len(blocks) != 1 || blocks[0].text() != "last" {
					t.Fatalf("pagination: %v %+v", err, blocks)
				}
			} else if err == nil || calls > 3 {
				t.Fatalf("invalid response/cycle accepted: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRateLimitPreservesLongCooldown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "172800")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	c := NewClient("secret-test-token")
	c.endpoint = server.URL
	var apiErr *APIError
	err := c.request(context.Background(), http.MethodGet, "/limited", nil, nil)
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != 48*time.Hour {
		t.Fatalf("server cooldown shortened: %v", err)
	}
}
