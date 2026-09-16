# OpsCart Watcher — Helm Chart

Read-only Kubernetes operational triage dashboard with persistent
operational memory (incident history, timelines).

## Prerequisites

- Kubernetes 1.24+
- Helm 3.0+
- A default StorageClass (for persistence; see [Persistence](#persistence))

## Install

```bash
helm install opscart-watcher ./helm/opscart-watcher \
  --namespace opscart-system \
  --create-namespace

# Access the dashboard
kubectl port-forward -n opscart-system svc/opscart-watcher 8080:80
```

Open http://localhost:8080

> The chart does not create the namespace itself — always pass
> `--create-namespace` on first install.

## Authentication

When `auth.existingSecret` is empty, Helm creates a release-managed Secret and
preserves its generated credentials across pod restarts and upgrades. Retrieve
them with:

```bash
kubectl get secret -n opscart-system opscart-watcher-auth \
  -o jsonpath='{.data.username}' | base64 --decode
echo
kubectl get secret -n opscart-system opscart-watcher-auth \
  -o jsonpath='{.data.password}' | base64 --decode
echo
```

To manage credentials yourself, create a Secret containing `username` and
`password`, then set:

```yaml
auth:
  existingSecret: "opscart-auth"
```

The existing-Secret path remains compatible with existing installations.

## Uninstall

```bash
helm uninstall opscart-watcher -n opscart-system
```

The PVC holding incident history is **intentionally kept** on uninstall
(`helm.sh/resource-policy: keep`). To discard all operational memory:

```bash
kubectl delete pvc opscart-watcher -n opscart-system
kubectl delete namespace opscart-system   # if no longer needed
```

## Persistence

Incident history and timelines are stored in SQLite on a PVC (default
1Gi, ReadWriteOnce). Enabled by default.

| Scenario | Setting |
|----------|---------|
| Use a specific StorageClass | `--set persistence.storageClassName=<name>` |
| Use a pre-created PVC | `--set persistence.existingClaim=<name>` |
| Disable (ephemeral, history lost on restart) | `--set persistence.enabled=false` |

When persistence is enabled the Deployment uses `strategy: Recreate` —
an RWO volume cannot be mounted by the old and new pod simultaneously,
so upgrades incur brief downtime. This is expected.

When Helm persistence is enabled, failure to open the database stops startup
instead of silently using ephemeral storage. Check the pod logs and the
permissions and node-placement sections below.

### Volume permissions (hostPath provisioners, minikube default)

The pod runs as non-root (UID 65534). Some provisioners — including
minikube's default `hostpath-provisioner` — do not honor `fsGroup`, so
the volume is created root-owned and unwritable. Fix with the bundled
init container:

```bash
helm upgrade opscart-watcher ./helm/opscart-watcher -n opscart-system \
  --set volumePermissions.enabled=true
```

This runs a root `busybox` init container that chowns `/data` before the
dashboard starts. Not needed on CSI drivers that honor fsGroup
(EBS, Azure Disk, GKE PD, etc.).

### Multi-node minikube

minikube's default hostPath PVs have **no node affinity** — if the pod
is rescheduled to a different node it mounts an empty directory and
history appears to reset (incidents re-detected with new timestamps).
Options:

```bash
# Workaround: pin the pod to the node holding the volume
helm upgrade ... --set nodeSelector."kubernetes\.io/hostname"=<node>

# Preferred: use the CSI hostpath driver (real node affinity)
minikube addons enable volumesnapshots
minikube addons enable csi-hostpath-driver
helm upgrade ... --set persistence.storageClassName=csi-hostpath-sc
```

## Local development images

Official images support `linux/amd64` and `linux/arm64`.

With `image.pullPolicy=Never`, kubelet requires the image to be loaded
under the **exact** `repository:tag` the chart renders — there is no
fuzzy matching:

```bash
docker build -t ghcr.io/opscart/opscart-dashboard:dev .
minikube image load ghcr.io/opscart/opscart-dashboard:dev
helm upgrade ... --set image.tag=dev --set image.pullPolicy=Never
```

## Container log preview

The Investigation page can fetch a bounded live preview for one container at
a time, including separate application and sidecar containers such as
`app` and `istio-proxy`. Current and previous container logs are selectable;
previous logs are available only after that container has restarted.

The feature is enabled by default. Because application logs may contain
sensitive data, disable it when dashboard users should not have access to
application logs:

```bash
helm upgrade opscart-watcher ./helm/opscart-watcher \
  --namespace opscart-system \
  --reuse-values \
  --set logs.enabled=false
```

When enabled, the chart adds only `get` on the `pods/log` subresource. Each request is
limited to the last 200 lines and 256 KiB. Logs are fetched directly from
Kubernetes, returned with `Cache-Control: no-store`, and never written to the
OpsCart SQLite database. There is no combined-container view or download.

## AI analysis

AI analysis is disabled by default. It is manual and read-only: an operator
must select Generate or Regenerate for a supported active issue. The dashboard
sends only the sanitized evidence assembled by OpsCart. It does not send
Secrets, environment variables, raw Kubernetes objects, or raw logs, and it
does not execute cluster actions.

An internal-hosted, OpenAI-API-compatible endpoint is the recommended
default — sanitized evidence never leaves your cluster or private network.
A third-party provider such as OpenAI is also supported, but sends that
evidence outside your environment; treat enabling it as a deliberate
data-governance decision for your organization, not a configuration choice.

Create the credential Secret in the release namespace:

~~~bash
kubectl create secret generic opscart-ai --namespace opscart --from-literal=api-key='replace-with-provider-key'
~~~

Copy the example file and edit it for your environment — see
`values-ai.example.yaml` for the internal-hosted and third-party patterns,
with the trade-off documented inline:

~~~bash
cp helm/opscart-watcher/values-ai.example.yaml helm/opscart-watcher/values-ai.yaml
~~~

From the repository root, apply that file with its actual path:

~~~bash
helm upgrade --install opscart helm/opscart-watcher --namespace opscart --create-namespace -f helm/opscart-watcher/values-ai.yaml
~~~

The chart never creates, renders, or persists the provider credential. It reads
the selected key from the existing Secret into the dashboard process.

The current provider value is openai. The configured baseURL determines the
actual destination, so it may point to an approved internal service. That
service must implement the Responses API used by this adapter, including
POST /responses, strict structured output, and the completed response envelope.
Chat Completions compatibility alone is insufficient.

For an internal endpoint, use a cluster-reachable HTTPS URL. Within the
dashboard Pod, 127.0.0.1 refers to the Pod itself and does not reach a laptop or
a separate GPU VM.

Verify the rendered configuration without printing the Secret value:

~~~bash
kubectl rollout status deployment/opscart-watcher -n opscart

kubectl get deployment opscart-watcher -n opscart -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}{"\n"}{end}' |
  grep '^OPSCART_AI_'
~~~

The Settings page reports Enabled or Disabled. When enabled it shows only the
provider and model; the base URL, Secret name, and credential are not rendered.

## Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `image.repository` | `ghcr.io/opscart/opscart-dashboard` | Image repository |
| `image.tag` | `v1.14.0` | Image tag |
| `image.pullPolicy` | `Always` | Pull policy |
| `persistence.enabled` | `true` | Persist incident history on a PVC |
| `persistence.size` | `1Gi` | PVC size |
| `persistence.storageClassName` | `""` | StorageClass (empty = cluster default) |
| `persistence.existingClaim` | `""` | Use a pre-created PVC |
| `logs.enabled` | `true` | Enable bounded, non-persistent current/previous container log preview |
| `ai.enabled` | `false` | Enable manual AI analysis |
| `ai.provider` | `openai` | Provider adapter |
| `ai.baseURL` | `https://api.openai.com/v1` | Responses API base URL |
| `ai.model` | `""` | Required model name when enabled |
| `ai.timeout` | `30s` | Provider request timeout |
| `ai.existingSecret` | `""` | Existing Secret containing the provider credential |
| `ai.apiKeyKey` | `api-key` | Credential key within the existing Secret |
| `volumePermissions.enabled` | `false` | Root init container to chown the data volume |
| `nodeSelector` | `{}` | Pod node selector |
| `service.type` | `ClusterIP` | Service type |
| `resources.requests.cpu` | `100m` | CPU request |
| `resources.requests.memory` | `64Mi` | Memory request |
| `resources.limits.cpu` | `500m` | CPU limit |
| `resources.limits.memory` | `256Mi` | Memory limit |

## Security

- Runs as non-root (UID 65534)
- Read-only ClusterRole (`get`/`list`; includes `pods/log` access by default and removes it when `logs.enabled=false`)
- Core scanning and embedded pricing require no cloud credentials. Optional AWS
  public pricing uses workload identity; see
  [Cost Intelligence](../../docs/07-Cost-Intelligence.md).
- No agents, no mutations
- Image built FROM scratch
- The optional `volumePermissions` init container runs as root by
  design (one-shot chown); disabled by default
