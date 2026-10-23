package documents

import (
	"gitlab-doc-platform/internal/evidence"
	"regexp"
)

var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
	regexp.MustCompile(`\bgl(?:pat|rt|dt|cbt)-[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`(?i)\b(?:password|passwd|token|secret|api[_-]?key|authorization)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`),
	regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s/@]+:[^\s/@]+@`),
}

func RedactText(text string) string {
	for _, pattern := range sensitivePatterns {
		text = pattern.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}

// Copy before redaction so callers cannot accidentally reuse unsanitized slices.
func SanitizeSnapshot(s evidence.Snapshot) evidence.Snapshot {
	s.ProjectID, s.CommitSHA = RedactText(s.ProjectID), RedactText(s.CommitSHA)
	s.Language, s.Framework, s.Database = RedactText(s.Language), RedactText(s.Framework), RedactText(s.Database)
	s.Evidence = append([]evidence.Item(nil), s.Evidence...)
	for i := range s.Evidence {
		e := &s.Evidence[i]
		value := RedactText(e.Value)
		if value != e.Value {
			e.Confidence = evidence.Unavailable
			e.Verification = "redacted; original value withheld"
		}
		e.Value, e.SourceFile = value, RedactText(e.SourceFile)
		e.RelatedComponents = append([]string(nil), e.RelatedComponents...)
		for j := range e.RelatedComponents {
			e.RelatedComponents[j] = RedactText(e.RelatedComponents[j])
		}
	}
	s.Inventory = append([]evidence.File(nil), s.Inventory...)
	for i := range s.Inventory {
		s.Inventory[i].Path = RedactText(s.Inventory[i].Path)
	}
	s.Warnings = append([]string(nil), s.Warnings...)
	for i := range s.Warnings {
		s.Warnings[i] = RedactText(s.Warnings[i])
	}
	return s
}
