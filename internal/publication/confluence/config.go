package confluence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
)

type Target struct {
	GroupID      string `json:"group_id"`
	SpaceID      string `json:"space_id,omitempty"`
	SpaceKey     string `json:"space_key,omitempty"`
	ParentPageID string `json:"parent_page_id"`
	Enabled      bool   `json:"enabled"`
}

type Settings struct {
	BaseURL              string   `json:"base_url"`
	Edition              string   `json:"edition"`
	Username             string   `json:"username,omitempty"`
	MermaidMacro         string   `json:"mermaid_macro,omitempty"`
	MermaidBodyParameter string   `json:"mermaid_body_parameter,omitempty"`
	MermaidApp           string   `json:"mermaid_app,omitempty"`
	MermaidExtensionKey  string   `json:"mermaid_extension_key,omitempty"`
	Targets              []Target `json:"targets"`
}

var numericID = regexp.MustCompile(`^[0-9]{1,20}$`)
var groupIDPattern = regexp.MustCompile(`^(github:)?[0-9]{1,20}$`)
var macroName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,99}$`)
var spaceKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

var narvaExtensionKey = regexp.MustCompile(`^336440d6-2d95-4275-833f-6351e86634e3/[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}/static/mermaid-diagram$`)

func validateMermaid(s Settings) error {
	if s.MermaidApp != "" && s.MermaidApp != "narva" {
		return errors.New("unsupported Confluence Mermaid app")
	}
	if s.MermaidApp == "narva" {
		if s.Edition != "cloud" || !narvaExtensionKey.MatchString(s.MermaidExtensionKey) || s.MermaidMacro != "" || s.MermaidBodyParameter != "" {
			return errors.New("Narva Mermaid requires Cloud, its Forge extension key, and empty legacy macro settings")
		}
	} else if s.MermaidExtensionKey != "" {
		return errors.New("Confluence Mermaid extension key requires the Narva adapter")
	}
	if (s.MermaidMacro != "" && !macroName.MatchString(s.MermaidMacro)) || (s.MermaidBodyParameter != "" && (!macroName.MatchString(s.MermaidBodyParameter) || s.MermaidMacro == "")) {
		return errors.New("invalid Confluence Mermaid macro configuration")
	}
	return nil
}

func ParseSettings(data []byte) (Settings, error) {
	var s Settings
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if len(data) > 64<<10 {
		return s, errors.New("Confluence configuration exceeds size limit")
	}
	if err := d.Decode(&s); err != nil {
		// Decoder errors may include arbitrary field names or values. Report only
		// fixed categories; unknown fields often indicate an older worker image.
		var syntax *json.SyntaxError
		var valueType *json.UnmarshalTypeError
		switch {
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return s, errors.New("invalid Confluence configuration: unknown field; check worker/chart version compatibility and setting names")
		case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			return s, errors.New("invalid Confluence configuration: malformed or empty JSON")
		case errors.As(err, &valueType):
			return s, errors.New("invalid Confluence configuration: wrong JSON value type; IDs must be strings and enabled must be boolean")
		default:
			return s, errors.New("invalid Confluence configuration")
		}
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return s, errors.New("invalid Confluence configuration: trailing JSON content")
	}
	u, err := url.Parse(s.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Path, "..") || u.RawPath != "" {
		return s, errors.New("Confluence base URL must be HTTPS without credentials, query, or fragment")
	}
	s.BaseURL = strings.TrimRight(s.BaseURL, "/")
	if s.Edition != "cloud" && s.Edition != "data-center" {
		return s, errors.New("Confluence edition must be cloud or data-center")
	}
	if strings.ContainsAny(s.Username, ":\r\n") || (s.Edition == "cloud" && s.Username == "") {
		return s, errors.New("Confluence Cloud requires an account email for API-token authentication")
	}
	if err := validateMermaid(s); err != nil {
		return s, err
	}
	if len(s.Targets) == 0 || len(s.Targets) > 256 {
		return s, errors.New("Confluence requires 1-256 group targets")
	}
	seen := map[string]bool{}
	for _, t := range s.Targets {
		if !groupIDPattern.MatchString(t.GroupID) || !numericID.MatchString(t.ParentPageID) || seen[t.GroupID] ||
			(s.Edition == "cloud" && (!numericID.MatchString(t.SpaceID) || t.SpaceKey != "")) ||
			(s.Edition == "data-center" && (!spaceKey.MatchString(t.SpaceKey) || t.SpaceID != "")) {
			return s, errors.New("Confluence targets require unique group IDs (numeric GitLab IDs or github:<owner ID>), numeric parent page IDs, and edition-specific spaces")
		}
		seen[t.GroupID] = true
	}
	return s, nil
}

func LoadFiles(settingsFile, tokenFile string) (Settings, string, error) {
	data, err := readBounded(settingsFile, 64<<10)
	if err != nil {
		return Settings{}, "", errors.New("cannot read Confluence configuration file")
	}
	s, err := ParseSettings(data)
	if err != nil {
		return s, "", err
	}
	data, err = readBounded(tokenFile, 4096)
	if err != nil {
		return s, "", errors.New("cannot read Confluence token file")
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 10 {
		return s, "", errors.New("invalid Confluence token file")
	}
	for _, ch := range token {
		if ch < '!' || ch > '~' {
			return s, "", errors.New("invalid Confluence token file")
		}
	}
	return s, token, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("configuration exceeds limit")
	}
	return b, nil
}

func (s Settings) destination(t Target) string {
	h := sha256.Sum256([]byte(s.BaseURL + "\x00" + s.Edition + "\x00" + t.SpaceID + "\x00" + t.SpaceKey + "\x00" + t.ParentPageID))
	return hex.EncodeToString(h[:])
}

func (s Settings) titleScope(t Target) string {
	// Confluence page titles are unique across a space, not just within a parent.
	t.ParentPageID = ""
	return "confluence-space:" + s.destination(t)
}
