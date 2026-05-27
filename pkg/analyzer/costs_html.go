package analyzer

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// CostsHTMLData holds all data for the costs HTML template
type CostsHTMLData struct {
	ClusterName      string
	GeneratedAt      time.Time
	MonthlyCost      float64
	HasCost          bool
	Method           string
	Confidence       string
	TotalAllocated   float64
	Unallocated      float64
	AllocatedPct     float64
	UnallocatedPct   float64
	Namespaces       []CostsNSRow
	Scenarios        []CostsScenarioRow
	TotalSavingsLow  float64
	TotalSavingsBest float64
	TotalSavingsHigh float64
	SavingsPct       float64
	AfterCost        float64
	Assumptions      []string
	Disclaimers      []string
	TotalCPU         float64
	TotalMem         float64
}

type DeploymentHTMLRow struct {
	Name       string
	Kind       string
	Replicas   int
	CPUCores   float64
	MemoryGB   float64
	NSSharePct float64 // NSShare * 100
	CostBest   float64
	CostLow    float64
	CostHigh   float64
}

type CostsNSRow struct {
	Name          string
	WeightedShare float64
	CPUCores      float64
	MemoryGB      float64
	PodCount      int
	IdlePods      int
	WasteScore    float64
	CostBest      float64
	CostLow       float64
	CostHigh      float64
	Confidence    string
	ShareBarWidth int // 0-100 for CSS width
	WasteClass    string
	Deployments   []DeploymentHTMLRow
}

type CostsScenarioRow struct {
	Num         int
	Name        string
	Description string
	SavingsLow  float64
	SavingsBest float64
	SavingsHigh float64
	Impact      string
	Effort      string
	Risk        string
	Timeline    string
	Actions     []string
	EffortClass string // css class
	RiskClass   string
	HasDollars  bool
}

