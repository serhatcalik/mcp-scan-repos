package documents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"gitlab-doc-platform/internal/ai"
	"gitlab-doc-platform/internal/evidence"
)

var paperSchema = makePaperSchema()

func makePaperSchema() json.RawMessage {
	claim := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"component", "text", "evidence_ids"}, "properties": map[string]any{
		"component":    map[string]any{"type": "string", "maxLength": 200},
		"text":         map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
		"evidence_ids": map[string]any{"type": "array", "minItems": 1, "maxItems": 3, "items": map[string]any{"type": "string"}},
	}}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"summaries", "recommendations"}, "properties": map[string]any{
		"summaries":       map[string]any{"type": "array", "maxItems": 6, "items": claim},
		"recommendations": map[string]any{"type": "array", "maxItems": 3, "items": claim},
	}}
	data, _ := json.Marshal(schema)
	return data
}

type interpretation struct {
	Component   string   `json:"component"`
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type paperResponse struct {
	Summaries       []interpretation `json:"summaries"`
	Recommendations []interpretation `json:"recommendations"`
}

type promptRecord struct {
	ID         string              `json:"id"`
	Component  string              `json:"component"`
	Category   string              `json:"category,omitempty"`
	Type       string              `json:"type"`
	Value      string              `json:"value"`
	Source     string              `json:"source,omitempty"`
	Confidence evidence.Confidence `json:"confidence"`
}

func relevant(e evidence.Item, kind string) bool {
	if kind == "infrastructure" {
		return e.Category == "infrastructure" || e.Category == "configuration" || observedTelemetry(e)
	}
	switch e.Category {
	case "application", "api", "data", "dependency", "security", "configuration", "observability":
		return true
	}
	return false
}

func component(e evidence.Item) string {
	if e.Category == "api" && e.Type == "route" && path.Dir(e.SourceFile) == "." && (len(e.RelatedComponents) == 0 || e.RelatedComponents[0] == ".") {
		_, title := apiGroup(e)
		return title
	}
	if len(e.RelatedComponents) > 0 && e.RelatedComponents[0] != "" {
		return e.RelatedComponents[0]
	}
	if dir := path.Dir(e.SourceFile); dir != "." {
		return dir
	}
	return "repository"
}

func priority(e evidence.Item) int {
	switch e.Type {
	case "route", "resource", "framework", "go_module", "java_module":
		return 0
	case "model", "controller", "struct", "middleware":
		return 1
	case "go_import":
		return 8
	case "go_checksum_module":
		return 9
	}
	return 3
}

func source(e evidence.Item) string {
	if e.LineStart > 0 {
		return fmt.Sprintf("%s:%d", e.SourceFile, e.LineStart)
	}
	return e.SourceFile
}

func buildPrompt(s evidence.Snapshot, kind string, options Options) (ai.Request, []evidence.Item, error) {
	header := "Describe the " + kind + " paper's components using concrete declarations, not advice or generic restatements. Return up to six brief summaries; leave recommendations empty. Each text must be at most 200 characters with one to three citations. Never infer active deployment from Compose or CI files, service-to-image associations, enabled integrations from imports, credential policies, or required configuration from key names. Cite only the IDs supporting the exact names mentioned. Omitted details are unavailable, not absent. Use supplied component names. Response schema:\n" + string(paperSchema) + "\nRepository evidence JSON:\n"
	budget := options.ContextLength - options.MaxTokens - 1024
	if budget <= len(guardedSystemPrompt())+len(header)+2 {
		return ai.Request{}, nil, errors.New("context budget is too small for the structured prompt")
	}
	groups := map[string][]evidence.Item{}
	for _, e := range s.Evidence {
		if !relevant(e, kind) || !promptSignal(e, kind) || e.ID == "" || e.SourceFile == "" || len(e.Value) > 1024 || len(e.SourceFile) > 512 || len(component(e)) > 200 || e.Confidence != evidence.Verified && e.Confidence != evidence.Inferred {
			continue
		}
		groups[component(e)] = append(groups[component(e)], e)
	}
	keys := make([]string, 0, len(groups))
	for key, items := range groups {
		sort.Slice(items, func(i, j int) bool {
			if promptPriority(items[i], kind) != promptPriority(items[j], kind) {
				return promptPriority(items[i], kind) < promptPriority(items[j], kind)
			}
			return items[i].ID < items[j].ID
		})
		groups[key] = items
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if promptPriority(groups[keys[i]][0], kind) != promptPriority(groups[keys[j]][0], kind) {
			return promptPriority(groups[keys[i]][0], kind) < promptPriority(groups[keys[j]][0], kind)
		}
		return keys[i] < keys[j]
	})
	var selected []evidence.Item
	records := []promptRecord{}
	seen := map[string]bool{}
	seenFacts := map[string]bool{}
	positions := map[string]int{}
	for tier := 0; tier <= 10; tier++ {
		for {
			remaining := false
			for _, key := range keys {
				pos := positions[key]
				if pos >= len(groups[key]) {
					continue
				}
				if promptPriority(groups[key][pos], kind) > tier {
					continue
				}
				remaining = true
				positions[key]++
				e := groups[key][pos]
				if seen[e.ID] {
					continue
				}
				seen[e.ID] = true
				identity, _ := json.Marshal([]string{key, e.Type, e.Value, string(e.Confidence)})
				if seenFacts[string(identity)] {
					continue
				}
				seenFacts[string(identity)] = true
				trial := append(records, promptRecord{ID: e.ID, Component: key, Type: e.Type, Value: e.Value, Confidence: e.Confidence})
				data, err := json.Marshal(trial)
				if err != nil {
					return ai.Request{}, nil, err
				}
				// UTF-8 byte accounting is deliberately conservative; no tokenizer is assumed.
				if len(guardedSystemPrompt())+len(header)+len(data) <= budget {
					records = trial
					selected = append(selected, e)
				}
			}
			if !remaining {
				break
			}
		}
	}
	data, err := json.Marshal(records)
	if err != nil {
		return ai.Request{}, nil, err
	}
	return ai.Request{SystemPrompt: guardedSystemPrompt(), UserPrompt: header + string(data), Temperature: options.Temperature, MaxTokens: options.MaxTokens, ContextLength: options.ContextLength, JSONSchema: scopedSchema(selected)}, selected, nil
}

