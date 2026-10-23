# Using the Platform in GitLab Pipelines

The [reusable CI template](../gitlab-ci/documentation.yml) adds a `generate_documentation` job to an application repository. On the exact `main` branch, it packages the checked-out source, authenticates with a GitLab OIDC ID token, submits a documentation job, waits for completion, and retains the downloaded bundle as a GitLab job artifact. No personal access token is needed.

## Deployment Prerequisites

The API, worker, and MCP binaries connect to the same PostgreSQL database through `DATABASE_URL` and apply embedded migrations at startup. API and worker also need access to the same archive and artifact storage. On the first authorized upload, the validated GitLab namespace is registered as an enabled group with concurrency one; existing disabled groups and group project restrictions are preserved.

All job routes require the GitLab OIDC bearer token, including status polling and artifact downloads. Read access is scoped to the authenticated project and namespace. The reusable template sends the token on every request. Deploy the updated CI template before enabling the updated API, and ensure the explicit CI job timeout covers queue wait, generation, and download so the token remains valid. Retrying a pipeline issues a new token and can retrieve already completed artifacts for the same project.

## 1. Configure the Platform

The platform operator should configure these API environment variables:

| Variable | Example | Purpose |
| --- | --- | --- |
| `GITLAB_ISSUER` | `https://gitlab.example.com` | Must match the token's `iss` claim exactly. |
| `GITLAB_OIDC_AUDIENCE` | `gitlab-doc-platform` | Must match the pipeline's `DOCUMENTATION_AUDIENCE`. |
| `GITLAB_JWKS_URL` | `https://gitlab.example.com/oauth/discovery/keys` | Signing key endpoint; defaults to this path under the issuer. |
| `ALLOWED_GITLAB_GROUPS` | `123,my-group/my-subgroup` | Comma-separated namespace IDs or exact namespace paths. |
| `ALLOWED_GITLAB_PROJECTS` | `456,789` | Comma-separated project IDs. |
| `REQUIRE_PROTECTED_REF` | `true` | Also requires `main` to be protected in GitLab. |
| `STORAGE_DIR` | `/var/lib/gitlab-docs` | Archive/artifact storage accessible to API and worker. |

When both allowlists are configured, the project must pass both checks. Namespace matching is exact: allowing a parent group path does not automatically allow its subgroups. One empty list leaves authorization to the other configured list; both lists empty deny ingestion. Configure at least one for the repositories you intend to accept.

The runner must reach the API over HTTPS, and the API must reach GitLab's signing key endpoint. The worker needs access to the configured Ollama endpoint and model. Use [deployment.md](deployment.md) for Helm setup.