// GenerateCostsHTML renders a FinOps-grade HTML report for the costs command
func GenerateCostsHTML(estimate *models.CostEstimate, outputPath string) error {
	noCost := estimate.TotalClusterCost <= 0

	// Build namespace rows
	totalAllocated := 0.0
	totalCPU, totalMem := 0.0, 0.0
	var nsRows []CostsNSRow
	for _, ns := range estimate.NamespaceCosts {
		totalAllocated += ns.EstimatedCost.Best
		totalCPU += ns.CPUCores
		totalMem += ns.MemoryGB

		wasteClass := ""
		if ns.WasteScore > 70 {
			wasteClass = "waste-high"
		} else if ns.WasteScore > 40 {
			wasteClass = "waste-medium"
		}

		shareBarWidth := int(ns.WeightedShare * 100 / 0.5) // scale: 0.5% share = 1px, 50% = 100px
		if shareBarWidth > 100 {
			shareBarWidth = 100
		}

		var depRows []DeploymentHTMLRow
		for _, d := range ns.Deployments {
			depRows = append(depRows, DeploymentHTMLRow{
				Name:       d.Name,
				Kind:       d.Kind,
				Replicas:   d.Replicas,
				CPUCores:   d.CPUCores,
				MemoryGB:   d.MemoryGB,
				NSSharePct: d.NSShare * 100,
				CostBest:   d.EstimatedCost.Best,
				CostLow:    d.EstimatedCost.Low,
				CostHigh:   d.EstimatedCost.High,
			})
		}

		nsRows = append(nsRows, CostsNSRow{
			Name:          ns.Name,
			WeightedShare: ns.WeightedShare * 100,
			CPUCores:      ns.CPUCores,
			MemoryGB:      ns.MemoryGB,
			PodCount:      ns.PodCount,
			IdlePods:      ns.IdlePods,
			WasteScore:    ns.WasteScore,
			CostBest:      ns.EstimatedCost.Best,
			CostLow:       ns.EstimatedCost.Low,
			CostHigh:      ns.EstimatedCost.High,
			Confidence:    determineConfidence(ns),
			ShareBarWidth: shareBarWidth,
			WasteClass:    wasteClass,
			Deployments:   depRows,
		})
	}

	unallocated := estimate.TotalClusterCost - totalAllocated
	if unallocated < 0 {
		unallocated = 0
	}

	// Build scenario rows
	var scenarioRows []CostsScenarioRow
	for i, s := range estimate.OptimizationScenarios {
		effortClass := map[string]string{"Low": "effort-low", "Medium": "effort-med", "High": "effort-high"}[s.Effort]
		riskClass := map[string]string{"Low": "risk-low", "Medium": "risk-med", "High": "risk-high"}[s.Risk]
		scenarioRows = append(scenarioRows, CostsScenarioRow{
			Num:         i + 1,
			Name:        s.Name,
			Description: s.Description,
			SavingsLow:  s.Savings.Low,
			SavingsBest: s.Savings.Best,
			SavingsHigh: s.Savings.High,
			Impact:      s.Impact,
			Effort:      s.Effort,
			Risk:        s.Risk,
			Timeline:    s.Timeline,
			Actions:     s.Actions,
			EffortClass: effortClass,
			RiskClass:   riskClass,
			HasDollars:  s.Savings.Best > 0,
		})
	}

	savingsPct := 0.0
	afterCost := estimate.TotalClusterCost
	if !noCost && estimate.TotalClusterCost > 0 {
		savingsPct = estimate.TotalSavingsPotential.Best / estimate.TotalClusterCost * 100
		afterCost = estimate.TotalClusterCost - estimate.TotalSavingsPotential.Best
	}

	allocatedPct, unallocatedPct := 0.0, 0.0
	if estimate.TotalClusterCost > 0 {
		allocatedPct = totalAllocated / estimate.TotalClusterCost * 100
		unallocatedPct = unallocated / estimate.TotalClusterCost * 100
	} else {
		allocatedPct = 100
	}

	clusterName := estimate.ClusterName
	if clusterName == "" {
		clusterName = "cluster"
	}
	data := CostsHTMLData{
		ClusterName:      clusterName,
		GeneratedAt:      time.Now(),
		MonthlyCost:      estimate.TotalClusterCost,
		HasCost:          !noCost,
		Method:           estimate.Method,
		Confidence:       estimate.Confidence,
		TotalAllocated:   totalAllocated,
		Unallocated:      unallocated,
		AllocatedPct:     allocatedPct,
		UnallocatedPct:   unallocatedPct,
		Namespaces:       nsRows,
		Scenarios:        scenarioRows,
		TotalSavingsLow:  estimate.TotalSavingsPotential.Low,
		TotalSavingsBest: estimate.TotalSavingsPotential.Best,
		TotalSavingsHigh: estimate.TotalSavingsPotential.High,
		SavingsPct:       savingsPct,
		AfterCost:        afterCost,
		Assumptions:      estimate.Assumptions,
		Disclaimers:      estimate.Disclaimers,
		TotalCPU:         totalCPU,
		TotalMem:         totalMem,
	}

	tmpl, err := template.New("costs").Funcs(template.FuncMap{
		"money":  func(f float64) string { return fmt.Sprintf("$%.0f", f) },
		"moneyd": func(f float64) string { return fmt.Sprintf("$%.2f", f) },
		"pct":    func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
		"f2":     func(f float64) string { return fmt.Sprintf("%.2f", f) },
		"join":   func(ss []string, sep string) string { return strings.Join(ss, sep) },
		"gt0":    func(f float64) bool { return f > 0 },
		"gti":    func(i int, n int) bool { return i > n },
		"not":    func(b bool) bool { return !b },
		"mul":    func(a, b float64) float64 { return a * b },
		"wasteColor": func(score float64) string {
			if score > 70 {
				return "#e53e3e" // red
			}
			if score > 40 {
				return "#dd6b20" // orange
			}
			if score > 20 {
				return "#d69e2e" // yellow
			}
			return "#38a169" // green (low waste)
		},
		"confClass": func(c string) string {
			switch c {
			case "High":
				return "conf-high"
			case "Medium":
				return "conf-med"
			default:
				return "conf-low"
			}
		},
	}).Parse(costsHTMLTemplate)
	if err != nil {
		return fmt.Errorf("parsing HTML template: %w", err)
	}

	// Determine output file
	filename := outputPath
	if filename == "" {
		today := time.Now().Format("2006-01-02")
		dir := filepath.Join("reports", today)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("creating reports dir: %w", err)
		}
		ts := time.Now().Format("1504")
		filename = filepath.Join(dir, fmt.Sprintf("costs-%s-%s.html", strings.ReplaceAll(clusterName, "/", "-"), ts))
	}

	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("creating HTML file: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("rendering HTML: %w", err)
	}

	fmt.Printf("✅ FinOps HTML report written → %s\n", filename)
	return nil
}

