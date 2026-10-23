# Verification: 2026-10-12

This is a historical report. Its Go 1.26.9 vulnerability results predate [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809), published on 2026-10-21. The current toolchain, CI and Docker builder use Go 1.26.10 to include that fix.

Local checks pass after security fixes and dependency updates. This report verifies the repository's implemented behavior; it does not certify every requirement in the original specification or a live production deployment.

## Results

| Check | Result |
| --- | --- |
| Full suite with PostgreSQL 17.11, race detection and uncached execution | Passed: 96 top-level tests, 156 cases including subtests; no named tests skipped |
| Cross-package statement coverage | 75.9%; coverage is evidence of exercised code, not proof of correctness |
| PostgreSQL migrations, shared job scheduling, concurrency and lease recovery | Passed |
| Evidence/document transaction rollback, idempotency and failed-job resubmission | Passed |
| PostgreSQL inference capacity, cancellation and lease loss | Passed |
| Notion durable retries, replica coordination, shared rate cooldown, permission failure and retry bound | Passed with real PostgreSQL and mocked Notion |
| Complete Go and Spring workflows | Passed: signed OIDC upload → PostgreSQL job → safe extraction/static analysis → mocked Ollama → persisted artifacts → worker Notion publication → authorized artifact/URL retrieval |
| Native builds and Linux amd64 builds for all commands | Passed |
| `go vet ./...` | Passed |
| Helm lint and default/optional/Kubernetes/OKD rendering tests | Passed |
| Actual API, worker and MCP process smoke checks | Passed: readiness, health, metrics, authentication and graceful shutdown |
| Official Go vulnerability checker v1.9.0 using Go 1.26.9 | Zero reachable vulnerabilities and zero vulnerabilities in imported packages |
| `git diff --check` | Passed |

The evidence package has no standalone tests; it is exercised by analysis, generation and workflow tests. There were no skipped named database, chart or workflow tests in the final run.

## Fixes made during verification

- Empty GitLab group and project allowlists now deny ingestion. A signature from the configured issuer alone does not authorize every project in the instance.
- OIDC validates optional issued-at claims and rejects negative/inconsistent time claims.
- Notion rejects missing block lists and cyclic/unbounded pagination, validates token characters, and preserves long server cooldowns.
- Added combined workflow coverage for both language fixtures, publication permission/rate handling, and the durable retry bound.
- The initial vulnerability scan identified 13 reachable advisories. Updated MCP SDK to v1.4.1, pgx to v5.9.2, QUIC to v0.59.1 and the required compatible Go networking/text/crypto dependencies. The final scan no longer reports reachable findings.
- Raised the module minimum to Go 1.26 and pinned the preferred toolchain, Docker builder and CI to Go 1.26.9. Verified the official `golang:1.26.9-alpine3.24` image tag exists. Runtime Alpine is now 3.24 and installs CA certificates for outbound TLS explicitly.
- Added a pinned vulnerability-check job to GitLab CI so future reachable findings fail the pipeline.

The final scanner reports one module-only advisory, [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), for the unused `golang.org/x/crypto/openpgp` package. This application does not import it, and the advisory has no patched version. This is separate from the zero imported-package/reachable findings.

## Process smoke tests

All three rebuilt binaries ran on temporary localhost ports against the test database. Each returned 200 for `/readyz`, `/healthz` and `/metrics`. API job access without a token returned 401. MCP initialization returned 401 without credentials and 200 with a scoped credential. All processes exited with status 0 after SIGTERM.

A temporary PostgreSQL instance was initialized under `/private/tmp`, bound only to localhost, and stopped after validation. No persistent database service was enabled. PostgreSQL 17 was installed through Homebrew for these tests. Existing GitLab/Notion/cluster data was not used.

## Reproduction

Use an isolated PostgreSQL test database whose role can create schemas/extensions and have Helm installed:

```sh
export TEST_DATABASE_URL='postgres://TEST_ROLE@127.0.0.1:TEST_PORT/TEST_DATABASE?sslmode=disable'
go test -race -count=1 -coverpkg=./... -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go vet ./...
go build ./cmd/api ./cmd/worker ./cmd/mcp ./cmd/set-image-tag
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/api ./cmd/worker ./cmd/mcp ./cmd/set-image-tag
helm lint deploy/helm/gitlab-doc-platform
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

At the time of this verification, the pinned toolchain was Go 1.26.9. See `go.mod` for the current pin; Go's automatic toolchain selection can download it when the local Go installation is older.

Machine-readable local artifacts from this run are `/private/tmp/gitlab-docs-tests-final.jsonl`, `/private/tmp/gitlab-docs-coverage-final.out`, `/private/tmp/gitlab-docs-smoke-result.json` and `/private/tmp/gitlab-docs-vulnerability-final.txt`. They are temporary and are not committed to the repository.

## Remaining verification and product limits

Live Notion and Ollama were mocked. Before production rollout, verify a real private Notion parent/token, page sharing and rate behavior; actual CPU model availability, generation quality and duration; real GitLab OIDC claims and token lifetime; and cluster TLS, Gateway routes, PVC permissions, NetworkPolicies and capacity.

The container image itself was not built/run because no container engine was running. Linux compilation and image-tag verification passed; Docker build/registry publishing still need the GitLab runner. Kubernetes resources were rendered and checked locally, without applying them to a cluster.

The previously identified specification gaps remain in [requirements review](requirements-review.md): ordinary generation failures do not automatically retry with backoff, some per-group policy/model/timeout/archive settings are schema-only, several audit tables are not populated, and static analysis/document semantic validation is incomplete. These are existing capability limits, not failures in this test run. Address them before claiming the entire original production specification is complete.
