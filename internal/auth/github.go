package auth

import (
	"encoding/hex"
	"strings"
)

func (v *Validator) validateGitHubClaims(c Claims) error {
	if c.Subject == "" || c.IssuedAt <= 0 || c.NotBefore <= 0 {
		return ErrInvalidToken
	}
	if !numericID(c.RepositoryID) || !numericID(c.OwnerID) || c.Ref != "refs/heads/main" || c.RefType != "branch" {
		return ErrUnauthorized
	}
	owner, repo, ok := strings.Cut(c.Repository, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") || owner != c.Owner {
		return ErrUnauthorized
	}
	if len(c.SHA) != 40 && len(c.SHA) != 64 {
		return ErrUnauthorized
	}
	if _, err := hex.DecodeString(c.SHA); err != nil {
		return ErrUnauthorized
	}
	// pull_request_target can carry the base branch's ref and repository identity.
	// Only events that document trusted main-branch source may ingest or read jobs.
	switch c.EventName {
	case "push", "workflow_dispatch", "schedule":
	default:
		return ErrUnauthorized
	}
	if v.RequireProtectedRef && c.RefProtected != "true" {
		return ErrUnauthorized
	}
	// Use immutable numeric IDs, never recyclable owner/repository names.
	if len(v.AllowedGroups) == 0 && len(v.AllowedProjects) == 0 {
		return ErrUnauthorized
	}
	if len(v.AllowedGroups) > 0 && !v.AllowedGroups[c.OwnerID] {
		return ErrUnauthorized
	}
	if len(v.AllowedProjects) > 0 && !v.AllowedProjects[c.RepositoryID] {
		return ErrUnauthorized
	}
	return nil
}

func numericID(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// UploadProjectID is the provider's raw ID accepted in multipart upload metadata.
// Internal identities carry a provider prefix for isolation in all shared stores.
func (c Claims) UploadProjectID() string {
	if c.Provider == "github" {
		return c.RepositoryID
	}
	return c.ProjectID
}