func promptSignal(e evidence.Item, kind string) bool {
	if testEvidence(e) || e.Type == "model_field" || e.Type == "go_checksum_module" || e.Type == "go_dependency" || e.Type == "go_package" || e.Type == "handler" || e.Type == "java_import" {
		return false
	}
	if kind == "technical" && workflowFile(e.SourceFile) {
		return false
	}
	if e.Type == "go_import" && !strings.Contains(strings.SplitN(e.Value, "/", 2)[0], ".") {
		return false
	}
	return true
}

func promptPriority(e evidence.Item, kind string) int {
	if kind == "infrastructure" {
		if e.Category == "infrastructure" && !workflowFile(e.SourceFile) {
			return 0
		}
		if e.Category == "infrastructure" || observedTelemetry(e) {
			return 1
		}
		return 4
	}
	if e.Type == "framework" || e.Type == "go_module" || e.Type == "java_module" || e.Type == "database_client" || e.Type == "orm" || e.Type == "route" || e.Type == "model" || e.Type == "controller" {
		return 0
	}
	return priority(e)
}

var namedSetting = regexp.MustCompile(`\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b`)

func scopedSchema(selected []evidence.Item) json.RawMessage {
	if len(selected) == 0 {
		return paperSchema
	}
	var schema map[string]any
	_ = json.Unmarshal(paperSchema, &schema)
	groups := map[string][]string{}
	for _, e := range selected {
		name := component(e)
		groups[name] = append(groups[name], e.ID)
	}
	components := make([]string, 0, len(groups))
	for name := range groups {
		components = append(components, name)
	}
	sort.Strings(components)
	variants := make([]any, 0, len(components))
	for _, name := range components {
		ids := groups[name]
		sort.Strings(ids)
		var base map[string]any
		_ = json.Unmarshal(paperSchema, &base)
		claim := base["properties"].(map[string]any)["summaries"].(map[string]any)["items"].(map[string]any)
		properties := claim["properties"].(map[string]any)
		properties["component"].(map[string]any)["enum"] = []string{name}
		properties["evidence_ids"].(map[string]any)["items"].(map[string]any)["enum"] = ids
		variants = append(variants, claim)
	}
	properties := schema["properties"].(map[string]any)
	for _, name := range []string{"summaries", "recommendations"} {
		// Bind each component to its own IDs instead of independent global enums.
		properties[name].(map[string]any)["items"] = map[string]any{"anyOf": variants}
	}
	data, _ := json.Marshal(schema)
	return data
}

