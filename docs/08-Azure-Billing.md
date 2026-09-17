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

## Configuration

Billing is disabled unless `OPSCART_AZURE_BILLING_CONFIG` points to a valid
YAML file. No cluster is billed unless it has an explicit, `enabled: true`
entry in that file.

```yaml
# azure-billing.yaml
clusters:
  - cluster: rxr-rxp-e2e-01-cus-aks         # must exactly match --cluster/--clusters
    enabled: true
    subscriptionId: "<subscription-id>"      # placeholder — supply your real subscription ID
    aksResourceId: "/subscriptions/<subscription-id>/resourceGroups/rxr-rxp-e2e-01-cus-rg/providers/Microsoft.ContainerService/managedClusters/rxr-rxp-e2e-01-cus-aks"
    # nodeResourceGroup: "MC_rxr-rxp-e2e-01-cus-rg_rxr-rxp-e2e-01-cus-aks_centralus"
    # ^ optional. If omitted, OpsCart resolves it once from the AKS
    #   resource's own properties.nodeResourceGroup and caches it for the
    #   life of the process. Set it explicitly only if that read is not
    #   permitted for the identity OpsCart runs as.
    costBasis: ActualCost                    # ActualCost (default) or AmortizedCost
    refreshInterval: 6h                       # default; billing never refreshes more often than this
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

Token acquisition uses the maintained `azidentity` library's
`DefaultAzureCredential` chain, which transparently covers both required
paths with no separate code:

- **Local dashboard**: run `az login` once; `AzureCLICredential` in the
  chain picks up that session automatically.
- **AKS via Helm**: [Azure Workload Identity](https://azure.github.io/azure-workload-identity/docs/introduction.html)
  — `WorkloadIdentityCredential` in the chain activates from the environment
  variables AKS's Workload Identity webhook injects into the pod.

No credential is ever placed in a URL, template, log line, or browser
response.

### Local (Azure CLI)

```bash
az login                                            # placeholder — interactive login
export OPSCART_AZURE_BILLING_CONFIG=/path/to/azure-billing.yaml
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
throttling with `Retry-After`, timeouts, malformed responses, cache reuse,
stale-on-error retention, concurrent-refresh prevention, and per-cluster
isolation. `cmd/opscart-dashboard`'s tests cover the Cost page's billing
labels and confirm the disabled/estimate-only path renders unchanged.

Run:

```bash
go test ./pkg/billing/... ./cmd/opscart-dashboard/...
go test -race ./pkg/billing/... ./cmd/opscart-dashboard/...
```
