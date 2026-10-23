# GitLab and GitHub Documentation Platform

Self-hosted documentation generation for private GitLab and GitHub repositories. GitLab CI or GitHub Actions uploads a source archive with a short-lived OIDC ID token; the platform validates the token, stores a job, analyzes source and infrastructure files, and generates Markdown/Mermaid documentation using Ollama (default), OpenAI, or Claude.

Requires Go 1.26; the preferred toolchain and CI are pinned to Go 1.26.10, which fixes [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809). See the [verification report](docs/verification-2026-10-12.md) for historical PostgreSQL integration, security checks and remaining deployment validation.

## Components

- Documentation API: Gin HTTP API for ingestion, polling, artifact download, health, readiness, and metrics.
- Worker: leases PostgreSQL jobs, safely extracts archives, runs deterministic analysis, calls the AI provider, validates documents, and stores artifacts.
- MCP Server: official Go MCP SDK over Streamable HTTP for querying already-ingested projects and documents.
- PostgreSQL: group configuration, job state, idempotency, attempts, evidence, snapshots, and artifacts.
- Ollama: private CPU-oriented inference backend, defaulting to `qwen2.5-coder:7b`.
- Optional hosted AI: OpenAI or Anthropic (Claude), selected through Helm with worker-only API-key Secrets. See [AI provider setup](docs/ai-providers.md).
- Optional Notion publication: workers publish papers and diagrams under group-specific shared parent pages, with stable project URLs and generation snapshots. See [Notion setup](docs/notion.md).
- Optional Confluence publication: workers publish formatted papers and complete diagrams to group-specific spaces/parents, with configurable Mermaid app macros and durable recovery. See [Confluence setup](docs/confluence.md).

## Quick Start

```powershell
go test ./...
go run ./cmd/api
go run ./cmd/worker
go run ./cmd/mcp
```

Default service ports:

- API: `8080`
- MCP: `8081`
- Worker health/metrics: `9090`

The initial API uses the same handler and interfaces as the production path. Configure PostgreSQL, GitLab OIDC, allowed groups/projects, and storage paths through environment variables documented in [docs/development.md](docs/development.md).

API, worker, and MCP use the PostgreSQL database configured by `DATABASE_URL`; embedded migrations run at startup. API and worker must also share the configured storage directory. Authorized uploads automatically register new GitLab namespaces or GitHub owners for scheduling, while preserving existing group restrictions.

MCP requires a scoped credential file via `MCP_AUTH_FILE`; it deliberately refuses to start without credentials. See [MCP access](docs/phase5.md).

## GitLab CI

This project's [`.gitlab-ci.yml`](.gitlab-ci.yml) tests and compiles the Go commands, then builds and pushes the shared service image to the GitLab Container Registry. See [project CI setup](docs/project-ci.md) for runner requirements, image tags, and deployment examples.

See the [GitLab pipeline guide](docs/gitlab-pipelines.md) for setup, configuration, artifact downloads, and troubleshooting. A complete example pipeline is available in [gitlab-ci/examples/application.yml](gitlab-ci/examples/application.yml).

Application repositories include [gitlab-ci/documentation.yml](gitlab-ci/documentation.yml). The template packages source code, requests a GitLab OIDC ID token, uploads the archive to `POST /api/v1/jobs`, polls `GET /api/v1/jobs/{job_id}`, and downloads artifacts from `GET /api/v1/jobs/{job_id}/artifacts`.

## GitHub Actions

GitHub ingestion is opt-in and can run alongside GitLab. Enable it with numeric owner/repository ID allowlists, then copy [github-actions/documentation.yml](github-actions/documentation.yml) into an application's `.github/workflows/` directory. The workflow uploads the exact `main` commit to `/api/v1/github/jobs`, refreshes OIDC tokens while polling, and saves generated artifacts. See [GitHub setup](docs/github-actions.md) for Helm settings, API routes, and identity scopes.

A complete Go application example with test, build, and documentation jobs is available in [github-actions/examples/application.yml](github-actions/examples/application.yml). Copy it to `.github/workflows/application.yml` and set the `DOCUMENTATION_API_URL` Actions variable.

## Deployment

Phase 4 generation settings, distributed inference admission, confidence handling, and validation are documented in [controlled local generation](docs/phase4.md).

Paper layout, component-level application diagrams, full reference artifacts, and schema 0.6.2 rollout are documented in [document quality](docs/document-quality.md).

Helm chart sources are in [deploy/helm/gitlab-doc-platform](deploy/helm/gitlab-doc-platform). The chart deploys API, MCP, worker, optional Ollama StatefulSet, RBAC, NetworkPolicies, ServiceMonitor, and Gateway API HTTPRoute.

See [Phase 6 operations](docs/phase6.md) for credential provisioning, Kubernetes/OKD values, routing, monitoring, and scaling. Application diagrams group endpoints into named API sections with source references retained in Mermaid comments.
