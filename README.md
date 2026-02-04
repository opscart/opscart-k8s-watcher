# opscart-k8s-watcher

**Version:** 0.2 (Beta)  
**Purpose:** Emergency Kubernetes cluster scanner for war room situations  
**Focus:** Security awareness, resource optimization, and rapid troubleshooting across multiple clusters

---

## ⚠️ Important Disclaimer

**This is a security awareness and troubleshooting tool - NOT for:**
- Compliance auditing (use kube-bench for CIS compliance)
- Financial decision-making (consult cloud architects for cost analysis)
- Production security decisions (consult security professionals)

**What it IS for:**
- Quick security posture checks
- War room troubleshooting
- Resource optimization opportunities
- Trend tracking across environments
- Multi-cluster analysis and comparison

---

## Features

### 🆕 v0.2: Multi-Cluster Support
- **Scan all clusters at once** with `--all-clusters`
- **Scan by environment group** with `--cluster-group`
- **Compare two clusters** side-by-side
- **Centralized configuration** in `~/.opscart/config.yaml`
- **Sequential execution** for clear, readable output
- **Cluster identification** in all scan outputs

### 🔒 Security Auditing
- **CIS Kubernetes Benchmark scoring** (Pod Security subset)
- **Environment-aware analysis** (PRODUCTION vs DEVELOPMENT)
- **Top 5 specific resources** per issue type
- **Actionable remediation steps**

Example output:
```
🔍 Cluster: prod-aks-01

  • Containers running as root: 31
    └─ PRODUCTION: 6 (⚠️  REQUIRES IMMEDIATE ACTION)
    └─ DEVELOPMENT: 25 (acceptable for dev, monitor)
    Top resources:
      1. backend-api in namespace prod-api [PROD]
      2. web-frontend in namespace prod-web [PROD]
```

**Checks performed:**
- Privileged containers (CIS 5.2.1)
- Host namespace sharing (CIS 5.2.2-5.2.4)
- Root containers (CIS 5.2.6)
- Privilege escalation
- Resource limits
- Security contexts
- Service account usage

