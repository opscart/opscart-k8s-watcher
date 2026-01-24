# OpsCart Kubernetes Watcher

**Production-grade Kubernetes war room toolkit for DevOps engineers managing multi-cluster environments.**

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

## Overview

OpsCart K8s Watcher is a comprehensive CLI tool designed for emergency response, security auditing, cost optimization, and risk assessment across multiple Kubernetes clusters. Built for enterprise environments managing 8+ AKS clusters in regulated industries (pharmaceutical, fintech, healthcare).

### Key Features

- 🚨 **Emergency Response** - Find critical issues immediately during incidents
- 🔒 **Security Audit** - Comprehensive security posture analysis with 0-100 scoring
- 💰 **Cost Analysis** - Honest, range-based cost estimation with optimization scenarios
- ⚠️ **Risk Quantification** - Industry-first security → financial risk mapping
- 🔍 **Multi-Cluster Search** - Find resources across all clusters instantly
- 📊 **Resource Analysis** - CPU/memory breakdown with waste detection
- 📸 **Enhanced Snapshots** - Complete cluster state for war room visibility

## Quick Start

### Installation

```bash
# Clone repository
git clone https://github.com/opscart/opscart-k8s-watcher.git
cd opscart-k8s-watcher

# Build
go build -o opscart-scan cmd/opscart-scan/main.go

# Verify installation
./opscart-scan --help
```

### Basic Usage

```bash
# Emergency war room scan
./opscart-scan emergency --cluster prod-aks-01

# Security audit
./opscart-scan security --cluster prod-aks-01

# Cost analysis with optimization
./opscart-scan costs --cluster prod-aks-01 --monthly-cost 12000

# Risk-cost analysis (pharma industry)
./opscart-scan risk-cost --cluster prod-aks-01 --industry pharma

# Find resource across all clusters
./opscart-scan find backend-api --all-clusters
```

## Commands

### Emergency Response

```bash
# Find critical issues immediately
./opscart-scan emergency --cluster prod-aks-01

# Output:
# 🔴 CRITICAL ISSUES:
# - 3 CrashLoopBackOff pods
# - 2 failed deployments
# - 1 OOMKilled pod
```

### Security Audit

```bash
# Comprehensive security analysis
./opscart-scan security --cluster prod-aks-01

# Features:
# - 0-100 security score
# - Privileged container detection
# - hostPath volume checks
# - Root user detection
# - Missing resource limits
# - Network policy gaps
```

### Cost Analysis

```bash
# Namespace-level cost breakdown
./opscart-scan costs --cluster prod-aks-01 --monthly-cost 10000

# Features:
# - Honest range estimates (Low/Best/High)
# - Spot migration scenarios
# - Idle resource detection
# - Right-sizing recommendations
# - HPA opportunities
```

### Risk-Cost Analysis

```bash
# Map security vulnerabilities → financial risk
./opscart-scan risk-cost --cluster prod-aks-01 --industry pharma

# Industries: generic, pharma, fintech, startup

# Output:
# Risk Exposure: $601K
# Fix Cost: $28K
# ROI: 21.3x
# Payback: 0.6 months
```

### Resource Analysis

```bash
# CPU/Memory breakdown by namespace
./opscart-scan resources --cluster prod-aks-01

# Features:
# - Waste score (0-100)
# - Spot eligibility detection
# - Idle pod identification
# - Over-provisioning detection
```

### Multi-Cluster Search

```bash
# Find resource across all clusters
./opscart-scan find backend-api --all-clusters

# Search by pod name, deployment, service, etc.
```

### Enhanced Snapshot

```bash
# Complete cluster state
./opscart-scan snapshot --cluster prod-aks-01 --enhanced

# Includes:
# - Pods with health status
# - Deployments with replica status
# - Services with endpoints
# - Ingresses with TLS status
# - PVCs with usage tracking
# - Network policies
```

## Industry-Specific Configurations

### Pharmaceutical/Healthcare (HIPAA)

```bash
./opscart-scan risk-cost --cluster prod-aks-01 --industry pharma

# Features:
# - 2x higher breach costs (PHI data)
# - HIPAA compliance focus
# - Patient data protection emphasis
```

### Financial Services (PCI-DSS)

```bash
./opscart-scan risk-cost --cluster prod-aks-01 --industry fintech

# Features:
# - 1.6x higher breach costs
# - Payment card data focus
# - PCI compliance emphasis
```

### Startups

```bash
./opscart-scan risk-cost --cluster dev-cluster --industry startup

# Features:
# - Lower breach cost estimates
# - Reduced engineer rates
# - Budget-conscious recommendations
```

## Architecture