func validateResponse(text string, selected []evidence.Item) (paperResponse, error) {
	if len(text) > 128<<10 {
		return paperResponse{}, errors.New("structured model output exceeds size limit")
	}
	var output paperResponse
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return output, errors.New("model output must match the structured paper schema")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return output, errors.New("model output contains trailing data")
	}
	if output.Summaries == nil || output.Recommendations == nil || len(output.Summaries) > 6 || len(output.Recommendations) > 3 {
		return output, errors.New("model output is missing required arrays or exceeds section limits")
	}
	known := map[string]evidence.Item{}
	for _, e := range selected {
		known[e.ID] = e
	}
	for _, claim := range append(append([]interpretation{}, output.Summaries...), output.Recommendations...) {
		if strings.TrimSpace(claim.Text) == "" || len(claim.Text) > 200 || len(claim.EvidenceIDs) < 1 || len(claim.EvidenceIDs) > 3 || len(claim.Component) > 200 {
			return output, errors.New("model interpretation is empty, too large, or lacks evidence references")
		}
		if RedactText(claim.Text) != claim.Text {
			return output, errors.New("sensitive content detected in model output")
		}
		var citedValues strings.Builder
		for _, id := range claim.EvidenceIDs {
			e, ok := known[id]
			if !ok {
				return output, errors.New("model interpretation cites evidence not supplied in this paper's prompt")
			}
			if component(e) != claim.Component {
				return output, errors.New("model interpretation cites evidence belonging to a different component")
			}
			citedValues.WriteString(e.Value + "\n")
		}
		settings := map[string]bool{}
		for _, setting := range namedSetting.FindAllString(citedValues.String(), -1) {
			settings[setting] = true
		}
		for _, setting := range namedSetting.FindAllString(claim.Text, -1) {
			if !settings[setting] {
				return output, errors.New("model interpretation names a setting not present in its cited evidence")
			}
		}
	}
	return output, nil
}

func (g Generator) generatePaper(ctx context.Context, s evidence.Snapshot, kind string, jobID string) (string, int, int, error) {
	request, selected, err := buildPrompt(s, kind, g.options)
	if err != nil {
		return "", 0, 0, err
	}
	request.TraceID = jobID + ":" + kind
	output := paperResponse{}
	tokens := 0
	if len(selected) > 0 {
		for attempt := 0; attempt < 2; attempt++ {
			if err := ctx.Err(); err != nil {
				return "", 0, 0, err
			}
			response, err := g.provider.Generate(ctx, request)
			if err != nil {
				return "", 0, 0, err
			}
			tokens += response.GeneratedTokens
			output, err = validateResponse(response.Text, selected)
			if err == nil {
				break
			}
			if attempt == 1 {
				return "", 0, 0, fmt.Errorf("structured output invalid after two attempts: %w", err)
			}
			// Retry the original evidence without echoing untrusted model output.
			request.Temperature = 0
			request.TraceID += ":validation-retry"
		}
	}
	paper, err := renderPaper(s, kind, output, len(selected))
	return paper, len(selected), tokens, err
}
