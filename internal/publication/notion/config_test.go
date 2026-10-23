package notion

import (
	"encoding/json"
	"strings"
	"testing"
)

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
			var targets []Target
			for _, group := range test.groups {
				targets = append(targets, Target{GroupID: group, ParentPageID: testParent, Enabled: true})
			}
			data, _ := json.Marshal(map[string]any{"targets": targets})
			got, err := ParseTargets(data)
			if (err == nil) != test.valid {
				t.Fatalf("validation error=%v, want acceptance=%v", err, test.valid)
			}
			if err == nil {
				for i, target := range got {
					if target.GroupID != test.groups[i] {
						t.Fatal("provider scope was lost")
					}
				}
			}
		})
	}
}