```
opscart-k8s-watcher/
├── cmd/
│   └── opscart-scan/
│       └── main.go              # CLI entry point
├── pkg/
│   ├── analyzer/
│   │   ├── costs.go             # Cost analysis engine
│   │   ├── resources.go         # Resource analysis
│   │   ├── security.go          # Security audit
│   │   ├── riskcost.go          # Risk quantification
│   │   └── riskcost_config.go   # Industry configs
│   ├── scanner/
│   │   ├── cluster.go           # Cluster scanning
│   │   └── enhanced.go          # Enhanced snapshots
│   └── models/
│       ├── resources.go         # Resource models
│       ├── security.go          # Security models
│       └── riskcost.go          # Risk-cost models
└── README.md
```

## Key Differentiators

### vs Kubecost
- No installation required (single binary)
- Honest range estimates (not fake precision)
- Security + cost integration
- Industry-specific configurations

### vs Traditional Security Scanners
- Financial impact quantification
- ROI calculation built-in
- Executive-friendly reporting
- Remediation cost estimation

### vs Azure Cost Management
- Namespace-level granularity
- Optimization scenario generation
- Security risk integration
- Multi-cluster support

## Unique Features

### 1. Security → Financial Risk Mapping
World's first tool to map Kubernetes security vulnerabilities to quantified financial risk with industry-specific breach costs.

### 2. Honest Cost Ranges
Shows Low/Best/High estimates instead of fake precision, with confidence-based uncertainty calculations.

### 3. Industry Configurations
Pharmaceutical, fintech, and startup-specific cost models based on real-world incident data.

### 4. War Room Optimized
Built for emergency response with instant issue detection and multi-cluster search.

## Use Cases

### Emergency Response
"Dashboard is down in production!"
```bash
./opscart-scan emergency --cluster prod-aks-01
# Instantly finds CrashLoopBackOff pods, failed deployments
```

### Budget Justification
"Need budget for security fixes"
```bash
./opscart-scan risk-cost --cluster prod-aks-01 --industry pharma
# Shows $601K risk, $28K fix cost, 21.3x ROI
```

### Cost Optimization
"How can we reduce cloud costs?"
```bash
./opscart-scan costs --cluster prod-aks-01 --monthly-cost 12000
# Identifies $2,400/month in spot migration savings
```

### Compliance Audit
"Preparing for SOC2/PCI audit"
```bash
./opscart-scan security --cluster prod-aks-01
# Shows security score, remediation timeline, compliance gaps
```

## Configuration

### Cluster Access
Tool uses standard kubeconfig (~/.kube/config). Ensure you have access to target clusters:

```bash
# Add cluster credentials
az aks get-credentials --resource-group <rg> --name prod-aks-01

# Verify access
kubectl get nodes --context prod-aks-01
```

### Industry Configuration
Default: Generic industry
Override: Use `--industry` flag

```bash
--industry pharma     # Pharmaceutical/Healthcare
--industry fintech    # Financial Services
--industry startup    # Early-stage companies
--industry generic    # Default
```

## Output Formats

All commands support table (default) and JSON output:

```bash
# Table format (human-readable)
./opscart-scan security --cluster prod-aks-01

# JSON format (machine-readable)
./opscart-scan security --cluster prod-aks-01 --format json
```

## Requirements

- Go 1.21 or higher
- kubectl configured with cluster access
- Azure CLI (for AKS clusters)

## Development

```bash
# Run tests
go test ./...

# Build
go build -o opscart-scan cmd/opscart-scan/main.go

# Install locally
go install ./cmd/opscart-scan
```

## Roadmap

- [ ] HTML report generation
- [ ] Carbon footprint calculator (ESG)
- [ ] Compliance module (PCI/HIPAA/SOC2)
- [ ] Historical cost tracking
- [ ] Azure Pricing API integration
- [ ] Multi-cloud support (EKS, GKE)

## Contributing

Contributions welcome! Please open an issue first to discuss proposed changes.

## License

MIT License - See LICENSE file for details

## Author

**Shamsher Khan**
- Senior DevOps Engineer, GlobalLogic (Hitachi Group)
- IEEE Senior Member
- Blog: [OpsCart.com](https://opscart.com)

## Acknowledgments

Built for production environments managing 8+ AKS clusters in pharmaceutical industry.

Based on real-world incident data from:
- Ponemon Institute (breach costs)
- Aqua Security (K8s security reports)
- CNCF (incident analysis)
- StackRox/Red Hat (container security)

---

**Note:** This tool provides risk estimates based on industry averages. Actual costs depend on your specific environment, security posture, and regulatory requirements. Use Azure Cost Management for precise billing data.