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

Find the current Authentication section and replace with:
markdown## Authentication

OpsCart ships with a safety-net authentication layer on by default — the
dashboard is never silently open, even if you skip configuration entirely.

### Default behavior: auto-generated credentials

If no credentials are configured, OpsCart generates a random password on
startup and prints it once to the pod logs:
2026-07-16 09:12:03  auth: no credentials configured — generated one-time password
2026-07-16 09:12:03  auth: username=admin password=x7K9-mQ2p-Rt4w
2026-07-16 09:12:03  auth: set OPSCART_AUTH_USER / OPSCART_AUTH_PASS (or authSecretName
in values.yaml) to use a stable password across restarts

Retrieve it with:
```bash
kubectl logs deploy/opscart-watcher -n opscart-system | grep "auth:"
```

This password is regenerated on every pod restart unless you configure a
stable one — by design, so there is no default credential to discover or
leave unrotated.

### Stable credentials (two ways)

**Environment variables** (simplest):
```yaml
auth:
  username: "admin"
  passwordEnv: "OPSCART_AUTH_PASS"   # set via a Secret-backed env var
```

**Kubernetes Secret reference** (if you prefer managing credentials as a
Secret directly):
```yaml
auth:
  existingSecret: "opscart-auth"
  secretUsernameKey: "username"
  secretPasswordKey: "password"
```

Either path is optional — if neither is set, the auto-generated flow above
applies. There is no configuration value that disables authentication
entirely; this is deliberate. If you are fronting OpsCart with oauth2-proxy
(see below) and want to avoid a double login prompt, terminate basic auth
at the ingress layer instead of removing it from OpsCart.

### Startup validation

On every start, OpsCart logs one explicit line about its auth state:
2026-07-16 09:12:03  auth: WARNING — using auto-generated password (see above).
Configure OPSCART_AUTH_USER/PASS for a stable credential.

or, when configured:
2026-07-16 09:12:03  auth: basic auth configured (source: env)

This is intentionally a log line, not a UI element — it is aimed at the
person deploying the chart, checked once at rollout, not at end users of
the dashboard.

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

### Until authentication is configured

The dashboard has no protection beyond network placement. Recommended
deployment postures, in order of preference:

1. **Behind an authenticated ingress** (see above) — safe for shared/team use
2. **`kubectl port-forward`** — safe for individual use; traffic is
   encrypted via the existing API server tunnel and never leaves your
   machine's authenticated kubectl session
3. **ClusterIP only, no ingress** — safe only if every user with cluster
   network access is already trusted at the level OpsCart's data warrants

**Do not** expose the OpsCart Service via a public LoadBalancer or
unauthenticated Ingress. There is nothing between an anonymous HTTP request
and full read access to your cluster's operational state.

### What's next (v1.10)

Native OIDC support with namespace-scoped authorization — where dashboard
visibility maps to the viewer's own Kubernetes RBAC — is planned for v1.10.
That's a genuinely different problem (OpsCart needs to know *who* is asking
to filter *what* they see) and can't be solved by a reverse proxy alone.
The interim pattern above remains valid until then; v1.10 adds
authorization on top of it, not a replacement for it.

## Reporting a vulnerability

[Standard disclosure process — email/GitHub security advisory link]