**Reference:** Based on [CIS Kubernetes Benchmark v1.8](https://www.cisecurity.org/benchmark/kubernetes)

### 🚨 Emergency Scanner
- Crash looping pods
- Pending pods
- Image pull failures
- High restart counts

### 📊 Resource Analysis
- Cluster capacity utilization
- Namespace resource breakdown
- Idle namespace detection
- Spot instance eligibility

### 💰 Cost Optimization
- Idle resource detection
- Spot instance recommendations
- Resource right-sizing opportunities

**Note:** Spot instance savings based on cloud provider published rates (~70-90%)

### 🔍 Resource Search
- Find resources by type (pod, deployment, service)
- Filter by name pattern or status
- Multi-cluster search support
- Quick troubleshooting

### 📸 Enhanced Snapshots
- Complete cluster state capture
- Deployment, service, PVC status
- Configuration inventory

---

## Installation

```bash
# Clone repository
git clone https://github.com/opscart/opscart-k8s-watcher.git
cd opscart-k8s-watcher

# Build
go build -o opscart-scan cmd/opscart-scan/main.go

# Initialize multi-cluster config
./opscart-scan config init

# Edit config with your clusters
nano ~/.opscart/config.yaml

# Run
./opscart-scan --help
```

---

## Quick Start (v0.2)

### 1. Configure Your Clusters

```bash
# Initialize config file
./opscart-scan config init

# Edit ~/.opscart/config.yaml
nano ~/.opscart/config.yaml
```

Example config:
```yaml
clusters:
  - name: prod-aks-01
    context: prod-aks-01-context
    group: production

  - name: prod-aks-02
    context: prod-aks-02-context
    group: production

  - name: staging-aks-01
    context: staging-aks-01-context
    group: staging

  - name: dev-aks-01
    context: dev-aks-01-context
    group: development
```

**Find your context names:**
```bash
kubectl config get-contexts
```

### 2. Verify Configuration

```bash
./opscart-scan config show
```

### 3. Scan All Clusters

```bash
# Security scan across all clusters
./opscart-scan security --all-clusters

# Resource analysis across all clusters
./opscart-scan resources --all-clusters

# Emergency scan across all clusters
./opscart-scan emergency --all-clusters
```

---

## Usage

### Multi-Cluster Commands (New in v0.2)

#### Scan All Configured Clusters
```bash
./opscart-scan security --all-clusters
./opscart-scan resources --all-clusters
./opscart-scan emergency --all-clusters
./opscart-scan costs --all-clusters --monthly-cost 5000
```

#### Scan by Cluster Group
```bash
# Scan only production clusters
./opscart-scan security --cluster-group production

# Scan only staging clusters
./opscart-scan resources --cluster-group staging

# Scan only development clusters
./opscart-scan emergency --cluster-group development
```

#### Compare Two Clusters
```bash
# Side-by-side security comparison
./opscart-scan security --compare=prod-aks-01,staging-aks-01

# Note: Use comma-separated cluster names
```

### Single-Cluster Commands (v0.1 - Still Supported)

#### Security Audit
```bash
# Full security scan
./opscart-scan security --cluster prod-aks-01

# JSON output
./opscart-scan security --cluster prod-aks-01 --format json
```

#### Emergency Scanner
```bash
# Find critical issues immediately
./opscart-scan emergency --cluster prod-aks-01
```

#### Resource Analysis
```bash
# Analyze cluster resources
./opscart-scan resources --cluster prod-aks-01

# By namespace
./opscart-scan resources --cluster prod-aks-01 --namespace production
```

#### Cost Analysis
```bash
# Requires monthly cluster cost
./opscart-scan costs --cluster prod-aks-01 --monthly-cost 5000
```

#### Optimization
```bash
# Quick optimization wins
./opscart-scan optimize --cluster prod-aks-01
```

#### Find Resources
```bash
# Find all pods
./opscart-scan find pod --cluster prod-aks-01

# Find all deployments
./opscart-scan find deployment --cluster prod-aks-01

# Find all services
./opscart-scan find service --cluster prod-aks-01

# Filter by name pattern
./opscart-scan find pod --cluster prod-aks-01 --name=backend

# Filter by status
./opscart-scan find pod --cluster prod-aks-01 --status=Failed

# Combine filters
./opscart-scan find pod --cluster prod-aks-01 --name=api --status=Running
```

#### Cluster Snapshot
```bash
# Capture cluster state
./opscart-scan snapshot --cluster prod-aks-01

# Minimal cluster state
./opscart-scan snapshot --cluster prod --enhanced=false
```

---

## Output Examples

### Multi-Cluster Scan (v0.2)
```
╔═══════════════════════════════════════════════════════════╗
║           MULTI-CLUSTER SCAN                              ║
╚═══════════════════════════════════════════════════════════╝
  📦 Scanning 3 clusters...
  ─────────────────────────────────────────────
  • prod-aks-01          [production]
  • prod-aks-02          [production]
  • staging-aks-01       [staging]
  ─────────────────────────────────────────────

🔄 Scanning prod-aks-01 (1/3)...

🔍 Cluster: prod-aks-01
[Full security scan output for prod-aks-01]
✅ prod-aks-01 done (1.2s)

🔄 Scanning prod-aks-02 (2/3)...

🔍 Cluster: prod-aks-02
[Full security scan output for prod-aks-02]
✅ prod-aks-02 done (1.5s)

🔄 Scanning staging-aks-01 (3/3)...

🔍 Cluster: staging-aks-01
[Full security scan output for staging-aks-01]
✅ staging-aks-01 done (0.9s)

╔═══════════════════════════════════════════════════════════╗
║           MULTI-CLUSTER SUMMARY                           ║
╚═══════════════════════════════════════════════════════════╝

  CLUSTER              GROUP        STATUS    
  ─────────────────────────────────────────────
  prod-aks-01          production   ✅ (1.2s)
  prod-aks-02          production   ✅ (1.5s)
  staging-aks-01       staging      ✅ (0.9s)
  ─────────────────────────────────────────────
  ✅ Success: 3  |  ❌ Failed: 0  |  📦 Total: 3
```

### Security Scan (Single Cluster)
```
╔════════════════════════════════════════════════════════════╗
║                    ⚠️  DISCLAIMER ⚠️                        ║
║  • SECURITY AWARENESS TOOL - NOT FOR COMPLIANCE AUDITS     ║
║  • Use kube-bench for complete CIS compliance assessment   ║
╚════════════════════════════════════════════════════════════╝

🔍 Cluster: prod-aks-01

CIS Compliance Score: 67/100
Controls Passed: 4/7
Controls Failed: 3/7

🔴 CRITICAL FINDINGS:
  • Privileged containers: 3 (Container escape risk)
    └─ SYSTEM: 3 (expected for infrastructure)
    Top resources:
      1. kube-proxy in namespace kube-system
      2. metrics-server in namespace kube-system
```

### Emergency Scanner
```
🔍 Cluster: prod-aks-01

🔴 CRITICAL: 1    🟡 HIGH: 2    🟢 MEDIUM: 1

🔴 CRITICAL ISSUES:
  kubernetes-dashboard/dashboard-xxx
  └─ Status: CrashLoopBackOff | Restarts: 2157
  └─ Container crash looping
```

### Resource Analysis
```
🔍 Cluster: prod-aks-01

Cluster Capacity:  24.0 CPU cores, 29.1 GB memory
Total Requested:   4.0 CPU cores (16.5%), 5.8 GB memory (20.0%)

OPTIMIZATION OPPORTUNITIES:
🔴 HIGH IMPACT:
  • staging idle for 21+ days (0.3 CPU, 0.4 GB)
    └─ kubectl delete namespace staging
```

---

## Configuration

### Config File Location

**Global config:** `~/.opscart/config.yaml`  
**Project config:** `.opscart.yaml` (overrides global)

### Config File Format

```yaml
# OpsCart Multi-Cluster Configuration

clusters:
  - name: prod-aks-01           # Friendly name
    context: prod-aks-context   # kubectl context name
    group: production           # Group name

  - name: staging-aks-01
    context: staging-aks-context
    group: staging

# Groups are auto-generated from the 'group' field
# But you can also define custom groups:

groups:
  production:
    - prod-aks-01
    - prod-aks-02
  
  critical:                     # Custom group mixing environments
    - prod-aks-01
    - staging-aks-01
```

### Finding Your Cluster Contexts

```bash
# List all available contexts
kubectl config get-contexts

# Output shows:
# CURRENT   NAME                  CLUSTER               AUTHINFO
# *         prod-aks-01-context   prod-aks-01-cluster   prod-user
#           staging-aks-context   staging-aks-cluster   staging-user
```

Use the **NAME** column values in your config file.

---

## Troubleshooting

### Kubernetes Warnings (Windows/Corporate Networks)

If you see many "Use tokens from the TokenRequest API" warnings:
```bash
# Filter warnings
./opscart-scan snapshot --cluster prod 2>&1 | grep -v "Use tokens"

# Or suppress all warnings
./opscart-scan snapshot --cluster prod 2>/dev/null
```

These are Kubernetes deprecation warnings (not errors). The tool works correctly.

### Config Issues

```bash
# Config file not found
./opscart-scan config init

# Show current config
./opscart-scan config show

# Verify YAML syntax (no tabs, only spaces)
cat ~/.opscart/config.yaml
```

### Cluster Not Found

```bash
# Error: cluster 'prod-aks-01' not found in config

# Solution: Check your config
./opscart-scan config show

# Verify context exists
kubectl config get-contexts | grep prod-aks-01
```

---

## What's New in v0.2

### Multi-Cluster Support
- **Config management** - Centralized cluster configuration
- **Scan all clusters** - `--all-clusters` flag on all commands
- **Cluster groups** - Scan by environment with `--cluster-group`
- **Side-by-side comparison** - Compare security posture across clusters
- **Sequential execution** - Clear, readable output for multiple clusters
- **Cluster identification** - Every scan output shows which cluster

### Real-World Value
During v0.2 testing, found:
- Production namespace idle for 70+ days
- Staging namespace idle for 21+ days
- Development namespace idle for 14+ days
- Spot instance optimization opportunities across multiple clusters

### Developer Experience
- **100% backward compatible** - All v0.1 commands still work
- **Clear output** - Sequential execution prevents output confusion
- **Progress indicators** - Shows "Scanning X (1/3)..."
- **Summary views** - Aggregated results across all clusters

---

## What's New in v0.1

### Security Improvements
- **Removed unvalidated financial risk calculations**
  - Eliminated fake probabilities and ROI projections
  - Removed linear risk scaling assumptions
  - No more made-up breach cost estimates

- **Added CIS Kubernetes Benchmark scoring**
  - Based on official CIS Benchmark v1.8
  - Covers 7 pod security controls
  - Weighted scoring system
  - Clear pass/fail status

- **Environment-aware recommendations**
  - Detects PRODUCTION, STAGING, DEVELOPMENT, SYSTEM
  - Prioritizes production issues
  - Context-appropriate severity levels

- **Specific resource identification**
  - Shows top 5 resources per issue type
  - Sorted by environment (production first)
  - Direct kubectl commands for remediation

- **Issue count validation**
  - Transparent breakdown of all issues
  - Validation that counts match
  - Debug output if discrepancies detected

### Transparency Improvements
- Prominent disclaimers on all commands
- Citations for industry benchmarks
- Clear about tool limitations
- References to authoritative tools (kube-bench)

---

## Limitations

### Security Scanning
- ✅ **Covers:** Pod security controls (CIS section 5)
- ❌ **Does NOT cover:**
  - Control plane configuration
  - etcd security
  - Node security
  - RBAC policies
  - Admission controllers
  - Network policies (yet)

**For comprehensive compliance:** Use [kube-bench](https://github.com/aquasecurity/kube-bench)

### Cost Analysis
- Shows relative resource usage
- Identifies optimization opportunities
- Does NOT provide:
  - Exact cost calculations
  - ROI projections
  - Financial recommendations

**For cost analysis:** Use cloud provider tools (AWS Cost Explorer, GCP Cost Management, Azure Cost Management)

### Multi-Cluster Comparison
- Security comparison shows both outputs side-by-side
- Full diff functionality coming in v0.3
- Currently limited to security command only

---

## Environment Detection

The tool automatically detects environment types based on namespace naming:

```
prod-api          → PRODUCTION  (⚠️ REQUIRES IMMEDIATE ACTION)
production-web    → PRODUCTION
staging-api       → STAGING     (should fix before prod)
qa-environment    → STAGING
kube-system       → SYSTEM      (expected for infrastructure)
istio-system      → SYSTEM
default           → DEVELOPMENT (acceptable for dev, monitor)
my-app            → DEVELOPMENT
```

You can customize detection logic in `pkg/analyzer/security.go` function `detectEnvironment()`.

---

## Roadmap

### v0.3 (Next Release)
- [ ] HTML report generation
- [ ] JSON/CSV output formats
- [ ] Full diff view for cluster comparison
- [ ] Network policy detection and scoring
- [ ] Pod Security Standards (PSS) compliance

### v0.4 (Future)
- [ ] Watch mode with Slack/Teams alerts
- [ ] Historical trend tracking
- [ ] Baseline comparison
- [ ] Custom policy definitions

### v0.5 (Future)
- [ ] Multi-cluster aggregation dashboard
- [ ] Prometheus metric export
- [ ] SARIF output for CI/CD
- [ ] Helm chart deployment

---

## Contributing

This is currently an internal tool being refined for broader use.

**Feedback welcome:**
- Bug reports
- Feature requests
- Use case examples
- Documentation improvements

**Not accepting:**
- Financial modeling features
- Unvalidated risk calculations
- Arbitrary scoring systems

---

## References

- [CIS Kubernetes Benchmark](https://www.cisecurity.org/benchmark/kubernetes)
- [kube-bench](https://github.com/aquasecurity/kube-bench) - Official CIS benchmark tool
- [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
- [AWS Spot Instances](https://aws.amazon.com/ec2/spot/pricing/)
- [GCP Preemptible VMs](https://cloud.google.com/compute/docs/instances/preemptible)
- [Azure Spot VMs](https://azure.microsoft.com/en-us/pricing/spot/)

---

## Known Issues

### Corporate Network Performance
On Windows machines behind corporate proxies, the enhanced snapshot 
may be slow due to network latency. Use `--enhanced=false` for faster 
basic snapshots, or run from Mac/Linux/WSL.

Workaround:
```bash
./opscart-scan snapshot --cluster prod --enhanced=false
```

---

## Acknowledgments

Built with insights from:
- **CIS Benchmarks** - Security baseline controls
- **Aqua Security** - kube-bench methodology
- **CNCF** - Kubernetes security best practices
- **StackRox** - Pod security guidance

Special thanks to the Kubernetes security community for establishing these standards.

---

## Support

For questions or issues:
- Create a GitHub issue
- Contact: opscart.inc@gmail.com
- Blog: https://opscart.com

---

**Remember:** This tool provides awareness, not decisions. Always validate findings with security professionals and cloud architects before making production changes.