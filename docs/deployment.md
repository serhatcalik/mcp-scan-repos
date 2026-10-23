# Deployment

Deploy into the `gitlab-docs` namespace with Helm:

First provision the [MCP credential Secret](phase5.md), or set `mcp.enabled=false` until it exists. Vanilla Kubernetes needs the UID/group overrides in `deploy/examples/values-kubernetes.yaml`; OKD can use `deploy/examples/values-okd.yaml`. See [Phase 6](phase6.md) for a complete configured install, routing, and monitoring.

```sh
helm upgrade --install gitlab-doc-platform ./deploy/helm/gitlab-doc-platform --namespace gitlab-docs --create-namespace
```

The chart includes API, MCP, worker, optional Ollama, Services, ServiceAccounts, NetworkPolicies, Gateway API HTTPRoute, and ServiceMonitor.

HTTPRoutes and ServiceMonitor are disabled by default; enable them only when the relevant CRDs/controllers are installed and their Helm values are configured.

Ollama is deployed as a private StatefulSet by default. Pull the initial model after the pod is ready:

```sh
kubectl -n gitlab-docs exec statefulset/gitlab-doc-platform-ollama -- ollama pull qwen2.5-coder:7b
```

Set `ollama.enabled=false` and provide `llm.endpoint` to use an external private Ollama instance.

## API and Worker Storage

The API and worker mount the same PVC at `storage.mountPath` (default `/var/lib/gitlab-docs`), which is also set as `STORAGE_DIR`. Without this writable mount, the non-root API cannot create archive storage and returns HTTP 500.

The chart creates `gitlab-doc-platform-data` with `ReadWriteOnce` filesystem access by default, suitable for a Ceph RBD storage class. API and worker default to one replica each, with required pod affinity placing both on the same node. `ReadWriteOnce` allows multiple pods on that node to share the filesystem. Deployments use `Recreate` updates in this configuration, which causes downtime during updates. See [Kubernetes access modes](https://kubernetes.io/docs/tasks/administer-cluster/change-pv-access-mode-readwriteoncepod/) and [self-affinity scheduling](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/).

Select your cluster's RBD storage class with `storage.storageClassName`, or supply an existing claim with compatible access and volume modes:

```yaml
storage:
  existingClaim: gitlab-docs-shared
```

The claim must exist in the deployment namespace and be writable by both services. Keep `storage.coLocate: true` with RBD/ReadWriteOnce storage. Both services need sufficient capacity on the same node; this configuration does not provide availability across nodes. A per-pod `emptyDir` does not let workers read API uploads. On OKD, let its security policy assign the allowed volume group. Elsewhere, set `storage.fsGroup` to a group permitted by your cluster and ensure the containers use a permitted non-root UID. Existing storage permissions may require administrator changes.

For multiple replicas across nodes, use a shared filesystem storage class such as CephFS or NFS, then configure:

```yaml
api:
  replicas: 2
worker:
  replicas: 2
storage:
  accessModes: ["ReadWriteMany"]
  coLocate: false
  storageClassName: <your-shared-filesystem-storage-class>
```

An RBD provisioning error saying multi-node access is supported only for block volumes means the class cannot provision the requested RWX filesystem. Do not change this chart to `volumeMode: Block`: the application reads and writes files, not raw devices.

### Replacing a Failed Unbound Claim

Existing PVC access modes cannot be changed in place. If the previous RWX claim failed to provision, first inspect it:

```sh
oc -n gitlab-mcp get pvc gitlab-doc-platform-data \
  -o jsonpath='{.status.phase}{" volume="}{.spec.volumeName}{"\n"}'
```

Only if it is `Pending` with no bound volume, confirm it has no data, scale the API and worker to zero, delete that failed claim, and upgrade the corrected chart with your usual values overrides. Scaling down removes the pods holding PVC protection finalizers. The upgrade recreates the claim as RWO and restores replicas from the values. Do not delete the Ollama claim. If the documentation claim is Bound or has a volume assigned, preserve it and arrange a migration to a compatible claim instead of deleting it.

API, worker, and MCP use PostgreSQL through the same `DATABASE_URL` Secret reference. On startup they connect, apply embedded migrations once under a database advisory lock, and log `database ready`. The database role must own the target schema or have permissions to create the migration tables and supported extensions. Startup fails if the database or migrations are unavailable; it does not silently fall back to an in-memory store. An existing complete manually installed initial schema is adopted; a partial schema is rejected.

The first authorized upload registers its validated GitLab namespace as an enabled group with concurrency one. To prevent further uploads or scheduling for that group, disable its `gitlab_groups` row. Existing rows are never re-enabled by ingestion. API `/readyz` checks database connectivity.

## Ollama Storage Permissions

The Ollama volume is mounted at `/var/lib/ollama`. `HOME` points there so Ollama can create `.ollama/id_ed25519`, and `OLLAMA_MODELS` points to `/var/lib/ollama/models`. The latter preserves the existing volume's `models/` layout when upgrading from the previous `/root/.ollama` mount. See [Ollama's model directory configuration](https://github.com/ollama/ollama/blob/main/envconfig/config.go).

On OKD/OpenShift, leave explicit user and group IDs unset so the namespace's security policy can assign allowed values. The storage driver must support writing the volume using the assigned group. On standard Kubernetes, where no policy assigns a non-root UID or volume group, supply an override such as:

```yaml
ollama:
  podSecurityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    seccompProfile:
      type: RuntimeDefault
```

Use IDs permitted by your cluster. If permission errors persist at `/var/lib/ollama`, inspect the pod's assigned `fsGroup` and the volume's ownership; an existing volume or an NFS export may require storage administrator changes. Upgrading changes the pod template and restarts Ollama without replacing its PVC.
