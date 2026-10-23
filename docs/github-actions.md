# GitHub Actions ingestion

GitHub repositories use the same archive extraction, PostgreSQL queue, evidence storage, generation, MCP catalog, and optional publication workers as GitLab repositories. The platform receives archives from Actions; it never clones a repository or keeps GitHub access tokens.

## Platform setup

Add these Helm values (replace example numeric IDs):

```yaml
github:
  enabled: true
  issuer: https://token.actions.githubusercontent.com
  audience: gitlab-doc-platform
  jwksURL: https://token.actions.githubusercontent.com/.well-known/jwks
  allowedOwners: "101"
  allowedRepositories: "42"
  requireProtectedRef: false
```

Use immutable numeric `repository_owner_id` and `repository_id` values, not owner names or `owner/repo` paths. At least one allowlist must be nonempty; when both are set, both must match. Owner-only authorization permits every repository owned by that account. Repository-only authorization follows the repository if it is transferred; configure both lists to constrain ownership too. GitLab allowlists remain independent.

For direct execution, configure:

| Environment variable | Default / meaning |
| --- | --- |
| `GITHUB_ENABLED` | `false` |
| `GITHUB_ISSUER` | `https://token.actions.githubusercontent.com` |
| `GITHUB_OIDC_AUDIENCE` | `gitlab-doc-platform` |
| `GITHUB_JWKS_URL` | Issuer + `/.well-known/jwks` |
| `ALLOWED_GITHUB_OWNERS` | Comma-separated numeric owner IDs |
| `ALLOWED_GITHUB_REPOSITORIES` | Comma-separated numeric repository IDs |
| `GITHUB_REQUIRE_PROTECTED_REF` | `false`; when enabled, requires signed `ref_protected="true"` |

Issuer and JWKS URL are administrator-controlled trust settings. Use HTTPS in deployment. If configuring a different Actions issuer, set its exact issuer and JWKS URL explicitly. NetworkPolicies/firewalls must allow the API to fetch signing keys from that JWKS host over HTTPS. Runners must reach the API over HTTPS; a self-hosted runner can be used for private cluster ingress.

## Repository setup

1. Copy [the workflow](../github-actions/documentation.yml) to `.github/workflows/documentation.yml` in the application repository.
2. Set repository Actions variable `DOCUMENTATION_API_URL` to the platform's HTTPS base URL, without `/api/v1`.
3. Optionally set `DOCUMENTATION_AUDIENCE` to match the platform audience.
4. Push to `main`, or run the workflow manually on `main`.

For a complete Go application pipeline, copy [github-actions/examples/application.yml](../github-actions/examples/application.yml) to `.github/workflows/application.yml` instead. It runs `go test ./...`, builds `./cmd/...`, saves application binaries, and generates documentation after both jobs succeed. The Go version comes from the application's `go.mod`; adjust the build path for your application. Only the documentation job has OIDC token permission. Use either this complete example or the documentation-only workflow to avoid duplicate ingestion jobs.

The job grants only `contents: read` and `id-token: write`, disables checkout credential persistence and submodule checkout, packages the exact `GITHUB_SHA`, uploads it, polls status, and downloads `generated-docs/artifacts.tar.gz`. Each API call requests a fresh short-lived token. No GitHub PAT or platform API secret is needed. When generation fails, status and upload responses remain in the Actions artifact for diagnosis.

`git archive` packages tracked files only and respects committed export attributes. Git metadata and checkout credentials are excluded. Never commit credential files; the shared storage holds the uploaded archive. Symlinks, submodule entries, or other unsupported archive content can fail secure extraction. The existing upload/extracted-size limits apply. Exclude unnecessary tracked build output using `.gitattributes` `export-ignore` if needed. Source is only analyzed, never executed by the platform or the example documentation job.

For Go repositories, tags available in the runner checkout that point to the exact commit are supplied using the existing `.gitlab-docs-release.json` evidence format, replacing repository-supplied metadata. The worker treats this as a CI declaration, not independent verification of remote tags.

