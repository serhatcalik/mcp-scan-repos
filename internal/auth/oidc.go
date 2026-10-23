package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid oidc token")
	ErrUnauthorized = errors.New("unauthorized repository identity")
)

type Claims struct {
	Issuer       string `json:"iss"`
	Subject      string `json:"sub"`
	Audience     any    `json:"aud"`
	ExpiresAt    int64  `json:"exp"`
	NotBefore    int64  `json:"nbf"`
	IssuedAt     int64  `json:"iat"`
	ProjectID    string `json:"project_id"`
	ProjectPath  string `json:"project_path"`
	NamespaceID  string `json:"namespace_id"`
	Namespace    string `json:"namespace_path"`
	Ref          string `json:"ref"`
	RefType      string `json:"ref_type"`
	RefProtected string `json:"ref_protected"`
	SHA          string `json:"sha"`
	RepositoryID string `json:"repository_id"`
	Repository   string `json:"repository"`
	OwnerID      string `json:"repository_owner_id"`
	Owner        string `json:"repository_owner"`
	EventName    string `json:"event_name"`
	Provider     string `json:"-"`
}

type Validator struct {
	// Empty retains the GitLab policy for existing callers.
	Provider            string
	Issuer              string
	Audience            string
	JWKSURL             string
	AllowedGroups       map[string]bool
	AllowedProjects     map[string]bool
	RequireProtectedRef bool
	Now                 func() time.Time
	Client              *http.Client

	mu               sync.Mutex
	cached           map[string]crypto.PublicKey
	cacheExp         time.Time
	unknownRefreshAt time.Time
}

func (v *Validator) Validate(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}
	if len(token) > 32768 {
		return Claims{}, ErrInvalidToken
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := decodeJSON(parts[0], &header); err != nil {
		return Claims{}, fmt.Errorf("%w: header: %v", ErrInvalidToken, err)
	}
	if header.Algorithm == "" || header.Algorithm == "none" || header.KeyID == "" {
		return Claims{}, ErrInvalidToken
	}
	key, err := v.key(ctx, header.KeyID)
	if err != nil {
		return Claims{}, err
	}
	signed := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	if err := verify(header.Algorithm, key, []byte(signed), sig); err != nil {
		return Claims{}, err
	}
	var claims Claims
	if err := decodeJSON(parts[1], &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: claims: %v", ErrInvalidToken, err)
	}
	if err := v.validateClaims(claims); err != nil {
		return Claims{}, err
	}
	if v.Provider == "github" {
		// Never trust GitLab-shaped fields supplied in a GitHub token.
		claims.ProjectID, claims.ProjectPath = "github:"+claims.RepositoryID, claims.Repository
		claims.NamespaceID, claims.Namespace = "github:"+claims.OwnerID, claims.Owner
		claims.Ref = "main"
		claims.Provider = "github"
	}
	return claims, nil
}

func (v *Validator) validateClaims(c Claims) error {
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	if c.Issuer != v.Issuer || !audienceContains(c.Audience, v.Audience) {
		return ErrInvalidToken
	}
	if c.ExpiresAt <= now.Unix() || (c.NotBefore != 0 && c.NotBefore > now.Unix()) {
		return ErrInvalidToken
	}
	if c.IssuedAt < 0 || c.NotBefore < 0 || c.IssuedAt > now.Unix() || c.IssuedAt >= c.ExpiresAt || c.NotBefore >= c.ExpiresAt {
		return ErrInvalidToken
	}
	if v.Provider == "github" {
		return v.validateGitHubClaims(c)
	}
	if v.Provider != "" && v.Provider != "gitlab" {
		return ErrUnauthorized
	}
	if c.ProjectID == "" || c.NamespaceID == "" || c.SHA == "" || c.Ref != "main" || c.RefType != "branch" {
		return ErrUnauthorized
	}
	if v.RequireProtectedRef && c.RefProtected != "true" {
		return ErrUnauthorized
	}
	// A signed GitLab identity alone must not authorize every project on the instance.
	if len(v.AllowedProjects) == 0 && len(v.AllowedGroups) == 0 {
		return ErrUnauthorized
	}
	if len(v.AllowedProjects) > 0 && !v.AllowedProjects[c.ProjectID] {
		return ErrUnauthorized
	}
	if len(v.AllowedGroups) > 0 && !v.AllowedGroups[c.NamespaceID] && !v.AllowedGroups[c.Namespace] {
		return ErrUnauthorized
	}
	return nil
}

func (v *Validator) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	if v.cached != nil && time.Now().Before(v.cacheExp) {
		key := v.cached[kid]
		v.mu.Unlock()
		if key != nil {
			return key, nil
		}
		// A previously unseen kid may be a newly rotated GitLab signing key.
		v.mu.Lock()
		if time.Now().Before(v.unknownRefreshAt) {
			v.mu.Unlock()
			return nil, ErrInvalidToken
		}
		v.unknownRefreshAt = time.Now().Add(5 * time.Second)
		v.mu.Unlock()
	} else {
		v.mu.Unlock()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks fetch failed: %s", resp.Status)
	}
	var set jwks
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]crypto.PublicKey{}
	for _, raw := range set.Keys {
		if raw.Use != "" && raw.Use != "sig" {
			continue
		}
		key, err := raw.publicKey()
		if err == nil {
			keys[raw.KID] = key
		}
	}
	v.mu.Lock()
	v.cached = keys
	v.cacheExp = time.Now().Add(time.Hour)
	key := v.cached[kid]
	v.mu.Unlock()
	if key == nil {
		return nil, ErrInvalidToken
	}
	return key, nil
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	ALG string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (j jwk) publicKey() (crypto.PublicKey, error) {
	switch j.KTY {
	case "RSA":
		nb, err := base64.RawURLEncoding.DecodeString(j.N)
		if err != nil {
			return nil, err
		}
		eb, err := base64.RawURLEncoding.DecodeString(j.E)
		if err != nil {
			return nil, err
		}
		e := 0
		for _, b := range eb {
			e = e<<8 + int(b)
		}
		if len(nb) < 256 || len(eb) > 4 || e < 3 || e%2 == 0 {
			return nil, ErrInvalidToken
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}, nil
	case "EC":
		return nil, errors.New("ec jwk not implemented yet")
	case "OKP":
		if j.Crv != "Ed25519" {
			return nil, errors.New("unsupported okp curve")
		}
		x, err := base64.RawURLEncoding.DecodeString(j.X)
		if err != nil {
			return nil, err
		}
		if len(x) != ed25519.PublicKeySize {
			return nil, ErrInvalidToken
		}
		return ed25519.PublicKey(x), nil
	default:
		return nil, errors.New("unsupported key type")
	}
}

func verify(alg string, key crypto.PublicKey, signed, sig []byte) error {
	switch alg {
	case "RS256":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return ErrInvalidToken
		}
		h := crypto.SHA256.New()
		_, _ = h.Write(signed)
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h.Sum(nil), sig); err != nil {
			return ErrInvalidToken
		}
		return nil
	case "EdDSA":
		pub, ok := key.(ed25519.PublicKey)
		if !ok || !ed25519.Verify(pub, signed, sig) {
			return ErrInvalidToken
		}
		return nil
	default:
		if _, ok := key.(*ecdsa.PublicKey); ok {
			return ErrInvalidToken
		}
		return ErrInvalidToken
	}
}

func decodeJSON(segment string, target any) error {
	payload, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, target)
}

func audienceContains(raw any, want string) bool {
	switch aud := raw.(type) {
	case string:
		return aud == want
	case []any:
		for _, item := range aud {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}
