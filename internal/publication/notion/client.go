package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiVersion = "2022-06-28"

type APIError struct {
	Status     int
	RetryAfter time.Duration
	Operation  string
	Code       string
	Fields     []string
}

func (e *APIError) Error() string {
	message := fmt.Sprintf("Notion API returned HTTP %d", e.Status)
	if e.Operation != "" {
		message += "; operation=" + e.Operation
	}
	if e.Code != "" {
		message += "; code=" + e.Code
	}
	if len(e.Fields) > 0 {
		message += "; fields=" + strings.Join(e.Fields, ",")
	}
	return message
}
func (e *APIError) Retryable() bool { return e.Status == 429 || e.Status >= 500 }

type Client struct {
	token         string
	endpoint      string
	http          *http.Client
	beforeRequest func(context.Context) error
}

func NewClient(token string) *Client {
	return &Client{token: token, endpoint: "https://api.notion.com/v1", http: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// request never returns response bodies or transport errors that could contain secrets.
// Writes are reconciled on the next queue attempt instead of being blindly replayed.
func (c *Client) request(ctx context.Context, method, path string, body any, out any) error {
	if c.beforeRequest != nil {
		if err := c.beforeRequest(ctx); err != nil {
			return err
		}
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("invalid Notion request")
		}
	}
	if len(data) > 450<<10 {
		return errors.New("Notion request exceeds size limit")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Notion request URL")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", apiVersion)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Notion transport failed; remote write outcome may be unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		delay := 60 * time.Second
		if seconds, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 {
			// Preserve long server cooldowns without overflowing time.Duration.
			if seconds > int64((1<<63-1)/time.Second) {
				seconds = int64((1<<63 - 1) / time.Second)
			}
			delay = time.Duration(seconds) * time.Second
		}
		failure := &APIError{Status: resp.StatusCode, RetryAfter: delay, Operation: requestOperation(method, path)}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		if err == nil {
			var response struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(data, &response) == nil {
				failure.Code = safeErrorCode(response.Code)
				failure.Fields = validationFields(response.Message)
			}
		}
		return failure
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return errors.New("invalid or oversized Notion response")
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return errors.New("invalid Notion response JSON")
	}
	return nil
}

type richText struct {
	Type string `json:"type,omitempty"`
	Text struct {
		Content string    `json:"content"`
		Link    *textLink `json:"link,omitempty"`
	} `json:"text"`
	Annotations annotations `json:"annotations,omitempty"`
}
type textLink struct {
	URL string `json:"url"`
}
type annotations struct {
	Bold          bool   `json:"bold,omitempty"`
	Italic        bool   `json:"italic,omitempty"`
	Strikethrough bool   `json:"strikethrough,omitempty"`
	Underline     bool   `json:"underline,omitempty"`
	Code          bool   `json:"code,omitempty"`
	Color         string `json:"color,omitempty"`
}
type blockContent struct {
	RichText []richText   `json:"rich_text,omitempty"`
	Language string       `json:"language,omitempty"`
	Title    string       `json:"title,omitempty"`
	Children []block      `json:"children,omitempty"`
	Cells    [][]richText `json:"cells,omitempty"`
	Checked  bool         `json:"checked,omitempty"`
	Color    string       `json:"color,omitempty"`
}
type tableContent struct {
	TableWidth      int     `json:"table_width"`
	HasColumnHeader bool    `json:"has_column_header"`
	HasRowHeader    bool    `json:"has_row_header"`
	Children        []block `json:"children,omitempty"`
}
type block struct {
	Object      string        `json:"object,omitempty"`
	ID          string        `json:"id,omitempty"`
	Type        string        `json:"type"`
	Paragraph   *blockContent `json:"paragraph,omitempty"`
	Heading1    *blockContent `json:"heading_1,omitempty"`
	Heading2    *blockContent `json:"heading_2,omitempty"`
	Heading3    *blockContent `json:"heading_3,omitempty"`
	Bullet      *blockContent `json:"bulleted_list_item,omitempty"`
	Numbered    *blockContent `json:"numbered_list_item,omitempty"`
	Quote       *blockContent `json:"quote,omitempty"`
	ToDo        *blockContent `json:"to_do,omitempty"`
	Divider     *struct{}     `json:"divider,omitempty"`
	Table       *tableContent `json:"table,omitempty"`
	TableRow    *blockContent `json:"table_row,omitempty"`
	HasChildren bool          `json:"has_children,omitempty"`
	Code        *blockContent `json:"code,omitempty"`
	ChildPage   *blockContent `json:"child_page,omitempty"`
}

