# Azure Billing

OpsCart can show the AKS cluster's actual Azure billing total next to the
existing public/list-price worker-node estimate ([07-Cost-Intelligence](07-Cost-Intelligence.md)).
Billing is a separate, disabled-by-default feature: it never changes what
the existing estimate means, and the estimate never disappears — it is
always shown, labeled as an estimate, whether or not billing is configured.

## What this is, and is not

`pkg/billing` is a provider-neutral `BillingProvider` contract, separate
from `pkg/analyzer`'s `PricingProvider`. `PricingProvider` answers "what
would this node cost today at public/list rates." `BillingProvider` answers
"what did the Azure Cost Management API say this cluster's resources were
actually billed for a given period." The two are never conflated on the
Cost page or in code.

The Azure implementation queries the [Azure Cost Management Query API](https://learn.microsoft.com/en-us/rest/api/cost-management/query/usage)
(`type: ActualCost` or `AmortizedCost`, grouped by `ResourceId`) at exactly
two resource-group scopes:

1. The AKS cluster resource's own resource group.
2. The AKS cluster's node resource group (where node VMs/VMSS, disks, load
   balancers, and public IPs actually live).

It never queries the whole subscription and never an unrelated or shared
resource group. Costs from both scopes are summed by exact Azure resource
ID, so a resource billed in the node resource group is never missed
(filtering on the AKS resource ID alone would miss it) and nothing is
double-counted.

### What is not included, and why

The Azure Portal's AKS **Cost Analysis** view (Idle/Used/System allocation
by namespace) is built on an in-cluster OpenCost agent reconciled with
invoice data — it is not exposed through any public REST API. OpsCart does
not scrape the portal or depend on undocumented endpoints, so this
allocation view is out of scope here. Every dashboard billing figure is
resource-level Azure billing only, never a per-namespace or per-pod
Kubernetes cost split. This is stated explicitly on the Cost page
(`Coverage`/disclosure text) whenever billing is shown.

Also out of scope, and disclosed the same way:

- Shared or externally hosted resources billed outside the cluster and node
  resource groups (a hub-network egress path, shared DNS, cross-subscription
  resources).
- Historical resources that were deleted before the queried period — the
  Cost Management API is queried directly for the period, not reconstructed
  from current Kubernetes objects, so this is not actually a gap, but is
  worth calling out: a resource's billed cost is attributed even after it no
  longer exists in the cluster.

### What this total can overstate

This is a **two-resource-group billing total**, not a filtered "AKS-owned
resources only" total: it sums every resource billed inside the cluster's
own resource group and every resource billed inside the node resource
group, attributed by exact Azure resource ID. Neither resource group's
membership is independently verified by this package — the node resource
group is ordinarily AKS-managed, but it can also be set explicitly via
`nodeResourceGroup` to any value, and the cluster's own resource group is
never guaranteed to contain only AKS-related resources either. If either
resource group hosts resources unrelated to this AKS cluster, their cost is
included in this figure. The Cost page's `Coverage` text and a dedicated
disclosure both say this explicitly, every time billing is shown; OpsCart
does not attempt to filter to "cluster-only" resources, since nothing
about a resource's billing record reliably proves AKS ownership without
inventing an unsupported convention (a tagging scheme, for example) this
repository does not require operators to adopt. If precise attribution
matters for your environment, keep both configured resource groups free of
unrelated resources.

## Configuration

Billing is disabled unless `OPSCART_AZURE_BILLING_CONFIG` points to a valid
YAML file. No cluster is billed unless it has an explicit, `enabled: true`
entry in that file.

