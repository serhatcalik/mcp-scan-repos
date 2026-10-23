package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidatorAcceptsValidGitLabOIDC(t *testing.T) {
	v, token := testValidator(t, map[string]any{"ref": "main", "aud": "gitlab-doc-platform"})
	claims, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if claims.ProjectID != "42" {
		t.Fatalf("project id = %q", claims.ProjectID)
	}
}

func TestValidatorRejectsInvalidAudience(t *testing.T) {
	v, token := testValidator(t, map[string]any{"aud": "other"})
	_, err := v.Validate(context.Background(), token)
	if err != ErrInvalidToken {
		t.Fatalf("error = %v, want ErrInvalidToken", err)
	}
}

func TestValidatorRejectsNonMainBranch(t *testing.T) {
	v, token := testValidator(t, map[string]any{"ref": "feature"})
	_, err := v.Validate(context.Background(), token)
	if err != ErrUnauthorized {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}

func TestValidatorRejectsMissingNamespace(t *testing.T) {
	v, token := testValidator(t, map[string]any{"namespace_id": ""})
	_, err := v.Validate(context.Background(), token)
	if err != ErrUnauthorized {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}

func TestValidatorRejectsExpiredToken(t *testing.T) {
	v, token := testValidator(t, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})
	_, err := v.Validate(context.Background(), token)
	if err != ErrInvalidToken {
		t.Fatalf("error = %v, want ErrInvalidToken", err)
	}
}

func TestValidatorRejectsTamperedSignature(t *testing.T) {
	v, token := testValidator(t, nil)
	parts := strings.Split(token, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) == 0 {
		t.Fatalf("invalid test signature: %v", err)
	}
	// Mutate actual signature bytes, not potentially unused Base64 padding bits.
	signature[0] ^= 1
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	token = strings.Join(parts, ".")
	_, err = v.Validate(context.Background(), token)
	if err != ErrInvalidToken {
		t.Fatalf("error = %v, want ErrInvalidToken", err)
	}
}

func TestValidatorRefreshesUnknownRotatedKey(t *testing.T) {
	first, _ := rsa.GenerateKey(rand.Reader, 2048)
	second, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid, key := "test", first
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": kid, "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	}))
	defer server.Close()
	v := &Validator{Issuer: "gitlab", Audience: "docs", JWKSURL: server.URL, AllowedGroups: map[string]bool{"101": true}}
	claims := map[string]any{"iss": "gitlab", "aud": "docs", "exp": time.Now().Add(time.Hour).Unix(), "project_id": "42", "namespace_id": "101", "sha": "abc", "ref": "main", "ref_type": "branch"}
	if _, err := v.Validate(context.Background(), signJWT(t, first, claims)); err != nil {
		t.Fatal(err)
	}
	kid, key = "rotated", second
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid})
	payload, _ := json.Marshal(claims)
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	hash := crypto.SHA256.New()
	_, _ = hash.Write([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, second, crypto.SHA256, hash.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(context.Background(), signed+"."+base64.RawURLEncoding.EncodeToString(sig)); err != nil {
		t.Fatalf("rotation: %v", err)
	}
}

func TestValidatorClaimPolicies(t *testing.T) {
	for _, test := range []struct {
		name      string
		overrides map[string]any
		protected bool
	}{
		{"future nbf", map[string]any{"nbf": time.Now().Add(time.Hour).Unix()}, false},
		{"future iat", map[string]any{"iat": time.Now().Add(time.Hour).Unix()}, false},
		{"negative iat", map[string]any{"iat": -1}, false},
		{"negative nbf", map[string]any{"nbf": -1}, false},
		{"wrong issuer", map[string]any{"iss": "evil"}, false},
		{"unauthorized group", map[string]any{"namespace_id": "102", "namespace_path": "other"}, false},
		{"missing sha", map[string]any{"sha": ""}, false},
		{"unprotected ref", map[string]any{"ref_protected": "false"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			v, token := testValidator(t, test.overrides)
			v.RequireProtectedRef = test.protected
			if _, err := v.Validate(context.Background(), token); err == nil {
				t.Fatal("accepted invalid claims")
			}
		})
	}
}

func TestValidatorFailsClosedWithoutAllowLists(t *testing.T) {
	v, token := testValidator(t, nil)
	v.AllowedGroups, v.AllowedProjects = nil, nil
	if _, err := v.Validate(context.Background(), token); err != ErrUnauthorized {
		t.Fatalf("empty allowlists accepted an identity: %v", err)
	}
}

func testValidator(t *testing.T, override map[string]any) (*Validator, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(server.Close)
	now := time.Now()
	claims := map[string]any{
		"iss": "https://gitlab.example.com", "aud": "gitlab-doc-platform",
		"exp": now.Add(time.Hour).Unix(), "nbf": now.Add(-time.Minute).Unix(),
		"project_id": "42", "namespace_id": "101", "namespace_path": "backend",
		"project_path": "backend/app", "ref": "main", "ref_type": "branch",
		"ref_protected": "true", "sha": strings.Repeat("a", 40),
	}
	for k, v := range override {
		claims[k] = v
	}
	return &Validator{
		Issuer: "https://gitlab.example.com", Audience: "gitlab-doc-platform",
		JWKSURL: server.URL, AllowedGroups: map[string]bool{"101": true},
		Now: func() time.Time { return now },
	}, signJWT(t, key, claims)
}

func signJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test"}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signed := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	hash := crypto.SHA256.New()
	_, _ = hash.Write([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}
