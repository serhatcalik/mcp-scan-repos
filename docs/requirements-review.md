# Requirements review and Notion publication

Reviewed on 2026-10-12 against the original platform specification. This is a source and automated-test review; it does not certify a running GitLab, PostgreSQL, Ollama, or Kubernetes installation.

## Requirement coverage

| Area | Current implementation | Remaining work or limitation |
| --- | --- | --- |
| Processes and interfaces | Gin API, worker, official MCP Go SDK with Streamable HTTP, PostgreSQL, Ollama provider interface | Only Ollama is implemented as a provider, as required initially. |
| CI ingestion | Main-only archive upload, asynchronous polling/download, configuration-derived idempotency | Every poll/download requires a valid original OIDC token; no job-scoped download credential exists. Verify GitLab token lifetime against long CPU inference. |
| OIDC | Signature, issuer, audience, expiry, optional not-before/issued-at checks, explicit project/namespace allowlists, exact main branch, optional protected-ref check, SHA comparison, JWKS refresh | The validation follow-up added issued-at checks and denial for empty allowlists. Project path can be empty. SHA is required even where a GitLab version might omit that claim. Validate actual instance claim compatibility. |
| Archives | Upload/body bounds, extracted-byte/file limits, traversal and special-entry rejection, isolated cleanup | Retention/cleanup of stored archives and artifacts needs an operational policy. |
| Group scheduling | PostgreSQL group enablement, allowed projects, concurrency, transactional leasing and expired-lease recovery | Group policy, model config, timeout and archive limit are schema fields without runtime enforcement. Worker uses global timeout/model settings; API uses global archive limit. |
| Retry lifecycle | Expired active leases retry up to an attempt limit; failed uploads can be resubmitted; Ollama has request retries | Ordinary worker failures become terminal immediately. No automatic job backoff scheduling, attempt-row recording, or lease-owner fencing on writes. |
| Static analysis | Go AST, structured module/package/POM/YAML parsing; Go and Spring fixtures; language/infrastructure evidence | Java/JS routes and Terraform use lexical extraction. Complex routing, entity relationships, dependency wiring and full language semantics are incomplete. Uploaded Helm templates are not executed. |
| Evidence and documents | Redacted snapshots, normalized source evidence, separate papers, deterministic Mermaid, metadata and validation artifacts | Validation verifies citation membership, not semantic truth. Original paper topics such as disaster recovery and design patterns are not all guaranteed as explicit sections. Redaction is pattern-based. |
| Persistence | Migrations, jobs, snapshots, evidence, generated documents and MCP catalog | `projects`, `job_attempts`, `ingestion_requests`, `documentation_artifacts` and `publication_records` have schema but no runtime inserts. Projects are listed from jobs; artifact bundles live on shared storage. |
| MCP | Scoped credentials, project authorization, document/evidence access and regeneration | Credential rotation and deployment still require operational provisioning. |
| Kubernetes | Helm, independent replicas, restricted workloads, probes, services, private Ollama/PVC, network policies, optional Gateway API and ServiceMonitor | No HPA; RWO storage co-location restricts cross-node availability. Actual capacity, TLS, model pull, DNS/CNI and rollout need cluster verification. |
| Observability | JSON logs, job/analysis/LLM/queue/upload metrics, operational documentation | Durable ingestion/attempt audit records remain missing. |
| Tests | Security, analyzers, generation, API, MCP, queue and chart tests exist | PostgreSQL tests skip without `TEST_DATABASE_URL`. Live CI-to-Ollama-to-artifact validation is still required. |
| Notion | Worker publisher, group targets, worker-only token file, durable queue/page mappings, API/MCP status and URLs | Added after this review; see [Notion publishing](notion.md). Live verification requires operator credentials. |