```yaml
# azure-billing.yaml
clusters:
  - cluster: rxr-rxp-e2e-01-cus-aks         # must exactly match --cluster/--clusters
    enabled: true
    authMode: workload-identity              # or azure-cli — required, no default (see Authentication below)
    subscriptionId: "<subscription-id>"      # placeholder — supply your real subscription ID
    aksResourceId: "/subscriptions/<subscription-id>/resourceGroups/rxr-rxp-e2e-01-cus-rg/providers/Microsoft.ContainerService/managedClusters/rxr-rxp-e2e-01-cus-aks"
    # nodeResourceGroup: "MC_rxr-rxp-e2e-01-cus-rg_rxr-rxp-e2e-01-cus-aks_centralus"
    # ^ optional. If omitted, OpsCart resolves it once from the AKS
    #   resource's own properties.nodeResourceGroup and caches it for the
    #   life of the process. Set it explicitly only if that read is not
    #   permitted for the identity OpsCart runs as.
    costBasis: ActualCost                    # ActualCost (default) or AmortizedCost
    refreshInterval: 6h                       # default; minimum 15m (MinRefreshInterval) — a smaller value is rejected at startup
    # managementEndpoint: https://management.azure.com
    # ^ optional; this is the default and the only host currently accepted.
    #   Azure US Government and Azure China are not yet supported — see
    #   Security below.
    period:
      mode: month-to-date                     # default: current calendar month, UTC, matching the Portal's default view
    # For portal reconciliation against a fixed historical window instead:
    # period:
    #   mode: custom
    #   start: "2026-08-17"                   # inclusive
    #   end: "2026-09-15"                     # inclusive — resolves to 23:59:59 UTC of this day
```

`subscriptionId` and `aksResourceId`'s subscription must match — this is
validated at startup, before any Azure call is attempted. `nodeResourceGroup`,
if supplied, must not equal the cluster's own resource group.

No secret ever belongs in this file — it holds only Azure resource
identifiers and reporting settings. Authentication is entirely separate
(below).

## Authentication

Every cluster's `authMode` selects **exactly one** Azure credential type
from the maintained `azidentity` library, with **no fallback** to any
other credential source: an explicit mode that fails to authenticate stays
failed for that cluster's billing until the underlying problem (missing
`az login` session, missing/misconfigured Workload Identity federation) is
fixed. This is deliberate — a generic "try several identities in order"
chain would let a misconfigured Workload Identity setup silently succeed
against some other ambient identity instead of failing visibly.

| `authMode` | Credential | Use for |
|---|---|---|
| `azure-cli` | `azidentity.NewAzureCLICredential` | The local dashboard, backed by an `az login` session |
| `workload-identity` | `azidentity.NewWorkloadIdentityCredential` | AKS via Helm, backed by the federated token AKS's Workload Identity webhook injects into the pod |

No credential is ever placed in a URL, template, log line, or browser
response.

### Local (Azure CLI)

```bash
az login                                            # placeholder — interactive login
export OPSCART_AZURE_BILLING_CONFIG=/path/to/azure-billing.yaml
# In azure-billing.yaml, set authMode: azure-cli for this cluster's entry.
./opscart-dashboard --cluster=rxr-rxp-e2e-01-cus-aks
```

### AKS / Helm (Workload Identity)

1. Create (or reuse) a user-assigned managed identity and federate it to
   this Service Account:

   ```bash
   # Placeholders — substitute your own identity, subscription, cluster,
   # and namespace names. Do not run these against a real subscription
   # without adapting every value first.
   az identity create --name <identity-name> --resource-group <identity-rg>

   az identity federated-credential create \
     --name opscart-dashboard \
     --identity-name <identity-name> \
     --resource-group <identity-rg> \
     --issuer "$(az aks show -n <aks-name> -g <cluster-rg> --query oidcIssuerProfile.issuerUrl -o tsv)" \
     --subject "system:serviceaccount:<namespace>:opscart-dashboard"
   ```

