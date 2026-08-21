# Cost Intelligence

OpsCart estimates Kubernetes worker-node compute costs from node metadata and
allocates the priced total across namespaces and workloads. The figures use
public/list pricing and are not invoice or billing data.

## Provider and pricing support

| Provider | Detection evidence | Pricing source | Initial coverage |
|---|---|---|---|
| Azure | `spec.providerID` starts with `azure://` | Azure | `spec.providerID` starts with `azure://` | Embedded catalog + Azure Retail Prices API fallback | Compatible Azure worker VM SKUs; catalog entries may also provide Spot and RI data |
| AWS | `spec.providerID` starts with `aws://` | Optional AWS Price List Query API | EC2 Linux, shared-tenancy, no-preinstalled-software On-Demand compute in USD |
| Unknown | No supported providerID evidence | None | Pricing unavailable |
| Mixed | More than one detected provider | Provider-specific | Only nodes resolved by their own provider are included |

Provider detection also reads the standard instance-type, region, and zone
labels. EKS node groups and capacity types come from
`eks.amazonaws.com/nodegroup` and `eks.amazonaws.com/capacityType`.

## Configuration

The dashboard and `opscart-scan cloud-costs` support:

| CLI flag | Environment variable | Default | Purpose |
|---|---|---|---|
| `--cloud-provider=auto\|azure\|aws` | `OPSCART_CLOUD_PROVIDER` | `auto` | Use detected provider evidence or an explicit development override |
| `--pricing-source=auto\|embedded\|aws-api` | `OPSCART_PRICING_SOURCE` | `auto` | Enable the applicable pricing backend |
| `--region=<region>` | — | Node label | Override the region used by the effective provider |

In `auto` pricing-source mode, Azure first uses the embedded catalog. When an
Azure VM SKU is not present in the embedded catalog, OpsCart falls back to the
public Azure Retail Prices API for an exact Linux Consumption price in the
detected region. AWS pricing remains disabled unless `aws-api` is explicitly
selected.

Azure Retail Prices API fallback does not require Azure credentials. AWS queries
occur only when `aws-api` is explicitly selected. A manual AWS provider override does not
bypass this opt-in.

Provider mode defaults to `auto`: Azure and AWS providerIDs are detected,
whereas Minikube and unsupported providers remain Unknown. OpsCart never uses
Azure prices as an Unknown-provider fallback.

## Azure pricing
### Azure price resolution

OpsCart resolves Azure worker-node pricing using an embedded-first strategy:

```text
Azure node
    │
    ▼
Embedded SKU available?
    │
 ┌──┴───┐
Yes     No
 │       │
 ▼       ▼
Embedded  Azure Retail
price     Prices API
 │       │
 └───┬───┘
     ▼
Node-pool estimate
```

The embedded catalog remains the preferred source because it can provide
Pay-As-You-Go, Spot, and reservation pricing information. When a VM SKU is
missing from the catalog, OpsCart queries the Azure Retail Prices API for an
exact Linux On-Demand Consumption price in the detected region.

API-resolved SKUs do not receive fabricated Spot or reservation savings.

Azure pricing uses an embedded catalog first. Compatible node instance-type
labels are matched directly to catalog SKUs, preserving the existing bundled
Pay-As-You-Go, Spot, and reservation pricing data.

When an exact Azure VM SKU is not present in the embedded catalog, OpsCart
queries the public Azure Retail Prices API using the VM SKU and region. The
fallback selects an exact Linux, regular On-Demand Consumption hourly price and
caches successful resolutions in memory.

The Azure Retail Prices API fallback does not require Azure credentials. It does
require outbound access to `prices.azure.com`.

Spot and reservation savings are shown only when that information is available
from the embedded catalog. OpsCart does not invent Spot or RI savings for a SKU
resolved only through the Retail Prices API.

If neither exact catalog pricing nor the Retail Prices API resolves the SKU,
an actual detected Azure cluster may retain the existing Azure-only capacity
fallback for a compatible operational estimate.

## Optional AWS public pricing

AWS pricing uses the AWS SDK for Go v2 and the Price List Query API
`GetProducts` operation. The workload identity needs this IAM action:

```json
{
  "Effect": "Allow",
  "Action": "pricing:GetProducts",
  "Resource": "*"
}
```

Authentication uses the AWS SDK default credential chain. On EKS, use EKS Pod
Identity or IAM Roles for Service Accounts (IRSA); do not put access keys in
dashboard or Helm values. The workload also needs outbound HTTPS connectivity
to the AWS Pricing API. Successful price resolutions are cached in memory for
24 hours.

AWS queries require a real EC2 instance type and AWS region, either from the
standard Kubernetes labels or an explicit region override. Authorization
failures, network failures, missing metadata, and ambiguous API results leave
pricing unavailable with a warning.

Spot capacity is detected and displayed but is not priced as On-Demand. Until
EC2 Spot pricing is not integrated. Spot cost is shown as Not calculated and
excluded from the estimate.

## Coverage, allocation, and scope

Coverage reports matched nodes against total nodes. With partial coverage, the
compute total includes only resolved nodes and explicitly warns that unmatched
nodes were excluded.

Namespace and workload figures allocate that worker-node total using the
existing weighted average of CPU and memory requests. They are allocated
estimates, not charges obtained from an invoice.

The initial scope is worker-node compute only. It excludes control-plane
charges, disks and snapshots, load balancers, NAT gateways, data transfer,
monitoring, security services, support, taxes, negotiated or account-specific
discounts, and other managed-service costs. AWS estimates also exclude Savings
Plans and Reserved Instance commitments.

## Minikube Azure demonstration

Manual override is intended for local development and demonstrations. To show
embedded Azure pricing on Minikube, explicitly label the local node with a real
catalog-compatible Azure SKU and region, then select Azure:

```bash
kubectl label node minikube node.kubernetes.io/instance-type=Standard_D4s_v3 --overwrite
kubectl label node minikube topology.kubernetes.io/region=eastus2 --overwrite

OPSCART_CLOUD_PROVIDER=azure ./opscart-dashboard
# or:
./opscart-dashboard --cloud-provider=azure --region=eastus2
```

The report prominently records detected provider `unknown`, effective provider
`azure`, detection mode `manual`, and a warning that Azure was not detected.

Clean up the development labels after the demonstration:

```bash
kubectl label node minikube node.kubernetes.io/instance-type-
kubectl label node minikube topology.kubernetes.io/region-
```

Do not relabel real cloud nodes. Provider and topology labels on managed
clusters are authoritative infrastructure metadata; changing them can break
scheduling and other controllers.

## Troubleshooting

### Provider is Unknown

Inspect `spec.providerID`:

```bash
kubectl get nodes -o custom-columns=NAME:.metadata.name,PROVIDER:.spec.providerID
```

Unknown is expected for Minikube, local clusters, and unsupported providers.
Use a manual provider override only for an intentional development
demonstration.

### Pricing says Not calculated

Check the instance type, region, and capacity type:

```bash
kubectl get nodes -L node.kubernetes.io/instance-type,topology.kubernetes.io/region,eks.amazonaws.com/capacityType
```

For Azure, confirm the node reports a real Azure VM SKU and region. OpsCart
checks the embedded catalog first and, for missing SKUs, can query the Azure
Retail Prices API for an exact Linux On-Demand price. If API fallback is needed,
confirm outbound access to `prices.azure.com`.

For AWS, select `--pricing-source=aws-api`, confirm the EC2 instance type and
region, allow `pricing:GetProducts`, and verify AWS Pricing API connectivity.