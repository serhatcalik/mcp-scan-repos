package main

import (
	"strings"
	"testing"
)

func TestUpdateTagPreservesEverythingExceptImageTag(t *testing.T) {
	for _, old := range []string{`"latest"`, `'latest'`, `latest`} {
		for _, newline := range []string{"\n", "\r\n"} {
			original := "# Deployment\nimage:\n  repository: nexus.example/project\n  tag: " + old + " # deployed image\n  pullPolicy: Always\n\nollama:\n  image:\n    tag: keep-me\n"
			original = strings.ReplaceAll(original, "\n", newline)
			updated, err := updateTag([]byte(original), "0123456789abcdef")
			if err != nil {
				t.Fatalf("old=%q newline=%q: %v", old, newline, err)
			}
			want := strings.Replace(original, "tag: "+old, `tag: "0123456789abcdef"`, 1)
			if string(updated) != want {
				t.Fatalf("unexpected values change:\ngot: %s\nwant: %s", updated, want)
			}
		}
	}
}

func TestUpdateTagRejectsInvalidInput(t *testing.T) {
	for _, tag := range []string{"", "bad/tag", "bad tag", strings.Repeat("a", 129)} {
		if _, err := updateTag([]byte("image:\n  tag: latest\n"), tag); err == nil {
			t.Errorf("accepted invalid tag %q", tag)
		}
	}
	for _, data := range []string{"image: {}\n", "image:\n  tag: [latest]\n", "image:\n  tag: latest\n---\nother: value\n"} {
		if _, err := updateTag([]byte(data), "abc123"); err == nil {
			t.Errorf("accepted unsupported values: %s", data)
		}
	}
}
