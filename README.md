# opscart-k8s-watcher

**Version:** 0.7.0  
**Purpose:** Production-grade Kubernetes security auditing and cloud cost analysis with multi-cluster support, HTML reporting, network policy analysis, and waste detection  
**Focus:** Cloud cost estimation, CIS compliance, HTML reports, network isolation, waste detection, and multi-cluster analysis

---

## Important Disclaimer

**This is a security awareness and troubleshooting tool - NOT for:**
- Compliance auditing (use kube-bench for CIS compliance)
- Financial decision-making (consult cloud architects for cost analysis)
- Production security decisions (consult security professionals)

**What it IS for:**
- Quick security posture checks
- Multi-cluster health monitoring
- Resource optimization opportunities
- War room troubleshooting
- Executive-ready HTML reports

---

## What's New in v0.7

### Cloud Cost Analysis (`cloud-costs` command)
Accurate cloud cost estimation by detecting AKS node pool VM sizes and computing real pricing from an embedded Azure retail price catalog. No external API calls required.

**Key capabilities:**
- **Node pool auto-detection** — Reads node labels (`node.kubernetes.io/instance-type`, `agentpool`, `kubernetes.azure.com/scalesetpriority`) to identify exact VM SKUs
- **Embedded Azure pricing catalog** — 40+ VM SKUs with Pay-As-You-Go, Reserved Instance (1yr/3yr), and Spot pricing
- **Region-aware pricing** — Auto-detects region from node labels or specify with `--region`; multipliers for 15+ Azure regions
- **Namespace cost allocation** — Distributes real node pool costs proportionally based on CPU + Memory resource requests
- **Deployment-level breakdown** — Optional `--breakdown deployment` shows per-deployment costs within each namespace
- **Optimization scenarios** — Right-sizing, idle workload removal, Reserved Instance recommendations with projected savings
- **Enterprise HTML dashboard** — Dark-theme report with SVG confidence ring, collapsible namespace tables, optimization cards, and sidebar navigation
- **Multi-cluster support** — Works with `--all-clusters` and `--cluster-group`

```bash
# Auto-detect everything from cluster node labels
./opscart-scan cloud-costs --cluster prod

# Specify region for pricing lookup
./opscart-scan cloud-costs --cluster prod --region eastus2

# Per-deployment cost breakdown
./opscart-scan cloud-costs --cluster prod --breakdown deployment

# Generate enterprise HTML report
./opscart-scan cloud-costs --cluster prod --format html

# Single namespace analysis
./opscart-scan cloud-costs --cluster prod -n my-namespace

# JSON output for programmatic use
./opscart-scan cloud-costs --cluster prod --format json

# All clusters
./opscart-scan cloud-costs --all-clusters
```

**Example output (real cluster with 2 node pools):**
```
☁️  CLOUD COST ANALYSIS
   Cluster: my-prod-cluster
   Region:  centralus
   Provider: Azure (embedded catalog)

💰 NODE POOL COSTS
   Pool: systempool
     VM Size: Standard_D4s_v3 (4 vCPU / 16 GB)
     Nodes:   2
     Monthly: $280.32/node → $560.64 total
     Pricing: Pay-As-You-Go | RI-1yr: $361.30 | RI-3yr: $231.50

   Pool: userpool
     VM Size: Standard_D48as_v5 (48 vCPU / 192 GB)
     Nodes:   6
     Monthly: $1,635.20/node → $9,811.20 total
     Pricing: Pay-As-You-Go | RI-1yr: $6,339.24 | RI-3yr: $4,048.86

   TOTAL CLUSTER COST: $10,371.84/mo ($124,462.08/yr)
```

**Reports saved to:** `reports/YYYY-MM-DD/cloud-costs-HHMM.html`

**Pricing disclaimers:**
- Prices are approximate — based on Azure public pricing as of 2026
- Actual costs depend on Enterprise Agreement, MACC commitments, and negotiated rates
- Use Azure Cost Management + Billing for exact billing data
- Reserved Instance savings shown are potential — requires commitment purchase
---

## What's New in v0.5.2

### HTML Reports for Waste Detection
The `waste` command now supports HTML output alongside CLI format.

```bash
# Generate HTML report (same professional format as security reports)
./opscart-scan waste --cluster prod --format html

# CLI output (default - unchanged)
./opscart-scan waste --cluster prod
```

**HTML report includes:**
- Visual scorecard showing all 9 waste categories at a glance
- Color-coded severity (red=critical, orange=warning, blue=success)
- Detailed findings with kubectl investigation commands
- Separate "Housekeeping" section for Old ReplicaSets (not counted in total)
- Kubernetes blue theme for professional/corporate environments

