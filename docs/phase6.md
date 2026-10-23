# Phase 6: Deployment and Observability

## Installation

Use the built commit-SHA image tag, not `latest`. The chart retains the Nexus repository `mcp-gitlab`; the project pipeline updates the image tag after pushing a build. Ensure Nexus exposes that tag. Configure `imagePullSecrets` when registry authentication is required.

API, worker, and MCP all read `DATABASE_URL` from Secret `cluster-with-metrics-app`, key `fqdn-uri` by default. The database is PostgreSQL. For example:

```text
postgres://gitlab_docs:URL_ENCODED_PASSWORD@cluster-with-metrics-rw.gitlab-mcp.svc.cluster.local:5432/gitlab_docs?sslmode=require
```

Use the actual operator-provided connection URI rather than this placeholder. Startup applies embedded migrations under a PostgreSQL advisory lock. The worker now persists generated documents, and migration 003 adds catalog lookup indexes without deleting existing data.

Provision the [MCP credential Secret](phase5.md) before enabling MCP. Disable with `--set mcp.enabled=false` until it is available. Gateway API and Prometheus Operator CRDs are optional: their chart resources are disabled by default.

```sh
helm lint deploy/helm/gitlab-doc-platform
helm upgrade --install gitlab-doc-platform deploy/helm/gitlab-doc-platform \
  --namespace gitlab-mcp --create-namespace \
  -f deploy/examples/values-okd.yaml \
  --set-string gitlab.allowedGroups=11527771 \
  --set-string gitlab.allowedProjects=51703321
```

On vanilla Kubernetes use `deploy/examples/values-kubernetes.yaml` instead. It sets non-root Ollama UID and volume groups explicitly. OKD must retain its SCC-assigned UID/groups; do not grant privileged/anyuid SCC. Workloads use a ServiceAccount with token automount disabled, dropped capabilities, seccomp, probes, requests/limits, and no Kubernetes API permissions. API/MCP/worker roots are read-only, with writable `/tmp`. Ollama stores HOME/models on its PVC.

## Routing and Network Policy

Use an existing TLS-enabled Traefik Gateway. Enable routes with:

```sh
helm template gitlab-doc-platform deploy/helm/gitlab-doc-platform \
  --namespace gitlab-mcp \
  --set gateway.enabled=true \
  --set gateway.name=traefik \
  --set gateway.namespace=traefik \
  --set gateway.sectionName=https \
  --set gateway.apiHostname=docs-api.example.com \
  --set gateway.mcpHostname=docs-mcp.example.com
```

Apply those values through Helm/GitOps. Gateway listeners must permit routes from `gitlab-mcp`; certificates and the Gateway itself are managed separately. Only `/api/v1` and `/mcp` are routed. Ollama, metrics, and health endpoints are not exposed by these HTTPRoutes.

Default-deny policies select this release only. Allow lists admit gateway namespaces, monitoring namespaces, DNS, PostgreSQL, and worker-to-Ollama traffic. Set the namespace lists to actual cluster namespaces. Tighten `databaseNamespaceSelector`/`databasePodSelector` to your database and `httpsCIDRs` to approved destinations where practical; defaults allow TCP 5432 to cluster pods and outbound HTTPS for GitLab OIDC/model downloads. External databases require `externalDatabaseCIDRs`. External Ollama requires `ollama.enabled=false`, the matching `llm.endpoint`, and `externalOllamaCIDRs`/`ollamaPort`. DNS interception, service NAT, and NetworkPolicy behavior depend on the cluster CNI; verify connectivity in-cluster.

## Storage and Replicas

API and workers share archives/artifacts. RBD filesystem volumes should use `ReadWriteOnce` with `storage.coLocate=true`; affinity keeps consumers on one node. Do not request multi-node filesystem access on RBD. For cross-node replicas use an actual RWX filesystem claim, set `storage.accessModes=[ReadWriteMany]`, and disable co-location. Helm rejects RWO without co-location. Existing claims must actually provide the declared access mode.

