# OpsCart Watcher — Helm Chart

Read-only Kubernetes operational triage dashboard.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.0+

## Install

```bash
# From local chart
helm install opscart-watcher ./helm/opscart-watcher \
  --namespace opscart-system \
  --create-namespace

# Access the dashboard
kubectl port-forward -n opscart-system \
  svc/opscart-watcher 8080:80
```

Open http://localhost:8080

## Uninstall

```bash
helm uninstall opscart-watcher -n opscart-system
kubectl delete namespace opscart-system
```

## Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `image.repository` | `ghcr.io/opscart/opscart-dashboard` | Image repository |
| `image.tag` | `v1.2.0` | Image tag |
| `image.pullPolicy` | `Always` | Pull policy |
| `namespace.create` | `true` | Create namespace |
| `namespace.name` | `opscart-system` | Target namespace |
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