The API validates the token signature using configured JWKS, exact issuer/audience, expiry/not-before/issued-at times, nonempty subject, numeric repository/owner identities, consistent repository owner/path, commit SHA, and exact `ref=refs/heads/main` with `ref_type=branch`. Only `push`, `workflow_dispatch`, and `schedule` events are accepted. Pull request events, including `pull_request_target`, are rejected even when their signed ref names `main`. Subject strings may use legacy, immutable, environment, or custom formats: authorization uses the separate signed IDs/ref/event claims. See [GitHub's OIDC reference](https://docs.github.com/en/actions/reference/security/oidc) and [issuer discovery](https://token.actions.githubusercontent.com/.well-known/openid-configuration).

## API and stored identities

| Method | Route |
| --- | --- |
| POST | `/api/v1/github/jobs` |
| GET | `/api/v1/github/jobs/{id}` |
| GET | `/api/v1/github/jobs/{id}/artifacts` |
| GET | `/api/v1/github/jobs/{id}/publications` |

All routes require a valid GitHub Actions bearer token. Upload multipart fields are `archive`, optional raw numeric `project_id`, `commit_sha`, and `branch=main`; supplied metadata must match the signed claims. The token's commit identifies the uploaded snapshot. Job reads require the same repository and owner identities; another project's job returns `404`. GitHub and GitLab tokens cannot authenticate to one another's routes.

Internally, repository `42` becomes project `github:42`, and owner `101` becomes group `github:101`. Names remain human-readable (`owner/repo`, `owner`). This isolates jobs, idempotency keys, scheduling, evidence, and publication scopes from GitLab numeric IDs without a database migration. Existing GitLab IDs remain unchanged. The historical `gitlab_groups` table stores both providers' scheduling policies.

Use the prefixed IDs in database `allowed_projects`, worker group filters, MCP `project_ids`, and Notion/Confluence `group_id` targets. For example:

```sql
update gitlab_groups
set max_concurrent_jobs = 2, allowed_projects = array['github:42']
where id = 'github:101';
```

Groups are registered on their first authorized upload, preserving existing disabled-group and allowed-project restrictions. Helm/API allowlists take raw numeric IDs; persisted policies and MCP/publication scopes take prefixed IDs.

## Publish to Notion and Confluence

Publication routes by the repository's **owner ID**, not its repository ID. For `allowedOwners: "101"`, append `group_id: "github:101"` to both publication target lists. Every allowed repository owned by that account uses that owner's destination; each repository receives its own project page and generated snapshots.

For example, merge these entries into your existing Helm settings, keeping current GitLab targets and existing credentials/destination settings:

```yaml
notion:
  enabled: true
  # Keep your existing tokenSecretName and tokenSecretKey.
  targets:
    # Keep existing GitLab target entries here too.
    - group_id: "github:101"
      parent_page_id: "11111111-1111-1111-1111-111111111111"
      enabled: true

confluence:
  enabled: true
  # Keep your existing Cloud/Data Center URL, credentials and Mermaid settings.
  targets:
    # Keep existing GitLab target entries here too.
    - group_id: "github:101"
      space_id: "200"
      parent_page_id: "300"
      enabled: true
```

Replace owner/page/space IDs with real values. For Confluence Data Center, use `space_key` instead of `space_id`. Helm replaces target lists rather than appending them, so include all destinations you want to keep. Omitted targets are disabled on worker startup. Destinations can be shared with GitLab or use separate parent pages; existing Notion and Confluence token Secrets can be reused when they have access to those destinations.

Build and deploy a worker image containing the GitHub publication-target validation fix, then apply your Helm values. Older workers reject `github:` group IDs. Both publishers must be enabled and their accounts/connections must have access to their respective parent pages. See [Notion setup](notion.md) and [Confluence setup](confluence.md) for Secret and destination configuration.

After the worker registers the targets, run documentation generation for a new `main` commit. Completed jobs are queued independently for each enabled publisher. First activation publishes jobs completed after target activation; older completed jobs are not automatically backfilled. See [Confluence existing-job publication](confluence.md#publish-an-existing-job) when publishing a previously completed job. Check both results using `GET /api/v1/github/jobs/{job_id}/publications` with a fresh GitHub Actions OIDC token, or through MCP's authorized job/document queries.
