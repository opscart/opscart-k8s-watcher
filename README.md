# OpsCart-K8s-Watcher

**Your Kubernetes cluster is hiding things. This finds them.**

[![Version](https://img.shields.io/badge/version-v0.8.0-blue)](https://github.com/opscart/opscart-k8s-watcher/releases)
[![Go](https://img.shields.io/badge/go-1.21+-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![Docker](https://img.shields.io/badge/docker-ghcr.io%2Fopscart-blue)](https://ghcr.io/opscart/opscart-dashboard)

Production-grade Kubernetes intelligence — security posture, cloud costs, waste detection, and network policy analysis. Built from real Fortune 500 AKS cluster experience.

---

## 🚀 What's New in v0.8.0 — Live FinOps Dashboard

**OpsCart moves from CLI scanner to always-on in-cluster daemon.**

```bash
# Deploy the live dashboard to your cluster
kubectl apply -f https://raw.githubusercontent.com/opscart/opscart-k8s-watcher/main/deploy/dashboard.yaml

# Access it
kubectl port-forward -n opscart-system svc/opscart-dashboard 8080:80
open http://localhost:8080
```

![Dashboard Preview](https://raw.githubusercontent.com/opscart/opscart-k8s-watcher/main/docs/dashboard-preview.png)

**The dashboard gives you:**
- 💰 **Real-time cost tracking** — Node pool costs from actual VM SKUs, updated every 60 seconds
- 📊 **Namespace cost allocation** — See exactly which teams are spending what
- 🔍 **Confidence scoring** — 100% SKU match confidence with embedded Azure pricing catalog
- 🔄 **Multi-cluster selector** — Switch between clusters in the sidebar
- ⚡ **Live refresh** — Auto-refreshes every 60s with "last updated" badge

**Or run it locally:**
```bash
./opscart-dashboard --cluster my-cluster --port 8080
```

---

## 📦 Installation

```bash
git clone https://github.com/opscart/opscart-k8s-watcher.git
cd opscart-k8s-watcher
go build -o opscart-scan cmd/opscart-scan/main.go
./opscart-scan config init
```

---

## ⚡ Quick Start

```bash
# ☁️ Cloud costs — real pricing from node labels, no API keys needed
./opscart-scan cloud-costs --cluster prod
./opscart-scan cloud-costs --cluster prod --format html

# 🔒 Security posture — CIS Benchmark scoring
./opscart-scan security --cluster prod
./opscart-scan security --cluster prod --format html

# 🗑️ Waste detection — find orphaned, idle, and zombie resources
./opscart-scan waste --cluster prod
./opscart-scan waste --cluster prod --format html

# 🌐 Network policy gaps — unprotected namespaces
./opscart-scan network --cluster prod

# 🚨 War room — what's broken right now
./opscart-scan emergency --cluster prod

# 📊 All clusters at once
./opscart-scan cloud-costs --all-clusters
./opscart-scan security --all-clusters
```

> 💡 **Corporate AKS clusters:** Append `2>/dev/null` to suppress harmless klog warnings.

---

## 🧠 What It Detects

### ☁️ Cloud Costs
Reads Kubernetes node labels → looks up Azure retail pricing → allocates costs to namespaces proportionally. No Azure credentials, no API calls — fully offline.

- 40+ VM SKUs (B/D/E/F/L series), Spot and On-Demand
- Reserved Instance savings potential (1yr/3yr)
- Per-deployment cost breakdown with `--breakdown deployment`
- 15+ Azure region multipliers

### 🔒 Security Posture
CIS Kubernetes Benchmark v1.8 scoring with environment-aware analysis.

- Separates **actionable issues** from **expected infrastructure** configs
- Privileged container whitelist — distinguishes CNI/CSI/monitoring from unexpected
- 50+ infrastructure namespace patterns (calico, tigera, ama-logs, gatekeeper, etc.)

### 🗑️ Waste & Drift
9 resource types — **never modifies the cluster**, suggestions only.

| Type | What It Catches |
|------|-----------------|
| Abandoned Namespaces | No running pods for N days |
| Zombie Pods | CrashLoopBackOff, OOMKilled lingering |
| Orphaned PVCs | Unbound or pod-less storage still charging |
| Stale Jobs | Completed jobs not cleaned up |
| Zero-Replica Workloads | Deployments scaled to 0 |
| Broken Ingresses | Backends pointing to missing services |
| Misconfigured HPAs | Stuck at minReplicas, scaling disabled |

### 🌐 Network Policies
Which namespaces have zero network isolation — before an attacker finds out first.

---

## 📋 Commands

| Command | Description |
|---------|-------------|
| `cloud-costs` | Real-time Azure cost analysis from node labels |
| `security` | CIS Benchmark security posture scoring |
| `waste` | Orphaned, idle, and zombie resource detection |
| `network` | Network policy gap analysis |
| `emergency` | War room — crash loops, pending pods, pull failures |
| `costs` | Resource-share cost allocation (manual monthly cost) |
| `report` | Comprehensive cluster health report |
| `resources` | Cluster resource inventory |
| `config` | Multi-cluster configuration management |

**Common flags:**
```bash
--cluster CLUSTER         # Target cluster context
--all-clusters            # Scan all configured clusters
--cluster-group GROUP     # Scan a named group
--format html|json|table  # Output format
--namespace NS            # Scope to single namespace
```

---

## 🏗️ Architecture

```
opscart-k8s-watcher/
├── cmd/
│   ├── opscart-scan/       ← CLI scanner binary
│   └── opscart-dashboard/  ← Live dashboard server (v0.8)
└── pkg/
    ├── analyzer/           ← Detection engines
    ├── models/             ← Data structures
    ├── report/             ← HTML report generators
    └── scanner/            ← Multi-cluster orchestration
```

**Dashboard deployment:**
```
opscart-system namespace
└── opscart-dashboard pod
    ├── ClusterRole: read-only (nodes, pods, deployments)
    ├── Polls cluster every 60 seconds
    ├── REST API: /api/overview, /api/report
    └── Scratch image (~15MB), non-root (UID 65534)
```

---

## 📅 Version History

| Version | Date | Highlights |
|---------|------|------------|
| **v0.8.0** | Jun 2026 | Live in-cluster FinOps dashboard, 60s background polling, multi-cluster UI |
| v0.7.0 | Jun 2026 | `cloud-costs` command, embedded Azure pricing catalog, enterprise HTML dashboard |
| v0.6.0 | May 2026 | `costs` command, resource-share allocation, FinOps-grade output |
| v0.5.x | Feb 2026 | Waste detection (9 types), HTML reports, bug fixes |
| v0.4.0 | Feb 2026 | Network policy gap analysis, infrastructure filtering |
| v0.3.0 | Feb 2026 | HTML report generation, CIS scoring improvements |
| v0.2.0 | Feb 2026 | Multi-cluster support, cluster groups, comparison |
| v0.1.0 | Jan 2026 | Initial release — security auditing, CIS Benchmark |

---

## 🗺️ Roadmap

- **v0.9** — SQLite cost history, trend charts, security + waste tabs in dashboard
- **v1.0** — Helm chart, AWS/GCP pricing, Slack/Teams alerts, Prometheus integration

---

## ⚠️ Disclaimer

Security awareness tool — **not for compliance auditing**. Use [kube-bench](https://github.com/aquasecurity/kube-bench) for official CIS compliance. Cost estimates based on Azure public retail pricing — actual costs vary with EA/MACC agreements.

---

## 🤝 Contributing

Issues, PRs, and feature requests welcome. Built for the Kubernetes community.

**Author:** Shamsher Khan — [IEEE Senior Member](https://ieee.org) · [opscart.com](https://opscart.com) · [DZone Core Member](https://dzone.com/users/shamsher_khan)

---

**License:** MIT