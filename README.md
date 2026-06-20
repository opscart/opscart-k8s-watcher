# OpsCart Kubernetes Watcher

**Kubectl shows resources. OpsCart shows what deserves your attention.**

[![Version](https://img.shields.io/badge/version-v1.0.0-blue)](https://github.com/opscart/opscart-k8s-watcher/releases)
[![Go](https://img.shields.io/badge/go-1.21+-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![Docker](https://img.shields.io/badge/docker-ghcr.io%2Fopscart-blue)](https://ghcr.io/opscart/opscart-dashboard)

✅ **Read-only** &nbsp;·&nbsp; ✅ **No agents** &nbsp;·&nbsp; ✅ **No cloud credentials** &nbsp;·&nbsp; ✅ **Deploy in 30 seconds**

Kubernetes operational intelligence for incident response, cost visibility, and security posture. Aggregates risk across your cluster and tells you what to fix first — without touching production.

![Dashboard Preview](https://raw.githubusercontent.com/opscart/opscart-k8s-watcher/main/docs/dashboard-preview.png)

---

## What OpsCart Does

OpsCart answers one question:

> **"What deserves my attention right now?"**

Every Kubernetes engineer wakes up wondering what's broken, what's wasting money, and what to fix first. OpsCart aggregates issues across operational risk, cost, security, and waste — then surfaces the top 5 things to fix, prioritized by impact.

### The War Room

The flagship feature. One screen showing every critical issue across your cluster:

- 🔴 CrashLoopBackOff pods with restart counts and age
- 🟠 ImagePullBackOff failures with `kubectl describe` commands
- 🔴 OOMKilled containers
- 🟡 Unprotected namespaces (no NetworkPolicy)
- 🟡 Orphaned PVCs wasting storage cost

Each issue includes severity, impact, and a `kubectl` command you can copy-paste.

---

## 🚀 Deployment Options

Three ways to run OpsCart. Pick what fits your workflow.

### Option 1: Deploy In-Cluster (Recommended)

The fastest path to running production OpsCart. Read-only ClusterRole, scratch image, non-root.

```bash
kubectl apply -f https://raw.githubusercontent.com/opscart/opscart-k8s-watcher/main/deploy/dashboard.yaml
kubectl port-forward -n opscart-system svc/opscart-dashboard 8080:80
open http://localhost:8080
```

**What this gives you:**
- Auto-refreshes every 60 seconds
- Multi-cluster aware
- No external dependencies (no Prometheus, no metrics-server required)
- Removable in one command: `kubectl delete -f deploy/dashboard.yaml`

### Option 2: Local Binary

Useful for testing OpsCart against a cluster from your laptop. Uses your local kubeconfig.

```bash
git clone https://github.com/opscart/opscart-k8s-watcher.git
cd opscart-k8s-watcher
go build -o opscart-dashboard ./cmd/opscart-dashboard
./opscart-dashboard --cluster my-cluster --port 8080
open http://localhost:8080
```

### Option 3: Docker

Cross-platform, no Go installation required.

```bash
docker run -p 8080:8080 \
  -v ~/.kube:/root/.kube \
  ghcr.io/opscart/opscart-dashboard:v1.0.0
```

### Option 4: CLI Scanner

If you prefer the terminal or want to integrate OpsCart into pipelines:

```bash
go build -o opscart-scan ./cmd/opscart-scan

./opscart-scan emergency --cluster prod      # War Room from terminal
./opscart-scan security --cluster prod       # CIS scoring
./opscart-scan waste --cluster prod          # Find idle resources
./opscart-scan cloud-costs --cluster prod    # Azure cost analysis
```

---

## 🧠 What It Detects

### 🚨 War Room — Operational Risk

Every issue that needs human attention, surfaced and grouped by type:

- **CrashLoopBackOff** — pod identity, namespace, restart count, age
- **OOMKilled** — out-of-memory containers
- **ImagePullBackOff** — image pull failures with `describe` commands
- **Unprotected Namespaces** — no NetworkPolicy defined
- **Orphaned PVCs** — storage charging with no consuming pod
- **Zero-Replica Workloads** — deployments scaled to 0

### 💰 Cloud Costs

Reads Kubernetes node labels, looks up Azure retail pricing, allocates costs to namespaces proportionally. **No Azure credentials, no API calls — fully offline.**

- 40+ VM SKUs (B/D/E/F/L series), Spot and On-Demand
- Reserved Instance savings potential (1yr/3yr)
- Per-deployment cost breakdown
- 15+ Azure region multipliers

### 🔒 Security Posture

CIS Kubernetes Benchmark v1.8 scoring with environment-aware analysis.

- Separates **actionable issues** from **expected infrastructure** configs
- Privileged container whitelist (CNI/CSI/monitoring distinguished from unexpected)
- 50+ infrastructure namespace patterns recognized (calico, tigera, gatekeeper, etc.)

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

### 🌐 Network Policy Gaps

Which namespaces have zero network isolation — before an attacker finds out first.

---

## 📋 CLI Commands

| Command | Description |
|---------|-------------|
| `emergency` | War Room — what's broken right now |
| `security` | CIS Benchmark security posture |
| `waste` | Orphaned, idle, and zombie resources |
| `cloud-costs` | Real-time Azure cost analysis |
| `network` | Network policy gap analysis |
| `costs` | Resource-share cost allocation |
| `report` | Comprehensive cluster health HTML report |
| `resources` | Cluster resource inventory |
| `config` | Multi-cluster configuration |

**Common flags:**

```bash
--cluster CLUSTER         # Target cluster context
--all-clusters            # Scan all configured clusters
--format html|json|table  # Output format
--namespace NS            # Scope to single namespace
```

---

## 🏗️ Architecture

opscart-k8s-watcher/
├── cmd/
│   ├── opscart-scan/       ← CLI scanner binary
│   └── opscart-dashboard/  ← Live dashboard server
└── pkg/
├── analyzer/           ← Detection engines (security, waste, cost, network)
├── models/             ← Data structures
├── report/             ← HTML report generators
└── scanner/            ← Multi-cluster orchestration

**Dashboard runtime:**

opscart-system namespace

└── opscart-dashboard pod
├── ClusterRole: read-only (nodes, pods, deployments, NetworkPolicies, PVCs)
├── Polls cluster every 60 seconds
├── REST API: /api/overview, /api/warroom, /api/report
└── Scratch image (~15MB), non-root (UID 65534)
---

## 🆚 vs. Other Tools

| Tool | Shows | OpsCart Difference |
|------|-------|---------------------|
| **kubectl** | Resources | OpsCart prioritizes |
| **Lens** | Cluster state | OpsCart aggregates risk |
| **k9s** | Real-time pods | OpsCart explains impact |
| **Datadog/New Relic** | Metrics + logs | OpsCart needs no agents |
| **Kubecost** | Detailed cost only | OpsCart correlates cost + risk |

OpsCart isn't a replacement — it's the operational intelligence layer between `kubectl` and full observability platforms.

---

## 📅 Version History

| Version | Date | Highlights |
|---------|------|------------|
| **v1.0.0** | Jun 2026 | **New Overview page**, Top 5 Things to Fix, War Room featured panel, sidebar restructure (Operations as own category), trust-first positioning, complete code refactor |
| v0.9.0 | Jun 2026 | Full dashboard with 5 tabs (Infrastructure, Namespaces, Optimizations, War Room, Cost Overview) |
| v0.8.0 | Jun 2026 | Live in-cluster FinOps dashboard, 60s background polling, multi-cluster UI |
| v0.7.0 | Jun 2026 | `cloud-costs` command, embedded Azure pricing catalog |
| v0.6.0 | May 2026 | `costs` command, resource-share allocation |
| v0.5.x | Feb 2026 | Waste detection (9 types), HTML reports |
| v0.4.0 | Feb 2026 | Network policy gap analysis |
| v0.3.0 | Feb 2026 | HTML report generation |
| v0.2.0 | Feb 2026 | Multi-cluster support |
| v0.1.0 | Jan 2026 | Initial release — security auditing |

---

## 🗺️ Roadmap

**v1.1** — SQLite history, trend charts (Critical Issues ↑/↓, Cost trend), per-issue detail pages, recommended actions section, light theme

**v1.2** — AWS/GCP cost analysis, Helm chart, Prometheus integration

**v2.0** — Slack/Teams alerts, multi-tenancy, RBAC for dashboard users

---

## ⚠️ Disclaimer

Security awareness tool — **not for formal compliance auditing**. Use [kube-bench](https://github.com/aquasecurity/kube-bench) for official CIS compliance. Cost estimates based on Azure public retail pricing — actual costs vary with EA/MACC agreements.

---

## 🤝 Contributing

Issues, PRs, and feature requests welcome.

**Author:** Shamsher Khan — [IEEE Senior Member](https://ieee.org) · [opscart.com](https://opscart.com) · [DZone Core Member](https://dzone.com/users/shamsher_khan)

---

**License:** MIT