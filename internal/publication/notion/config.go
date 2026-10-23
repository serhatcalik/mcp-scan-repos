package notion

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
)

type Target struct {
	GroupID      string `json:"group_id"`
	ParentPageID string `json:"parent_page_id"`
	Enabled      bool   `json:"enabled"`
}

var pageIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$|^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
var groupIDPattern = regexp.MustCompile(`^(github:)?[0-9]{1,20}$`)

func normalizeID(id string) string { return strings.ToLower(strings.ReplaceAll(id, "-", "")) }

// LoadFiles reads operator-owned configuration. Tokens never enter target records.
func LoadFiles(targetsFile, tokenFile string) ([]Target, string, error) {
	data, err := readBounded(targetsFile, 64<<10)
	if err != nil {
		return nil, "", errors.New("cannot read Notion targets file")
	}
	targets, err := ParseTargets(data)
	if err != nil {
		return nil, "", err
	}
	data, err = readBounded(tokenFile, 4096)
	if err != nil {
		return nil, "", errors.New("cannot read Notion token file")
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 10 || strings.ContainsAny(token, " \t\r\n") {
		return nil, "", errors.New("invalid Notion token file")
	}
	for _, char := range token {
		if char < '!' || char > '~' {
			return nil, "", errors.New("invalid Notion token file")
		}
	}
	return targets, token, nil
}

func ParseTargets(data []byte) ([]Target, error) {
	var config struct {
		Targets []Target `json:"targets"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil {
		return nil, errors.New("invalid Notion targets configuration")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || len(config.Targets) == 0 || len(config.Targets) > 256 {
		return nil, errors.New("Notion requires 1-256 group targets")
	}
	seen := map[string]bool{}
	for i := range config.Targets {
		t := &config.Targets[i]
		if !groupIDPattern.MatchString(t.GroupID) || !pageIDPattern.MatchString(t.ParentPageID) || seen[t.GroupID] {
			return nil, errors.New("Notion targets require unique group IDs (numeric GitLab IDs or github:<owner ID>) and valid parent page IDs")
		}
		seen[t.GroupID] = true
		t.ParentPageID = normalizeID(t.ParentPageID)
	}
	return config.Targets, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("configuration file exceeds limit")
	}
	return data, nil
}
