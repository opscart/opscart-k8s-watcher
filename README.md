# opscart-k8s-watcher

**Version:** 0.3  
**Purpose:** Production-grade Kubernetes security auditing with multi-cluster support and HTML reporting  
**Focus:** CIS compliance, HTML reports, and multi-cluster analysis

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

### Multi-Cluster Support (v0.2)
- **Config management** - Centralized cluster configuration
- **Multi-cluster scanning** - Scan all clusters with `--all-clusters`
- **Cluster groups** - Scan by environment with `--cluster-group production`
- **Side-by-side comparison** - Compare security posture with `--compare=a,b`
- **Sequential execution** - Clear, readable output for multiple clusters

### HTML Reports (v0.3)
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

### Cost Optimization
- Idle resource detection
- Spot instance recommendations
- Resource right-sizing opportunities
- Potential savings estimation

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

# Checkout v0.3
git checkout v0.3

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

### Other Commands
```bash
# Resource analysis
./opscart-scan resources --cluster CLUSTER

# Cost analysis
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

## Version History

### v0.3 (Current - February 2026)
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
- Identified spot instance optimization opportunities
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

### v0.4 (Planned - 4-6 weeks)
**From v0.2 Promises:**
- ~~HTML report generation~~ ✅ Delivered in v0.3
- ~~JSON/CSV output formats~~ ✅ Delivered in v0.3
- Full diff view for cluster comparison
- Network policy detection

**Enhanced Comprehensive Reports:**
- Per-namespace breakdown with resource metrics
- Idle workload detection details
- Enhanced cost optimization breakdown
- Container image analysis
- PVC storage analysis
- Historical trends

**Goal:** Comprehensive HTML report should have full parity with CLI output detail level.

### v0.5 (Future)
- Prometheus integration for metrics
- Grafana dashboard templates
- Webhook notifications (Slack, email)
- Custom policy definitions
- Multi-cluster aggregated dashboard

---

## Contributing

Key areas for contribution:
1. Additional security checks
2. Enhanced report templates
3. Integration with other tools
4. Network policy detection
5. Cluster comparison enhancements

---

## License

MIT License - See LICENSE file for details

---

## Support

- **Issues:** GitHub Issues
- **Documentation:** [opscart.com](https://opscart.com)
- **Author:** Shamsher Khan (IEEE Senior Member)

---

## Acknowledgments

Built for production pharmaceutical environments handling 500+ cores across multiple Fortune 500 clients. Validated in enterprise Kubernetes deployments with strict compliance requirements.

**Version:** v0.3  
**Status:** Production-ready for multi-cluster security auditing  
**Last Updated:** February 2026