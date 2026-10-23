package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

type Credential struct {
	ID            string    `json:"id"`
	TokenSHA256   string    `json:"token_sha256"`
	ProjectIDs    []string  `json:"project_ids"`
	CanRegenerate bool      `json:"can_regenerate"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
}

func ParseCredentials(data []byte) ([]Credential, error) {
	var config struct {
		Clients []Credential `json:"clients"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, errors.New("invalid MCP credential configuration")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid MCP credential configuration")
	}
	if len(config.Clients) == 0 || len(config.Clients) > 256 {
		return nil, errors.New("MCP requires between 1 and 256 scoped clients")
	}
	seen := map[string]bool{}
	identities := map[string]bool{}
	for _, c := range config.Clients {
		hash, err := hex.DecodeString(c.TokenSHA256)
		if err != nil || len(hash) != 32 || c.ID == "" || len(c.ProjectIDs) == 0 || len(c.ProjectIDs) > 256 {
			return nil, errors.New("MCP client requires an identity, SHA-256 token hash, and explicit project IDs")
		}
		key := strings.ToLower(c.TokenSHA256)
		if seen[key] || identities[c.ID] {
			return nil, errors.New("duplicate MCP credential")
		}
		seen[key] = true
		identities[c.ID] = true
		for _, project := range c.ProjectIDs {
			if project == "" || project == "*" {
				return nil, errors.New("MCP wildcard or empty project scope is forbidden")
			}
		}
	}
	return config.Clients, nil
}

func authenticateToken(header string, clients []Credential) (Credential, bool) {
	if !strings.HasPrefix(header, "Bearer ") {
		return Credential{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if len(token) < 32 || len(token) > 4096 {
		return Credential{}, false
	}
	hash := sha256.Sum256([]byte(token))
	var found Credential
	valid := false
	for _, c := range clients {
		expected, err := hex.DecodeString(c.TokenSHA256)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare(hash[:], expected) == 1 && (c.ExpiresAt.IsZero() || time.Now().Before(c.ExpiresAt)) {
			found = c
			valid = true
		}
	}
	return found, valid
}

func allowed(c Credential, project string) bool {
	for _, id := range c.ProjectIDs {
		if id == project {
			return true
		}
	}
	return false
}
