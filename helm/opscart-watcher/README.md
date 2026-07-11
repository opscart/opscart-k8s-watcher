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

If the store cannot open its database the dashboard **falls back to
in-memory mode and keeps running** — the symptom is
`store: persistence disabled` in the pod logs and incident history that
resets on every restart. If you see that, check the sections below.

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

With `image.pullPolicy=Never`, kubelet requires the image to be loaded
under the **exact** `repository:tag` the chart renders — there is no
fuzzy matching:

```bash
docker build -t ghcr.io/opscart/opscart-dashboard:dev .
minikube image load ghcr.io/opscart/opscart-dashboard:dev
helm upgrade ... --set image.tag=dev --set image.pullPolicy=Never
```

## Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `image.repository` | `ghcr.io/opscart/opscart-dashboard` | Image repository |
| `image.tag` | `v1.7.0` | Image tag |
| `image.pullPolicy` | `Always` | Pull policy |
| `persistence.enabled` | `true` | Persist incident history on a PVC |
| `persistence.size` | `1Gi` | PVC size |
| `persistence.storageClassName` | `""` | StorageClass (empty = cluster default) |
| `persistence.existingClaim` | `""` | Use a pre-created PVC |
| `volumePermissions.enabled` | `false` | Root init container to chown the data volume |
| `nodeSelector` | `{}` | Pod node selector |
| `service.type` | `ClusterIP` | Service type |
| `resources.requests.cpu` | `100m` | CPU request |
| `resources.requests.memory` | `64Mi` | Memory request |
| `resources.limits.cpu` | `500m` | CPU limit |
| `resources.limits.memory` | `256Mi` | Memory limit |

## Security

- Runs as non-root (UID 65534)
- Read-only ClusterRole (get/list only)
- No cloud credentials required
- No agents, no mutations
- Image built FROM scratch
- The optional `volumePermissions` init container runs as root by
  design (one-shot chown); disabled by default