Reports saved to: `reports/YYYY-MM-DD/opscart-waste-HHMM.html`

---

## What's New in v0.5

### Waste & Drift Detection (`waste` command)
Detects forgotten, idle, and orphaned resources. **Suggestions only - never modifies the cluster.**

- **Abandoned namespaces** - Old namespaces with no running pods (`dev-john`, `test-2024`, `poc-ai`)
- **Zombie pods** - CrashLoopBackOff, ImagePullBackOff, OOMKilled for days
- **Unmanaged pods** - Bare pods with no controller (forgotten `kubectl run` sessions)
- **Orphaned PVCs** - Unbound, released, or bound-but-no-pod (silent storage cost leaks)
- **Stale Jobs/CronJobs** - Completed jobs not cleaned up, CronJobs that never ran, no history limits set
- **Zero-replica workloads** - Deployments and StatefulSets scaled to 0
- **Old ReplicaSets** - Leftover rollout artifacts accumulating over time
- **Services with no endpoints** - LoadBalancers flagged with cloud cost warning
- **Broken Ingresses** - Backends pointing to services with no endpoints
- **Misconfigured HPAs** - Scaling disabled or always stuck at minReplicas

Every finding includes: observed data, reason it's suspicious, and a `kubectl` command to investigate.

```bash
./opscart-scan waste --cluster prod                        # default: 7+ days old
./opscart-scan waste --cluster prod --min-age-days 30      # stricter threshold
./opscart-scan waste --cluster prod --namespace staging    # single namespace
./opscart-scan waste --all-clusters --min-age-days 14      # all clusters
./opscart-scan waste --cluster CLUSTER 2>/dev/null         # Corporate clusters: suppress harmless klog warnings
```

## Troubleshooting

### Corporate Cluster Warnings

When scanning corporate AKS/EKS clusters, you may see Kubernetes client library warnings:
```bash
W0217 11:00:42.760152 warnings.go:70] Use tokens from the TokenRequest API...
```