// SetClusterNameInHTML allows the caller to inject the cluster name before rendering
// (called from main.go after we know the context name)
func GenerateCostsHTMLWithCluster(estimate *models.CostEstimate, clusterName, outputPath string) error {
	// Temporarily patch estimate — we use a wrapper approach
	_ = clusterName // handled via global set in main
	return GenerateCostsHTML(estimate, outputPath)
}

const costsHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>FinOps Cost Report — {{.ClusterName}}</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#f0f4f8;color:#1a202c;padding:24px}
.container{max-width:1400px;margin:0 auto}
.header{background:linear-gradient(135deg,#1a365d 0%,#2b6cb0 100%);color:#fff;padding:32px 36px;border-radius:12px;margin-bottom:24px}
.header h1{font-size:26px;font-weight:700;margin-bottom:6px}
.header-meta{font-size:13px;opacity:.85}
.kpi-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:16px;margin-bottom:24px}
.kpi{background:#fff;border-radius:10px;padding:20px 24px;box-shadow:0 1px 4px rgba(0,0,0,.08);border-top:4px solid #2b6cb0}
.kpi.green{border-color:#38a169}.kpi.orange{border-color:#dd6b20}.kpi.red{border-color:#e53e3e}.kpi.gray{border-color:#718096}
.kpi-label{font-size:12px;font-weight:600;text-transform:uppercase;letter-spacing:.05em;color:#718096;margin-bottom:8px}
.kpi-value{font-size:28px;font-weight:700;color:#1a202c;line-height:1}
.kpi-sub{font-size:12px;color:#718096;margin-top:6px}
.card{background:#fff;border-radius:10px;padding:24px;box-shadow:0 1px 4px rgba(0,0,0,.08);margin-bottom:24px}
.card-title{font-size:16px;font-weight:700;color:#2d3748;margin-bottom:16px;display:flex;align-items:center;gap:8px}
.disclaimer{background:#fffbeb;border:1px solid #f6ad55;border-radius:8px;padding:14px 18px;margin-bottom:24px;font-size:13px;color:#744210}
.disclaimer ul{margin-left:16px;margin-top:6px}
table{width:100%;border-collapse:collapse;font-size:13px}
thead th{background:#edf2f7;padding:10px 12px;text-align:left;font-weight:600;color:#4a5568;border-bottom:2px solid #e2e8f0;white-space:nowrap}
tbody td{padding:10px 12px;border-bottom:1px solid #e2e8f0;vertical-align:middle}
tbody tr:hover{background:#f7fafc}
.bar-wrap{background:#e2e8f0;border-radius:4px;height:8px;width:120px;overflow:hidden}
.bar-fill{background:linear-gradient(90deg,#3182ce,#63b3ed);height:100%;border-radius:4px}
.badge{display:inline-block;padding:2px 10px;border-radius:10px;font-size:11px;font-weight:600}
.conf-high{background:#c6f6d5;color:#22543d}
.conf-med{background:#fefcbf;color:#744210}
.conf-low{background:#e2e8f0;color:#4a5568}
.waste-high td:first-child{border-left:3px solid #e53e3e}
.waste-medium td:first-child{border-left:3px solid #dd6b20}
.idle-tag{color:#e53e3e;font-weight:600}
.unalloc-row td{background:#edf2f7;font-style:italic;color:#718096}
.total-row td{background:#ebf8ff;font-weight:700;border-top:2px solid #3182ce}
.scenario-card{border:1px solid #e2e8f0;border-radius:8px;padding:18px;margin-bottom:16px}
.scenario-header{display:flex;justify-content:space-between;align-items:flex-start;gap:16px;margin-bottom:10px}
.scenario-name{font-size:15px;font-weight:700;color:#2d3748}
.savings-badge{background:linear-gradient(135deg,#38a169,#48bb78);color:#fff;padding:6px 14px;border-radius:20px;font-size:13px;font-weight:600;white-space:nowrap}
.savings-na{background:#e2e8f0;color:#718096;padding:6px 14px;border-radius:20px;font-size:13px;white-space:nowrap}
.scenario-meta{display:flex;gap:10px;flex-wrap:wrap;margin-bottom:10px}
.effort-low{background:#c6f6d5;color:#22543d;padding:3px 10px;border-radius:8px;font-size:12px;font-weight:600}
.effort-med{background:#fefcbf;color:#744210;padding:3px 10px;border-radius:8px;font-size:12px;font-weight:600}
.effort-high{background:#fed7d7;color:#742a2a;padding:3px 10px;border-radius:8px;font-size:12px;font-weight:600}
.risk-low{background:#c6f6d5;color:#22543d;padding:3px 10px;border-radius:8px;font-size:12px;font-weight:600}
.risk-med{background:#fefcbf;color:#744210;padding:3px 10px;border-radius:8px;font-size:12px;font-weight:600}
.risk-high{background:#fed7d7;color:#742a2a;padding:3px 10px;border-radius:8px;font-size:12px;font-weight:600}
.action-list{margin-left:18px;margin-top:8px;color:#4a5568;font-size:13px}
.action-list li{margin-bottom:4px}
.savings-summary{background:linear-gradient(135deg,#1a365d,#2b6cb0);color:#fff;border-radius:10px;padding:24px 28px;margin-bottom:24px;display:flex;gap:40px;align-items:center;flex-wrap:wrap}
.ss-item{text-align:center}
.ss-label{font-size:12px;opacity:.8;margin-bottom:4px}
.ss-value{font-size:24px;font-weight:700}
.ss-sub{font-size:12px;opacity:.7;margin-top:2px}
.tip-box{background:#ebf8ff;border:1px solid #90cdf4;border-radius:8px;padding:14px 18px;margin-bottom:24px;font-size:13px;color:#2c5282}
.footer{text-align:center;margin-top:32px;padding:16px;color:#718096;font-size:12px}
@media print{body{background:#fff;padding:0}.tip-box{display:none}}
</style>
</head>
<body>
<div class="container">

  <!-- Header -->
  <div class="header">
    <h1>💰 FinOps Cost Analysis Report</h1>
    <div class="header-meta">
      <strong>Cluster:</strong> {{.ClusterName}} &nbsp;|&nbsp;
      <strong>Generated:</strong> {{.GeneratedAt.Format "January 2, 2006 3:04 PM"}} &nbsp;|&nbsp;
      <strong>Method:</strong> {{.Method}} &nbsp;|&nbsp;
      <strong>Confidence:</strong> {{.Confidence}}
    </div>
  </div>

  {{if (not .HasCost)}}
  <div class="tip-box">
    💡 <strong>Resource Share Mode:</strong> No monthly cost provided.
    Showing CPU/memory resource shares only.
    Re-run with <code>--monthly-cost &lt;amount&gt;</code> to see dollar estimates.
  </div>
  {{end}}

  <!-- Disclaimers -->
  <div class="disclaimer">
    ⚠️ <strong>Important Disclaimers:</strong>
    <ul>{{range .Disclaimers}}<li>{{.}}</li>{{end}}</ul>
  </div>

  <!-- KPI Cards -->
  {{if .HasCost}}
  <div class="kpi-grid">
    <div class="kpi blue">
      <div class="kpi-label">Monthly Cluster Cost</div>
      <div class="kpi-value">{{money .MonthlyCost}}</div>
      <div class="kpi-sub">User-provided anchor</div>
    </div>
    <div class="kpi green">
      <div class="kpi-label">Allocated to Namespaces</div>
      <div class="kpi-value">{{money .TotalAllocated}}</div>
      <div class="kpi-sub">{{pct .AllocatedPct}} of total</div>
    </div>
    <div class="kpi gray">
      <div class="kpi-label">Unallocated / Shared</div>
      <div class="kpi-value">{{money .Unallocated}}</div>
      <div class="kpi-sub">{{pct .UnallocatedPct}} — node OS, DaemonSets</div>
    </div>
    {{if gt0 .TotalSavingsBest}}
    <div class="kpi orange">
      <div class="kpi-label">Optimization Potential</div>
      <div class="kpi-value">{{money .TotalSavingsBest}}</div>
      <div class="kpi-sub">{{pct .SavingsPct}} potential reduction</div>
    </div>
    {{end}}
  </div>
  {{else}}
  <div class="kpi-grid">
    <div class="kpi blue">
      <div class="kpi-label">Total CPU Requested</div>
      <div class="kpi-value">{{f2 .TotalCPU}}</div>
      <div class="kpi-sub">cores across all namespaces</div>
    </div>
    <div class="kpi green">
      <div class="kpi-label">Total Memory Requested</div>
      <div class="kpi-value">{{f2 .TotalMem}}</div>
      <div class="kpi-sub">GB across all namespaces</div>
    </div>
  </div>
  {{end}}

  {{if .HasCost}}
  {{if gt0 .TotalSavingsBest}}
  <!-- Savings Summary Bar -->
  <div class="savings-summary">
    <div class="ss-item">
      <div class="ss-label">Current Monthly Cost</div>
      <div class="ss-value">{{money .MonthlyCost}}</div>
    </div>
    <div style="font-size:28px;opacity:.6">→</div>
    <div class="ss-item">
      <div class="ss-label">After Optimizations</div>
      <div class="ss-value">{{money .AfterCost}}</div>
      <div class="ss-sub">best case</div>
    </div>
    <div style="font-size:28px;opacity:.6">=</div>
    <div class="ss-item">
      <div class="ss-label">Potential Savings</div>
      <div class="ss-value">{{money .TotalSavingsBest}} / mo</div>
      <div class="ss-sub">{{money .TotalSavingsLow}} – {{money .TotalSavingsHigh}} range</div>
    </div>
    <div class="ss-item">
      <div class="ss-label">Reduction</div>
      <div class="ss-value">{{pct .SavingsPct}}</div>
    </div>
  </div>
  {{end}}
  {{end}}

  <!-- Namespace Table -->
  <div class="card">
    <div class="card-title">
      📊 {{if .HasCost}}Namespace Cost Allocation{{else}}Namespace Resource Share{{end}}
    </div>
    <table>
      <thead>
        <tr>
          <th>Namespace</th>
          <th>Share %</th>
          <th>Visual</th>
          <th>CPU Cores</th>
          <th>Mem GB</th>
          <th>Pods</th>
          <th>Idle</th>
          {{if .HasCost}}<th>Est. Cost/Mo</th>{{end}}
          {{if .HasCost}}<th>Range (Low – High)</th>{{end}}
          <th>Confidence</th>
          <th>Waste</th>
        </tr>
      </thead>
      <tbody>
        {{range .Namespaces}}
        <tr class="{{.WasteClass}}">
          <td><strong>{{.Name}}</strong></td>
          <td>{{pct .WeightedShare}}</td>
          <td>
            <div class="bar-wrap">
              <div class="bar-fill" style="width:{{.ShareBarWidth}}%"></div>
            </div>
          </td>
          <td>{{f2 .CPUCores}}</td>
          <td>{{f2 .MemoryGB}}</td>
          <td>{{.PodCount}}</td>
          <td>{{if gt .IdlePods 0}}<span class="idle-tag">{{.IdlePods}} ⚠</span>{{else}}—{{end}}</td>
          {{if $.HasCost}}<td><strong>{{moneyd .CostBest}}</strong></td>{{end}}
          {{if $.HasCost}}<td style="color:#718096;font-size:12px">{{moneyd .CostLow}} – {{moneyd .CostHigh}}</td>{{end}}
          <td><span class="badge {{confClass .Confidence}}">{{.Confidence}}</span></td>
          <td>
            <div style="display:flex;align-items:center;gap:6px">
              <div style="width:44px;height:6px;background:#e2e8f0;border-radius:3px;overflow:hidden">
                <div style="width:{{printf "%.0f" .WasteScore}}%;height:100%;background:{{wasteColor .WasteScore}};border-radius:3px"></div>
              </div>
              <span style="font-weight:700;font-size:12px;color:{{wasteColor .WasteScore}}">{{printf "%.0f" .WasteScore}}</span>
            </div>
          </td>
        </tr>
        {{range .Deployments}}
        <tr style="background:#f9fafb;font-size:12px;color:#4a5568">
          <td style="padding-left:28px">
            <span style="color:#a0aec0">└──</span>
            <strong>{{.Name}}</strong>
            {{if eq .Kind "StatefulSet"}}<span style="background:#e9d8fd;color:#553c9a;padding:1px 5px;border-radius:3px;font-size:10px;margin-left:4px">STS</span>{{end}}
            {{if eq .Kind "DaemonSet"}}<span style="background:#bee3f8;color:#2a4365;padding:1px 5px;border-radius:3px;font-size:10px;margin-left:4px">DS</span>{{end}}
          </td>
          <td style="color:#718096">{{pct .NSSharePct}}</td>
          <td></td>
          <td style="color:#718096">{{f2 .CPUCores}}</td>
          <td style="color:#718096">{{f2 .MemoryGB}}</td>
          <td style="color:#718096">{{.Replicas}}</td>
          <td>—</td>
          {{if $.HasCost}}<td>{{if gt0 .CostBest}}<strong>{{moneyd .CostBest}}</strong>{{else}}—{{end}}</td>{{end}}
          {{if $.HasCost}}<td style="color:#718096;font-size:11px">{{if gt0 .CostBest}}{{moneyd .CostLow}} – {{moneyd .CostHigh}}{{end}}</td>{{end}}
          <td>—</td>
          <td>—</td>
        </tr>
        {{end}}
        {{end}}
        {{if .HasCost}}
        <tr class="unalloc-row">
          <td colspan="7"><em>Unallocated / Shared Infrastructure (node OS, DaemonSets, reserved)</em></td>
          <td><strong>{{money .Unallocated}}</strong></td>
          <td colspan="3" style="color:#718096;font-size:12px">{{pct .UnallocatedPct}} of total</td>
        </tr>
        <tr class="total-row">
          <td colspan="7"><strong>TOTAL</strong></td>
          <td><strong>{{money .MonthlyCost}}</strong></td>
          <td colspan="3"></td>
        </tr>
        {{end}}
      </tbody>
    </table>
    {{if .HasCost}}
    <p style="font-size:12px;color:#718096;margin-top:12px">
      🔴 High waste (score &gt;70) &nbsp; 🟡 Medium waste (score &gt;40) &nbsp; ✅ Low waste &nbsp; ⚠ Idle pods detected
    </p>
    {{end}}
  </div>

  <!-- Optimization Scenarios -->
  {{if .Scenarios}}
  <div class="card">
    <div class="card-title">🚀 Optimization Scenarios</div>
    {{range .Scenarios}}
    <div class="scenario-card">
      <div class="scenario-header">
        <div>
          <div class="scenario-name">{{.Num}}. {{.Name}}</div>
          <div style="font-size:13px;color:#718096;margin-top:4px">{{.Description}}</div>
        </div>
        {{if .HasDollars}}
        <div class="savings-badge">💰 {{money .SavingsBest}} / mo</div>
        {{else}}
        <div class="savings-na">📊 Resource savings only</div>
        {{end}}
      </div>
      <div class="scenario-meta">
        <span class="{{.EffortClass}}">Effort: {{.Effort}}</span>
        <span class="{{.RiskClass}}">Risk: {{.Risk}}</span>
        <span style="background:#e2e8f0;color:#4a5568;padding:3px 10px;border-radius:8px;font-size:12px">⏱ {{.Timeline}}</span>
        {{if .HasDollars}}
        <span style="background:#e6fffa;color:#234e52;padding:3px 10px;border-radius:8px;font-size:12px">
          Range: {{money .SavingsLow}} – {{money .SavingsHigh}} / mo
        </span>
        {{end}}
      </div>
      <div style="font-size:13px;color:#4a5568;margin-bottom:8px"><strong>Impact:</strong> {{.Impact}}</div>
      {{if .Actions}}
      <ul class="action-list">
        {{range .Actions}}<li>{{.}}</li>{{end}}
      </ul>
      {{end}}
    </div>
    {{end}}
  </div>
  {{end}}

  <!-- Assumptions -->
  <div class="card">
    <div class="card-title">📋 Assumptions & Methodology</div>
    <ol style="margin-left:18px;font-size:13px;color:#4a5568;line-height:1.8">
      {{range .Assumptions}}<li>{{.}}</li>{{end}}
    </ol>
    <p style="margin-top:14px;font-size:13px;color:#718096">
      <strong>Formula:</strong>
      Namespace weighted share = (CPU% + Mem%) / 2 &nbsp;|&nbsp;
      Namespace cost = weighted share × total cluster cost &nbsp;|&nbsp;
      Confidence = High (&gt;15% share), Medium (5–15%), Low (&lt;5%)
    </p>
  </div>

  <div class="footer">
    Generated by <strong>OpsCart K8s Watcher</strong> — FinOps Cost Analysis<br>
    <small>Numbers are estimates only. Validate with Azure Cost Management for billing accuracy.</small>
  </div>
</div>
</body>
</html>`
