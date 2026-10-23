# Phase 5: Private MCP Access

The official MCP Go SDK serves Streamable HTTP at `/mcp` on port 8081. The endpoint only reads already-ingested projects and queues regeneration from their stored archives. It never clones repositories or needs a GitLab personal access token. CI ingestion retains its separate GitLab OIDC authentication.

Only `/mcp` is served on the public listener. The former root endpoint `/` is no longer supported. Configure your Cloudflare/Traefik route to forward `/mcp` unchanged to Service `gitlab-doc-platform-mcp`, port 80; do not strip the path. Bearer authentication remains mandatory, even on a public hostname.

Metrics and health checks use a separate management listener (`MCP_MANAGEMENT_ADDR=:9091`, Helm `mcp.metricsPort: 9091`) and ClusterIP Service `gitlab-doc-platform-mcp-metrics`. Never expose that Service through an ingress, tunnel, or public route. The ServiceMonitor scrapes it automatically when enabled. With `networkPolicy.enabled=true`, only configured monitoring namespaces can access this port. A ClusterIP alone does not restrict access from other cluster workloads when NetworkPolicy is disabled.

After deploying the newly built image and updated chart, verify public exposure:

```sh
curl -i https://YOUR_MCP_HOST/metrics  # 404
curl -i -X POST https://YOUR_MCP_HOST/mcp  # 401 without a bearer token
oc -n gitlab-mcp port-forward service/gitlab-doc-platform-mcp-metrics 9091:9091
# In a second terminal:
curl --fail http://localhost:9091/metrics
```

## Credentials

Each client has a platform-issued random bearer token, explicit GitLab project IDs, and optional regeneration permission. Only the SHA-256 hash is mounted on the server. Missing/invalid credentials prevent startup; missing, invalid, and expired bearer tokens return HTTP 401. Wildcard project scopes are forbidden. This is private bearer authentication, not an OAuth authorization server; use an MCP client that supports custom Authorization headers.

Example provisioning on the bastion with OpenSSL and jq installed:

```sh
umask 077
MCP_TOKEN=$(openssl rand -hex 32)
MCP_HASH=$(printf %s "$MCP_TOKEN" | openssl dgst -sha256 -r | awk '{print $1}')
jq -n --arg hash "$MCP_HASH" '{clients: [{
  id: "docs-reader",
  token_sha256: $hash,
  project_ids: ["51703321"],
  can_regenerate: false
}]}' > mcp-clients.json
oc -n gitlab-mcp create secret generic gitlab-doc-platform-mcp-auth \
  --from-file=clients.json=mcp-clients.json \
  --dry-run=client -o yaml | oc apply -f -
```

Store `$MCP_TOKEN` in the client's secret manager, not Git, Helm values, pipeline logs, or prompts. Configure its URL as `https://YOUR_MCP_HOST/mcp` and header `Authorization: Bearer YOUR_TOKEN`. The chart defaults to Secret `gitlab-doc-platform-mcp-auth` for release `gitlab-doc-platform`; override `mcp.authSecretName` and `mcp.authSecretKey` if needed. For local execution, set `MCP_AUTH_FILE` to the credential JSON path.

To permit regeneration, use a separate client with `can_regenerate: true` and the minimum necessary project IDs. Optional `expires_at` is a UTC RFC3339 timestamp. Credentials are read at startup: restart MCP after rotating the Secret, including to revoke a token. Defaults limit requests to 120/minute per client per replica and request bodies to 1 MiB. Browser Origin headers are rejected. Require TLS on the gateway and keep health/metrics private.

## Tools

| Tool | Arguments | Result |
| --- | --- | --- |
| `list_projects` | `limit`, `offset` | Authorized ingested projects |
| `list_documentation_jobs` | Optional `project_id`, `limit`, `offset` | Scoped jobs |
| `get_documentation_status` | `job_id` | Job status/error |
| `get_application_summary` | `job_id` or `project_id` | Persisted analysis summary |
| `get_technical_paper` | `job_id` or `project_id` | Validated technical Markdown |
| `get_infrastructure_paper` | `job_id` or `project_id` | Validated infrastructure Markdown |
| `get_application_architecture` | `job_id` or `project_id` | Application Mermaid source |
| `get_evidence` | `job_id` or `project_id`, `limit`, `offset` | Persisted, redacted evidence |
| `regenerate_documentation` | `job_id`, `request_id` | Queued job from the original archive |

Pagination defaults to 50, with maximum 100 and offset maximum 10000. Project-based document queries select the newest job, even if it is not completed; use an explicit completed `job_id` for older documents. Responses for pending jobs contain status, not stale generated content. Unauthorized projects are masked as not found. Legacy jobs without persisted document rows return an explicit regeneration message.

Example tool arguments:

```json
{"project_id":"51703321"}
```

```json
{"job_id":"d05a30e7f450181d56e983783258b680","request_id":"grouped-api-diagrams-1"}
```

Reuse `request_id` when retrying the same request. A new request ID queues fresh generation. Normal group restrictions, durable leases, retry limits, and inference admission still apply. A missing stored archive causes worker failure: re-upload from GitLab CI instead of granting repository credentials.

## Persistence and Diagrams

Workers persist validated papers and Mermaid documents in PostgreSQL before completing the job. MCP does not need an artifacts PVC. Documentation schema 0.5.0 also produces named endpoint subgraphs such as `Appointments API - /api/v1/appointments`, with method/path labels and file/line provenance in comments. When no literal route prefix is available, source component names provide the grouping. Groups are visual organization, not claims that endpoints are separate deployed services. Field-level model details remain in papers/evidence rather than the architecture graph.

Deploy the same new image for API, worker, and MCP, then regenerate an existing ingested job or rerun CI. Previously completed artifacts are not rewritten automatically.