Relevant implementations: [OIDC](../internal/auth/oidc.go), [API](../internal/api/router.go), [job scheduling](../internal/jobs/postgres.go), [worker](../internal/jobs/jobs.go), [document catalog](../internal/jobs/catalog.go), [schema](../migrations/001_init.sql), and [deployment verification](phase6.md#verification).

## Proposed Notion behavior

The proposal below records the initial review. The implemented first release uses stable project pages with immutable generation snapshots. Its supported configuration and behavior are documented in [Notion publishing](notion.md).

Use a shared parent page as the documentation folder, with a separate configured parent per GitLab group. Notion uses pages/subpages rather than filesystem folders. Share the parent with readers and grant a Notion internal connection access to that page. Reader sharing and connection access are separate configuration steps. See [Notion subpages](https://www.notion.com/en-gb/help/create-a-subpage) and [connection authorization](https://developers.notion.com/guides/get-started/authorization).

Suggested layout:

```text
Engineering Documentation
  Backend
    Project name [GitLab project ID]
      Technical Paper
      Infrastructure Paper
      Architecture
      Human Notes
```

The worker updates only pages it owns. Keep human notes separate. Include project identity, commit, generation time, evidence citations and verification labels. Keep the authoritative evidence and downloadable artifacts in platform storage. Verify Mermaid presentation in Notion before promising rendered diagrams; preserve diagram source regardless.

## Proposed configuration contract (not yet implemented)

Store non-secret target configuration in PostgreSQL, associated with a GitLab group. Only trusted operators may change destination and credential mappings; uploaded repository configuration must never select the destination or token.

```yaml
# Proposed operator configuration; current binaries do not load this schema.
notion:
  enabled: true
  parent_page_id: "configured-group-parent-page-id"
  credential_ref: "backend-notion"
  publication_mode: "update-managed-pages"
  publish_technical_paper: true
  publish_infrastructure_paper: true
  publish_diagrams: true
```

`credential_ref` maps to a worker-mounted Secret file. Store tokens in Kubernetes Secrets or an external secret manager; never in ConfigMaps, group JSON, evidence, prompts, generated metadata or logs. Mount tokens only into workers. A single token can serve groups in one workspace, but separate connections allow narrower access. A token grants external Notion access and does not change the credential-free GitLab ingestion design.

Give the connection only the content capabilities required to read, create and update the configured hierarchy, without user-information permissions. Validate access to the target before accepting publication work. Scope HTTP requests to the fixed Notion API endpoint and prevent authorization forwarding on redirects.

## Durable publication workflow

1. Generate, validate and persist artifacts as today.
2. In a database transaction, complete the documentation job and enqueue a publication record with a unique job/target/configuration-version key. Recover historical completed jobs with an explicit backfill operation.
3. Workers lease publication work independently using PostgreSQL. Keep publication status separate from generation status; CI artifact retrieval remains available during a Notion outage.
4. Persist project/document page mappings and content hashes. Serialize writes per target/project and prevent older generations from replacing newer ones. Recover interrupted creates by reconciling identity markers before retrying; a local unique constraint alone cannot prevent duplicate remote pages after an ambiguous response.
5. Publish or update managed content, recording remote IDs, safe errors, attempt count, lease ownership and next retry time. Expose publication status and page URLs through authorized API/MCP reads.
6. Use bounded requests and rate coordination across replicas sharing a connection. Respect `Retry-After`, classify permission/authentication errors, and reconcile uncertain writes before retrying. See [Notion request limits and write retry guidance](https://developers.notion.com/reference/request-limits).

Extend `publication_records` with queue fields and uniqueness constraints; add explicit publication-target and page-mapping tables. Implement a focused `internal/publication/notion` client with context cancellation, HTTP timeouts, response bounds, token-safe errors and mock-server tests. Notion now documents [Markdown page updates](https://developers.notion.com/reference/update-page-markdown); validate its supported Markdown/features and pin the API version when implementing the client.

Required tests include disabled destinations, group isolation, worker-only credentials, rate limits, partial/ambiguous writes, restart recovery, duplicate jobs, concurrent replicas, stale generation protection and preservation of human notes.

## Recommended implementation order

First enforce group runtime settings and complete automatic job retries/audit records and lease fencing. Then add durable Notion publication, worker-only Secret mounting, configuration documentation and mock integration tests. Finish with real PostgreSQL integration checks and one controlled end-to-end CI job that publishes into a private test parent page.
