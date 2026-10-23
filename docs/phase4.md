# Phase 4: Controlled Local Generation

The worker uses the provider interface to generate technical and infrastructure papers separately. Ollama is the implemented backend; configuring another provider fails at worker startup rather than silently falling back. Inference remains CPU-oriented, with `qwen2.5-coder:7b`, one running request, and persistent model storage.

## Generation Pipeline

1. Analyze source without executing it.
2. Redact recognizable credential patterns and persist the sanitized snapshot and normalized evidence before inference.
3. Select relevant technical or infrastructure evidence, group it by component, and prioritize routes, resources, frameworks, models, and middleware over import/checksum detail.
4. Build a bounded JSON evidence prompt with fair selection across components. The budget conservatively accounts for UTF-8 bytes and reserves output capacity plus 1,024 tokens for model/template overhead; it is not an exact model tokenizer.
5. Acquire a PostgreSQL inference permit independently of the group/job lease.
6. Request structured JSON using Ollama's `format` schema, separate `system` instructions, `num_ctx`, and `num_predict`. See the [generate API](https://docs.ollama.com/api/generate) and [structured outputs](https://docs.ollama.com/capabilities/structured-outputs).
7. Validate citation membership and component scope, output bounds, sensitive patterns, and completion/truncation status.
8. Render embedded Markdown templates. Verified facts are copied from normalized verified evidence. Static inferences and model interpretation remain explicitly inferred. Missing evidence is unavailable, not proof that a feature is absent.
9. Validate required Markdown sections and the deterministic Mermaid subset, then atomically publish the artifact bundle.

Each model response contains up to six brief component summaries and three recommendations, with one to three evidence citations per interpretation. Both requests are sequential; no additional per-component model calls are made by default. A paper with no usable evidence does not invoke the model and records unavailable information. The full snapshot is retained even when the model sees only a selected subset.

## Configuration

| Environment | Helm value | Default |
| --- | --- | --- |
| `LLM_PROVIDER` | `llm.provider` | `ollama` |
| `LLM_MODEL` | `llm.model` | `qwen2.5-coder:7b` |
| `LLM_CONTEXT_LENGTH` | `llm.contextLength` | `8192` |
| `LLM_MAX_TOKENS` | `llm.maxTokens` | `1800` per request |
| `LLM_TEMPERATURE` | `llm.temperature` | `0.1` |
| `LLM_REQUEST_TIMEOUT` | `llm.requestTimeout` | `30m` per HTTP attempt |
| `LLM_RETRIES` | `llm.retries` | `2` additional transient attempts |
| `LLM_RETRY_BACKOFF` | `llm.retryBackoff` | `2s`, exponential, capped at `30s` |
| `LLM_MAX_CONCURRENT_REQUESTS` | `llm.maxConcurrentRequests` | `1` across worker replicas |
| `LLM_MAX_QUEUED_REQUESTS` | `llm.maxQueuedRequests` | `16` waiting requests |
| `WORKER_JOB_TIMEOUT` | `worker.jobTimeout` | `75m`, including inference queue wait |

HTTP 429/500/502/503/504 and transport failures can be retried. Missing models, malformed output, citation failures, truncation, request deadlines, and job cancellation are not automatically retried. Slow timed-out inference is not repeated automatically. Set `LLM_RETRIES=0` to disable transient retries. The simple `NewClient` constructor used by tests has no retries unless options enable them.

The worker rejects impossible context/output budgets and unsupported providers. Context is limited to 2,048-65,536 and output to 128-8,192 tokens, with enough space required for the actual prompt scaffold. Increasing context or output can significantly increase CPU time and memory usage; align model resources, request/job deadlines, and CI polling limits. Keep the initial one-request limit for CPU-only deployments.

## Distributed Admission

Migration `002_inference_queue.sql` adds `inference_pools` and `inference_requests`. Short transactions lock a pool row when enforcing capacity and starting FIFO requests. No database connection or advisory lock is held during model generation. Active requests renew a 90-second lease every 20 seconds; waiting requests renew while polling. Cancellation releases admission, and failed renewal cancels the provider request with a lease-loss cause. Expired rows are pruned on admission.

All replicas must use the same endpoint spelling and the same PostgreSQL database/schema. The pool key hashes the normalized configured endpoint and is shared across models at that endpoint. Aliases pointing to the same physical Ollama server are not automatically reconciled. Drain active and waiting requests before changing pool limits. A full queue fails the documentation job with a retryable-by-resubmission error, rather than accepting unbounded waiting work.

Lease coordination limits admitted clients, not arbitrary callers to Ollama or inference that a server continues after disconnect. Keep Ollama private and its `OLLAMA_NUM_PARALLEL=1` backstop for the initial deployment. Queue wait counts toward worker and CI deadlines.

Inspect admission independently of documentation job status:

```sql
select p.id, p.max_running, p.max_waiting, r.status,
       count(r.id) as requests, min(r.lease_expires_at) as earliest_expiry
from inference_pools p
left join inference_requests r on r.pool_id=p.id
group by p.id, p.max_running, p.max_waiting, r.status;
```

Worker logs distinguish `waiting for inference admission`, `inference admitted`, and `ollama request finished`. Generation request IDs use `<job-id>:technical` or `<job-id>:infrastructure`; logs include attempt, wait/duration, and token counts, never prompt or response text.

## Validation and Artifacts

Bundles now contain `validation.json` alongside papers, diagrams, `evidence.json`, and `metadata.json`. Metadata records context/output budgets, selected evidence counts, generated token totals, and generation configuration identity. The validation report records checks, omitted prompt items per paper, and limitations.

Citation validation does not prove semantic entailment. All model prose stays inferred even when its citations are valid. Sensitive-pattern screening is not comprehensive DLP; prevent credentials in source/resource names and exclude credential files in CI. Mermaid validation covers the emitted flowchart grammar and node references, not every Mermaid dialect or a browser rendering test.

Schema `0.4.0` plus the provider/model/context/output/temperature fingerprint forms the ingestion cache identity. Changing those content settings creates a new job for the same commit; changing only scheduling or retry settings does not. Keep API and worker generation settings synchronized through the shared ConfigMap. Rebuild and deploy both services, then start a new application pipeline. Existing completed bundles remain unchanged.

Unit tests cover prompt bounds/grouping, structured output, confidence separation, redaction, schema/citation rejection, HTTP retries, cancellation, response bounds, and artifact publication. PostgreSQL admission tests cover shared capacity, cancellation cleanup, lease-loss cancellation/recovery, and drained limit changes when `TEST_DATABASE_URL` is available. Real Ollama/GitLab/cluster verification is still required in the deployment environment.
