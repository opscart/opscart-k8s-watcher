package analyzer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// PrintCloudCostReport renders the comprehensive cloud cost report
func PrintCloudCostReport(report *models.CloudCostReport, format string) error {
	switch format {
	case "json":
		return printCloudCostJSON(report)
	case "html":
		return printCloudCostHTML(report)
	default:
		return printCloudCostCLI(report)
	}
}

func printCloudCostJSON(report *models.CloudCostReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func printCloudCostCLI(report *models.CloudCostReport) error {
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════════════════════════")
	fmt.Printf("  💰 CLOUD COST ANALYSIS — %s\n", report.ClusterName)
	fmt.Printf("     Region: %s | Provider: %s | Pricing: %s\n", report.Region, report.Provider, report.PricingSource)
	fmt.Println("═══════════════════════════════════════════════════════════════════════════════")

	// ── Node Pool Costs ─────────────────────────────────────────────
	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────────────────────────────┐")
	fmt.Println("│  🖥️  NODE POOL COSTS                                                        │")
	fmt.Println("├─────────────────────────────────────────────────────────────────────────────┤")
	fmt.Printf("│  %-18s %-16s %5s %8s %10s %11s │\n", "POOL", "VM SIZE", "NODES", "PRIORITY", "$/NODE/MO", "TOTAL/MO")
	fmt.Println("├─────────────────────────────────────────────────────────────────────────────┤")

	for _, pool := range report.NodePoolCosts {
		priority := pool.Priority
		if strings.EqualFold(priority, "spot") {
			priority = "⚡ Spot"
		}
		fmt.Printf("│  %-18s %-16s %5d %8s %10.2f %11.2f │\n",
			truncate(pool.Name, 18),
			truncate(pool.VMSize, 16),
			pool.NodeCount,
			priority,
			pool.PricePerNodeMonth,
			pool.TotalMonthly,
		)
		// Utilization sub-line
		fmt.Printf("│    └─ CPU: %.0f%%  MEM: %.0f%%  (%.1f/%.1f cores, %.1f/%.1f GB)%s│\n",
			pool.CPUUtilizationPct, pool.MemoryUtilizationPct,
			pool.CPURequested, pool.TotalCPUCapacity,
			pool.MemoryRequested, pool.TotalMemoryCapacity,
			strings.Repeat(" ", maxInt(1, 22-len(fmt.Sprintf("%.1f/%.1f cores, %.1f/%.1f GB",
				pool.CPURequested, pool.TotalCPUCapacity, pool.MemoryRequested, pool.TotalMemoryCapacity)))),
		)
		if pool.RISavings > 0 {
			fmt.Printf("│    └─ 💡 RI Savings Potential: $%.2f/mo with 1-year reserved%s│\n",
				pool.RISavings,
				strings.Repeat(" ", maxInt(1, 20-len(fmt.Sprintf("%.2f", pool.RISavings)))),
			)
		}
	}
	fmt.Println("├─────────────────────────────────────────────────────────────────────────────┤")
	fmt.Printf("│  TOTAL COMPUTE COST:                                            $%9.2f/mo │\n", report.TotalNodeCost)
	fmt.Println("└─────────────────────────────────────────────────────────────────────────────┘")

	// ── Namespace Cost Allocation ─────────────────────────────────
	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────────────────────────────┐")
	fmt.Println("│  📊 NAMESPACE COST ALLOCATION (by resource share)                           │")
	fmt.Println("├─────────────────────────────────────────────────────────────────────────────┤")
	fmt.Printf("│  %-25s %7s %7s %8s %10s %7s │\n", "NAMESPACE", "CPU", "MEM", "SHARE", "EST. COST", "PODS")
	fmt.Println("├─────────────────────────────────────────────────────────────────────────────┤")

	for i, ns := range report.NamespaceCosts {
		if i >= 20 {
			fmt.Printf("│  ... and %d more namespaces%s│\n",
				len(report.NamespaceCosts)-20,
				strings.Repeat(" ", 45))
			break
		}
		fmt.Printf("│  %-25s %5.1fc %5.1fG %6.1f%% %10s %5d │\n",
			truncate(ns.Name, 25),
			ns.CPUCores,
			ns.MemoryGB,
			ns.WeightedShare*100,
			cloudFormatCost(ns.EstimatedCost),
			ns.PodCount,
		)

		// Show deployment breakdown if available
		if len(ns.Deployments) > 0 {
			maxDeploys := 5
			for j, dep := range ns.Deployments {
				if j >= maxDeploys {
					fmt.Printf("│    └─ ... +%d more workloads%s│\n",
						len(ns.Deployments)-maxDeploys,
						strings.Repeat(" ", 44))
					break
				}
				fmt.Printf("│    ├─ %-22s %-10s %dx  %5.1fc %5.1fG  %s │\n",
					truncate(dep.Name, 22),
					dep.Kind,
					dep.Replicas,
					dep.CPUCores,
					dep.MemoryGB,
					cloudFormatCost(dep.EstimatedCost),
				)
			}
		}
	}
	fmt.Println("└─────────────────────────────────────────────────────────────────────────────┘")

	// ── Summary ─────────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────────────────────────────┐")
	fmt.Println("│  📋 COST SUMMARY                                                            │")
	fmt.Println("├─────────────────────────────────────────────────────────────────────────────┤")
	fmt.Printf("│  Compute (Node Pools):          $%10.2f/mo                              │\n", report.CostBreakdown.Compute)
	if report.CostBreakdown.ManagedDisks > 0 {
		fmt.Printf("│  Managed Disks (est):           $%10.2f/mo                              │\n", report.CostBreakdown.ManagedDisks)
	}
	if report.CostBreakdown.Networking > 0 {
		fmt.Printf("│  Networking (est):              $%10.2f/mo                              │\n", report.CostBreakdown.Networking)
	}
	fmt.Println("│  ─────────────────────────────────────────────                              │")
	fmt.Printf("│  TOTAL ESTIMATED:               $%10.2f/mo ($%.2f/year)        │\n", report.TotalMonthlyCost, report.TotalAnnualCost)
	fmt.Println("├─────────────────────────────────────────────────────────────────────────────┤")

	if report.TotalSavingsPotential.Best > 0 {
		fmt.Printf("│  💡 SAVINGS POTENTIAL:           $%8.2f – $%.2f/mo                     │\n",
			report.TotalSavingsPotential.Low, report.TotalSavingsPotential.High)
	}
	fmt.Println("└─────────────────────────────────────────────────────────────────────────────┘")

	// ── Optimization Scenarios ──────────────────────────────────────
	if len(report.OptimizationScenarios) > 0 {
		fmt.Println()
		fmt.Println("┌─────────────────────────────────────────────────────────────────────────────┐")
		fmt.Println("│  🎯 OPTIMIZATION SCENARIOS                                                  │")
		fmt.Println("├─────────────────────────────────────────────────────────────────────────────┤")
		for i, scenario := range report.OptimizationScenarios {
			fmt.Printf("│  %d. %s\n", i+1, scenario.Name)
			fmt.Printf("│     %s\n", scenario.Description)
			fmt.Printf("│     Savings: $%.2f – $%.2f/mo | Effort: %s | Risk: %s\n",
				scenario.Savings.Low, scenario.Savings.High, scenario.Effort, scenario.Risk)
			if len(scenario.Actions) > 0 {
				fmt.Printf("│     → %s\n", scenario.Actions[0])
			}
			fmt.Println("│")
		}
		fmt.Println("└─────────────────────────────────────────────────────────────────────────────┘")
	}

	// ── Disclaimers ─────────────────────────────────────────────────
	fmt.Println()
	for _, d := range report.Disclaimers {
		fmt.Printf("  %s\n", d)
	}
	fmt.Println()

	return nil
}

// printCloudCostHTML generates an HTML report file in reports/YYYY-MM-DD/
func printCloudCostHTML(report *models.CloudCostReport) error {
	today := time.Now().Format("2006-01-02")
	dir := filepath.Join("reports", today)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating reports dir: %w", err)
	}

	ts := time.Now().Format("1504")
	clusterSafe := strings.ReplaceAll(report.ClusterName, "/", "-")
	clusterSafe = strings.ReplaceAll(clusterSafe, "\\", "-")
	filename := filepath.Join(dir, fmt.Sprintf("cloud-costs-%s-%s.html", clusterSafe, ts))

	html := generateCloudCostHTML(report)

	if err := os.WriteFile(filename, []byte(html), 0644); err != nil {
		return fmt.Errorf("writing HTML report: %w", err)
	}

	fmt.Printf("\nCloud cost report saved to: %s\n", filename)
	return nil
}

