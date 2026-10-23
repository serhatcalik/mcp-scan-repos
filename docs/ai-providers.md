# AI Providers

Select one generation backend per deployment with `llm.provider`: `ollama` (default), `openai`, or `anthropic` (Claude). All worker replicas in a release must use the same configuration. This is an operator setting, not an upload parameter: CI callers cannot choose arbitrary endpoints, credentials, or providers. Per-group provider routing is not implemented.

Ollama remains fully local with `qwen2.5-coder:7b`. Hosted providers are opt-in: selected, redacted repository evidence and prompts leave the cluster. No source archive is uploaded to either provider, and no hosted fallback is used if local inference fails. Redaction is defense in depth, not a guarantee that all sensitive information is detected; obtain repository-owner approval and review provider retention, residency, billing, and account policies before enabling cloud generation.

## OpenAI

Use [deploy/examples/values-openai.yaml](../deploy/examples/values-openai.yaml), merged after your cluster profile:

```yaml
llm:
  provider: openai
  endpoint: https://api.openai.com/v1
  model: gpt-4o-mini
  apiKeySecretName: documentation-openai
  apiKeySecretKey: api-key
  requestTimeout: 5m
ollama:
  enabled: false
```

The adapter calls the Responses API with `store: false`, no tools, and strict JSON-schema output through `text.format`. Model availability depends on your API project; the example is not a latest-model recommendation. See [official OpenAI structured-output documentation](https://developers.openai.com/api/docs/guides/structured-outputs). `store: false` does not by itself establish a zero-data-retention agreement.

## Claude

Use [deploy/examples/values-anthropic.yaml](../deploy/examples/values-anthropic.yaml):

```yaml
llm:
  provider: anthropic
  endpoint: https://api.anthropic.com/v1
  model: claude-sonnet-4-6
  apiKeySecretName: documentation-anthropic
  apiKeySecretKey: api-key
  requestTimeout: 5m
ollama:
  enabled: false
```

The adapter calls the Messages API with `anthropic-version: 2023-06-01`, no tools, and structured JSON output through `output_config.format`. Choose a model supporting this feature in your account. Unsupported string/array length constraints are removed only from the transmitted Claude schema; the prompt and local document validation retain the original restrictions. See [Anthropic structured-output documentation](https://platform.claude.com/docs/en/build-with-claude/structured-outputs).

## Credentials and Deployment

Create an existing Secret with key `api-key`. On the bastion, this Bash example prompts without putting the key in shell history or command arguments. Do not enable shell tracing:

```bash
read -rsp 'API key: ' LLM_KEY; printf '\n'
printf '%s' "$LLM_KEY" | oc -n gitlab-mcp create secret generic documentation-openai \
  --from-file=api-key=/dev/stdin --dry-run=client -o yaml | oc -n gitlab-mcp apply -f -
unset LLM_KEY
```

For Claude, use Secret name `documentation-anthropic`. Prefer your existing secret manager in production. Keys are mounted read-only into workers only, never into the shared ConfigMap, API, or MCP pods. Rotate the Secret and restart worker pods; keys are read at startup. Missing/empty/oversized credentials fail startup. No permanent GitLab credentials are introduced.

Apply a newly built application image and the updated chart together. For example:

```sh
helm upgrade --install gitlab-doc-platform deploy/helm/gitlab-doc-platform \
  --namespace gitlab-mcp \
  -f deploy/examples/values-okd.yaml \
  -f deploy/examples/values-openai.yaml
```

Keep your existing GitLab, database, storage, publication, image, and cluster overrides in the normal GitOps values. Switching an installed release to `ollama.enabled: false` removes its StatefulSet; plan separately for any existing model PVC before changing providers.

When NetworkPolicy is enabled, hosted-provider workers receive outbound TCP 443 access through `networkPolicy.httpsCIDRs`. Tighten this list to your approved egress destinations or enforce provider hostnames through your cluster's egress controls; Kubernetes NetworkPolicy CIDRs do not filter by DNS name. Custom hosted endpoints must be trusted HTTPS API base URLs including `/v1`; redirects are never followed. Azure OpenAI, Bedrock, Vertex, and OAuth-based consumer subscriptions are not supported by these adapters.

## Operation

The PostgreSQL inference queue, concurrency limits, context/output budgets, evidence citations, output screening, and publication workflows apply to every provider. Metadata records provider/model. Provider, endpoint, model, or generation-setting changes invalidate generation cache identity; rotating a key does not. Completed historical jobs are not rewritten: use MCP regeneration or submit a new commit to generate with the new provider.

`LLM_REQUEST_TIMEOUT` bounds each HTTP attempt; `WORKER_JOB_TIMEOUT` bounds the whole job including queueing and retries. Transient transport failures, HTTP 429, and 5xx responses get bounded retries with cancellable backoff and `Retry-After` handling. Delays above one minute stop internal retries and return a safe failure for job-level handling. HTTP 400/401/403, refusals, truncated outputs, malformed responses, and budget violations fail without provider fallback. Remote error bodies, prompts, and keys are not logged or persisted as errors.

Hosted adapters omit temperature for model compatibility and use each model's default sampling; `llm.temperature` currently controls Ollama only. Reasoning models can consume output budget on reasoning, so increase `llm.maxTokens` within the generator's supported range when appropriate. `llm.contextLength` is a local evidence budget, not a request to enlarge a hosted model's context window.

For non-Helm execution, set `LLM_PROVIDER`, `LLM_MODEL`, and `LLM_API_KEY_FILE` (default `/etc/llm-secret/api-key`). `LLM_ENDPOINT` defaults to the selected provider's API base; hosted models must be explicit. Ollama still defaults to its local endpoint/model and needs no API key. API and MCP must receive the same non-secret LLM settings as workers so regeneration and idempotency use the same identity.
