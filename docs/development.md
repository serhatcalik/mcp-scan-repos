# Development

Important environment variables:

- `API_ADDR`: API listen address, default `:8080`.
- `MCP_ADDR`: MCP listen address, default `:8081`.
- `MCP_AUTH_FILE`: required scoped MCP client credential JSON, default `/etc/mcp-auth/clients.json`; see [Phase 5](phase5.md).
- `WORKER_HTTP_ADDR`: worker health/readiness/metrics address, default `:9090`.
- `DATABASE_URL`: PostgreSQL connection string.
- `WORKER_POLL_INTERVAL`: queue polling delay, default `5s`.
- `WORKER_JOB_TIMEOUT`: total job deadline, default `75m`; the lease lasts one minute longer.
- `LLM_REQUEST_TIMEOUT`: timeout per AI HTTP attempt, default `30m`. Two document requests run sequentially within the total job deadline.
- `GITLAB_ISSUER`: self-hosted GitLab issuer URL.
- `GITLAB_OIDC_AUDIENCE`: expected CI OIDC audience.
- `GITLAB_JWKS_URL`: JWKS endpoint, default `<issuer>/oauth/discovery/keys`.
- `ALLOWED_GITLAB_GROUPS`: comma-separated namespace IDs or paths.
- `ALLOWED_GITLAB_PROJECTS`: comma-separated project IDs.
- `REQUIRE_PROTECTED_REF`: require protected main branches.
- `GITHUB_ENABLED`: opt in to GitHub Actions ingestion; configure its issuer, audience, JWKS, and numeric owner/repository allowlists as described in [GitHub setup](github-actions.md).
- `MAX_ARCHIVE_BYTES`: uploaded archive byte limit.
- `MAX_EXTRACTED_BYTES`: extracted workspace byte limit.
- `API_REQUESTS_PER_MINUTE`: authenticated project requests per API replica, default `120`; also configurable through Helm `api.requestsPerMinute`.
- `LLM_PROVIDER`: `ollama` (default), `openai`, or `anthropic`; see [AI provider setup](ai-providers.md).
- `LLM_ENDPOINT`: provider API base URL; defaults to the selected provider's endpoint.
- `LLM_MODEL`: model name, default `qwen2.5-coder:7b` for Ollama; required for hosted providers.
- `LLM_API_KEY_FILE`: hosted-provider worker credential file, default `/etc/llm-secret/api-key`.
- Generation context, output, temperature, retries, and shared inference admission limits: see [Phase 4](phase4.md).

Workflow archive tests require Git, a shell, and GNU tar. The Alpine CI test job installs `bash git tar` to match the GitHub workflow's shell; the GitHub archive test skips when Git is unavailable locally. Install Helm to run chart render tests.

Run:

```sh
go fmt ./...
go test ./...
go build ./cmd/api ./cmd/worker ./cmd/mcp
```

All three service binaries require PostgreSQL and run embedded migrations at startup. The in-memory store remains available for unit tests. For database integration tests, set `TEST_DATABASE_URL` to a PostgreSQL URI for a test database whose role can create schemas and extensions. Tests create and remove isolated `docs_test_*` schemas. These tests are skipped locally when the variable is absent; the GitLab test job provisions PostgreSQL and runs them automatically.

Integration coverage includes repeated startup migrations, independently connected API/worker stores, idempotency, group concurrency, expired lease recovery, group restrictions, and document generation with mocked Ollama. No real model or private repository is needed.

Phase 4 adds PostgreSQL inference queue integration tests and structured document generation unit tests. The initial model is not pulled or invoked by tests. Queue integration tests are skipped without `TEST_DATABASE_URL`; the project's GitLab test job supplies it.

## Phase 2 and Phase 3

Phase 2 ingestion now authenticates upload, polling, and downloads; validates supplied project/branch/commit metadata; scopes reads to project and namespace; handles signing-key rotation; limits requests; and rejects unsafe archive types and paths on Linux and Windows. The CI template uses authentication on every call, bounded HTTP requests, and transient HTTP retries. Deploy the updated CI template before the updated API.

Phase 3 snapshots include a deterministic source inventory and warnings for skipped oversized files or parse failures. Per-file analysis is bounded at 2 MiB; nonregular files, dependency/build directories, tests for Go runtime analysis, and old generated documents are excluded from relevant analysis. Evidence carries source files and line numbers where available, never full source snippets.

| Analyzer | Current extraction | Limits |
| --- | --- | --- |
| Go | Parsed modules, sums, imports, packages, structs/GORM model fields, Gin literal routes and groups, handlers, middleware references, `os.Getenv` keys | No whole-program type resolution; dynamic paths are unavailable; cross-function router groups and model relationships are not resolved. |
| Java | Maven XML dependencies, module names, Spring component/route annotations, security/Actuator dependencies | Source annotations use conservative lexical extraction, not a complete Java AST. Effective Maven inheritance, full route composition, entity relationships, and security rule semantics are unresolved. Gradle detection remains limited. |
| Node | Runtime/development package dependencies, Next.js route exports, Express literal route candidates, environment keys | Express receiver binding and mounted prefixes remain inferred; computed routes, middleware semantics, and TypeScript type resolution are unresolved. |
| Infrastructure | Docker image/ports/user/probe declarations; YAML resources, images, ports, replicas, resource quantities, probe declarations, Secret/ConfigMap references; Compose services; CI jobs/stages/include references; Helm chart names; Kustomize references; Terraform declaration names | No script execution, Helm rendering, remote CI include resolution, or Terraform evaluation. Service selectors and runtime traffic are not inferred. |

Mermaid arrows represent parsed Go imports or explicit Ingress/HTTPRoute/OpenShift Route references to a uniquely identified Service supplied in the archive. Undetected or ambiguous targets are not invented. The worker persists the complete analysis snapshot and normalized evidence in PostgreSQL before calling Ollama, retaining evidence even if generation fails. Snapshots are retained across retries; SQL evidence IDs are scoped to a job to avoid collisions between projects or commits. Successful bundles also contain the full evidence and inventory in `evidence.json`. Schema version `0.3.0` creates fresh jobs for previously ingested commits.

Fixtures in `internal/analyzer/testdata/go-app` and `spring-app` are static analysis inputs, not deployed services. Tests run these repositories through analysis and mocked document generation without downloading their dependencies or executing application code. PostgreSQL, real GitLab OIDC/pipeline execution, and real Ollama still require environment integration validation.