func generateCloudCostHTML(report *models.CloudCostReport) string {
	var sb strings.Builder

	// Calculate accuracy info
	knownVMs := 0
	unknownVMs := 0
	for _, pool := range report.NodePoolCosts {
		if pool.PricePerNodeMonth > 0 {
			knownVMs++
		} else {
			unknownVMs++
		}
	}
	accuracyPct := 0
	if knownVMs+unknownVMs > 0 {
		accuracyPct = (knownVMs * 100) / (knownVMs + unknownVMs)
	}
	confidenceLabel := "High"
	if accuracyPct < 80 {
		confidenceLabel = "Medium"
	}
	if accuracyPct < 50 {
		confidenceLabel = "Low"
	}

	totalNodes := 0
	totalRISavings := 0.0
	for _, p := range report.NodePoolCosts {
		totalNodes += p.NodeCount
		totalRISavings += p.RISavings
	}
	totalNamespaces := len(report.NamespaceCosts)
	totalPods := 0
	for _, ns := range report.NamespaceCosts {
		totalPods += ns.PodCount
	}

	// Build namespace cost chart data (top 10)
	topNs := report.NamespaceCosts
	if len(topNs) > 10 {
		topNs = topNs[:10]
	}

	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>` + report.ClusterName + ` — Cloud FinOps Report</title>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700;800;900&display=swap" rel="stylesheet">
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0f172a;--surface:#1e293b;--surface-hover:#334155;--border:#334155;
  --text:#f1f5f9;--text-secondary:#94a3b8;--text-muted:#64748b;
  --primary:#6366f1;--primary-light:#818cf8;--primary-bg:rgba(99,102,241,0.1);
  --success:#10b981;--success-bg:rgba(16,185,129,0.1);
  --warning:#f59e0b;--warning-bg:rgba(245,158,11,0.1);
  --danger:#ef4444;--danger-bg:rgba(239,68,68,0.1);
  --info:#06b6d4;--info-bg:rgba(6,182,212,0.1);
  --radius:12px;--radius-sm:8px;--radius-xs:6px;
  --shadow:0 4px 6px -1px rgba(0,0,0,0.3),0 2px 4px -2px rgba(0,0,0,0.3);
  --shadow-lg:0 20px 25px -5px rgba(0,0,0,0.4),0 8px 10px -6px rgba(0,0,0,0.4);
}
body{font-family:'Inter',system-ui,sans-serif;background:var(--bg);color:var(--text);line-height:1.6;min-height:100vh}

