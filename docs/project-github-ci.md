# Building and Publishing This Project in GitHub Actions

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) mirrors this project's [GitLab pipeline](../.gitlab-ci.yml). It runs on branch pushes, tag pushes, and pull requests using GitHub-hosted Ubuntu runners. The existing GitLab pipeline remains available.

| Stage | Jobs and behavior |
| --- | --- |
| Test | `test` runs `go test ./...` against an isolated PostgreSQL 17 service; `chart_test` installs Helm v3.17.3, lints the chart, and runs chart tests; `vulnerability_check` runs govulncheck v1.9.0. |
| Build | `build` waits for all three checks and compiles API, worker, and MCP. |
| Publish | `docker_build_and_push` builds the Dockerfile and pushes to GHCR after a successful build, only on push events. |
| Update chart | `update_helm_image_tag` updates Helm `image.tag` after a successful image push from the default branch. |

Go setup reads the preferred toolchain from `go.mod`. Checkout and Go setup actions are pinned to commit SHAs. The database password in the test job is a disposable CI fixture, never a production credential.

## Registry and Permissions

Images are published as `ghcr.io/<owner>/<repository>`, with the owner and repository converted to lowercase. Authentication uses GitHub's automatically generated `GITHUB_TOKEN`; no personal access token or registry password needs to be added. Only the publish job requests `packages: write`, and only the chart-update job requests `contents: write`. Pull requests, including forks, receive read-only repository access and cannot publish or update the chart.

Enable Actions for the repository and permit the requested workflow token permissions under **Settings > Actions > General**. Organization policies and existing package access settings must also allow this repository to publish to its GHCR package. See [GitHub's image publishing guide](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images).

The chart-update job pushes directly to the default branch. Branch protection and rulesets must permit this bot push; `contents: write` does not bypass required pull requests or required checks. If policy blocks it, the image still publishes but the chart-update job fails. Update the Helm tag through your normal reviewed change process instead. No extra credential is configured to bypass protection.

## Image Tags and Helm Updates

| Event | Published image tags | Helm update |
| --- | --- | --- |
| Default-branch push | Full commit SHA | Sets `image.tag` to that SHA |
| Other branch push | Full commit SHA | None |
| Git-tag push | Full commit SHA and `git-<normalized-tag>` | None |
| Pull request | None | None |

For example, `v0.1.0` adds `git-v0-1-0`. Normalization lowercases tags, replaces non-alphanumeric runs with hyphens, trims edge hyphens, and limits the slug to 63 characters. Distinct Git tags can normalize to the same image tag; deployments should use the full SHA.

Chart-update jobs are serialized with a concurrency group. Each job fetches the current default branch and skips its update if files other than the values file have changed since the image's source commit. Otherwise it edits the current values file, preserving the other Helm settings, and commits with `[skip ci]`. Pushes never force-update the branch; retry a rejected concurrent push to fetch and check again. GitHub may replace a pending job with a newer one in the same [concurrency group](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency).

The update changes **only `image.tag`**. Configure `image.repository` to the actual lowercase GHCR path in your deployment values, or configure your registry proxy to expose this image under the existing repository path. For direct GHCR pulls:

```sh
helm upgrade --install gitlab-doc-platform ./deploy/helm/gitlab-doc-platform \
  --namespace gitlab-docs --create-namespace \
  --set image.repository=ghcr.io/your-owner/your-repository \
  --set-string image.tag=<full-commit-sha>
```

CI publishes the image and commits the desired tag. Argo CD synchronization or a Helm upgrade performs the deployment. New GHCR packages may be private even when the source repository is public; set the package's intended visibility separately or configure cluster image-pull credentials. The temporary CI token is not a cluster pull credential.

For uploading application repositories to the documentation platform with Actions OIDC, use the separate [GitHub ingestion guide](github-actions.md) and the examples under `github-actions/`.
