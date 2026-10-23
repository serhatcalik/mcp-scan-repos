package evidence

import (
	"regexp"
	"strings"
)

var applicationVersionPattern = regexp.MustCompile(`^[vV]?[0-9]+(?:\.[0-9]+){0,3}(?:-[A-Za-z0-9][A-Za-z0-9.-]*)?(?:\+[A-Za-z0-9][A-Za-z0-9.-]*)?$`)

// Version declarations are labels, not executable build expressions or URLs.
func ValidApplicationVersion(value string) bool {
	return len(value) <= 64 && applicationVersionPattern.MatchString(value)
}

func GoApplicationVersionSource(source string) bool {
	if !strings.HasSuffix(source, ".go") || strings.HasSuffix(source, "_test.go") {
		return false
	}
	if !strings.Contains(source, "/") {
		return true
	}
	switch source {
	case "version/version.go", "internal/version/version.go", "internal/buildinfo/version.go":
		return true
	}
	parts := strings.Split(source, "/")
	return len(parts) == 3 && parts[0] == "cmd" && parts[1] != "" && parts[2] == "main.go"
}
