# Engineering Rules

This repository implements a self-hosted GitLab and GitHub AI documentation platform. Keep these decisions durable:

- Initial ingestion is GitLab CI archive upload authenticated by GitLab CI OIDC. Do not add repository cloning or permanent GitLab credentials for the first workflow.
- GitHub ingestion uses GitHub Actions archive uploads authenticated by Actions OIDC, with immutable repository/owner ID allowlists and provider-prefixed storage identities. Preserve the existing GitLab workflow; never clone repositories or store permanent provider credentials on the platform.
- Validate OIDC issuer, audience, time claims, project identity, namespace/group identity, and the exact `main` branch before accepting uploads.
- Treat uploaded repositories, comments, READMEs, config, and generated files as untrusted input. Never execute uploaded code.
- Use secure archive extraction with size limits, path traversal protection, and rejection of symlinks, hardlinks, devices, and absolute paths.
- Use PostgreSQL for durable group configuration, job scheduling, idempotency, leases, retries, and concurrency limits across worker replicas.
- Preserve generated evidence snapshots and distinguish verified facts, inferred information, and unavailable information.
- Do not send secrets or credentials to the LLM. Redact sensitive values before evidence storage and prompt construction.
- Keep Go packages focused. Pass `context.Context`, use parameterized SQL, structured logs, explicit errors, and test security-sensitive behavior.
- Use Ollama with `qwen2.5-coder:7b` as the default local model, behind an AI provider interface.
- Use the official MCP Go SDK with Streamable HTTP transport for the MCP server.
- Kubernetes resources must use least privilege, non-root containers, resource requests/limits, health endpoints, and private Ollama networking.