/* Layout */
.layout{display:grid;grid-template-columns:260px 1fr;min-height:100vh}
.sidebar{background:var(--surface);border-right:1px solid var(--border);padding:2rem 1.5rem;position:sticky;top:0;height:100vh;overflow-y:auto}
.main{padding:2rem 2.5rem;overflow-y:auto}

/* Sidebar */
.logo{display:flex;align-items:center;gap:0.75rem;margin-bottom:2.5rem}
.logo-icon{width:40px;height:40px;background:var(--primary);border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:1.2rem}
.logo-text{font-size:1.1rem;font-weight:700;color:var(--text)}
.logo-sub{font-size:0.7rem;color:var(--text-muted);text-transform:uppercase;letter-spacing:1px}
.nav-section{margin-bottom:1.5rem}
.nav-label{font-size:0.7rem;text-transform:uppercase;letter-spacing:1.5px;color:var(--text-muted);margin-bottom:0.75rem;padding-left:0.75rem}
.nav-item{display:flex;align-items:center;gap:0.75rem;padding:0.7rem 0.75rem;border-radius:var(--radius-sm);color:var(--text-secondary);font-size:0.9rem;cursor:pointer;transition:all 0.15s}
.nav-item:hover{background:var(--surface-hover);color:var(--text)}
.nav-item.active{background:var(--primary-bg);color:var(--primary-light);font-weight:500}
.cluster-info{margin-top:auto;padding-top:2rem;border-top:1px solid var(--border)}
.cluster-badge{background:var(--primary-bg);border:1px solid rgba(99,102,241,0.2);border-radius:var(--radius-sm);padding:1rem;margin-bottom:0.75rem}
.cluster-badge h4{font-size:0.8rem;color:var(--primary-light);margin-bottom:0.4rem}
.cluster-badge p{font-size:0.75rem;color:var(--text-muted);word-break:break-all}