GitLab supplies the job token through [`id_tokens`](https://docs.gitlab.com/ci/secrets/id_token_authentication/). The platform accepts branch tokens for `main`; changing the CI rule to another branch does not change that server-side restriction.

## 2. Include the Template

Host this repository on the same GitLab instance as your application, then add the following to the application's `.gitlab-ci.yml`. Replace the project path and API URL with your own values.

```yaml
include:
  - project: "platform/gitlab-doc-platform"
    ref: "main"
    file: "/gitlab-ci/documentation.yml"

stages:
  - documentation

variables:
  DOCUMENTATION_API_URL: "https://docs-api.example.com"
  DOCUMENTATION_AUDIENCE: "gitlab-doc-platform"

generate_documentation:
  timeout: 100m
  dependencies: []
```

Use the API base URL without a trailing slash or `/api/v1` suffix. For production, replace `ref: "main"` with a reviewed full commit SHA to pin the template. For private template projects, the user starting the pipeline must have access to both projects. See GitLab's [`include:project` reference](https://docs.gitlab.com/ci/yaml/#includeproject).

Alternatively, put the template in your application repository at `.gitlab/ci/documentation.yml` and use `include: { local: "/.gitlab/ci/documentation.yml" }`. Keep the stages, variables, and job overrides shown above.

For an existing pipeline, add `documentation` to its complete stage list. GitLab merges the included job with the application's `generate_documentation` settings. `dependencies: []` prevents downloading earlier jobs' artifacts into the source workspace.

## 3. Example Test, Build, and Documentation Pipeline

The complete [Go application example](../gitlab-ci/examples/application.yml) includes tests, a build, and documentation generation:

```yaml
include:
  - project: "platform/gitlab-doc-platform"
    ref: "main"
    file: "/gitlab-ci/documentation.yml"

stages:
  - test
  - build
  - documentation

variables:
  DOCUMENTATION_API_URL: "https://docs-api.example.com"
  DOCUMENTATION_AUDIENCE: "gitlab-doc-platform"
  DOCUMENTATION_POLL_SECONDS: "10"
  DOCUMENTATION_TIMEOUT_SECONDS: "5400"

test_application:
  stage: test
  image: golang:1.26.10-alpine3.24
  script:
    - go test ./...

build_application:
  stage: build
  image: golang:1.26.10-alpine3.24
  script:
    - mkdir -p build
    - go build -o build/ ./cmd/...
  artifacts:
    expire_in: 1 day
    paths:
      - build/

generate_documentation:
  timeout: 100m
  dependencies: []
```

Replace the test and build commands for your application's language. Documentation runs after successful earlier stages and only on `main`, including a manually started pipeline for that branch. It is skipped in merge request and tag pipelines. A documentation failure fails the pipeline by default; set `allow_failure: true` on `generate_documentation` to make it optional.

## Configuration and Artifacts

Variables can also be set in GitLab under **Settings > CI/CD > Variables**. Ensure their environment scope and protection settings make them available to this job.

| Pipeline variable | Default | Meaning |
| --- | --- | --- |
| `DOCUMENTATION_API_URL` | Empty; required | Reachable platform API base URL. |
| `DOCUMENTATION_AUDIENCE` | `gitlab-doc-platform` | Audience requested for the OIDC token. |
| `DOCUMENTATION_POLL_SECONDS` | `10` | Delay between status checks. |
| `DOCUMENTATION_TIMEOUT_SECONDS` | `5400` | Maximum polling time, in seconds (90 minutes). |
| `DOCUMENTATION_ARCHIVE` | `source-for-documentation.tar.gz` | Source archive filename. |
| `DOCUMENTATION_HTTP_RETRIES` | `3` | Retries for transient HTTP failures; project/commit/schema idempotency prevents duplicate job creation. |
| `DOCUMENTATION_GIT_TAGS_ENABLED` | `true` | For a root Go module, query remote tags pointing exactly at the uploaded commit and include a CI release declaration. Set `false` to disable. |

Set the GitLab job timeout above the polling timeout to leave time for package installation, upload, and download. The template and example use 100 minutes for a 90-minute polling window; the project and runner must permit that timeout. Queue wait also counts toward the polling limit. GitLab also uses an explicit job timeout when determining ID token expiry. See the [ID token documentation](https://docs.gitlab.com/ci/secrets/id_token_authentication/).

After a successful run, open **Build > Jobs > generate_documentation** and browse or download its artifacts. The downloaded bundle is `generated-docs/artifacts.tar.gz`, retained for 14 days by default. To unpack it locally:

```sh
tar -xzf generated-docs/artifacts.tar.gz
```

The bundle contains `generated-docs/technical-paper.md`, `infrastructure-paper.md`, `evidence.json`, `metadata.json`, `validation.json`, and `diagrams/`. To change retention, override `artifacts: { expire_in: "30 days" }` on the included job. See [Phase 4 generation](phase4.md) for prompt selection, confidence labels, validation, and inference queue behavior.

## Archive Considerations

### Go Versions From Git Tags

For repositories with a root `go.mod`, the template queries `git ls-remote --tags origin` using the runner's existing checkout credentials, with a 30-second timeout. This works with shallow checkouts and does not clone or fetch repositories on the platform. Both lightweight and peeled annotated tags are supported. Only tags pointing exactly at `CI_COMMIT_SHA` are considered; the nearest older tag is not treated as the current application version. Valid release labels include `v1.2.3`, `1.2.3`, prerelease, and build suffixes. Non-release tag names are ignored; conflicting release tags result in date fallback.

The CI job serializes project ID, commit SHA, and tag names into the reserved `.gitlab-docs-release.json` archive entry. Repository-owned copies of that name are excluded and replaced only inside the temporary archive, never in the working directory. The worker parses this bounded declaration, checks its project/commit against the authenticated upload job, and stores its provenance as a CI declaration; it does not independently verify remote Git refs. OIDC authenticates the job, not the tag-to-commit assertion. No GitLab credential is passed to the API/worker or included in the archive. Version declarations are still untrusted input and pass normal redaction and label validation.

Tag capture does not permit tag pipelines: the documentation job and API still require the exact `main` branch. Create the release tag on the intended main commit before its documentation upload, or start a main-branch pipeline after creating the tag. No exact matching release tag, a failed lookup, or disabled capture means source declarations/date fallback, not a guessed release version. Existing completed jobs remain idempotent; a tag added after that job's archive was uploaded does not retroactively update its evidence or pages.

Deploy API and worker together with documentation schema `0.6.1`, and update the included CI template to capture tags. The schema change creates a new job when resubmitting a commit previously processed under `0.6.0`; later identical project/commit/schema submissions retain their existing job. Older stored archives have no CI tag declaration.

The template archives the working directory, excluding `.git`, `node_modules`, `dist`, `build`, `target`, `vendor`, `generated-docs`, the configured archive filename, and selected binary extensions. It disables inherited CI caches and earlier jobs' artifact downloads. It does not automatically exclude `.env`, credentials, or every cache directory. Review the workspace before uploading and customize the archive command for your repository when needed. Never enable shell tracing or `CI_DEBUG_TRACE` for token-bearing jobs.

The default limits are 100 MiB compressed and 500 MiB extracted. The worker rejects symlinks, hardlinks, path traversal, and unsupported archive entries. Repositories containing symlinks or hardlinks need a reviewed packaging strategy that omits those entries. The template creates its archive in a temporary directory under `/tmp`, uploads it from there, and removes it when the script exits. This avoids tar reading its own output and reporting `file changed as we read it`.

## Troubleshooting

### Architecture Diagrams

Diagrams describe the uploaded repository, not the documentation platform. Application diagrams show detected components and Go packages; dependency diagrams show declared dependencies and parsed Go imports. Import arrows are verified source-code dependencies, not runtime request flows. Infrastructure diagrams show detected repository infrastructure, including resource names from parseable Kubernetes YAML and explicit Ingress/HTTPRoute/OpenShift Route Service references when the target is uniquely present in the archive. Source references are included in Mermaid comments.

Missing evidence is reported as unavailable. Service selector relationships, Helm template rendering, and runtime interactions are not yet resolved, so resources without verified connections remain unconnected rather than receiving invented arrows. Non-Go repositories currently have less structural detail. Documentation schema version `0.4.0` and the generation settings fingerprint invalidate earlier caches: deploy both the API and worker with synchronized settings and submit the same commit again to create a new generation job. Existing completed artifacts remain unchanged.

Retrying `generate_documentation` resubmits the same project/commit/schema idempotency key. A failed job is atomically returned to `queued`, with its previous error, attempt count, retry delay, and lease cleared. The existing job ID and stored source archive are retained (`created: false`). Queued, active, completed, and cancelled jobs are returned unchanged. Deploy the updated API before retrying; polling the status endpoint alone does not requeue a job.

| Symptom | Check |
| --- | --- |
| Template cannot be included | Project path, pinned ref, file path, and the triggering user's access to the template repository. |
| Job is missing | The branch must be exactly `main`; ensure your full stage list contains `documentation`. |
| `DOCUMENTATION_API_URL is required` | Variable value, environment scope, and protected-variable availability. |
| Upload returns `401` | Issuer/audience match, signing-key connectivity, allowlists, clock accuracy, and protected `main` when required. Do not print the token. |
| TLS or connection error | Runner DNS, routing, proxy rules, and certificate trust. Install your internal CA rather than disabling verification. |
| Archive upload rejected | Compressed size and ingress request limits; multipart requests require some additional space. |
| Job stays `queued` | Check that both services log `database ready`, point to the same database, and run the rebuilt image. Inspect worker logs and group `enabled`, concurrency, and active leases. |
| Job becomes `failed` | Worker logs, archive entry types, extraction limits, shared storage, and Ollama model availability. |
| Polling times out | Worker health and queue backlog; increase polling and GitLab job timeouts together if generation needs longer. |
| Artifact download fails | Job must be completed and the API must see the worker's artifact storage. |

Before enabling the job on `main`, validate the merged configuration in GitLab's [Pipeline editor](https://docs.gitlab.com/ci/pipeline_editor/). A successful integration logs a job ID, reaches `completed`, and retains the documentation bundle.
