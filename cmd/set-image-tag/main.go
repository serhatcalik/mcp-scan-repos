package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

var validTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func main() {
	values := flag.String("values", "deploy/helm/gitlab-doc-platform/values.yaml", "Helm values file")
	tag := flag.String("tag", "", "Published image tag")
	flag.Parse()
	if err := updateFile(*values, *tag); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func updateFile(path, tag string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := updateTag(data, tag)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, updated, info.Mode().Perm())
}

func updateTag(data []byte, tag string) ([]byte, error) {
	if !validTag.MatchString(tag) {
		return nil, errors.New("a valid, nonempty Docker image tag is required")
	}
	file, err := parser.ParseBytes(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), parser.ParseComments)
	if err != nil {
		return nil, err
	}
	if len(file.Docs) != 1 {
		return nil, errors.New("expected a single Helm values document")
	}
	path, err := yaml.PathString("$.image.tag")
	if err != nil {
		return nil, err
	}
	node, err := path.FilterFile(file)
	if err != nil {
		return nil, err
	}
	scalar, ok := node.(*ast.StringNode)
	if !ok || !validTag.MatchString(scalar.Value) {
		return nil, errors.New("image.tag must be a literal string containing a valid Docker tag")
	}
	// Replace only the parser-located scalar to retain all other bytes and comments.
	position := scalar.GetToken().Position
	offset := 0
	for line := 1; line < position.Line; line++ {
		next := bytes.IndexByte(data[offset:], '\n')
		if next < 0 {
			return nil, errors.New("invalid YAML token position")
		}
		offset += next + 1
	}
	offset += position.Column - 1
	if offset >= len(data) {
		return nil, errors.New("invalid YAML token position")
	}
	old := scalar.Value
	if quote := data[offset]; quote == '\'' || quote == '"' {
		old = string(quote) + old + string(quote)
	}
	if !bytes.HasPrefix(data[offset:], []byte(old)) {
		return nil, fmt.Errorf("image.tag scalar does not match its source position (line %d, column %d)", position.Line, position.Column)
	}
	updated := make([]byte, 0, len(data)+len(tag))
	updated = append(updated, data[:offset]...)
	updated = append(updated, strconv.Quote(tag)...)
	updated = append(updated, data[offset+len(old):]...)
	return updated, nil
}