/* KPI Row */
.kpi-row{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:1rem;margin-bottom:2rem}
.kpi{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:1.25rem;transition:border-color 0.2s}
.kpi:hover{border-color:var(--primary)}
.kpi-top{display:flex;justify-content:space-between;align-items:center;margin-bottom:0.75rem}
.kpi-icon{width:36px;height:36px;border-radius:var(--radius-xs);display:flex;align-items:center;justify-content:center;font-size:1rem}
.kpi-icon.blue{background:var(--primary-bg)}
.kpi-icon.green{background:var(--success-bg)}
.kpi-icon.red{background:var(--danger-bg)}
.kpi-icon.yellow{background:var(--warning-bg)}
.kpi-icon.cyan{background:var(--info-bg)}
.kpi-trend{font-size:0.75rem;padding:2px 8px;border-radius:20px;font-weight:600}
.kpi-trend.up{background:var(--danger-bg);color:var(--danger)}
.kpi-trend.down{background:var(--success-bg);color:var(--success)}
.kpi h3{font-size:1.6rem;font-weight:800;margin-bottom:0.2rem}
.kpi p{font-size:0.8rem;color:var(--text-muted)}

/* Section */
.section{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:1.5rem;margin-bottom:1.5rem}
.section-header{display:flex;justify-content:space-between;align-items:center;margin-bottom:1.25rem}
.section-title{font-size:1.1rem;font-weight:700;display:flex;align-items:center;gap:0.5rem}
.section-actions{display:flex;gap:0.5rem}
.btn-sm{padding:0.4rem 0.8rem;border-radius:var(--radius-xs);font-size:0.75rem;border:1px solid var(--border);background:transparent;color:var(--text-secondary);cursor:pointer;transition:all 0.15s}
.btn-sm:hover{background:var(--surface-hover);color:var(--text)}
.btn-sm.active{background:var(--primary-bg);border-color:var(--primary);color:var(--primary-light)}

/* Tables */
.table-wrap{overflow-x:auto;border-radius:var(--radius-sm);border:1px solid var(--border)}
table{width:100%;border-collapse:collapse}
th{background:rgba(0,0,0,0.3);padding:0.75rem 1rem;text-align:left;font-size:0.72rem;text-transform:uppercase;letter-spacing:0.8px;color:var(--text-muted);font-weight:600;border-bottom:1px solid var(--border);white-space:nowrap}
td{padding:0.7rem 1rem;border-bottom:1px solid var(--border);font-size:0.85rem;white-space:nowrap}
tr:last-child td{border-bottom:none}
tr:hover td{background:rgba(99,102,241,0.03)}
tbody tr{transition:background 0.1s}

/* Cost styling */
.money{font-weight:700;font-variant-numeric:tabular-nums}
.money.red{color:var(--danger)}
.money.green{color:var(--success)}
.money.blue{color:var(--primary-light)}

/* Utilization */
.util-cell{min-width:120px}
.util-row{display:flex;align-items:center;gap:8px}
.util-track{flex:1;height:6px;background:rgba(255,255,255,0.06);border-radius:3px;overflow:hidden}
.util-fill{height:100%;border-radius:3px;transition:width 0.5s ease}
.util-fill.green{background:var(--success)}
.util-fill.yellow{background:var(--warning)}
.util-fill.red{background:var(--danger)}
.util-pct{font-size:0.8rem;color:var(--text-secondary);min-width:35px;text-align:right}