func (b block) content() *blockContent {
	switch b.Type {
	case "paragraph":
		return b.Paragraph
	case "heading_1":
		return b.Heading1
	case "heading_2":
		return b.Heading2
	case "heading_3":
		return b.Heading3
	case "bulleted_list_item":
		return b.Bullet
	case "numbered_list_item":
		return b.Numbered
	case "quote":
		return b.Quote
	case "to_do":
		return b.ToDo
	case "table_row":
		return b.TableRow
	case "code":
		return b.Code
	case "child_page":
		return b.ChildPage
	}
	return nil
}
func (b block) text() string {
	content := b.content()
	if content == nil {
		return ""
	}
	var text strings.Builder
	for _, r := range content.RichText {
		text.WriteString(r.Text.Content)
	}
	return text.String()
}

func (c *Client) children(ctx context.Context, id string) ([]block, error) {
	if !pageIDPattern.MatchString(id) {
		return nil, errors.New("invalid Notion page ID")
	}
	var result []block
	cursor := ""
	seen := map[string]bool{}
	for {
		path := "/blocks/" + id + "/children?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Results    []block `json:"results"`
			HasMore    bool    `json:"has_more"`
			NextCursor string  `json:"next_cursor"`
		}
		if err := c.request(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		if page.Results == nil {
			return nil, errors.New("Notion response is missing block results")
		}
		result = append(result, page.Results...)
		if len(result) > 20000 {
			return nil, errors.New("Notion page exceeds block inventory limit")
		}
		if !page.HasMore {
			return result, nil
		}
		if page.NextCursor == "" || seen[page.NextCursor] || len(seen) >= 200 {
			return nil, errors.New("invalid Notion pagination")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

// EnsurePage reconciles by a deterministic identity-bearing title after uncertain creates.
func (c *Client) EnsurePage(ctx context.Context, parent, title string) (string, error) {
	return c.ensurePage(ctx, parent, title, "", "")
}

// Identity is created atomically with the page, so equal readable titles remain safe.
func (c *Client) ensurePage(ctx context.Context, parent, title, identity, legacyTitle string) (string, error) {
	children, err := c.children(ctx, parent)
	if err != nil {
		return "", err
	}
	found := ""
	for _, child := range children {
		if child.Type == "child_page" && child.ChildPage != nil && (child.ChildPage.Title == title || legacyTitle != "" && child.ChildPage.Title == legacyTitle) {
			if identity != "" && child.ChildPage.Title != legacyTitle {
				contents, err := c.children(ctx, child.ID)
				if err != nil {
					return "", err
				}
				if len(contents) == 0 || contents[0].Type != "paragraph" || contents[0].text() != identity {
					continue
				}
			}
			if found != "" {
				return "", errors.New("duplicate managed Notion pages; operator reconciliation required")
			}
			found = child.ID
		}
	}
	if found != "" {
		if !pageIDPattern.MatchString(found) {
			return "", errors.New("invalid Notion page ID in response")
		}
		return normalizeID(found), nil
	}
	var page struct {
		ID string `json:"id"`
	}
	body := map[string]any{
		"parent":     map[string]string{"type": "page_id", "page_id": parent},
		"properties": map[string]any{"title": map[string]any{"type": "title", "title": []richText{textItem(title)}}},
	}
	if identity != "" {
		body["children"] = []block{textBlock("paragraph", identity)}
	}
	err = c.request(ctx, http.MethodPost, "/pages", body, &page)
	if err != nil {
		return "", err
	}
	if !pageIDPattern.MatchString(page.ID) {
		return "", errors.New("invalid Notion page ID in response")
	}
	return normalizeID(page.ID), nil
}

// Only replace a known legacy auto-generated title, never an operator's custom title.
func (c *Client) renameLegacyPage(ctx context.Context, id, legacy, desired string) error {
	if legacy == "" || legacy == desired {
		return nil
	}
	if !pageIDPattern.MatchString(id) {
		return errors.New("invalid Notion page ID")
	}
	var page struct {
		Properties map[string]struct {
			Type  string     `json:"type"`
			Title []richText `json:"title"`
		} `json:"properties"`
	}
	if err := c.request(ctx, http.MethodGet, "/pages/"+id, nil, &page); err != nil {
		return err
	}
	for key, property := range page.Properties {
		if property.Type != "title" && key != "title" {
			continue
		}
		var title strings.Builder
		for _, item := range property.Title {
			title.WriteString(item.Text.Content)
		}
		if title.String() != legacy {
			return nil
		}
		return c.request(ctx, http.MethodPatch, "/pages/"+id, map[string]any{"properties": map[string]any{key: map[string]any{"title": []richText{textItem(desired)}}}}, nil)
	}
	return errors.New("Notion page response is missing its title")
}

func textItem(text string) richText {
	var item richText
	item.Type = "text"
	item.Text.Content = text
	return item
}

func textBlock(kind, text string) block {
	content := &blockContent{RichText: []richText{textItem(text)}}
	b := block{Object: "block", Type: kind}
	switch kind {
	case "heading_1":
		b.Heading1 = content
	case "heading_2":
		b.Heading2 = content
	case "heading_3":
		b.Heading3 = content
	case "bulleted_list_item":
		b.Bullet = content
	case "numbered_list_item":
		b.Numbered = content
	case "quote":
		b.Quote = content
	case "to_do":
		b.ToDo = content
	case "code":
		content.Language = "plain text"
		b.Code = content
	default:
		b.Type = "paragraph"
		b.Paragraph = content
	}
	return b
}

func (c *Client) EnsureContent(ctx context.Context, page string, desired []block) error {
	actual, err := c.children(ctx, page)
	if err != nil {
		return err
	}
	if len(actual) > len(desired) {
		return errors.New("managed Notion content changed; refusing to overwrite")
	}
	for i, b := range actual {
		matches, err := c.matchBlock(ctx, b, desired[i])
		if err != nil {
			return err
		}
		if !matches {
			return errors.New("managed Notion content changed; refusing to overwrite")
		}
	}
	for offset := len(actual); offset < len(desired); {
		end, nested := offset, 0
		for end < len(desired) && end-offset < 20 {
			trial := desired[offset : end+1]
			data, err := json.Marshal(map[string]any{"children": trial})
			if err != nil {
				return errors.New("invalid Notion block batch")
			}
			count := blockCount(desired[end])
			if len(data) > 450<<10 || nested+count > 1000 {
				break
			}
			nested += count
			end++
		}
		if end == offset {
			return errors.New("Notion block exceeds request size limit")
		}
		if err := c.request(ctx, http.MethodPatch, "/blocks/"+page+"/children", map[string]any{"children": desired[offset:end]}, nil); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

func blockCount(b block) int {
	count := 1
	var children []block
	if b.Table != nil {
		children = b.Table.Children
	} else if b.content() != nil {
		children = b.content().Children
	}
	for _, child := range children {
		count += blockCount(child)
	}
	return count
}

func (c *Client) matchBlock(ctx context.Context, actual, desired block) (bool, error) {
	if actual.Type != desired.Type {
		return false, nil
	}
	var oldChildren, newChildren []block
	switch actual.Type {
	case "divider":
		return actual.Divider != nil && desired.Divider != nil, nil
	case "table":
		if actual.Table == nil || desired.Table == nil || actual.Table.TableWidth != desired.Table.TableWidth || actual.Table.HasColumnHeader != desired.Table.HasColumnHeader || actual.Table.HasRowHeader != desired.Table.HasRowHeader {
			return false, nil
		}
		oldChildren, newChildren = actual.Table.Children, desired.Table.Children
	default:
		old, next := actual.content(), desired.content()
		if old == nil || next == nil || !sameRichText(old.RichText, next.RichText) || old.Language != next.Language || old.Checked != next.Checked || normalColor(old.Color) != normalColor(next.Color) || len(old.Cells) != len(next.Cells) {
			return false, nil
		}
		for i := range old.Cells {
			if !sameRichText(old.Cells[i], next.Cells[i]) {
				return false, nil
			}
		}
		oldChildren, newChildren = old.Children, next.Children
	}
	if len(oldChildren) == 0 && (actual.HasChildren || len(newChildren) > 0) {
		var err error
		oldChildren, err = c.children(ctx, actual.ID)
		if err != nil {
			return false, err
		}
	}
	if len(oldChildren) != len(newChildren) {
		return false, nil
	}
	for i, child := range oldChildren {
		matches, err := c.matchBlock(ctx, child, newChildren[i])
		if err != nil || !matches {
			return false, err
		}
	}
	return true, nil
}

func PageURL(id string) string { return "https://www.notion.so/" + normalizeID(id) }
