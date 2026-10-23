package confluence

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	settings      Settings
	token         string
	http          *http.Client
	beforeRequest func(context.Context) error
}

func NewClient(settings Settings, token string) *Client {
	return &Client{settings: settings, token: token, http: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type APIError struct {
	Status     int
	Operation  string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Confluence API returned HTTP %d; operation=%s", e.Status, e.Operation)
}
func (e *APIError) Retryable() bool {
	return e.Status == 429 || e.Status == 408 || e.Status == 409 || e.Status >= 500
}

type page struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	SpaceID  string `json:"spaceId"`
	ParentID string `json:"parentId"`
	Space    struct {
		Key string `json:"key"`
	} `json:"space"`
	Ancestors []struct {
		ID string `json:"id"`
	} `json:"ancestors"`
	Body struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
}

func (c *Client) request(ctx context.Context, method, path, operation string, payload, out any) error {
	var data []byte
	var err error
	if payload != nil {
		data, err = json.Marshal(payload)
		if err != nil || len(data) > 8<<20 {
			return errors.New("Confluence request exceeds limit")
		}
	}
	if c.beforeRequest != nil {
		if err := c.beforeRequest(ctx); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.settings.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Confluence request")
	}
	if c.settings.Username != "" {
		req.SetBasicAuth(c.settings.Username, c.token)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("Confluence request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Remote error bodies can echo secrets or generated content. Never retain them.
		delay := time.Duration(0)
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			delay = time.Duration(min(seconds, 86400)) * time.Second
		} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			delay = min(max(time.Until(date), 0), 24*time.Hour)
		}
		return &APIError{Status: resp.StatusCode, Operation: operation, RetryAfter: delay}
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return errors.New("Confluence response exceeds limit")
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return errors.New("invalid Confluence response")
	}
	return nil
}

func (c *Client) pagesPath() string {
	if c.settings.Edition == "cloud" {
		return "/api/v2/pages"
	}
	return "/rest/api/content"
}

func (c *Client) readPage(ctx context.Context, id string) (page, error) {
	var p page
	if !numericID.MatchString(id) {
		return p, errors.New("invalid Confluence page ID")
	}
	query := "?body-format=storage"
	if c.settings.Edition == "data-center" {
		query = "?expand=body.storage,space,ancestors"
	}
	err := c.request(ctx, http.MethodGet, c.pagesPath()+"/"+id+query, "read_page", nil, &p)
	if err == nil && (p.ID != id || p.Status != "current") {
		err = errors.New("Confluence page is unavailable or not current")
	}
	return p, err
}

func (c *Client) inSpace(p page, target Target) bool {
	if c.settings.Edition == "cloud" {
		return p.SpaceID == target.SpaceID
	}
	return p.Space.Key == target.SpaceKey
}

func (c *Client) PageURL(id string) string {
	return c.settings.BaseURL + "/pages/viewpage.action?pageId=" + url.QueryEscape(id)
}

// EnsurePage never updates existing content. Whole-page creates are recoverable
// by exact title, destination, and body, even when the create response was lost.
func (c *Client) EnsurePage(ctx context.Context, target Target, parent, title, body, mappedID string) (string, error) {
	verify := func(p page) (string, error) {
		actualParent := p.ParentID
		if c.settings.Edition == "data-center" && len(p.Ancestors) > 0 {
			actualParent = p.Ancestors[len(p.Ancestors)-1].ID
		}
		reason := ""
		switch {
		case !c.inSpace(p, target):
			reason = "space_mismatch"
		case actualParent != parent:
			reason = "parent_mismatch"
		case p.Title != title:
			reason = "title_mismatch"
		case !equalStorage(p.Body.Storage.Value, body):
			reason = "body_mismatch"
		}
		if reason != "" {
			// Retain identifiers and fixed reason codes, never remote titles or bodies.
			return "", &permanentError{fmt.Sprintf("managed Confluence page differs; page_id=%s; reason=%s; refusing to overwrite", p.ID, reason)}
		}
		return p.ID, nil
	}
	if mappedID != "" {
		p, err := c.readPage(ctx, mappedID)
		if err != nil {
			return "", err
		}
		return verify(p)
	}
	parentPage, err := c.readPage(ctx, parent)
	if err != nil {
		return "", err
	}
	if !c.inSpace(parentPage, target) {
		return "", &permanentError{"Confluence parent is outside configured space"}
	}
	q := url.Values{"title": {title}, "limit": {"100"}, "status": {"current"}}
	if c.settings.Edition == "cloud" {
		q.Set("space-id", target.SpaceID)
	} else {
		q.Set("spaceKey", target.SpaceKey)
		q.Set("type", "page")
	}
	var list struct {
		Results []page `json:"results"`
		Links   struct {
			Next string `json:"next"`
		} `json:"_links"`
	}
	if err := c.request(ctx, http.MethodGet, c.pagesPath()+"?"+q.Encode(), "find_page", nil, &list); err != nil {
		return "", err
	}
	if list.Links.Next != "" || len(list.Results) > 1 {
		return "", &permanentError{"ambiguous Confluence page title"}
	}
	if len(list.Results) == 1 {
		p, err := c.readPage(ctx, list.Results[0].ID)
		if err != nil {
			return "", err
		}
		return verify(p)
	}
	var payload any
	if c.settings.Edition == "cloud" {
		payload = map[string]any{"spaceId": target.SpaceID, "parentId": parent, "status": "current", "title": title,
			"body": map[string]string{"representation": "storage", "value": body}}
	} else {
		payload = map[string]any{"type": "page", "title": title, "space": map[string]string{"key": target.SpaceKey},
			"ancestors": []map[string]string{{"id": parent}}, "body": map[string]any{"storage": map[string]string{"representation": "storage", "value": body}}}
	}
	var created page
	if err := c.request(ctx, http.MethodPost, c.pagesPath(), "create_page", payload, &created); err != nil {
		return "", err
	}
	if !numericID.MatchString(created.ID) {
		return "", errors.New("Confluence create response omitted page ID")
	}
	p, err := c.readPage(ctx, created.ID)
	if err != nil {
		return "", err
	}
	return verify(p)
}

