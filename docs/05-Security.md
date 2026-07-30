# Security

## Read-only by design

OpsCart's Kubernetes RBAC is `get`/`list` only — no create, update, patch, or
delete verbs on any resource, including `endpointslices` (added in v1.7.1).
There is no code path in the binary that issues a write call to the
Kubernetes API. This is enforced at the RBAC layer, not just by convention:
even if the binary were compromised, the ServiceAccount it runs under cannot
mutate cluster state.

## Container posture

- `FROM scratch` — no shell, no package manager, no attack surface beyond the
  Go binary itself
- Runs as non-root, UID 65534
- Statically compiled (`CGO_ENABLED=0`), no dynamic linking
- 0 known CVEs at time of each tagged release (Trivy-scanned)
- Core scanning and embedded pricing make no outbound calls. Explicitly enabled
  AWS pricing calls the AWS Pricing API; there is no telemetry, phone-home, or
  license server.

## What's stored

The SQLite database (`opscart.db`) holds operational metadata: resource
names, namespaces, restart counts, event timestamps, severity levels. It
does not store Secret *values*, environment variable contents, or log
bodies. Treat the database file with the same sensitivity as `kubectl get`
output — it reveals cluster topology and workload names, not application
data.

## Authentication

OpsCart ships with a safety-net authentication layer on by default — the
dashboard is never silently open, even if you skip configuration entirely.

### Helm-managed credentials

When `auth.existingSecret` is empty, the Helm chart creates a
release-managed Secret. Helm uses the existing Secret data during upgrades,
so the generated username and password remain stable across pod restarts and
chart upgrades.

```bash
kubectl get secret -n opscart-system opscart-watcher-auth \
  -o jsonpath='{.data.username}' | base64 --decode
echo
kubectl get secret -n opscart-system opscart-watcher-auth \
  -o jsonpath='{.data.password}' | base64 --decode
echo
```

You can instead manage the Secret yourself. It must contain non-empty
`username` and `password` keys:

```yaml
auth:
  existingSecret: "opscart-auth"
```

`auth.existingSecret` remains supported for existing installations and for
credentials managed by an external secret controller.

### Standalone execution

Outside Helm, OpsCart accepts `OPSCART_AUTH_USER` and `OPSCART_AUTH_PASS`, or
`OPSCART_AUTH_SECRET_NAME` with mounted `username` and `password` files. If no
credentials are configured, it generates and logs a random password at
startup. That standalone password is process-local and changes after every
restart.

### Recommended pattern: authenticate at the ingress

Put OpsCart behind the same reverse-proxy authentication pattern you
already use for other internal tools. **oauth2-proxy** is the most common
choice — it supports GitHub, Google, Azure AD, Okta, and generic OIDC as
backends, and integrates directly with an NGINX or any ingress controller
that supports `auth-url`/`auth-signin` annotations.

The pattern:

```
Browser → Ingress (auth-url annotation) → oauth2-proxy → OpsCart :8080
                                              ↓
                                     Your IdP (Azure AD / Google / GitHub)
```

Unauthenticated requests are redirected to your identity provider; OpsCart
itself never sees a request that hasn't already been authenticated at the
ingress layer. OpsCart's own listener can remain plain HTTP because the
ingress terminates TLS and enforces auth before traffic ever reaches the
pod.

See `helm/opscart-watcher/values-oauth2-proxy-example.yaml` for a working
example.

**Do not** expose the OpsCart Service via a public LoadBalancer or
an unauthenticated Ingress. Basic authentication protects the dashboard, but
TLS and an identity-aware ingress remain the recommended posture for shared
or internet-reachable deployments.

## Reporting a vulnerability

[Standard disclosure process — email/GitHub security advisory link]