API, MCP, and worker replicas are independently configurable. PostgreSQL provides shared scheduling, group concurrency, leases, and inference admission. MCP has no shared-filesystem dependency. Ollama's bundled StatefulSet is intentionally limited to one replica; manually increasing workers does not increase model inference concurrency. Tune group limits and `llm.maxConcurrentRequests` only after measuring available model capacity. Automatic HPA is not installed. RWO co-location allows multiple replicas on one node but is not cross-node high availability.

Processes handle termination; workers cancel processing and persist interrupted jobs as failed when the database remains reachable. Re-submit the CI job or request MCP regeneration to restart these jobs. If a worker dies without persisting a terminal state, expired leases are recovered through normal scheduling within the retry limit. A rollout can therefore interrupt long generation instead of delaying termination for its full runtime. Health checks remain responsive while inference runs. Keep the CI poll deadline above measured generation duration; `DOCUMENTATION_TIMEOUT_SECONDS=5400` and a 100-minute CI job timeout accommodate the previously observed 37-minute run.

## Metrics and Debugging

API serves `/metrics` on its HTTP port (keep public API routing restricted to `/api/v1`). MCP serves health/readiness/metrics only on its separate internal port 9091 and `-mcp-metrics` ClusterIP Service; its public listener serves only `/mcp`. Workers serve health/readiness/metrics on 9090. Enable `serviceMonitor.enabled=true` only with Prometheus Operator installed. The monitor selects explicitly labeled metrics Services, not the public MCP Service. Add `serviceMonitor.additionalLabels` matching your Prometheus ServiceMonitor selector. Namespace selection must include the release namespace. For OKD, user workload monitoring must be enabled by the cluster administrator.

Metrics include accepted/completed/failed jobs, job duration, analysis duration, inference attempts/duration/reported tokens, active worker jobs, queued jobs, upload bytes, HTTP status counts, and Go/process metrics. Queue depth comes from PostgreSQL on every scrape, not process memory. `documentation_queue_scrape_success=0` indicates a failed queue query; a false zero queue depth is not emitted. Labels deliberately exclude job IDs, projects, paths, and credentials.

Useful PromQL:

```promql
max(queued_worker_jobs)
sum(active_worker_jobs)
sum(rate(documentation_jobs_failed_total[15m]))
sum(rate(llm_requests_total{outcome="error"}[15m]))
histogram_quantile(0.95, sum by (le) (rate(documentation_job_duration_seconds_bucket[1h])))
```

Use `max`, not `sum`, for queue depth across replicas: each service reads the same shared database. Counters and active-worker gauges are process-local; aggregate those across the appropriate scrape targets. Tokens only count successfully returned Ollama responses, not incomplete/timed-out generation.

```sh
oc -n gitlab-mcp get pods -o wide
oc -n gitlab-mcp logs -f -l app.kubernetes.io/component=worker --prefix=true
oc -n gitlab-mcp port-forward service/gitlab-doc-platform-worker 9090:9090
curl --fail http://localhost:9090/readyz
curl --fail http://localhost:9090/metrics
```

Worker logs carry job, project, group, commit, phase, error, and duration. Readiness checks the database independently of model latency. If the queue remains nonzero while active workers are zero, check group enablement/concurrency, retry scheduling, worker readiness, and logs. If active workers remain nonzero, inspect analysis/inference duration and the private Ollama logs. Auth failures, database outages, model timeouts, and artifact-write failures remain distinct errors.

## Verification

`go test ./...` covers MCP SDK calls, authorization boundaries, grouped Mermaid validation, metrics, and Helm-rendered defaults/optional profiles. PostgreSQL integration tests require `TEST_DATABASE_URL`; the root CI test job supplies PostgreSQL. The dedicated chart CI job installs pinned Helm and runs rendering tests. No live cluster is changed by these tests. Deployment verification still requires an in-cluster rollout, Secret provisioning, gateway checks, PVC mounting, and a real CI-generated job.
