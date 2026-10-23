package confluence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeConfluence struct {
	mu             sync.Mutex
	pages          map[string]page
	creates, calls int
	failCreate     bool
	transformBody  func(string) string
}

func newFakeConfluence(t *testing.T, edition string) (*fakeConfluence, *Client) {
	t.Helper()
	f := &fakeConfluence{pages: map[string]page{}}
	root := page{ID: "300", Title: "Team docs", Status: "current", SpaceID: "200"}
	root.Space.Key = "DOCS"
	f.pages[root.ID] = root
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if edition == "cloud" {
			user, token, ok := r.BasicAuth()
			if !ok || user != "docs@example.com" || token != "test-token-12345" {
				t.Error("missing Cloud basic authentication")
			}
		} else if r.Header.Get("Authorization") != "Bearer test-token-12345" {
			t.Error("missing Data Center bearer authentication")
		}
		path := "/api/v2/pages"
		if edition == "data-center" {
			path = "/rest/api/content"
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, path+"/") {
			p, ok := f.pages[strings.TrimPrefix(r.URL.Path, path+"/")]
			if !ok {
				w.WriteHeader(404)
				fmt.Fprint(w, `{}`)
				return
			}
			_ = json.NewEncoder(w).Encode(p)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == path {
			if edition == "cloud" && r.URL.Query().Get("space-id") != "200" {
				t.Error("missing Cloud space filter")
			}
			if edition == "data-center" && r.URL.Query().Get("spaceKey") != "DOCS" {
				t.Error("missing Data Center space filter")
			}
			results := []page{}
			for _, p := range f.pages {
				if p.Title == r.URL.Query().Get("title") {
					results = append(results, p)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == path {
			var payload struct {
				Title    string `json:"title"`
				SpaceID  string `json:"spaceId"`
				ParentID string `json:"parentId"`
				Space    struct {
					Key string `json:"key"`
				} `json:"space"`
				Ancestors []struct {
					ID string `json:"id"`
				} `json:"ancestors"`
				Body struct {
					Value          string `json:"value"`
					Representation string `json:"representation"`
					Storage        struct {
						Value          string `json:"value"`
						Representation string `json:"representation"`
					} `json:"storage"`
				} `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			f.creates++
			p := page{ID: fmt.Sprint(400 + f.creates), Title: payload.Title, Status: "current", SpaceID: payload.SpaceID, ParentID: payload.ParentID, Space: payload.Space, Ancestors: payload.Ancestors}
			p.Body.Storage.Value = payload.Body.Value
			if edition == "data-center" {
				p.Body.Storage.Value = payload.Body.Storage.Value
			}
			if f.transformBody != nil {
				p.Body.Storage.Value = f.transformBody(p.Body.Storage.Value)
			}
			if edition == "cloud" && payload.Body.Representation != "storage" || edition == "data-center" && payload.Body.Storage.Representation != "storage" {
				t.Error("non-storage payload")
			}
			f.pages[p.ID] = p
			if f.failCreate {
				f.failCreate = false
				w.WriteHeader(503)
				fmt.Fprint(w, `{"message":"private source and secret"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(p)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(400)
	}))
	t.Cleanup(server.Close)
	s := validSettings()
	s.BaseURL = server.URL
	s.Edition = edition
	if edition == "data-center" {
		s.Username = ""
		s.Targets[0].SpaceID = ""
		s.Targets[0].SpaceKey = "DOCS"
	}
	c := NewClient(s, "test-token-12345")
	c.http.Transport = server.Client().Transport
	return f, c
}

func TestCreateRecoveryAndEditedPageProtection(t *testing.T) {
	for _, edition := range []string{"cloud", "data-center"} {
		t.Run(edition, func(t *testing.T) {
			f, c := newFakeConfluence(t, edition)
			f.failCreate = true
			body := `<p>Documentation job ID: job-1</p>` + codeStorage("flowchart LR\n A --> B\n", "mermaid", Settings{MermaidMacro: "mermaid"})
			_, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Project - Documentation - date", body, "")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 503 || strings.Contains(err.Error(), "private") {
				t.Fatalf("unexpected create failure: %v", err)
			}
			id, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Project - Documentation - date", body, "")
			if err != nil || f.creates != 1 {
				t.Fatalf("ambiguous create duplicated: %s %v creates=%d", id, err, f.creates)
			}
			if _, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Project - Documentation - date", body, id); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			p := f.pages[id]
			p.Body.Storage.Value += `<p>Human notes</p>`
			f.pages[id] = p
			f.mu.Unlock()
			if _, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Project - Documentation - date", body, id); err == nil {
				t.Fatal("human edits accepted")
			}
			if f.creates != 1 {
				t.Fatal("human page replaced")
			}
		})
	}
}

func TestDestinationAndIdentityMismatchesFailClosed(t *testing.T) {
	f, c := newFakeConfluence(t, "cloud")
	target := c.settings.Targets[0]
	target.SpaceID = "999"
	if _, err := c.EnsurePage(context.Background(), target, "300", "Title", "<p>Body</p>", ""); err == nil || f.creates != 0 {
		t.Fatal("cross-space parent accepted")
	}
	target = c.settings.Targets[0]
	if _, err := c.EnsurePage(context.Background(), target, "300", "Team docs", "<p>Job marker</p>", ""); err == nil || f.creates != 0 {
		t.Fatal("unrelated title collision accepted")
	}
}

func TestCloudSanitizedFootnotesCreateAndRecover(t *testing.T) {
	f, c := newFakeConfluence(t, "cloud")
	body, err := MarkdownStorage("Source[^s] and again[^s].\n\n[^s]: main.go:10.\n", false, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	f.transformBody = func(string) string { return cloudFootnoteStorage() }
	id, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Technical Paper", body, "")
	if err != nil {
		t.Fatal(err)
	}
	// Recover a remotely created page even if its mapping was never saved.
	recovered, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Technical Paper", body, "")
	if err != nil || recovered != id || f.creates != 1 {
		t.Fatalf("sanitized page duplicated or rejected: id=%s recovered=%s err=%v", id, recovered, err)
	}
	if _, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Technical Paper", body, id); err != nil {
		t.Fatal(err)
	}
}

func TestNarvaCloudCreateAndRecover(t *testing.T) {
	f, c := newFakeConfluence(t, "cloud")
	c.settings.MermaidApp, c.settings.MermaidExtensionKey = "narva", testNarvaKey
	body, err := MarkdownStorage("flowchart LR\n A --> B\n", true, c.settings)
	if err != nil {
		t.Fatal(err)
	}
	f.transformBody = func(body string) string {
		return strings.ReplaceAll(body, `<ac:adf-parameter key="guest-params">`, `<ac:adf-parameter key="embedded-macro-context"><ac:adf-parameter key="account-id">test-user</ac:adf-parameter></ac:adf-parameter><ac:adf-parameter key="guest-params">`)
	}
	f.failCreate = true
	if _, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Application Architecture", body, ""); err == nil {
		t.Fatal("expected lost create response")
	}
	id, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Application Architecture", body, "")
	if err != nil || f.creates != 1 {
		t.Fatalf("Narva recovery duplicated or rejected: %s %v", id, err)
	}
}

func TestManagedPageMismatchDiagnosticsAreSafe(t *testing.T) {
	for _, edition := range []string{"cloud", "data-center"} {
		for _, reason := range []string{"space_mismatch", "parent_mismatch", "title_mismatch", "body_mismatch"} {
			t.Run(edition+"/"+reason, func(t *testing.T) {
				f, c := newFakeConfluence(t, edition)
				body := `<p>Generated content</p>`
				id, err := c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Managed title", body, "")
				if err != nil {
					t.Fatal(err)
				}
				f.mu.Lock()
				p := f.pages[id]
				switch reason {
				case "space_mismatch":
					p.SpaceID = "999"
					p.Space.Key = "OTHER"
				case "parent_mismatch":
					p.ParentID = "999"
					if len(p.Ancestors) > 0 {
						p.Ancestors[len(p.Ancestors)-1].ID = "999"
					}
				case "title_mismatch":
					p.Title = "private-title-secret"
				case "body_mismatch":
					p.Body.Storage.Value = `<p>private-content-secret</p>`
				}
				f.pages[id] = p
				f.mu.Unlock()
				_, err = c.EnsurePage(context.Background(), c.settings.Targets[0], "300", "Managed title", body, id)
				var permanent *permanentError
				if !errors.As(err, &permanent) || !strings.Contains(err.Error(), "page_id="+id) || !strings.Contains(err.Error(), "reason="+reason) {
					t.Fatalf("missing permanent mismatch diagnostics: %v", err)
				}
				if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") || f.creates != 1 {
					t.Fatalf("unsafe diagnostics or replacement write: %v", err)
				}
				if status, _ := publicationResult(0, fmt.Errorf("Confluence technical page: %w", err)); status != "failed" {
					t.Fatal("wrapped permanent mismatch was not classified as failed")
				}
			})
		}
	}
}

func TestSafeErrorsRetryAfterAndRedirects(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"secret-token and source"}`)
			}))
			defer server.Close()
			s := validSettings()
			s.BaseURL = server.URL
			c := NewClient(s, "test-token-12345")
			_, err := c.readPage(context.Background(), "300")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.RetryAfter != 120*time.Second || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe error: %v", err)
			}
			want := "failed"
			if status == 429 || status == 503 {
				want = "retrying"
			}
			if got, _ := publicationResult(0, err); got != want {
				t.Fatalf("status=%s want=%s", got, want)
			}
		})
	}
	redirected := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer server.Close()
	s := validSettings()
	s.BaseURL = server.URL
	if _, err := NewClient(s, "test-token-12345").readPage(context.Background(), "300"); err == nil || redirected {
		t.Fatal("authenticated redirect followed")
	}
	if status, _ := publicationResult(9, errors.New("network failure")); status != "failed" {
		t.Fatal("retry cap ignored")
	}
}
