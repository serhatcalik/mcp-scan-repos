package confluence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validSettings() Settings {
	return Settings{BaseURL: "https://example.atlassian.net/wiki", Edition: "cloud", Username: "docs@example.com", Targets: []Target{{GroupID: "101", SpaceID: "200", ParentPageID: "300", Enabled: true}}}
}

func TestTargetsAcceptProviderScopedGroups(t *testing.T) {
	for _, test := range []struct {
		name   string
		groups []string
		valid  bool
	}{
		{"GitLab", []string{"101"}, true},
		{"GitHub", []string{"github:101"}, true},
		{"same numeric ID across providers", []string{"101", "github:101"}, true},
		{"duplicate GitHub owner", []string{"github:101", "github:101"}, false},
		{"missing owner", []string{"github:"}, false},
		{"owner name", []string{"github:example"}, false},
		{"unknown provider", []string{"other:101"}, false},
		{"nested prefix", []string{"github:github:101"}, false},
		{"path traversal", []string{"github:../101"}, false},
		{"whitespace", []string{"github:101 "}, false},
		{"oversized ID", []string{"github:" + strings.Repeat("1", 21)}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, edition := range []string{"cloud", "data-center"} {
				s := validSettings()
				target := s.Targets[0]
				s.Edition, s.Targets = edition, nil
				if edition == "data-center" {
					target.SpaceID, target.SpaceKey = "", "DOCS"
				}
				for _, group := range test.groups {
					target.GroupID = group
					s.Targets = append(s.Targets, target)
				}
				data, _ := json.Marshal(s)
				got, err := ParseSettings(data)
				if (err == nil) != test.valid {
					t.Fatalf("%s validation error=%v, want acceptance=%v", edition, err, test.valid)
				}
				if err == nil {
					for i, target := range got.Targets {
						if target.GroupID != test.groups[i] {
							t.Fatal("provider scope was lost")
						}
					}
				}
			}
		})
	}
	for _, field := range []string{"space", "parent"} {
		s := validSettings()
		s.Targets[0].GroupID = "github:101"
		if field == "space" {
			s.Targets[0].SpaceID = "github:200"
		} else {
			s.Targets[0].ParentPageID = "github:300"
		}
		data, _ := json.Marshal(s)
		if _, err := ParseSettings(data); err == nil {
			t.Fatalf("accepted provider prefix in Confluence %s ID", field)
		}
	}
}

func TestSettingsValidationAndDestinationIsolation(t *testing.T) {
	s := validSettings()
	data, _ := json.Marshal(s)
	parsed, err := ParseSettings(data)
	if err != nil {
		t.Fatal(err)
	}
	key := parsed.destination(parsed.Targets[0])
	if strings.Contains(key, parsed.BaseURL) || len(key) != 64 {
		t.Fatal("invalid destination key")
	}
	for _, mutate := range []func(*Settings){
		func(s *Settings) { s.BaseURL = "http://example.com" },
		func(s *Settings) { s.BaseURL = "https://user:secret@example.com" },
		func(s *Settings) { s.BaseURL += "?token=secret" },
		func(s *Settings) { s.Username = "" },
		func(s *Settings) { s.Edition = "server" },
		func(s *Settings) { s.Targets[0].SpaceID = "" },
		func(s *Settings) { s.Targets[0].ParentPageID = "../300" },
		func(s *Settings) { s.Targets = append(s.Targets, s.Targets[0]) },
		func(s *Settings) { s.MermaidMacro = "bad<macro" },
		func(s *Settings) { s.MermaidBodyParameter = "chart" },
	} {
		candidate := validSettings()
		mutate(&candidate)
		data, _ := json.Marshal(candidate)
		if _, err := ParseSettings(data); err == nil {
			t.Fatalf("invalid settings accepted: %s", data)
		}
	}
	if _, err := ParseSettings(append(data, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	if _, err := ParseSettings([]byte(`{"token":"secret"}`)); err == nil {
		t.Fatal("secret field accepted")
	}
	other := validSettings()
	other.BaseURL = "https://other.atlassian.net/wiki"
	if key == other.destination(other.Targets[0]) {
		t.Fatal("sites share a destination")
	}
	other = validSettings()
	other.Targets[0].SpaceID = "201"
	if key == other.destination(other.Targets[0]) {
		t.Fatal("spaces share a destination")
	}
	dc := validSettings()
	dc.Edition = "data-center"
	dc.Username = ""
	dc.Targets[0].SpaceID = ""
	dc.Targets[0].SpaceKey = "DOCS"
	data, _ = json.Marshal(dc)
	if _, err := ParseSettings(data); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFilesKeepsCredentialsSeparate(t *testing.T) {
	dir := t.TempDir()
	settingsPath, tokenPath := filepath.Join(dir, "config.json"), filepath.Join(dir, "token")
	data, _ := json.Marshal(validSettings())
	if err := os.WriteFile(settingsPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"test-token-12345", "bad token contains spaces", "short"} {
		if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		_, got, err := LoadFiles(settingsPath, tokenPath)
		if token == "test-token-12345" {
			if err != nil || got != token {
				t.Fatal("valid credentials rejected")
			}
		} else if err == nil || strings.Contains(err.Error(), token) {
			t.Fatal("invalid credential accepted or leaked")
		}
	}
}

func TestSnapshotTitleScopeIsSharedOnlyWithinSameSpace(t *testing.T) {
	s := validSettings()
	target := s.Targets[0]
	scope := s.titleScope(target)
	target.GroupID = "102"
	target.ParentPageID = "301"
	if s.titleScope(target) != scope {
		t.Fatal("same-space parents use separate title scopes")
	}
	target.SpaceID = "201"
	if s.titleScope(target) == scope {
		t.Fatal("different spaces share titles")
	}
	target = s.Targets[0]
	s.BaseURL = "https://other.atlassian.net/wiki"
	if s.titleScope(target) == scope {
		t.Fatal("different sites share titles")
	}
}

func TestConfigurationParseDiagnosticsDoNotExposeInput(t *testing.T) {
	for _, test := range []struct{ name, input, message string }{
		{"unknown field", `{"private-secret-field":"private-secret-value"}`, "unknown field"},
		{"malformed", `{"base_url":"private-secret-value`, "malformed or empty JSON"},
		{"empty", ``, "malformed or empty JSON"},
		{"type", `{"targets":[{"group_id":101}]}`, "wrong JSON value type"},
		{"trailing", `{} {"secret":"private-secret-value"}`, "trailing JSON content"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseSettings([]byte(test.input))
			if err == nil || !strings.Contains(err.Error(), test.message) || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("missing or unsafe diagnostics: %v", err)
			}
		})
	}
}
