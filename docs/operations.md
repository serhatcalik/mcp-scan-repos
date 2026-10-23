# Operations

Prometheus metrics exposed by API, MCP, and worker (the worker listens on port 9090):

- `documentation_jobs_total`
- `documentation_jobs_failed_total`
- `documentation_job_duration_seconds`
- `repository_analysis_duration_seconds`
- `llm_requests_total`
- `llm_request_duration_seconds`
- `llm_generated_tokens_total`
- `active_worker_jobs`
- `queued_worker_jobs`
- `archive_ingestion_bytes`
- `documentation_queue_scrape_success`
- `documentation_http_requests_total`

Example queries:

```promql
rate(documentation_jobs_failed_total[5m])
histogram_quantile(0.95, rate(documentation_job_duration_seconds_bucket[15m]))
max(queued_worker_jobs)
```

The queue gauge reads the shared PostgreSQL queue; do not sum it across replicas. See [Phase 6](phase6.md) for ServiceMonitor setup, readiness, scaling, and rollout behavior.

Troubleshooting starts with job status, job attempts, API logs keyed by job/project/group/commit, worker logs, and Ollama readiness.

API, worker, and MCP log `database ready` with `store=postgresql` after migrations succeed. The worker logs `job leased`, `extracting archive`, `analyzing repository`, `generating documents`, `writing artifacts`, and `job completed`, with job/project/group IDs. A processing error logs `job failed` and persists its error for API polling. An idle queue is not an error and does not emit a message on every poll.

For a job stuck in the database queue, inspect it alongside its group's scheduling policy:

```sql
select j.id, j.status, j.error, j.attempt_count, j.lease_expires_at,
       j.next_run_at, g.enabled, g.max_concurrent_jobs
from documentation_jobs j
join gitlab_groups g on g.id = j.group_id
where j.id = '<job-id>';
```

Worker replicas serialize leases per group to enforce concurrency. A crashed worker's expired lease can be reclaimed up to `max_attempts`; exhausted jobs are marked failed. Jobs have a total deadline configured by `WORKER_JOB_TIMEOUT`. Newly deployed PostgreSQL-backed binaries cannot recover jobs from previous processes' memory; rerun the application pipeline to upload them again.

## Ollama Model Not Found

An Ollama generation HTTP 404 can mean the model is missing. The client recognizes the bounded missing-model error and emits a safe diagnostic with the configured model name; arbitrary upstream error bodies are not logged. Otherwise check the configured endpoint or proxy route. See [Ollama error responses](https://docs.ollama.com/api/errors).

Setting `llm.model` does not download a model. Check the configured name and pull it into the running Ollama server (replace the model if you configured another one):

```sh
oc -n gitlab-mcp get configmap gitlab-doc-platform -o jsonpath='{.data.LLM_MODEL}{"\n"}'
oc -n gitlab-mcp exec statefulset/gitlab-doc-platform-ollama -- ollama list
oc -n gitlab-mcp exec statefulset/gitlab-doc-platform-ollama -- ollama pull qwen2.5-coder:7b
```

The pull stores the model on Ollama's PVC. For an external Ollama endpoint, pull the model into that server instead. If the model is already installed, check the worker's `LLM_ENDPOINT` and the server logs.

After fixing the cause, retry the application pipeline. Resubmitting the same project/commit/schema/configuration requeues a failed job using its existing archive and clears its error and attempts. Status polling alone does not requeue jobs. An operator can alternatively reset one specific failed job directly in PostgreSQL:

```sql
update documentation_jobs
set status='queued', error=null, attempt_count=0, next_run_at=null,
    lease_expires_at=null, updated_at=now()
where id='<failed-job-id>' and status='failed';
```

Only reset the failed job after its cause has been corrected and its archive is still present. The worker will pick it up automatically; a new pipeline for the same commit can poll and download its artifacts.

## Generation Timeouts

Non-streaming generation waits for the model's entire response. Configure `llm.requestTimeout` (default `30m`, exported as `LLM_REQUEST_TIMEOUT`) for each Ollama HTTP attempt, and `worker.jobTimeout` (default `75m`, exported as `WORKER_JOB_TIMEOUT`) for the whole job, including inference admission wait. Technical and infrastructure papers use up to two sequential requests. The worker logs both configured timeouts and inference limits at startup. See [Phase 4](phase4.md) for request retries, distributed admission, generation validation, and queue inspection.

The CI template polls for 90 minutes and sets a 100-minute GitLab job timeout. Increase all applicable limits together for slower inference or longer queue waits. The worker's total context deadline takes precedence over an individual HTTP request timeout. If logs still show a five-minute client timeout, verify that the running image tag contains the updated timeout code. ConfigMap changes require a worker restart to reload environment variables; failed jobs must be explicitly requeued after correcting the timeout.

The API writes structured request logs to stdout, including method, route, status, duration, and a server-generated request ID returned in `X-Request-ID`. Storage and job-store failures include the underlying error and that same ID. Authorization headers, tokens, query strings, and uploaded bodies are not included in these logs.

For example, follow every API replica in the deployed namespace:

```sh
oc -n gitlab-mcp logs -f -l app.kubernetes.io/component=api --all-containers=true --prefix=true --max-log-requests=10
```

The CI template retains `generated-docs/upload-response.json` and prints it if upload fails. `failed to prepare archive storage` or `failed to create archive` usually indicates a missing mount or inadequate volume permissions; check the correlated API log for the underlying filesystem error. If no request log appears after deploying the logging changes, confirm you are running the rebuilt image and that the request reaches this API rather than a proxy or another service.

## Applying Timeout Changes

Rebuild and deploy the worker image to use the separate `LLM_REQUEST_TIMEOUT` setting. Set Helm values `llm.requestTimeout` and `worker.jobTimeout`, then sync the chart and confirm both durations in the worker startup log. If an existing values override still sets `worker.jobTimeout: 30m`, update that override as well. Start a new application pipeline to use the updated CI polling and job deadlines.

## Model Citation Validation

A model response can fail when it cites an ID omitted from the prompt or attaches
a citation to a different component. Schema `0.4.1` binds each component to its
own selected evidence IDs. Invalid structured output gets one additional attempt
at temperature zero under the existing job deadline. Both attempts are validated;
persistently invalid output still fails, and no unsupported claims are published.

Rebuild and deploy both API and worker images so ingestion uses the updated
documentation schema cache key. Resubmit the application pipeline after deployment.
The rejected model text is not included in logs or retry prompts.
