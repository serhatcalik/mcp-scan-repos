# Building and Publishing This Project in GitLab CI

The root [`.gitlab-ci.yml`](../.gitlab-ci.yml) runs Go tests, compiles the three commands, then builds the [Dockerfile](../Dockerfile) and pushes its image to this project's GitLab Container Registry. The image contains `/app/api`, `/app/worker`, and `/app/mcp`; Helm selects the command for each service.

## Runner Setup

Enable the project's Container Registry and use a Linux GitLab Runner with the Docker executor. The publish job uses TLS-enabled Docker-in-Docker, which requires privileged mode and a shared client certificate volume. Configure the runner's `config.toml`:

```toml
[[runners]]
  executor = "docker"
  [runners.docker]
    image = "docker:27.4.1-cli"
    privileged = true
    volumes = ["/certs/client", "/cache"]
```

Restart the runner after updating its configuration. If your runner requires a tag, add its tag to `docker_build_and_push` in `.gitlab-ci.yml`:

```yaml
docker_build_and_push:
  tags:
    - docker
```

The runner needs access to Docker Hub, the Go module proxy, and your GitLab registry. For a registry using an internal CA, configure that CA in both the Docker client and daemon. See GitLab's [Docker-in-Docker setup](https://docs.gitlab.com/ci/docker/using_docker_build/#use-docker-in-docker).

Authentication uses GitLab's predefined `CI_REGISTRY`, `CI_REGISTRY_IMAGE`, `CI_REGISTRY_USER`, and `CI_REGISTRY_PASSWORD` variables. No additional registry password or personal access token needs to be configured. The password is passed to `docker login` through standard input. See [GitLab registry publishing](https://docs.gitlab.com/user/packages/container_registry/build_and_push_images/).

## Published Tags

| Pipeline | Published image tags |
| --- | --- |
| Branch | `$CI_REGISTRY_IMAGE:$CI_COMMIT_SHA` |
| Default branch | Commit SHA tag; pipeline updates Helm `image.tag` to that SHA |
| Git tag | Commit SHA tag plus `git-$CI_COMMIT_REF_SLUG` |
| Merge request | Tests and Go compilation; Docker publishing is skipped |

For example, a Git tag `v0.1.0` publishes `registry.gitlab.example.com/platform/gitlab-doc-platform:git-v0-1-0` alongside the full commit SHA tag. The Git tag is normalized using GitLab's ref slug; distinct tags can normalize to the same slug, so deployments use the commit SHA. The pipeline no longer publishes `latest`.

The publish stage runs only after tests and compilation succeed. View pushed tags under the project's **Deploy > Container Registry**.

## Automatic Helm Tag Update

After a successful default-branch image push, `update_helm_image_tag` updates only `image.tag` in `deploy/helm/gitlab-doc-platform/values.yaml` to the built `CI_COMMIT_SHA`, commits it with `[skip ci]`, and pushes it to the default branch. The configured Nexus image repository remains unchanged. Argo CD can then sync the changed image reference; Kubernetes replaces pods because their image tag changed. The existing `latest` value is replaced by the first successful chart-update job.

In GitLab, enable **Settings > CI/CD > Job token permissions > Allow Git push requests to the repository**. The user starting the pipeline must also be allowed to push to the default branch, including its protected-branch rules. The job uses `CI_JOB_TOKEN`; no personal access token is needed. Job-token pushes do not trigger another pipeline. See [GitLab job-token push permissions](https://docs.gitlab.com/ci/jobs/ci_job_token/#allow-git-push-requests-to-your-project-repository).

Chart-update jobs are serialized. Before editing, the job fetches the current default branch and skips an older build if anything besides the values file has changed since its source commit. It edits the current values file, preserving other configuration changes. Pushes never force-update the branch; a concurrent commit can reject the push, in which case retry the job to fetch and check the branch again.

Branch and Git-tag pipelines publish images but do not change the deployed Helm tag. Ensure Argo CD does not override `image.tag` with `latest` in its application parameters or another values file. Nexus must proxy this GitLab repository and expose the SHA-tagged image before pods can pull it.

## Deploying a Published Image

Set the Helm repository to your project's `CI_REGISTRY_IMAGE` value and select the full commit SHA:

```sh
helm upgrade --install gitlab-doc-platform ./deploy/helm/gitlab-doc-platform \
  --namespace gitlab-docs --create-namespace \
  --set image.repository=registry.gitlab.example.com/platform/gitlab-doc-platform \
  --set-string image.tag=<full-commit-sha>
```

For a private registry, configure registry pull credentials for the deployment's Kubernetes service account. CI job credentials are temporary; use a deploy token with `read_registry` for cluster pulls. The pipeline publishes images and commits the desired Helm tag; Argo CD synchronization or a Helm upgrade applies the deployment.

For adding documentation generation to application repositories, see [gitlab-pipelines.md](gitlab-pipelines.md).
