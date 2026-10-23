# Security

- Do not store GitLab personal access tokens, group access tokens, SSH deploy keys, or permanent repository credentials.
- Validate GitLab CI OIDC ID tokens using GitLab JWKS and cache keys for rotation.
- Optional GitHub Actions ingestion uses its own configured issuer/JWKS and numeric repository/owner allowlists, validates exact `refs/heads/main` and trusted event types, and prefixes persisted IDs with `github:`. See [GitHub setup](github-actions.md). GitHub tokens are refreshed for each workflow API call.
- Enforce allowlists for GitLab groups and projects. Empty group and project allowlists deny ingestion; explicitly configure at least one.
- Require `ref_type=branch` and `ref=main` for the initial deployment.
- Optionally require protected refs with `REQUIRE_PROTECTED_REF=true`.
- Limit request body size and archive extracted size.
- Reject archive path traversal, symlinks, hardlinks, devices, and absolute paths.
- Never execute uploaded source code.
- Redact sensitive values before evidence persistence and prompt construction.
- Never log JWTs, raw secrets, or sensitive source contents.
- Use non-root containers, restricted security contexts, namespace-scoped RBAC, and NetworkPolicies.
- MCP uses separate platform-issued bearer credentials stored as SHA-256 hashes, explicit project scopes, optional expiry, and an independent regeneration permission. It never uses a GitLab repository token. See [Phase 5](phase5.md) for provisioning and rotation.

All `/api/v1/jobs` routes require a valid CI OIDC token. Status and artifact requests verify both project and namespace ownership and return `404` for a different project's job. Health/readiness and metrics remain unauthenticated for cluster probes; restrict network access to them. CI must send its ID token when polling and downloading, and its explicit job timeout must cover generation to keep the token valid.

JWKS requests have a ten-second timeout and bounded response size. An unknown key ID triggers a refresh for rotation, limited to once per five seconds while the key cache is valid. Malformed public keys are rejected. Group and project allowlists and database group restrictions remain in force.

`API_REQUESTS_PER_MINUTE` defaults to 120 requests per authenticated project per API replica. This is a bounded in-process backstop, not a distributed limit; enforce global limits at the gateway when scaling replicas. `429` responses include `Retry-After`.

Static analysis records configuration key names, environment variable names, and Secret/ConfigMap references without literal values. Arbitrary Secret data, ConfigMap values, CI script contents, and environment assignments are not copied into evidence. Do not put credentials in resource names, route paths, image references, or dependency names. Uploaded archives still contain repository files: exclude credential files in CI and control shared storage access.
