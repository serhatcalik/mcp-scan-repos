package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func githubValidator(t *testing.T, overrides map[string]any) (*Validator, string) {
	t.Helper()
	claims := map[string]any{
		"iss": "https://token.actions.githubusercontent.com", "sub": "repo:backend/app:ref:refs/heads/main",
		"iat": time.Now().Add(-time.Minute).Unix(), "ref": "refs/heads/main", "event_name": "push",
		"repository_id": "42", "repository": "backend/app", "repository_owner_id": "101", "repository_owner": "backend",
		// A GitHub token's GitLab-shaped fields must never control storage identity.
		"project_id": "forged", "namespace_id": "forged", "project_path": "forged", "namespace_path": "forged",
	}
	for k, v := range overrides {
		claims[k] = v
	}
	v, token := testValidator(t, claims)
	v.Provider, v.Issuer = "github", "https://token.actions.githubusercontent.com"
	v.AllowedProjects = map[string]bool{"42": true}
	return v, token
}

func TestGitHubOIDCNormalizesIdentity(t *testing.T) {
	for _, subject := range []string{
		"repo:backend/app:ref:refs/heads/main",
		"repo:backend@101/app@42:ref:refs/heads/main",
		"repo:backend/app:environment:production",
	} {
		v, token := githubValidator(t, map[string]any{"sub": subject})
		claims, err := v.Validate(context.Background(), token)
		if err != nil {
			t.Fatal(err)
		}
		if claims.ProjectID != "github:42" || claims.NamespaceID != "github:101" || claims.ProjectPath != "backend/app" || claims.Namespace != "backend" || claims.Ref != "main" || claims.UploadProjectID() != "42" {
			t.Fatalf("incorrect normalized identity: %+v", claims)
		}
	}
}

func TestGitHubOIDCRejectsInvalidClaims(t *testing.T) {
	for _, test := range []struct {
		name   string
		claims map[string]any
	}{
		{"issuer", map[string]any{"iss": "https://evil.example"}},
		{"audience", map[string]any{"aud": "other"}},
		{"expired", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}},
		{"future iat", map[string]any{"iat": time.Now().Add(time.Hour).Unix()}},
		{"future nbf", map[string]any{"nbf": time.Now().Add(time.Hour).Unix()}},
		{"missing iat", map[string]any{"iat": 0}},
		{"missing nbf", map[string]any{"nbf": 0}},
		{"missing subject", map[string]any{"sub": ""}},
		{"missing repository", map[string]any{"repository_id": ""}},
		{"missing owner", map[string]any{"repository_owner_id": ""}},
		{"wrong owner", map[string]any{"repository_owner_id": "102"}},
		{"wrong repository", map[string]any{"repository_id": "43"}},
		{"recyclable owner name", map[string]any{"repository_owner_id": "backend"}},
		{"prefixed repository", map[string]any{"repository_id": "github:42"}},
		{"owner path mismatch", map[string]any{"repository_owner": "other"}},
		{"missing repository path", map[string]any{"repository": ""}},
		{"malformed path", map[string]any{"repository": "backend/app/other"}},
		{"feature branch", map[string]any{"ref": "refs/heads/feature"}},
		{"unqualified main", map[string]any{"ref": "main"}},
		{"tag named main", map[string]any{"ref": "refs/tags/main", "ref_type": "tag"}},
		{"pull request", map[string]any{"event_name": "pull_request"}},
		{"pull request target", map[string]any{"event_name": "pull_request_target"}},
		{"missing event", map[string]any{"event_name": ""}},
		{"missing sha", map[string]any{"sha": ""}},
		{"invalid sha", map[string]any{"sha": strings.Repeat("z", 40)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			v, token := githubValidator(t, test.claims)
			if _, err := v.Validate(context.Background(), token); err == nil {
				t.Fatal("accepted invalid GitHub identity")
			}
		})
	}
}

func TestGitHubOIDCAllowListsAndProtectedPolicy(t *testing.T) {
	for _, test := range []struct {
		name          string
		owners, repos map[string]bool
		protected     bool
		want          bool
	}{
		{"no allowlists", nil, nil, false, false},
		{"owner only", map[string]bool{"101": true}, nil, false, true},
		{"repository only", nil, map[string]bool{"42": true}, false, true},
		{"both must match", map[string]bool{"101": true}, map[string]bool{"43": true}, false, false},
		{"owner names forbidden", map[string]bool{"backend": true}, nil, false, false},
		{"protected", map[string]bool{"101": true}, nil, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			v, token := githubValidator(t, map[string]any{"ref_protected": "false"})
			v.AllowedGroups, v.AllowedProjects, v.RequireProtectedRef = test.owners, test.repos, test.protected
			_, err := v.Validate(context.Background(), token)
			if (err == nil) != test.want {
				t.Fatalf("validation error = %v, want acceptance = %v", err, test.want)
			}
		})
	}
	for _, event := range []string{"push", "workflow_dispatch", "schedule"} {
		v, token := githubValidator(t, map[string]any{"event_name": event})
		v.RequireProtectedRef = true
		if _, err := v.Validate(context.Background(), token); err != nil {
			t.Fatalf("protected main event %s rejected: %v", event, err)
		}
	}
}