/* Tags */
.tag{display:inline-flex;align-items:center;padding:3px 10px;border-radius:20px;font-size:0.72rem;font-weight:600;letter-spacing:0.3px}
.tag-spot{background:#422006;color:#fbbf24;border:1px solid rgba(251,191,36,0.2)}
.tag-regular{background:#172554;color:#60a5fa;border:1px solid rgba(96,165,250,0.2)}
.tag-deploy{background:#064e3b;color:#6ee7b7;border:1px solid rgba(110,231,183,0.2)}
.tag-sts{background:#4c1d95;color:#c4b5fd;border:1px solid rgba(196,181,253,0.2)}
.tag-ds{background:#78350f;color:#fcd34d;border:1px solid rgba(252,211,77,0.2)}

/* Namespace expandable */
.ns-group{border-bottom:1px solid var(--border)}
.ns-group:last-child{border-bottom:none}
.ns-header{cursor:pointer;transition:background 0.15s}
.ns-header:hover td{background:rgba(99,102,241,0.05)}
.ns-header td:first-child{font-weight:600}
.ns-toggle{display:inline-block;width:18px;height:18px;text-align:center;line-height:18px;border-radius:4px;background:rgba(255,255,255,0.05);margin-right:8px;font-size:0.7rem;transition:transform 0.2s}
.ns-header.open .ns-toggle{transform:rotate(90deg)}
.dep-rows{display:none}
.dep-rows.show{display:table-row-group}
.dep-row td{padding-left:2.5rem;background:rgba(0,0,0,0.15);font-size:0.82rem;color:var(--text-secondary)}
.dep-row td:first-child{color:var(--text-muted)}

/* Confidence */
.confidence-card{display:flex;gap:1.5rem;align-items:center}
.conf-ring{position:relative;width:80px;height:80px}
.conf-ring svg{transform:rotate(-90deg)}
.conf-ring-bg{fill:none;stroke:rgba(255,255,255,0.05);stroke-width:8}
.conf-ring-fill{fill:none;stroke-width:8;stroke-linecap:round;transition:stroke-dashoffset 0.5s}
.conf-pct{position:absolute;top:50%;left:50%;transform:translate(-50%,-50%);font-size:1.1rem;font-weight:800}
.conf-details h4{font-size:0.95rem;margin-bottom:0.3rem}
.conf-details p{font-size:0.82rem;color:var(--text-muted);max-width:500px}
.conf-method{margin-top:1rem;display:grid;grid-template-columns:repeat(auto-fit,minmax(250px,1fr));gap:0.75rem}
.conf-method-item{display:flex;align-items:flex-start;gap:0.5rem;font-size:0.8rem;color:var(--text-secondary)}
.conf-method-item::before{content:'✓';color:var(--success);font-weight:bold;margin-top:1px}

/* Scenario cards */
.scenarios-grid{display:grid;gap:1rem}
.scenario-card{background:rgba(0,0,0,0.2);border:1px solid var(--border);border-radius:var(--radius-sm);padding:1.25rem;transition:border-color 0.2s}
.scenario-card:hover{border-color:var(--success)}
.scenario-top{display:flex;justify-content:space-between;align-items:flex-start;margin-bottom:0.75rem}
.scenario-name{font-weight:700;font-size:0.95rem}
.scenario-save{background:var(--success-bg);color:var(--success);padding:4px 12px;border-radius:20px;font-size:0.8rem;font-weight:700}
.scenario-desc{font-size:0.85rem;color:var(--text-secondary);margin-bottom:0.75rem}
.scenario-pills{display:flex;gap:0.5rem;flex-wrap:wrap}
.pill{padding:3px 10px;border-radius:20px;font-size:0.72rem;font-weight:500;border:1px solid var(--border);color:var(--text-muted)}

/* Disclaimer bar */
.disclaimer-bar{background:rgba(245,158,11,0.08);border:1px solid rgba(245,158,11,0.2);border-radius:var(--radius);padding:1.25rem;margin-top:1.5rem}
.disclaimer-bar h4{color:var(--warning);font-size:0.85rem;margin-bottom:0.5rem}
.disclaimer-bar ul{list-style:none;display:grid;gap:0.3rem}
.disclaimer-bar li{font-size:0.8rem;color:var(--text-muted);padding-left:1.2rem;position:relative}
.disclaimer-bar li::before{content:'•';position:absolute;left:0;color:var(--warning)}

/* Footer */
.footer{text-align:center;padding:2rem 0;color:var(--text-muted);font-size:0.75rem}

/* Responsive */
@media(max-width:1024px){.layout{grid-template-columns:1fr}.sidebar{display:none}}
@media(max-width:768px){.kpi-row{grid-template-columns:repeat(2,1fr)}.main{padding:1.5rem}}

/* Animations */
@keyframes fadeIn{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:translateY(0)}}
.section,.kpi{animation:fadeIn 0.4s ease both}
.section:nth-child(2){animation-delay:0.1s}
.section:nth-child(3){animation-delay:0.2s}
</style>
</head>
<body>
<div class="layout">
`)

	// ─── Sidebar ──────────────────────────────────────────────────────
	sb.WriteString(`<aside class="sidebar">
		<div class="logo">
			<div class="logo-icon">⚡</div>
			<div><div class="logo-text">OpsCart</div><div class="logo-sub">FinOps Engine</div></div>
		</div>
		<div class="nav-section">
			<div class="nav-label">Analysis</div>
			<div class="nav-item active">📊 Cost Overview</div>
			<div class="nav-item">🖥️ Infrastructure</div>
			<div class="nav-item">📦 Namespaces</div>
			<div class="nav-item">🎯 Optimizations</div>
		</div>
		<div class="nav-section">
			<div class="nav-label">Details</div>
			<div class="nav-item">🔬 Node Pools</div>
			<div class="nav-item">📋 Deployments</div>
			<div class="nav-item">📈 Utilization</div>
		</div>
		<div class="cluster-info">
			<div class="cluster-badge">
				<h4>CLUSTER</h4>
				<p>` + report.ClusterName + `</p>
			</div>
			<div class="cluster-badge">
				<h4>REGION</h4>
				<p>` + report.Region + `</p>
			</div>
			<div class="cluster-badge">
				<h4>GENERATED</h4>
				<p>` + report.Timestamp.Format("Jan 2, 2006 15:04 MST") + `</p>
			</div>
		</div>
	</aside>`)

	// ─── Main Content ─────────────────────────────────────────────────
	sb.WriteString(`<main class="main">`)

	// Page header
	sb.WriteString(`<div style="margin-bottom:2rem">
		<h1 style="font-size:1.5rem;font-weight:800;margin-bottom:0.3rem">Cloud Cost Analysis</h1>
		<p style="color:var(--text-muted);font-size:0.9rem">Kubernetes infrastructure cost breakdown • ` + report.Timestamp.Format("Monday, January 2, 2006") + `</p>
	</div>`)

	// ─── KPI Row ───────────────────────────────────────────────────────
	sb.WriteString(`<div class="kpi-row">`)
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon red">💰</div></div>
		<h3 class="money red">$%s</h3><p>Monthly Cost</p></div>`, formatMoney(report.TotalMonthlyCost)))
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon blue">📅</div></div>
		<h3 class="money blue">$%s</h3><p>Annual Projection</p></div>`, formatMoney(report.TotalAnnualCost)))
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon green">💡</div><span class="kpi-trend down">↓ Potential</span></div>
		<h3 class="money green">$%s</h3><p>Savings / Month</p></div>`, formatMoney(report.TotalSavingsPotential.Best)))
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon cyan">🖥️</div></div>
		<h3>%d</h3><p>Nodes (%d pools)</p></div>`, totalNodes, len(report.NodePoolCosts)))
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon yellow">📦</div></div>
		<h3>%d</h3><p>Namespaces</p></div>`, totalNamespaces))
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon blue">🐳</div></div>
		<h3>%d</h3><p>Running Pods</p></div>`, totalPods))
	sb.WriteString(`</div>`)

	// ─── Confidence Section ────────────────────────────────────────────
	circumference := 226 // 2*PI*36
	offset := circumference - (circumference * accuracyPct / 100)
	confColor := "var(--success)"
	if accuracyPct < 80 {
		confColor = "var(--warning)"
	}
	if accuracyPct < 50 {
		confColor = "var(--danger)"
	}

	sb.WriteString(`<div class="section"><div class="section-header"><div class="section-title">🎯 Pricing Confidence</div></div>`)
	sb.WriteString(`<div class="confidence-card">`)
	sb.WriteString(fmt.Sprintf(`<div class="conf-ring">
		<svg width="80" height="80" viewBox="0 0 80 80">
			<circle cx="40" cy="40" r="36" class="conf-ring-bg"/>
			<circle cx="40" cy="40" r="36" class="conf-ring-fill" style="stroke:%s;stroke-dasharray:%d;stroke-dashoffset:%d"/>
		</svg>
		<div class="conf-pct" style="color:%s">%d%%</div>
	</div>`, confColor, circumference, offset, confColor, accuracyPct))
	sb.WriteString(fmt.Sprintf(`<div class="conf-details">
		<h4>Confidence Level: %s</h4>
		<p>%d of %d node pool VM SKUs matched to Azure retail pricing catalog. `, confidenceLabel, knownVMs, knownVMs+unknownVMs))
	if unknownVMs > 0 {
		sb.WriteString(fmt.Sprintf(`%d pool(s) estimated from CPU/memory capacity — actual cost may vary ±15%%.`, unknownVMs))
	} else {
		sb.WriteString(`All VM sizes resolved to exact Pay-As-You-Go pricing.`)
	}
	sb.WriteString(`</p></div></div>`)
	sb.WriteString(`<div class="conf-method">
		<div class="conf-method-item">VM SKU from node label <code>node.kubernetes.io/instance-type</code></div>
		<div class="conf-method-item">Azure Pay-As-You-Go retail pricing (730 hrs/mo)</div>
		<div class="conf-method-item">Spot detected via <code>kubernetes.azure.com/scalesetpriority</code></div>
		<div class="conf-method-item">Namespace cost = weighted CPU+Memory share × node cost</div>
	</div></div>`)

	// ─── Node Pool Infrastructure ──────────────────────────────────────
	sb.WriteString(`<div class="section">
		<div class="section-header"><div class="section-title">🖥️ Node Pool Infrastructure</div></div>
		<div class="table-wrap"><table>
		<thead><tr><th>Pool Name</th><th>VM Size</th><th>Nodes</th><th>Type</th><th>CPU Utilization</th><th>Memory Utilization</th><th>$/Node/Mo</th><th>Pool Total</th><th>RI Savings</th></tr></thead><tbody>`)
	for _, pool := range report.NodePoolCosts {
		tagClass := "tag-regular"
		tagLabel := "On-Demand"
		if strings.EqualFold(pool.Priority, "spot") {
			tagClass = "tag-spot"
			tagLabel = "⚡ Spot"
		}

		cpuColor := "green"
		if pool.CPUUtilizationPct > 85 {
			cpuColor = "red"
		} else if pool.CPUUtilizationPct > 65 {
			cpuColor = "yellow"
		}
		memColor := "green"
		if pool.MemoryUtilizationPct > 85 {
			memColor = "red"
		} else if pool.MemoryUtilizationPct > 65 {
			memColor = "yellow"
		}

		sb.WriteString(fmt.Sprintf(`<tr>
			<td><strong>%s</strong></td>
			<td><code style="background:rgba(255,255,255,0.05);padding:2px 8px;border-radius:4px;font-size:0.8rem">%s</code></td>
			<td>%d</td>
			<td><span class="tag %s">%s</span></td>
			<td class="util-cell"><div class="util-row"><div class="util-track"><div class="util-fill %s" style="width:%.0f%%"></div></div><span class="util-pct">%.0f%%</span></div></td>
			<td class="util-cell"><div class="util-row"><div class="util-track"><div class="util-fill %s" style="width:%.0f%%"></div></div><span class="util-pct">%.0f%%</span></div></td>
			<td class="money red">$%s</td>
			<td class="money red">$%s</td>
			<td class="money green">%s</td>
		</tr>`,
			pool.Name, pool.VMSize, pool.NodeCount, tagClass, tagLabel,
			cpuColor, pool.CPUUtilizationPct, pool.CPUUtilizationPct,
			memColor, pool.MemoryUtilizationPct, pool.MemoryUtilizationPct,
			formatMoney(pool.PricePerNodeMonth), formatMoney(pool.TotalMonthly),
			func() string {
				if pool.RISavings > 0 {
					return fmt.Sprintf("$%s", formatMoney(pool.RISavings))
				}
				return "—"
			}()))
	}
	if totalRISavings > 0 {
		sb.WriteString(fmt.Sprintf(`<tr style="background:rgba(16,185,129,0.05)">
			<td colspan="7" style="text-align:right;font-weight:600;color:var(--text-secondary)">Total RI Savings Potential (1-year):</td>
			<td></td><td class="money green" style="font-size:1rem">$%s/mo</td></tr>`, formatMoney(totalRISavings)))
	}
	sb.WriteString(`</tbody></table></div></div>`)

	// ─── Namespace & Deployment Breakdown ──────────────────────────────
	sb.WriteString(`<div class="section">
		<div class="section-header">
			<div class="section-title">📦 Namespace & Deployment Costs</div>
			<div class="section-actions"><button class="btn-sm active" onclick="toggleAll(true)">Expand All</button><button class="btn-sm" onclick="toggleAll(false)">Collapse All</button></div>
		</div>
		<div class="table-wrap"><table>
		<thead><tr><th>Namespace / Workload</th><th>Kind</th><th>CPU</th><th>Memory</th><th>Replicas</th><th>Share</th><th>Est. Cost/Mo</th></tr></thead>`)

	for i, ns := range report.NamespaceCosts {
		groupID := fmt.Sprintf("ns-%d", i)
		sb.WriteString(fmt.Sprintf(`<tbody class="ns-group"><tr class="ns-header" onclick="toggleNs('%s',this)">
			<td><span class="ns-toggle">▶</span><strong>%s</strong></td>
			<td><span class="tag tag-regular">%d pods</span></td>
			<td>%.1f cores</td>
			<td>%.1f GB</td>
			<td>—</td>
			<td>%.1f%%</td>
			<td class="money red">%s</td>
		</tr></tbody>`, groupID, ns.Name, ns.PodCount, ns.CPUCores, ns.MemoryGB, ns.WeightedShare*100, cloudFormatCost(ns.EstimatedCost)))

		// Deployment rows
		if len(ns.Deployments) > 0 {
			sb.WriteString(fmt.Sprintf(`<tbody class="dep-rows" id="%s">`, groupID))
			for _, dep := range ns.Deployments {
				kindTag := "tag-deploy"
				if dep.Kind == "StatefulSet" {
					kindTag = "tag-sts"
				} else if dep.Kind == "DaemonSet" {
					kindTag = "tag-ds"
				}
				sb.WriteString(fmt.Sprintf(`<tr class="dep-row">
					<td>%s</td>
					<td><span class="tag %s">%s</span></td>
					<td>%.2f</td>
					<td>%.1f GB</td>
					<td>%d</td>
					<td style="color:var(--text-muted)">%.1f%%</td>
					<td class="money" style="color:var(--text-secondary)">%s</td>
				</tr>`, dep.Name, kindTag, dep.Kind, dep.CPUCores, dep.MemoryGB, dep.Replicas, dep.NSShare*100, cloudFormatCost(dep.EstimatedCost)))
			}
			sb.WriteString(`</tbody>`)
		}
	}
	sb.WriteString(`</table></div></div>`)

	// ─── Optimization Scenarios ────────────────────────────────────────
	if len(report.OptimizationScenarios) > 0 {
		sb.WriteString(`<div class="section">
			<div class="section-header"><div class="section-title">🎯 Optimization Opportunities</div></div>
			<div class="scenarios-grid">`)
		for _, s := range report.OptimizationScenarios {
			sb.WriteString(fmt.Sprintf(`<div class="scenario-card">
				<div class="scenario-top">
					<div class="scenario-name">%s</div>
					<div class="scenario-save">↓ $%s – $%s/mo</div>
				</div>
				<div class="scenario-desc">%s</div>
				<div class="scenario-pills">
					<span class="pill">⏱️ %s</span>
					<span class="pill">💪 %s effort</span>
					<span class="pill">⚠️ %s risk</span>
				</div>
			</div>`, s.Name, formatMoney(s.Savings.Low), formatMoney(s.Savings.High), s.Description, s.Timeline, s.Effort, s.Risk))
		}
		sb.WriteString(`</div></div>`)
	}

	// ─── Disclaimers ───────────────────────────────────────────────────
	sb.WriteString(`<div class="disclaimer-bar"><h4>⚠️ Important Disclaimers</h4><ul>`)
	for _, d := range report.Disclaimers {
		sb.WriteString(fmt.Sprintf(`<li>%s</li>`, d))
	}
	sb.WriteString(`</ul></div>`)

	// Footer
	sb.WriteString(fmt.Sprintf(`<div class="footer">Generated by <strong>OpsCart FinOps Engine</strong> v0.7.0 • %s • Pricing source: %s</div>`,
		report.Timestamp.Format("2006-01-02 15:04:05 MST"), report.PricingSource))

	sb.WriteString(`</main></div>`)

	// ─── JavaScript ────────────────────────────────────────────────────
	sb.WriteString(`
<script>
function toggleNs(id, header) {
	const rows = document.getElementById(id);
	if (!rows) return;
	rows.classList.toggle('show');
	header.classList.toggle('open');
}
function toggleAll(expand) {
	document.querySelectorAll('.dep-rows').forEach(el => {
		if (expand) el.classList.add('show');
		else el.classList.remove('show');
	});
	document.querySelectorAll('.ns-header').forEach(el => {
		if (expand) el.classList.add('open');
		else el.classList.remove('open');
	});
}
</script>`)

	sb.WriteString(`</body></html>`)
	return sb.String()
}

// formatMoney formats a float as comma-separated currency
func formatMoney(amount float64) string {
	s := fmt.Sprintf("%.0f", amount)
	if len(s) <= 3 {
		return s
	}
	// Add commas
	result := ""
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result += ","
		}
		result += string(c)
	}
	return result
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

func cloudFormatCost(cr models.CostRange) string {
	if cr.Best == 0 {
		return "—"
	}
	if cr.Low == cr.Best && cr.Best == cr.High {
		return fmt.Sprintf("$%.0f", cr.Best)
	}
	return fmt.Sprintf("$%.0f", cr.Best)
}

func sanitizeFilename(name string) string {
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "_")
	return r.Replace(name)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
