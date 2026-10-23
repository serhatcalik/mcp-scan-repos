package notion

import (
	"net/http"
	"regexp"
	"strings"
)

func requestOperation(method, path string) string {
	switch {
	case method == http.MethodPost && path == "/pages":
		return "create_page"
	case method == http.MethodGet && strings.HasPrefix(path, "/blocks/"):
		return "list_blocks"
	case method == http.MethodPatch && strings.HasPrefix(path, "/blocks/"):
		return "append_blocks"
	case method == http.MethodGet && strings.HasPrefix(path, "/pages/"):
		return "read_page"
	case method == http.MethodPatch && strings.HasPrefix(path, "/pages/"):
		return "update_page_title"
	default:
		return "other"
	}
}

func safeErrorCode(code string) string {
	switch code {
	case "invalid_json", "invalid_request_url", "invalid_request", "validation_error", "missing_version", "unauthorized", "restricted_resource", "object_not_found", "conflict_error", "rate_limited", "internal_server_error", "service_unavailable", "database_connection_unavailable", "gateway_timeout":
		return code
	default:
		return ""
	}
}

var bodyPath = regexp.MustCompile(`\bbody(?:\.[a-z_][a-z_0-9]*|\[[0-9]{1,6}\])+`)
var pathKey = regexp.MustCompile(`[a-z_][a-z_0-9]*`)

// Notion validation messages can echo source values. Retain only known schema paths.
func validationFields(message string) []string {
	allowed := map[string]bool{}
	for _, key := range strings.Fields("body children object id type parent page_id properties title paragraph heading_1 heading_2 heading_3 bulleted_list_item numbered_list_item quote to_do divider table table_row table_width has_column_header has_row_header cells rich_text text content link url annotations bold italic strikethrough underline code color language caption checked") {
		allowed[key] = true
	}
	var fields []string
	seen := map[string]bool{}
	for _, field := range bodyPath.FindAllString(message, 64) {
		if len(field) > 256 || seen[field] {
			continue
		}
		valid := true
		for _, key := range pathKey.FindAllString(field, -1) {
			if !allowed[key] {
				valid = false
				break
			}
		}
		if valid {
			fields = append(fields, field)
			seen[field] = true
			if len(fields) == 8 {
				break
			}
		}
	}
	return fields
}
