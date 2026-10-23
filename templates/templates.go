package templates

import (
	"bytes"
	"embed"
	"errors"
	"text/template"
)

//go:embed technical.md infrastructure.md
var files embed.FS

type Data struct {
	Project     string
	Commit      string
	Facts       string
	Inferences  string
	Unavailable string
	Sources     string
}

func Render(kind string, data Data) (string, error) {
	if kind != "technical" && kind != "infrastructure" {
		return "", errors.New("unknown documentation template")
	}
	source, err := files.ReadFile(kind + ".md")
	if err != nil {
		return "", err
	}
	t, err := template.New(kind).Option("missingkey=error").Parse(string(source))
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}