**Workaround:** Redirect stderr to suppress these warnings (they're harmless):
```bash
./opscart-scan waste --cluster CLUSTER 2>/dev/null
./opscart-scan network --cluster CLUSTER 2>/dev/null
./opscart-scan security --cluster CLUSTER 2>/dev/null
```

These warnings come from the Kubernetes client library (`klog`) and don't affect functionality.

---

**Example scorecard:**
```
WASTE SCORECARD
  🔴 Abandoned Namespaces:           1
  🔴 Zombie Pods (CrashLoop/OOM):    2
  🔴 Unmanaged Pods (no controller): 1
  ✅ Orphaned PVCs:                  0
  🟢 Old ReplicaSets:                2
  🟢 Misconfigured HPAs:             1
  Total waste items found:  7
```

---

## What's New in v0.4

### Network Policy Detection
- **Namespace coverage analysis** - Which namespaces have NetworkPolicies and which don't
- **Smart infrastructure filtering** - Auto-skips system namespaces using 3 strategies (no manual list needed):
  - **Pattern-based** - Covers `kube-*`, `istio-*`, `calico-*`, `tigera-*`, `cert-manager`, `ingress-nginx`, `flux-system`, `argocd`, `velero`, `longhorn-*`, `cattle-*`, `openshift-*`, `gke-*`, `azure-*`, `karpenter`, `crossplane-*`
  - **Label-based** - Detects `pod-security.kubernetes.io/enforce=privileged` system namespaces
  - **User-defined** - `--skip-namespaces ns1,ns2` for anything not covered by patterns
- **Risk-based sorting** - HIGH risk (production/staging) shown first, sorted by pod count
- **Coverage percentage bar** - Visual indicator of cluster-wide policy coverage
- **Default-deny template** - Ready-to-apply kubectl policy in recommendations
- **Multi-cluster support** - Works with `--all-clusters` and `--cluster-group`

```bash
# Scan single cluster
./opscart-scan network --cluster prod

# All clusters
./opscart-scan network --all-clusters

# Cluster group
./opscart-scan network --cluster-group production

# Skip additional namespaces not covered by auto-detection
./opscart-scan network --cluster prod --skip-namespaces monitoring,vault

# Specific namespace only
./opscart-scan network --cluster prod --namespace production
```

**Example output:**
```
NETWORK POLICY SUMMARY
Total Namespaces:         8
Protected (policies):     0
Unprotected (no policy):  8
High Risk Namespaces:     3

Coverage: [░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░] 0% 🔴 Poor

🔴 UNPROTECTED NAMESPACES (sorted by risk):
  🔴 [PROD] production (10 pods) - HIGH RISK
  🔴 [SYS]  monitoring (5 pods)  - HIGH RISK
  🔴 [STAGE] staging (3 pods)    - HIGH RISK
  🟢 [DEV]  development (2 pods) - LOW RISK
```

---

## What's New in v0.3

### HTML Report Generation
- **Security HTML Reports** - Professional security audit reports with CIS compliance scoring
- **Comprehensive HTML Reports** - Full cluster health reports with real security data
- **Date-organized storage** - Reports auto-organized as `reports/YYYY-MM-DD/`
- **Real data extraction** - All reports use actual cluster data (validated against kubectl)

### Enhanced Security Reporting
- **Deduplicated pod names** - Shows "pod-name (4 issues)" for multiple issues per pod
- **Top 5 affected resources** per finding type
- **Recommended actions** in priority order
- **Validation steps** for remediation
- **Issue count breakdown** table
- **Validated accuracy** - All counts match kubectl queries exactly

### Helper Scripts
- `scripts/view-latest.sh` - Open most recent report in browser
- `scripts/cleanup-reports.sh` - Remove old reports (configurable retention)
- `scripts/daily-reports.sh` - Generate reports for all clusters

### New Commands
```bash
# Security HTML report
./opscart-scan security --cluster prod --format=html

# Security HTML for all clusters
./opscart-scan security --all-clusters --format=html

# Comprehensive cluster report
./opscart-scan report --cluster prod --monthly-cost 5000

# Comprehensive report for cluster group
./opscart-scan report --cluster-group production --monthly-cost 50000
```

---

## Features

### ☁️ Cloud Cost Analysis (v0.7)
- **Real VM pricing** — Detects actual node pool VM SKUs from Kubernetes node labels
- **Embedded pricing catalog** — 40+ Azure VM SKUs, no API keys or external calls needed
- **Region multipliers** — Automatic pricing adjustment for 15+ Azure regions
- **Namespace cost allocation** — Proportional distribution based on CPU + Memory requests
- **Deployment breakdown** — Optional per-deployment cost drill-down
- **Optimization scenarios** — RI savings, right-sizing, idle workload identification
- **Enterprise HTML dashboard** — Dark-theme SPA with SVG charts, collapsible tables, sidebar navigation
- **Multiple output formats** — table (CLI), JSON, HTML

### 🌐 Multi-Cluster Support (v0.2)
- **Config management** - Centralized cluster configuration
- **Multi-cluster scanning** - Scan all clusters with `--all-clusters`
- **Cluster groups** - Scan by environment with `--cluster-group production`
- **Side-by-side comparison** - Compare security posture with `--compare=a,b`
- **Sequential execution** - Clear, readable output for multiple clusters

### 🗑️ Waste & Drift Detection (v0.5)
- **9 resource types** - namespaces, pods, PVCs, jobs, deployments, ReplicaSets, services, ingresses, HPAs
- **Data-driven findings** - every result shows observed data, not assumptions
- **Smart filtering** - auto-skips infrastructure namespaces (same patterns as `network` command)
- **Configurable threshold** - `--min-age-days` (default: 7)
- **HTML reports** - `--format html` for visual dashboards (v0.5.2)
- **Suggestions only** - never modifies the cluster

### 🌐 Network Policy Detection (v0.4)
- **Namespace coverage analysis** - Protected vs unprotected namespaces
- **Smart infrastructure filtering** - Auto-skips 15+ known infrastructure patterns
- **Risk-based prioritization** - HIGH/LOW risk with clear reasoning per namespace
- **Actionable output** - Ready-to-apply kubectl default-deny policy template
- **User-defined skip list** - `--skip-namespaces` for custom infrastructure namespaces

### 📊 HTML Reports (v0.3)
- **Security Reports** - CIS compliance, findings, remediation steps
- **Comprehensive Reports** - Security + resources + cost analysis
- **Date-organized storage** - Easy archival and retention management
- **Professional templates** - Executive-ready presentations

### Security Auditing
- **CIS Kubernetes Benchmark scoring** (Pod Security subset)
- **8 security check types** - Validated against kubectl
- **Environment-aware analysis** (PRODUCTION vs DEVELOPMENT)
- **Actionable remediation steps**

**Checks performed:**
- Privileged containers (CIS 5.2.1)
- Host namespace sharing (CIS 5.2.2-5.2.4)
- Root containers (CIS 5.2.6)
- Privilege escalation
- Resource limits
- Security contexts
- Service account usage
- Added capabilities

### Emergency Scanner
- Crash looping pods
- Pending pods
- Image pull failures
- High restart counts

### Cloud Cost Analysis (v0.7.0)
```bash
# Real pricing from node pool VM detection (no monthly-cost input needed)
./opscart-scan cloud-costs --cluster CLUSTER
./opscart-scan cloud-costs --cluster CLUSTER --region eastus2
./opscart-scan cloud-costs --cluster CLUSTER --breakdown deployment
./opscart-scan cloud-costs --cluster CLUSTER --format html
./opscart-scan cloud-costs --cluster CLUSTER --format json
```

### Cost Analysis (v0.6.0) — Resource-Share Mode
```bash
# Requires manual monthly-cost input for allocation
```bash
./opscart-scan costs --cluster CLUSTER
./opscart-scan costs --cluster CLUSTER --monthly-cost 8500
./opscart-scan costs --cluster CLUSTER --breakdown deployment
./opscart-scan costs --cluster CLUSTER --monthly-cost 8500 --breakdown deployment --format html
./opscart-scan costs --cluster CLUSTER --format json
```

### Resource Search
- Find resources by type (pod, deployment, service)
- Filter by name pattern or status
- Multi-cluster search support

---

## Installation

```bash
# Clone repository
git clone https://github.com/opscart/opscart-k8s-watcher.git
cd opscart-k8s-watcher

# Checkout v0.6.0
git checkout v0.6.0

# Build
go build -o opscart-scan cmd/opscart-scan/main.go

# Initialize config for multi-cluster
./opscart-scan config init

# Run
./opscart-scan --help
```

---

## Quick Start

### 1. Configure Clusters (v0.2)
```bash
# Initialize cluster config
./opscart-scan config init

# Shows your kubeconfig clusters and lets you organize them into groups
# Creates: ~/.opscart/clusters.yaml

# View configuration
./opscart-scan config show
```

### 2. Security Audit

**CLI Output:**
```bash
# Single cluster
./opscart-scan security --cluster prod

# All clusters
./opscart-scan security --all-clusters

# By cluster group
./opscart-scan security --cluster-group production
```

**HTML Report (v0.3):**
```bash
# Single cluster HTML report
./opscart-scan security --cluster prod --format=html
# Output: reports/2026-02-05/prod-security-1430.html

# All clusters HTML reports
./opscart-scan security --all-clusters --format=html
# Output: reports/2026-02-05/prod-security-1430.html
#         reports/2026-02-05/staging-security-1431.html
#         reports/2026-02-05/dev-security-1432.html
```

**HTML Report Includes:**
- CIS compliance score with progress bar (e.g., 41/100)
- Pods scanned and issues found (e.g., 47 pods, 181 issues)
- Deduplicated pod names (e.g., "kube-apiserver (4 issues)")
- Critical findings and warnings
- Recommended actions in priority order
- Validation steps
- Issue count breakdown table

### 3. Comprehensive Cluster Report (v0.3)
```bash
# Full HTML report (security + resources + cost)
./opscart-scan report --cluster prod --monthly-cost 5000
# Output: reports/2026-02-05/prod-report-1431.html

# All clusters
./opscart-scan report --all-clusters --monthly-cost 50000
```

**Comprehensive Report Includes:**
- Real CIS security score (e.g., 41/100 from actual cluster scan)
- Security findings with pod counts (3 privileged, 31 hostPath, etc.)
- Cost analysis and potential savings ($1,200-$1,800/month)
- Overall health score
- Professional HTML template

**Note:** v0.4 will add per-namespace breakdown and resource metrics to match CLI detail level.

### 4. Compare Clusters (v0.2)
```bash
# Compare two clusters side-by-side
./opscart-scan security --compare=prod,staging

# Shows:
# - CIS score difference
# - Issue count deltas
# - Environment-specific findings
```

### 5. Network Policy Analysis (v0.4)
```bash
# Check network isolation across all namespaces
./opscart-scan network --cluster prod

# All clusters
./opscart-scan network --all-clusters

# Skip namespaces not caught by auto-detection
./opscart-scan network --cluster prod --skip-namespaces monitoring,vault
```
### 6. Cloud Cost Analysis (v0.7)
```bash
# Auto-detect VM pricing from cluster node labels
./opscart-scan cloud-costs --cluster prod

# Specify region explicitly
./opscart-scan cloud-costs --cluster prod --region eastus2

# Per-deployment breakdown
./opscart-scan cloud-costs --cluster prod --breakdown deployment

# Enterprise HTML dashboard
./opscart-scan cloud-costs --cluster prod --format html
```

### 7. Waste & Drift Detection (v0.5)
```bash
# Detect forgotten/idle/orphaned resources (default: 7+ days old)
./opscart-scan waste --cluster prod

# Generate HTML report (v0.5.2)
./opscart-scan waste --cluster prod --format html

# Adjust age threshold
./opscart-scan waste --cluster prod --min-age-days 30

# Focus on specific namespace
./opscart-scan waste --cluster prod --namespace staging

# All clusters
./opscart-scan waste --all-clusters --min-age-days 14
```

---

## Commands

### Config Management (v0.2)
```bash
# Initialize cluster configuration
./opscart-scan config init

# Show current configuration
./opscart-scan config show
```

### Security Audit
```bash
# CLI output (default)
./opscart-scan security --cluster CLUSTER

# HTML report (NEW in v0.3)
./opscart-scan security --cluster CLUSTER --format=html

# JSON output
./opscart-scan security --cluster CLUSTER --format=json

# All clusters
./opscart-scan security --all-clusters

# Cluster group
./opscart-scan security --cluster-group production

# Compare two clusters
./opscart-scan security --compare=prod,staging
```

### Comprehensive Report (NEW in v0.3)
```bash
# HTML report (default)
./opscart-scan report --cluster CLUSTER --monthly-cost 5000

# JSON report
./opscart-scan report --cluster CLUSTER --format=json

# CSV report
./opscart-scan report --cluster CLUSTER --format=csv

# All clusters
./opscart-scan report --all-clusters --monthly-cost 50000

# Cluster group
./opscart-scan report --cluster-group production --monthly-cost 50000
```

### Waste & Drift Detection (NEW in v0.5)
```bash
./opscart-scan waste --cluster CLUSTER
./opscart-scan waste --cluster CLUSTER --format html  # HTML report (v0.5.2)
./opscart-scan waste --cluster CLUSTER --min-age-days 30
./opscart-scan waste --cluster CLUSTER --namespace NAMESPACE
./opscart-scan waste --all-clusters
./opscart-scan waste --cluster-group production --min-age-days 14
```

### Network Policy Analysis (NEW in v0.4)
```bash
# Scan single cluster
./opscart-scan network --cluster CLUSTER

# All clusters
./opscart-scan network --all-clusters

# Cluster group
./opscart-scan network --cluster-group production

# Specific namespace only
./opscart-scan network --cluster CLUSTER --namespace production

# Skip namespaces not auto-detected
./opscart-scan network --cluster CLUSTER --skip-namespaces monitoring,vault
```

### Cloud Cost Analysis (NEW in v0.7)
```bash
# Auto-detect VM pricing from node labels
./opscart-scan cloud-costs --cluster CLUSTER

# Specify Azure region
./opscart-scan cloud-costs --cluster CLUSTER --region eastus2

# Per-deployment breakdown
./opscart-scan cloud-costs --cluster CLUSTER --breakdown deployment

# Enterprise HTML report
./opscart-scan cloud-costs --cluster CLUSTER --format html

# JSON output
./opscart-scan cloud-costs --cluster CLUSTER --format json

# All clusters
./opscart-scan cloud-costs --all-clusters

# Cluster group
./opscart-scan cloud-costs --cluster-group production
```

### Other Commands
```bash
# Resource analysis
./opscart-scan resources --cluster CLUSTER

# Cost analysis (resource-share mode, requires --monthly-cost)
./opscart-scan costs --cluster CLUSTER --monthly-cost 5000

# Emergency scan
./opscart-scan emergency --cluster CLUSTER

# Find specific resources
./opscart-scan find pod --cluster CLUSTER --name nginx

# Cluster snapshot
./opscart-scan snapshot --cluster CLUSTER
```

---

## Helper Scripts (v0.3)

### View Latest Report
```bash
./scripts/view-latest.sh
# Opens most recent HTML report in default browser
```

### Cleanup Old Reports
```bash
./scripts/cleanup-reports.sh 30
# Removes reports older than 30 days
```

### Daily Reports for All Clusters
```bash
./scripts/daily-reports.sh
# Generates security reports for all configured clusters
# Useful for scheduled cron jobs:
# 0 6 * * * /path/to/opscart-k8s-watcher/scripts/daily-reports.sh
```

---

## Report Storage Structure (v0.3)

Reports are automatically organized by date:
```
reports/
├── 2026-02-05/
│   ├── prod-aks-security-1430.html
│   ├── prod-aks-report-1431.html
│   ├── staging-aks-security-1432.html
│   └── dev-aks-security-1433.html
├── 2026-02-04/
└── 2026-02-03/
```

**Benefits:**
- Easy archival and retention management
- Clear chronological organization
- Simple to find reports by date
- Cleanup scripts work on date folders

**Note:** `reports/` directory is in `.gitignore`

---

## Validating Report Accuracy (v0.3)

All security counts can be validated against kubectl queries:

```bash
# Validate privileged containers count
kubectl get pods --all-namespaces -o json | \
  jq '[.items[] | select(.spec.containers[]?.securityContext?.privileged == true)] | length'
# Should match tool output: 3

# Validate host path volumes
kubectl get pods --all-namespaces -o json | \
  jq '[.items[] | select(.spec.volumes[]?.hostPath != null)] | length'
# Should match tool output: 31

# Validate host network usage
kubectl get pods --all-namespaces -o json | \
  jq '[.items[] | select(.spec.hostNetwork == true)] | length'
# Should match tool output: 11

# Validate missing resource limits
kubectl get pods --all-namespaces -o json | \
  jq -r '.items[] | select(.spec.containers[] | (.resources.limits == null or .resources.limits == {})) | "\(.metadata.namespace)/\(.metadata.name)"' | sort -u | wc -l
# Should match tool output: 33
```

**Result:** All counts match exactly

---

## Use Cases

### Cloud Cost Visibility (v0.7)
```bash
# Quick cost check — no Azure portal needed
./opscart-scan cloud-costs --cluster prod

# Monthly executive report
./opscart-scan cloud-costs --cluster prod --format html

# Which teams/namespaces are spending the most?
./opscart-scan cloud-costs --cluster prod --breakdown deployment

# Cross-cluster cost comparison
./opscart-scan cloud-costs --all-clusters

# Shows:
# - Exact VM SKUs and per-node costs
# - Reserved Instance savings potential (1yr and 3yr)
# - Spot node detection
# - Namespace-level cost allocation
# - Optimization scenarios with projected annual savings
```

### Weekly Waste Review (v0.5)
```bash
./opscart-scan waste --all-clusters --min-age-days 30

# Finds real issues like:
# - Namespace 'data-processing': 9 pods, none Running, 30 days old
# - Pod 'kubernetes-dashboard': CrashLoopBackOff, 7792 restarts
# - HPA 'worker': FailedGetResourceMetric - autoscaling silently broken
# - Bare pod 'webtest-34210': no controller, sitting in default namespace
```

### Network Policy Audit (v0.4)
```bash
# Weekly network isolation check across all clusters
./opscart-scan network --all-clusters

# Focus on production only
./opscart-scan network --cluster-group production

# Shows:
# - Which namespaces have NetworkPolicies
# - Risk level per namespace (HIGH/LOW)
# - Ready-to-apply default-deny policy template
```

### Multi-Cluster Security Review (v0.2 + v0.3)
```bash
# Generate HTML reports for all production clusters
./opscart-scan security --cluster-group production --format=html

# Email reports to security team
# Reports saved in reports/2026-02-05/
```

### Cluster Health Comparison (v0.2)
```bash
# Compare prod vs staging security posture
./opscart-scan security --compare=prod,staging

# Shows:
# - CIS score: prod 73 vs staging 45
# - Critical issues: prod 2 vs staging 8
# - Recommendations for staging improvements
```

### Executive Dashboard (v0.3)
```bash
# Monthly comprehensive reports for all clusters
./opscart-scan report --all-clusters --monthly-cost 100000

# Generates professional HTML reports showing:
# - Overall security posture across all clusters
# - Cost optimization opportunities
# - Potential savings aggregated
```

### CI/CD Security Gate
```bash
# Gate deployment based on security score
SCORE=$(./opscart-scan security --cluster staging --format=json | jq '.cis_score')
if [ $SCORE -lt 60 ]; then
  echo "Security score too low: $SCORE"
  exit 1
fi
```

---

## Configuration File

After running `config init`, clusters are stored in `~/.opscart/clusters.yaml`:

```yaml
clusters:
  - name: prod-aks-01
    context: prod-aks-01-context
    groups:
      - production
      - critical
  - name: staging-aks
    context: staging-aks-context
    groups:
      - staging
  - name: dev-local
    context: minikube
    groups:
      - development
```

This enables powerful multi-cluster workflows with `--all-clusters` and `--cluster-group`.

---


## Cost Analysis — How It Works

### Cloud Costs (v0.7) — Real VM Pricing

**Pipeline:**
1. Discover all nodes via Kubernetes API
2. Group nodes by `agentpool` label → node pools
3. Read `node.kubernetes.io/instance-type` label → VM SKU (e.g., `Standard_D4s_v3`)
4. Lookup SKU in embedded Azure pricing catalog → per-node monthly cost
5. Detect spot nodes via `kubernetes.azure.com/scalesetpriority=spot`
6. Multiply by node count → total pool cost
7. Sum all pools → total cluster cost
8. Allocate to namespaces by weighted resource share

**Formula:**
```
node_cost            = AzurePricingCatalog[vm_sku] × RegionMultiplier[region]
pool_cost            = node_cost × node_count
total_cluster_cost   = Σ pool_costs
weighted_share(ns)   = (CPU_requests% + Memory_requests%) / 2
namespace_cost       = weighted_share × total_cluster_cost
deployment_share     = (dep_CPU / ns_CPU + dep_Mem / ns_Mem) / 2
deployment_cost      = deployment_share × namespace_cost
```

**Embedded Pricing Catalog (40+ SKUs):**
| Family | Examples | vCPU Range |
|--------|----------|------------|
| D-series v3/v4/v5 | D2s_v3 – D64s_v5 | 2–64 |
| E-series (memory) | E2s_v5 – E64s_v5 | 2–64 |
| F-series (compute) | F2s_v2 – F72s_v2 | 2–72 |
| B-series (burstable) | B2s – B8ms | 2–8 |
| AMD variants | D48as_v5, E48as_v5 | 48 |

**Region Multipliers:**
| Region | Multiplier |
|--------|------------|
| East US / East US 2 | 1.00 (baseline) |
| West US 2/3 | 1.00 |
| Central US | 1.02 |
| West Europe | 1.15 |
| Southeast Asia | 1.08 |
| Australia East | 1.20 |
| Brazil South | 1.35 |

### Resource-Share Mode (v0.6 `costs` command)

**Formula (requires manual `--monthly-cost` input):**
```
weighted_share(ns)   = (CPU% + Mem%) / 2
namespace_cost       = weighted_share × total_cluster_cost
deployment_share     = (dep_CPU / ns_CPU + dep_Mem / ns_Mem) / 2
deployment_cost      = deployment_share × namespace_cost
```

### Confidence Scoring
| Signal | High | Medium | Low |
|---|---|---|---|
| Share size | ≥ 10% | 3–10% | < 3% |
| Pod count | ≥ 10 pods | 3–9 pods | < 3 pods |
| Waste score penalty | > 60 → −2 | > 35 → −1 | — |


### System Namespace Exclusions
`kube-system`, `kube-public`, `kube-node-lease`, `cert-manager`, `istio-system`, `istio-operator`, `monitoring`, `prometheus`, `grafana`, `logging`, `flux-system`, `argocd`, `velero`, `ingress-nginx`, `calico-*`, `tigera-*`, `longhorn-*`

### Assumptions & Limitations
- Pricing based on Azure public retail rates (Pay-As-You-Go baseline)
- Does NOT include: disk I/O, network egress, Log Analytics, Defender for Cloud, AKS uptime SLA
- Enterprise Agreement / MACC discounts not reflected
- Reserved Instance savings are potential — requires actual commitment purchase
- Resource allocation uses requests (not actual utilization)


---

## Version History

### v0.7.0 (June 2026) — Current
- **`cloud-costs` command** — Accurate cloud cost analysis from real node pool VM pricing
- Embedded Azure pricing catalog with 40+ VM SKUs (no external API calls)
- Auto-detects VM sizes from Kubernetes node labels (`node.kubernetes.io/instance-type`)
- Region-aware pricing with multipliers for 15+ Azure regions
- Spot node detection via `kubernetes.azure.com/scalesetpriority` label
- Namespace cost allocation based on proportional CPU + Memory requests
- Per-deployment breakdown with `--breakdown deployment`
- Enterprise dark-theme HTML dashboard with:
  - SVG confidence ring showing pricing accuracy
  - Collapsible namespace cost tables
  - Optimization scenario cards with projected savings
  - Sidebar navigation
  - Reserved Instance vs Pay-As-You-Go comparison per pool
- JSON output for programmatic integration
- Reports saved to `reports/YYYY-MM-DD/cloud-costs-HHMM.html`
- Multi-cluster support (`--all-clusters`, `--cluster-group`)
- Pricing disclaimers and assumptions clearly documented in output

### v0.6.0 (May 2026) — Current
- `costs` command production-ready with FinOps-grade output
- Resource-share mode — no monthly cost required
- `--breakdown deployment` — CLI tree + HTML sub-rows
- HTML dashboard: KPI cards, share bars, waste score bars, scenario cards
- Unallocated row reconciling namespace allocations vs cluster total
- Removed spot scenarios; added right-sizing, idle workload, consolidation
- System namespaces excluded from recommendations and breakdown
- Idle pod detection: allows up to 5 restarts (init churn tolerated)
- Confidence scoring: 3-signal model (share + pod count + waste)
- Waste score bar replaces emoji in HTML

### v0.5.2 (Current - February 2026)
**HTML Reports for Waste Detection:**
- `--format html` flag for waste command
- Visual scorecard with all 9 waste categories
- Color-coded severity (red/orange/blue Kubernetes theme)
- Detailed findings with kubectl commands
- Old ReplicaSets shown separately (not counted in total)
- Same professional format as security reports

### v0.5.1 (February 2026)
**Bug Fixes:**
- Fixed context cancellation leak in waste detector
- Fixed PVC detection failing when pod listing errors
- Fixed HPA detection on older Kubernetes clusters (< 1.23)
- Added v1 HPA API fallback

### v0.5 (February 2026)
**Waste & Drift Detection:**
- `waste` command - detects forgotten, idle, and orphaned resources across 9 types
- Abandoned namespaces, zombie pods, unmanaged bare pods
- Orphaned PVCs, stale jobs, zero-replica workloads, old ReplicaSets
- Services with no endpoints, broken ingresses, misconfigured HPAs
- Data-driven findings with kubectl investigation commands
- Smart infrastructure namespace filtering (same patterns as `network` command)
- Configurable age threshold (`--min-age-days`, default: 7)
- Suggestions only - never modifies the cluster

### v0.4 (February 2026)
**Network Policy Detection:**
- Namespace coverage analysis (protected vs unprotected)
- Smart infrastructure filtering - auto-skips 15+ patterns (`kube-*`, `istio-*`, `calico-*`, `tigera-*`, `cert-manager`, `ingress-nginx`, `flux-system`, `argocd`, `velero`, `longhorn-*`, `cattle-*`, `openshift-*`, `gke-*`, `azure-*`, `karpenter`, `crossplane-*`)
- Label-based detection (`pod-security.kubernetes.io/enforce=privileged`)
- User-defined skip list via `--skip-namespaces`
- Risk-based sorting (HIGH/LOW) with clear reasoning
- Coverage percentage bar
- Ready-to-apply default-deny policy template in recommendations
- Full multi-cluster support

### v0.3 (February 2026)
**HTML Report Generation:**
- Security HTML reports with CIS scoring
- Comprehensive cluster reports with real data
- Date-organized storage (reports/YYYY-MM-DD/)
- Helper scripts (view-latest, cleanup, daily-reports)

**Enhanced Security Reporting:**
- Deduplicated pod names with issue counts
- Top 5 affected resources per finding
- Recommended actions and validation steps
- Validated accuracy against kubectl

**Format Separation:**
- Separate `securityFormat` and `reportFormat` variables
- Security defaults to CLI table output
- Report defaults to HTML output

### v0.2 (Multi-Cluster Support)
**Major Features:**
- Centralized cluster configuration (`config init`)
- Multi-cluster scanning (`--all-clusters`)
- Cluster groups (`--cluster-group production`)
- Side-by-side comparison (`--compare=a,b`)
- Sequential execution with clear output

**Real-World Findings:**
- Found production namespace idle for 70+ days
- Found staging namespace idle for 21+ days
- Scan time: ~200ms per cluster

### v0.1 (Initial Release)
**Security Improvements:**
- Removed unvalidated financial risk calculations
- Added CIS Kubernetes Benchmark scoring
- Environment-aware recommendations
- Specific resource identification
- Issue count validation

---

## Roadmap

### v0.8 (Next)
- **Real-time cost dashboard** — Embedded web server with live cluster data polling
- Azure Cost Management API integration for exact billing reconciliation
- Dated cost snapshots for trend analysis (SQLite storage)
- Multi-cluster cost aggregation in a single HTML dashboard
- HTMX/Alpine.js interactive frontend

### v0.9 (Future)
- Prometheus integration for actual CPU/memory utilization (not just requests)
- Grafana dashboard templates
- Webhook notifications (Slack, Teams, email)
- Custom policy definitions
- Full diff view for cluster comparison
- AWS/GCP pricing catalog support

---

## Contributing

Key areas for contribution:
1. Additional security checks
2. Enhanced report templates
3. Waste and cleanup detection
4. Cluster comparison diff view
5. Integration with other tools

---

## License

MIT License - See LICENSE file for details

---

## Support

- **Issues:** GitHub Issues
- **Documentation:** [opscart.com](https://opscart.com)
- **Author:** Shamsher Khan (IEEE Senior Member)

---

**Version:** v0.7.0  
**Status:** Dev/Stag/Production-ready for multi-cluster security auditing, cloud cost analysis, network policy detection, and waste detection  
**Last Updated:** June 2026
