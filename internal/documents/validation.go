package documents

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gitlab-doc-platform/internal/evidence"
	doctemplates "gitlab-doc-platform/templates"
)

type ValidationReport struct {
	Status             string         `json:"status"`
	Checks             []string       `json:"checks"`
	Warnings           []string       `json:"warnings"`
	PromptOmittedItems map[string]int `json:"prompt_omitted_items"`
}

func plain(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "#", "\\#", "|", "\\|").Replace(text)
}

func renderPaper(s evidence.Snapshot, kind string, output paperResponse, included int) (string, error) {
	notes := newSourceNotes(s)
	facts := groupedFacts(s, kind, notes)
	known := map[string]evidence.Item{}
	for _, e := range s.Evidence {
		if relevant(e, kind) {
			known[e.ID] = e
		}
	}
	var b strings.Builder
	b.WriteString("Model summaries are inferred descriptions, not independently verified behavior.\n\n")
	if len(output.Summaries) == 0 {
		b.WriteString("No model summary supplied; the verified overview above is available.\n")
	}
	for _, claim := range output.Summaries {
		var cited []evidence.Item
		for _, id := range claim.EvidenceIDs {
			cited = append(cited, known[id])
		}
		fmt.Fprintf(&b, "- %s: %s %s\n", plain(claim.Component), plain(claim.Text), notes.cite(cited))
	}
	inferences := b.String()
	b.Reset()
	b.WriteString("Live deployment state, effective authorization, configuration defaults, and operational guarantees are not established by static declarations.\n\n")
	fmt.Fprintf(&b, "The model received %d selected evidence records; missing prompt details do not establish absence.\n", included)
	if len(s.Warnings) > 0 {
		fmt.Fprintf(&b, "\nAnalysis reported %d warnings; details are retained in evidence.json.\n", len(s.Warnings))
	}
	return doctemplates.Render(kind, doctemplates.Data{Project: plain(s.ProjectID), Commit: plain(s.CommitSHA), Facts: facts, Inferences: inferences, Unavailable: b.String(), Sources: notes.render()})
}

var mermaidNode = regexp.MustCompile(`^  (n[0-9]+|unavailable)\["[^"\r\n]*"\]$`)
var mermaidEdge = regexp.MustCompile(`^  (n[0-9]+) -->\|(imports|references)\| (n[0-9]+)$`)
var mermaidGroup = regexp.MustCompile(`^  subgraph (g[0-9]+)\["[^"\r\n]*"\]$`)

func ValidateArtifacts(a ArtifactSet) (ValidationReport, error) {
	if len(a.Diagrams) != 3 {
		return ValidationReport{}, errors.New("artifact set must contain exactly three supported diagrams")
	}
	report := ValidationReport{Status: "passed", Checks: []string{"required Markdown sections", "bounded structured output", "evidence citation membership and component scope", "sensitive-pattern screening", "deterministic Mermaid grammar and node references"}, Warnings: []string{"Model interpretation remains inferred; citation validation does not establish semantic entailment or detect every possible credential."}}
	for name, reference := range a.References {
		if name != "api-reference.md" && name != "configuration-reference.md" || RedactText(reference) != reference {
			return report, errors.New("invalid or sensitive reference artifact")
		}
	}
	for _, paper := range []string{a.TechnicalPaper, a.InfrastructurePaper} {
		if RedactText(paper) != paper {
			return report, errors.New("sensitive content detected in rendered document")
		}
		for _, section := range []string{"## Verified Facts", "## Inferred Information", "## Unavailable Information", "## Source Evidence"} {
			if strings.Count(paper, section+"\n") != 1 {
				return report, errors.New("document missing required sections")
			}
		}
	}
	for _, name := range []string{"application-architecture.mmd", "infrastructure-architecture.mmd", "dependency-diagram.mmd"} {
		text, ok := a.Diagrams[name]
		if !ok {
			return report, fmt.Errorf("required diagram missing: %s", name)
		}
		if RedactText(text) != text {
			return report, errors.New("sensitive content detected in diagram")
		}
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		if len(lines) < 2 || lines[0] != "flowchart LR" && lines[0] != "flowchart TB" {
			return report, errors.New("invalid Mermaid flowchart header")
		}
		nodes := map[string]bool{}
		groups := map[string]bool{}
		inGroup := false
		var edges [][2]string
		for _, line := range lines[1:] {
			line = "  " + strings.TrimSpace(line)
			if strings.HasPrefix(line, "  %% ") {
				continue
			}
			if match := mermaidGroup.FindStringSubmatch(line); match != nil {
				if inGroup || groups[match[1]] {
					return report, errors.New("nested or duplicate Mermaid group")
				}
				groups[match[1]], inGroup = true, true
				continue
			}
			if line == "  direction TB" && inGroup {
				continue
			}
			if line == "  end" && inGroup {
				inGroup = false
				continue
			}
			if match := mermaidNode.FindStringSubmatch(line); match != nil {
				if nodes[match[1]] {
					return report, errors.New("duplicate Mermaid node")
				}
				nodes[match[1]] = true
				continue
			}
			if match := mermaidEdge.FindStringSubmatch(line); match != nil {
				if inGroup {
					return report, errors.New("Mermaid edges must be outside groups")
				}
				edges = append(edges, [2]string{match[1], match[3]})
				continue
			}
			return report, errors.New("invalid deterministic Mermaid syntax")
		}
		if inGroup {
			return report, errors.New("unclosed Mermaid group")
		}
		for _, edge := range edges {
			if !nodes[edge[0]] || !nodes[edge[1]] {
				return report, errors.New("Mermaid edge references an unknown node")
			}
		}
	}
	return report, nil
}