2. Grant that identity the read permissions below (see [Required Azure
   permissions](#required-azure-permissions)) — this chart never grants
   Azure roles itself.

3. Set Helm values:

   ```yaml
   serviceAccount:
     annotations:
       azure.workload.identity/client-id: "<the identity's client ID>"
   azureBilling:
     enabled: true
     workloadIdentity:
       enabled: true
     config: |
       clusters:
         - cluster: rxr-rxp-e2e-01-cus-aks
           enabled: true
           authMode: workload-identity
           subscriptionId: "<subscription-id>"
           aksResourceId: "/subscriptions/<subscription-id>/resourceGroups/rxr-rxp-e2e-01-cus-rg/providers/Microsoft.ContainerService/managedClusters/rxr-rxp-e2e-01-cus-aks"
   ```

   `azureBilling.workloadIdentity.enabled` only adds the
   `azure.workload.identity/use: "true"` pod label the webhook requires —
   it never creates or modifies the identity, the federated credential, or
   any Kubernetes RBAC. The AKS cluster must already have the Workload
   Identity + OIDC issuer features enabled.

4. `helm upgrade --install ...` (placeholder — run through your normal
   deployment process).

## Required Azure permissions

Grant these at the narrowest scope that satisfies them — never at the
subscription level unless your organization already manages access that
way.

| Permission | Scope | Purpose |
|---|---|---|
| `Microsoft.CostManagement/query/action` (e.g. built-in role **Cost Management Reader**) | The AKS cluster's own resource group, **and** its node resource group | The two Cost Management Query API calls this feature makes |
| `Microsoft.ContainerService/managedClusters/read` (e.g. built-in role **Reader**) | The AKS cluster's own resource group (or just the cluster resource) | Resolving `properties.nodeResourceGroup` when `nodeResourceGroup` is not set explicitly in the config — skip this grant if you set it explicitly |

## Network access

Outbound HTTPS to `management.azure.com` (port 443) from wherever the
dashboard runs — the local machine for the Azure CLI path, or the pod's
egress path for the Helm/Workload Identity path. No inbound access is
required.

## Runtime behavior

Each configured cluster gets its own background refresh loop
(`pkg/billing.Runtime`), independent of Kubernetes acquisition and
scanning: it refreshes on its own `refreshInterval` (default 6h), never
during page rendering, and never as a side effect of a Kubernetes scan. A
refresh failure retains and marks stale the last successful result rather
than showing $0 or blanking the figure; an explicit `unavailable` state is
shown only when no successful result has ever been retrieved. A
successful query that returns zero rows is shown distinctly from a
successful query that returns rows summing to exactly zero (a valid $0
period, e.g. fully offset by credits).

The Cost page shows **last attempted refresh** and **last successful
refresh** as two separate timestamps — the first tells you whether billing
is actively trying, the second tells you how old the shown figure actually
is. They diverge exactly when refreshes have been failing.

### Throttling and retry

Every Cost Management call honors Azure's documented throttling response:
the standard `Retry-After` header, and the Cost Management Query API's own
QPU/entity/tenant/client-specific headers
(`x-ms-ratelimit-microsoft.costmanagement-*-retry-after`; see [Manage Azure
costs with automation](https://learn.microsoft.com/azure/cost-management-billing/costs/manage-automation#response-headers)).
When more than one of these is present — more than one quota exhausted at
once — the *longest* mandated delay is honored, since retrying before every
exhausted quota's window elapses would just be throttled again.

Retries are bounded (a small, fixed number of attempts) and never wait past
this refresh's own timeout budget: if the delay Azure mandates would not
fit in the time remaining, the attempt is **deferred**, not retried
early — it fails immediately with a clear reason. That deferral also sets
a cooldown (`RetryNotBefore`) the Runtime honors on its own ticker: a
refresh tick that lands before the mandated delay has actually elapsed is
skipped entirely — no Azure call, no state change — even if
`refreshInterval` is configured shorter than that delay (or the delay
happens to exceed `refreshInterval` itself). This is what keeps a
misconfigured short interval, or an unusually long mandated wait, from
causing a request Azure would just throttle again.

### Error sanitization

A failed Azure call never surfaces its raw response content anywhere the
dashboard renders — this applies uniformly across every failure this
package can produce, not only HTTP error responses: authentication
(credential/token acquisition), transport/network errors, malformed or
oversized responses, and invalid pagination continuations are all reduced
to the same safe shape before `Snapshot.UnavailableReason` (what the Cost
page shows) is ever set. Every error is a safe classification (e.g.
"authorization failed", "throttled", "network error", "invalid or
unexpected response"), the operation that failed (e.g. "Cost Management
query for resource group \"…\""), and — when an HTTP response was actually
received — Azure's own request ID (from `x-ms-request-id`, falling back to
`x-ms-client-request-id`, itself bounded and format-checked before being
displayed) for support correlation. Context cancellation and deadline
errors pass through as-is (their fixed messages are already safe); nothing
else does.

## Security

Beyond error sanitization, a few controls exist specifically because this
package attaches a live ARM bearer token to whatever host and URL it sends
a request to:

- **Management endpoint allowlist — Azure Commercial only, for now.** A
  configured `managementEndpoint` must be a plain `https://` URL — no
  userinfo, query string, fragment, or path — to `management.azure.com`,
  currently the *only* host this package accepts. This is checked both at
  config load time and again where the provider is actually constructed. An
  arbitrary operator-supplied host is never accepted. Azure US Government
  and Azure China are deliberately not yet in this allowlist: the token
  audience (`armTokenScope`) and credential construction are not wired to
  select a matching AAD authority per cloud, and accepting those hosts
  without that wiring would produce a token whose audience never matches
  the endpoint — every sovereign-cloud call would fail authentication, not
  work partially. Support for those clouds requires adding that wiring
  first, not just adding hosts to this list.
- **No redirects.** Every ARM `http.Client` this package constructs
  rejects every HTTP redirect outright. Go's default HTTP client follows
  redirects automatically, including forwarding the `Authorization` header
  under its own same-domain/subdomain matching rule — silently allowing
  that would let an unexpected redirect (misconfiguration, or a compromised
  intermediary) receive this feature's bearer token. A rejected redirect is
  treated as permanent, never retried (the same redirect would happen
  again identically).
- **Continuation (`nextLink`) validation.** Every pagination continuation
  is checked before ever attaching a token to it: it must target the exact
  same host *and* the exact same resource-group-scoped Cost Management
  query path as the original request (not just the same host — a same-host
  continuation pointing at a different subscription or resource group is
  rejected too), and a `nextLink` that repeats a URL already followed is
  rejected as non-progressing rather than followed again.
- **Bounded responses.** A single response body is capped at 10 MiB. Rows
  accumulated across one resource-group query's pages are capped at
  50,000 — each of the two resource-group queries a refresh makes (cluster
  resource group, node resource group) has its own independent 50,000-row
  budget, so a single refresh's worst case is up to 100,000 rows total,
  not 50,000. These are independent bounds from the request-count limits
  below, so a much-larger-than-expected response cannot be read fully into
  memory regardless of how many rows accumulate.
- **Per-refresh request budget.** All HTTP attempts one refresh makes —
  both resource-group queries, every page of each, every bounded retry,
  and the node-resource-group lookup — share one bounded budget (40
  requests). The per-call bounds alone (20 pages, 4 retry attempts) would
  otherwise compound to a much higher ceiling (2 scopes × 20 pages × 4
  attempts = 160) than any real refresh should ever approach.
- **Minimum refresh interval.** `refreshInterval` below 15 minutes
  (`MinRefreshInterval`) is rejected at config validation — generous
  headroom under Cost Management's per-tenant QPU quotas even at that
  floor, and enough to catch a configuration typo (e.g. `1s`) before it
  reaches production.
- **Startup jitter.** Each cluster's first refresh is delayed by a random
  0–30s before it ever runs, so multiple clusters — or several independent
  dashboard processes started at the same instant, such as a rolling
  restart — do not all call the Cost Management API in the same moment.
  Running multiple independent dashboard processes against the same
  cluster/subscription is otherwise uncoordinated: each process's Runtime
  only knows about its own refresh loop, not any other process's. Stagger
  `refreshInterval` across processes if you run more than one against the
  same scope.

## Comparing against the Azure Portal

To reconcile a dashboard figure against the Portal's Cost Analysis for the
same cluster:

1. In the Portal, open **Cost Analysis** scoped to the subscription, and
   set the same date range the dashboard is using — the Cost page shows the
   exact `period.start`–`period.end` dates it queried. A configured
   `period.mode: custom` window makes this exact; `month-to-date` moves
   every day, so reconcile against a `custom` window for a stable
   comparison.
2. Set the Portal's **Cost type** to match `costBasis` (**Actual cost** ↔
   `ActualCost`, **Amortized cost** ↔ `AmortizedCost`).
3. In the Portal, filter to exactly the two resource groups the dashboard
   queried (shown in the Cost page's "Billing scope" text): the cluster's
   own resource group and its node resource group. A subscription-wide
   Portal total will not match — it includes resources OpsCart deliberately
   excludes.
4. Confirm currency matches (`Currency` on the Cost page).

### Diagnosing a remaining difference

- **Portal total is higher**: check whether the Portal view still includes
  a resource group OpsCart excluded, or a shared/external cost disclosed on
  the Cost page (hub networking, cross-subscription resources). These are
  a documented, deliberate scope boundary, not a bug.
- **Dashboard shows `stale`**: the last refresh failed; the shown total is
  the last successful one, not necessarily today's. Check dashboard logs
  for the refresh error.
- **Dashboard shows `unavailable`**: no successful refresh has ever
  completed. Check the required permissions above and dashboard logs for
  the authentication or authorization error.
- **Dashboard shows "No billed usage for this period"**: the API returned
  zero rows for both resource groups for the exact queried window — confirm
  the period actually has billed activity for this cluster.
- **Diagnosing an authentication/authorization failure**: the dashboard's
  error never contains a raw Azure response — only a safe status (e.g.
  "authentication failed"), the operation that failed, and Azure's request
  ID. Open an Azure support case (or check Azure AD sign-in logs) with that
  request ID rather than expecting the dashboard to show more detail.
- **Refresh keeps deferring instead of succeeding**: this means Azure is
  mandating a wait longer than this refresh's own timeout budget on every
  attempt — a sustained throttling condition, not a one-off. Check whether
  something else is also calling the Cost Management API against the same
  scope (throttling quotas are shared per tenant, not per caller).
- **Dashboard shows "request budget exhausted"**: an unusual number of
  retries or pagination pages were needed within one refresh (see
  Security's per-refresh request budget above). This most likely means
  sustained throttling or an unusually large result set — check the
  throttling guidance above first.
- **Dashboard total looks higher than expected for "the cluster"**: read
  [What this total can overstate](#what-this-total-can-overstate) — this is
  a two-resource-group total, not a filtered cluster-only total.

Only a live query against a real subscription can confirm an exact match.
Nothing in this repository's automated tests contacts Azure (see
[Tests](#tests)) — treat any total in this document, this chart's example
values, or this repository's tests as illustrative, never as a claim that a
specific real total was reproduced.

## Tests

`pkg/billing`'s tests use synthetic `httptest` servers and a fake
`azcore.TokenCredential` — none of them contact Azure. They cover: query
scope and date-boundary construction, cost-basis selection, exclusion of
resources outside the two configured resource groups, prevention of
double-counting across those two scopes, pagination, reordered response
columns, mixed-currency rejection, negative (credit) line items, valid
zero-cost results distinguished from no-data, authentication failures,
throttling with `Retry-After` and the Cost Management-specific headers
(including which one wins when several are present), timeouts, malformed
responses, cache reuse, stale-on-error retention, concurrent-refresh
prevention, and per-cluster isolation. Also covered by this round's
hardening: the management-endpoint allowlist (accepted/rejected hosts,
scheme, userinfo, query string, fragment, path), redirect rejection (and
that it fails fast rather than retrying), continuation validation beyond
host (same-scope-path enforcement, repeated-continuation rejection),
response byte and row bounds, the shared per-refresh request budget
(including that it's the same budget across the node-resource-group lookup
and the queries, not independent ones), the minimum refresh interval, the
deferred-retry cooldown (a tick during cooldown is a true no-op; a success
clears it), and deterministic startup-jitter behavior. Explicit AuthMode
selection and its no-fallback guarantee are covered in `credential_test.go`.
`cmd/opscart-dashboard`'s tests cover the Cost page's billing labels and
confirm the disabled/estimate-only path renders unchanged.

Run:

```bash
go test ./pkg/billing/... ./cmd/opscart-dashboard/...
go test -race ./pkg/billing/... ./cmd/opscart-dashboard/...
```
