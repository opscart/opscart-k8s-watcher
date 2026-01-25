# opscart-k8s-watcher

**Version:** 0.1 (Beta)  
**Purpose:** Emergency Kubernetes cluster scanner for war room situations  
**Focus:** Security awareness, resource optimization, and rapid troubleshooting

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

---

## Features

### 🔒 Security Auditing
- **CIS Kubernetes Benchmark scoring** (Pod Security subset)
- **Environment-aware analysis** (PRODUCTION vs DEVELOPMENT)
- **Top 5 specific resources** per issue type
- **Actionable remediation steps**

Example output:
```
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

### 🔍 Multi-Cluster Search
- Find resources across clusters
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

# Run
./opscart-scan --help
```

---

## Usage

### Security Audit
```bash
# Full security scan
./opscart-scan security --cluster prod-aks-01

# JSON output
./opscart-scan security --cluster prod-aks-01 --format json
```

### Emergency Scanner
```bash
# Find critical issues immediately
./opscart-scan emergency --cluster prod-aks-01
```

### Resource Analysis
```bash
# Analyze cluster resources
./opscart-scan resources --cluster prod-aks-01

# By namespace
./opscart-scan resources --cluster prod-aks-01 --namespace production
```

### Cost Analysis
```bash
# Requires monthly cluster cost
./opscart-scan costs --cluster prod-aks-01 --monthly-cost 5000
```

### Optimization
```bash
# Quick optimization wins
./opscart-scan optimize --cluster prod-aks-01
```

### Find Resources
```bash
# Find pods across clusters
./opscart-scan find pod --cluster prod-aks-01

# Find deployments
./opscart-scan find deployment --cluster prod-aks-01
```

### Cluster Snapshot
```bash
# Capture cluster state
./opscart-scan snapshot --cluster prod-aks-01
```

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

## Output Examples

### Security Scan
```
╔════════════════════════════════════════════════════════════╗
║                    ⚠️  DISCLAIMER ⚠️                        ║
║  • SECURITY AWARENESS TOOL - NOT FOR COMPLIANCE AUDITS     ║
║  • Use kube-bench for complete CIS compliance assessment   ║
╚════════════════════════════════════════════════════════════╝

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
🔴 CRITICAL: 1    🟡 HIGH: 2    🟠 MEDIUM: 1

🔴 CRITICAL ISSUES:
  kubernetes-dashboard/dashboard-xxx
  └─ Status: CrashLoopBackOff | Restarts: 2157
  └─ Container crash looping
```

### Resource Analysis
```
Cluster Capacity:  24.0 CPU cores, 29.1 GB memory
Total Requested:   4.0 CPU cores (16.5%), 5.8 GB memory (20.0%)

OPTIMIZATION OPPORTUNITIES:
🔴 HIGH IMPACT:
  • staging idle for 21+ days (0.3 CPU, 0.4 GB)
    └─ kubectl delete namespace staging
```

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

### v0.2 (Next Release)
- [ ] Network policy detection and scoring
- [ ] Pod Security Standards (PSS) compliance
- [ ] Historical trend tracking
- [ ] Baseline comparison
- [ ] SARIF output for CI/CD

### v0.3 (Future)
- [ ] Custom policy definitions
- [ ] Multi-cluster aggregation
- [ ] Prometheus metric export
- [ ] Slack/Teams notifications

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