var footnoteID = regexp.MustCompile(`^fn:[1-9][0-9]*$`)
var footnoteRefID = regexp.MustCompile(`^fnref[0-9]*:[1-9][0-9]*$`)

// Cloud strips Goldmark's fragment navigation attributes from footnotes.
// Normalize only those generated attributes, not ordinary links or citation text.
func footnoteMetadata(element xml.StartElement, attr xml.Attr) bool {
	if element.Name.Space != "" || attr.Name.Space != "" {
		return false
	}
	class := ""
	for _, a := range element.Attr {
		if a.Name.Space == "" && a.Name.Local == "class" {
			class = a.Value
		}
	}
	switch element.Name.Local {
	case "sup":
		return attr.Name.Local == "id" && footnoteRefID.MatchString(attr.Value)
	case "li":
		return attr.Name.Local == "id" && footnoteID.MatchString(attr.Value)
	case "div":
		return class == "footnotes" && attr.Name.Local == "role" && attr.Value == "doc-endnotes"
	case "a":
		if class == "footnote-ref" {
			return attr.Name.Local == "role" && attr.Value == "doc-noteref" ||
				attr.Name.Local == "href" && strings.HasPrefix(attr.Value, "#") && footnoteID.MatchString(strings.TrimPrefix(attr.Value, "#"))
		}
		if class == "footnote-backref" {
			return attr.Name.Local == "role" && attr.Value == "doc-backlink" ||
				attr.Name.Local == "href" && strings.HasPrefix(attr.Value, "#") && footnoteRefID.MatchString(strings.TrimPrefix(attr.Value, "#"))
		}
	}
	return false
}

// Ignore generated macro metadata, stripped footnote navigation, and
// inter-element whitespace. Content and external links remain significant.
func equalStorage(a, b string) bool {
	canonical := func(s string) (string, error) {
		if len(s) > 4<<20 {
			return "", errors.New("storage exceeds limit")
		}
		d := xml.NewDecoder(strings.NewReader(`<root xmlns:ac="http://atlassian.com/content" xmlns:ri="http://atlassian.com/resource/identifier">` + s + `</root>`))
		// Cloud serializes the numeric HTML non-breaking-space entity as &nbsp;.
		// Decode only this observed entity; do not unescape the entire markup.
		d.Entity = map[string]string{"nbsp": "\u00a0"}
		var tokens []any
		var chars strings.Builder
		preserve := 0
		depth, count := 0, 0
		preservesText := func(name string) bool {
			switch name {
			case "plain-text-body", "pre", "p", "td", "th", "h1", "h2", "h3", "h4", "h5", "h6":
				return true
			}
			return false
		}
		flush := func() {
			value := chars.String()
			if value != "" && (preserve > 0 || strings.TrimSpace(value) != "") {
				tokens = append(tokens, []string{"text", value})
			}
			chars.Reset()
		}
		for {
			token, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			count++
			if count > 100000 {
				return "", errors.New("storage exceeds structure limit")
			}
			switch v := token.(type) {
			case xml.StartElement:
				if v.Name.Space == acNamespace && v.Name.Local == "adf-extension" {
					flush()
					value, err := canonicalADF(d, v, &count, depth+1)
					if err != nil {
						return "", err
					}
					tokens = append(tokens, []string{"adf", value})
					continue
				}
				depth++
				if depth > 100 {
					return "", errors.New("storage exceeds nesting limit")
				}
				flush()
				if preservesText(v.Name.Local) {
					preserve++
				}
				attrs := []string{}
				for _, attr := range v.Attr {
					if attr.Name.Space == "xmlns" || attr.Name.Local == "xmlns" || attr.Name.Space == "http://atlassian.com/content" && (attr.Name.Local == "macro-id" || attr.Name.Local == "schema-version") || footnoteMetadata(v, attr) {
						continue
					}
					attrs = append(attrs, attr.Name.Space+":"+attr.Name.Local+"="+attr.Value)
				}
				sort.Strings(attrs)
				tokens = append(tokens, []any{"start", v.Name.Space, v.Name.Local, attrs})
			case xml.EndElement:
				depth--
				flush()
				if preservesText(v.Name.Local) {
					preserve--
				}
				tokens = append(tokens, []string{"end", v.Name.Space, v.Name.Local})
			case xml.CharData:
				chars.Write(v)
			case xml.Comment:
				return "", errors.New("unexpected storage comment")
			case xml.Directive, xml.ProcInst:
				return "", errors.New("unexpected storage directive")
			}
		}
		data, err := json.Marshal(tokens)
		return string(data), err
	}
	x, err := canonical(a)
	if err != nil {
		return false
	}
	y, err := canonical(b)
	return err == nil && x == y
}
