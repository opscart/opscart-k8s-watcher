package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// ── Stub pages ────────────────────────────────────────────────────────────────

type stubPageData struct {
	Title         string
	ActivePage    string
	DashHref      string
	WrHref        string
	CostsHref     string
	InfraHref     string
	NsHref        string
	OptHref       string
	WasteHref     string
	SecurityHref  string
	IncidentsHref string
	ClusterName   string
	CriticalCount int
	Clusters      []sidebarCluster
}

var getStubTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("stub.html").
			ParseFS(templateFS, "templates/base.html", "templates/sidebar.html", "templates/stub.html"),
	)
})

func (srv *server) handleStubPage(page, title string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := srv.activeCtx(r)
		state := srv.getState(ctx)
		state.mu.RLock()
		scan := state.scan
		state.mu.RUnlock()

		q := "?cluster=" + url.QueryEscape(ctx)
		data := stubPageData{
			Title:         title,
			ActivePage:    page,
			DashHref:      "/" + q,
			WrHref:        "/warroom" + q,
			CostsHref:     "/costs" + q,
			InfraHref:     "/infrastructure" + q,
			NsHref:        "/namespaces" + q,
			OptHref:       "/optimizations" + q,
			WasteHref:     "/waste" + q,
			SecurityHref:  "/security" + q,
			IncidentsHref: "/incidents" + q,
			ClusterName:   displayName(ctx),
			CriticalCount: countCriticalIssues(scan),
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		var buf strings.Builder
		if err := getStubTmpl().Execute(&buf, data); err != nil {
			log.Printf("stub template: %v", err)
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(buf.String()))
	}
}

// ── Infrastructure page ───────────────────────────────────────────────────────

func (srv *server) handleInfrastructurePage(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)

	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		if err := state.refresh(srv.clusterList); err != nil {
			http.Error(w, "scan failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		state.mu.RLock()
		scan = state.scan
		state.mu.RUnlock()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, renderInfrastructurePage(scan, ctx, srv.clusterList))
}

type infrastructurePageData struct {
	ClusterName string
	DashURL     string
	Sidebar     template.HTML
	PoolCount   int
	TotalNodes  int
	TotalCores  string
	TotalMemGB  string
	TotalCost   string
	TotalRI1yr  string
	HasRI       bool
	HasPools    bool
	PoolRows    []template.HTML
	CPUReqStr   string
	MemReqStr   string
	RI1yrCell   template.HTML
	RI3yrCell   template.HTML
	ScannedAtMs int64
}

func renderInfrastructurePage(scan *clusterScan, activeCtx string, clusterList []string) string {
	var pools []models.NodePoolCost
	scannedAt := time.Now()
	clusterName := displayName(activeCtx)
	if scan != nil && scan.report != nil {
		pools = scan.report.NodePoolCosts
		scannedAt = scan.report.Timestamp
		clusterName = scan.report.ClusterName
	}

	var totalNodes int
	var totalCores, totalMemGB, totalCost, totalRI1yr, totalRI3yr float64
	for _, p := range pools {
		totalNodes += p.NodeCount
		totalCores += p.TotalCPUCapacity
		totalMemGB += p.TotalMemoryCapacity
		totalCost += p.TotalMonthly
		totalRI1yr += p.RISavings
		totalRI3yr += p.RISavings3yr
	}

	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}

	// Pre-render pool rows
	var poolRows []template.HTML
	for _, p := range pools {
		poolRows = append(poolRows, template.HTML(renderInfraPoolRow(p)))
	}

	ri1yrCell := template.HTML(`<span class="ri-na">—</span>`)
	if totalRI1yr > 0 {
		ri1yrCell = template.HTML(fmt.Sprintf(`<span class="ri-val">$%s</span>`, formatMoney(totalRI1yr)))
	}
	ri3yrCell := template.HTML(`<span class="ri-na">—</span>`)
	if totalRI3yr > 0 {
		ri3yrCell = template.HTML(fmt.Sprintf(`<span class="ri-val">$%s</span>`, formatMoney(totalRI3yr)))
	}

	data := infrastructurePageData{
		ClusterName: clusterName,
		DashURL:     "/" + q,
		Sidebar:     template.HTML(buildSidebar("infrastructure", activeCtx, clusterName, clusterList, countCriticalIssues(scan))),
		PoolCount:   len(pools),
		TotalNodes:  totalNodes,
		TotalCores:  fmt.Sprintf("%.0f", totalCores),
		TotalMemGB:  fmt.Sprintf("%.0f GB", totalMemGB),
		TotalCost:   "$" + formatMoney(totalCost),
		TotalRI1yr: func() string {
			if totalRI1yr > 0 {
				return "$" + formatMoney(totalRI1yr)
			}
			return ""
		}(),
		HasRI:       totalRI1yr > 0,
		HasPools:    len(pools) > 0,
		PoolRows:    poolRows,
		CPUReqStr:   fmt.Sprintf("%.1f / %.1f cores", sumCPUReq(pools), totalCores),
		MemReqStr:   fmt.Sprintf("%.1f / %.1f GB", sumMemReq(pools), totalMemGB),
		RI1yrCell:   ri1yrCell,
		RI3yrCell:   ri3yrCell,
		ScannedAtMs: scannedAt.UnixMilli(),
	}

	var buf strings.Builder
	if err := getInfrastructureTmpl().Execute(&buf, data); err != nil {
		log.Printf("infrastructure template: %v", err)
		return ""
	}
	return buf.String()
}

var getInfrastructureTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("infrastructure.html").
			ParseFS(templateFS, "templates/base.html", "templates/infrastructure.html"),
	)
})

func renderInfraPoolRow(p models.NodePoolCost) string {
	// Priority badge
	priTag := `<span class="tag tag-regular">On-Demand</span>`
	if strings.EqualFold(p.Priority, "spot") {
		priTag = `<span class="tag tag-spot">Spot</span>`
	}
	// Mode badge
	modeTag := `<span class="tag tag-user">User</span>`
	if strings.EqualFold(p.Mode, "system") {
		modeTag = `<span class="tag tag-system">System</span>`
	}
	// OS badge (only Windows, Linux is default)
	osTag := ""
	if strings.EqualFold(p.OS, "windows") {
		osTag = `<span class="tag tag-windows">Windows</span>`
	}

	// CPU utilization
	cpuPct := p.CPUUtilizationPct
	if cpuPct > 100 {
		cpuPct = 100
	}
	cpuColor := "fill-green"
	if p.CPUUtilizationPct > 80 {
		cpuColor = "fill-red"
	} else if p.CPUUtilizationPct > 50 {
		cpuColor = "fill-yellow"
	}

	// Memory utilization
	memPct := p.MemoryUtilizationPct
	if memPct > 100 {
		memPct = 100
	}
	memColor := "fill-green"
	if p.MemoryUtilizationPct > 80 {
		memColor = "fill-red"
	} else if p.MemoryUtilizationPct > 50 {
		memColor = "fill-yellow"
	}

	// Node count (show autoscaler max if set)
	nodeCell := fmt.Sprintf(`<div style="text-align:center;font-weight:600">%d</div>`, p.NodeCount)
	if p.MaxNodeCount > p.NodeCount {
		nodeCell += fmt.Sprintf(`<div class="sub" style="text-align:center">max %d</div>`, p.MaxNodeCount)
	}

	// RI savings cells
	ri1yr := `<span class="ri-na">—</span>`
	if p.RISavings > 0 {
		ri1yr = fmt.Sprintf(`<span class="ri-val">$%s</span>`, formatMoney(p.RISavings))
	}
	ri3yr := `<span class="ri-na">—</span>`
	if p.RISavings3yr > 0 {
		ri3yr = fmt.Sprintf(`<span class="ri-val">$%s</span>`, formatMoney(p.RISavings3yr))
	}

	// SKU sub-line: cores × memory
	skuSpec := ""
	if p.CPUCoresPerNode > 0 || p.MemoryGBPerNode > 0 {
		skuSpec = fmt.Sprintf(`<div class="sub">%.0f vCPU &middot; %.0f GB</div>`, p.CPUCoresPerNode, p.MemoryGBPerNode)
	}

	return fmt.Sprintf(`<tr>
<td>
  <div class="pool-name">%s</div>
  <div class="badge-row">%s %s %s</div>
</td>
<td>
  <div class="sku">%s</div>%s
</td>
<td>%s</td>
<td class="util-cell">
  <div class="util-nums">%.1f / %.1f cores</div>
  <div class="util-bar">
    <div class="util-track"><div class="util-fill %s" style="width:%.1f%%"></div></div>
    <span class="util-pct">%.0f%%</span>
  </div>
</td>
<td class="util-cell">
  <div class="util-nums">%.1f / %.1f GB</div>
  <div class="util-bar">
    <div class="util-track"><div class="util-fill %s" style="width:%.1f%%"></div></div>
    <span class="util-pct">%.0f%%</span>
  </div>
</td>
<td style="text-align:right" class="money">$%s</td>
<td style="text-align:right" class="money">$%s</td>
<td style="text-align:right">%s</td>
<td style="text-align:right">%s</td>
</tr>`,
		p.Name,
		priTag, modeTag, osTag,
		p.VMSize, skuSpec,
		nodeCell,
		p.CPURequested, p.TotalCPUCapacity,
		cpuColor, cpuPct, p.CPUUtilizationPct,
		p.MemoryRequested, p.TotalMemoryCapacity,
		memColor, memPct, p.MemoryUtilizationPct,
		formatMoney(p.PricePerNodeMonth),
		formatMoney(p.TotalMonthly),
		ri1yr, ri3yr,
	)
}

func sumCPUReq(pools []models.NodePoolCost) float64 {
	var t float64
	for _, p := range pools {
		t += p.CPURequested
	}
	return t
}

func sumMemReq(pools []models.NodePoolCost) float64 {
	var t float64
	for _, p := range pools {
		t += p.MemoryRequested
	}
	return t
}

// ── Namespaces page ───────────────────────────────────────────────────────────

func (srv *server) handleNamespacesPage(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)

	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		if err := state.refresh(srv.clusterList); err != nil {
			http.Error(w, "scan failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		state.mu.RLock()
		scan = state.scan
		state.mu.RUnlock()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, renderNamespacesPage(scan, ctx, srv.clusterList))
}

type namespacesPageData struct {
	ClusterName      string
	DashURL          string
	NamespaceCount   int
	ProtectedCount   int
	UnprotectedCount int
	TotalCost        float64
	NSRows           []namespacesNSRow
	TotalCostCell    template.HTML
	TotalPods        int
	TotalCPU         float64
	TotalMem         float64
	Sidebar          template.HTML
}

type namespacesNSRow struct {
	Name      string
	CostCell  template.HTML
	PodCount  int
	CPUCores  float64
	MemoryGB  float64
	NetCell   template.HTML
	WasteCell template.HTML
}

var getNamespacesTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("namespaces.html").
			Funcs(template.FuncMap{"money": formatMoney}).
			ParseFS(templateFS, "templates/base.html", "templates/namespaces.html"))
})

func renderNamespacesPage(scan *clusterScan, activeCtx string, clusterList []string) string {
	var nsCosts []models.NamespaceCostInfo
	clusterName := displayName(activeCtx)
	if scan != nil && scan.report != nil {
		nsCosts = make([]models.NamespaceCostInfo, len(scan.report.NamespaceCosts))
		copy(nsCosts, scan.report.NamespaceCosts)
		sort.Slice(nsCosts, func(i, j int) bool {
			return nsCosts[i].EstimatedCost.Best > nsCosts[j].EstimatedCost.Best
		})
		clusterName = scan.report.ClusterName
	}

	protectedSet, unprotectedSet := nsNetPolicySets(scan)
	wasteCounts := wasteCountByNS(scan)

	var totalCost float64
	for _, ns := range nsCosts {
		totalCost += ns.EstimatedCost.Best
	}

	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}

	var totalPods int
	var totalCPU, totalMem float64
	var nsRows []namespacesNSRow
	for _, ns := range nsCosts {
		totalPods += ns.PodCount
		totalCPU += ns.CPUCores
		totalMem += ns.MemoryGB

		costCell := template.HTML(`<span class="muted">—</span>`)
		if ns.EstimatedCost.Best >= 1 {
			costCell = template.HTML(`<span class="money">$` + formatMoney(ns.EstimatedCost.Best) + `</span>`)
		} else if ns.EstimatedCost.Best > 0 {
			costCell = `<span class="money">&lt;$1</span>`
		}

		var netCell template.HTML
		if protectedSet[ns.Name] {
			netCell = `<span class="badge badge-ok">✅ Protected</span>`
		} else if unprotectedSet[ns.Name] {
			netCell = `<span class="badge badge-danger">❌ Unprotected</span>`
		} else {
			netCell = `<span class="muted">—</span>`
		}

		wc := wasteCounts[ns.Name]
		var wasteCell template.HTML
		switch {
		case wc == 0:
			wasteCell = `<span class="muted">—</span>`
		case wc <= 2:
			wasteCell = template.HTML(fmt.Sprintf(`<span class="badge badge-warn">⚠ %d</span>`, wc))
		default:
			wasteCell = template.HTML(fmt.Sprintf(`<span class="badge badge-danger">⚠ %d</span>`, wc))
		}

		nsRows = append(nsRows, namespacesNSRow{
			Name:      ns.Name,
			CostCell:  costCell,
			PodCount:  ns.PodCount,
			CPUCores:  ns.CPUCores,
			MemoryGB:  ns.MemoryGB,
			NetCell:   netCell,
			WasteCell: wasteCell,
		})
	}

	totalCostCell := template.HTML(`<span class="muted">—</span>`)
	if totalCost >= 1 {
		totalCostCell = template.HTML(`$` + formatMoney(totalCost))
	}

	data := namespacesPageData{
		ClusterName:      clusterName,
		DashURL:          "/" + q,
		NamespaceCount:   len(nsCosts),
		ProtectedCount:   len(protectedSet),
		UnprotectedCount: len(unprotectedSet),
		TotalCost:        totalCost,
		NSRows:           nsRows,
		TotalCostCell:    totalCostCell,
		TotalPods:        totalPods,
		TotalCPU:         totalCPU,
		TotalMem:         totalMem,
		Sidebar:          template.HTML(buildSidebar("namespaces", activeCtx, clusterName, clusterList, countCriticalIssues(scan))),
	}

	var buf strings.Builder
	if err := getNamespacesTmpl().Execute(&buf, data); err != nil {
		log.Printf("namespaces template: %v", err)
		return ""
	}
	return buf.String()
}

// nsNetPolicySets returns two maps of namespace names: one protected, one unprotected.
func nsNetPolicySets(scan *clusterScan) (protected, unprotected map[string]bool) {
	protected = make(map[string]bool)
	unprotected = make(map[string]bool)
	if scan == nil || scan.netAudit == nil {
		return
	}
	for _, ns := range scan.netAudit.ProtectedNamespaces {
		protected[ns.Name] = true
	}
	for _, ns := range scan.netAudit.UnprotectedNamespaces {
		unprotected[ns.Name] = true
	}
	return
}

// wasteCountByNS counts all waste audit items per namespace.
func wasteCountByNS(scan *clusterScan) map[string]int {
	counts := make(map[string]int)
	if scan == nil || scan.wasteAudit == nil {
		return counts
	}
	wa := scan.wasteAudit
	for _, p := range wa.StalePods {
		counts[p.Namespace]++
	}
	for _, pvc := range wa.OrphanedPVCs {
		counts[pvc.Namespace]++
	}
	for _, w := range wa.ZeroReplicaWorkloads {
		counts[w.Namespace]++
	}
	for _, j := range wa.StaleJobs {
		counts[j.Namespace]++
	}
	for _, s := range wa.OrphanedServices {
		counts[s.Namespace]++
	}
	for _, ing := range wa.BrokenIngresses {
		counts[ing.Namespace]++
	}
	for _, hpa := range wa.MisconfiguredHPAs {
		counts[hpa.Namespace]++
	}
	return counts
}

// ── Optimizations page ───────────────────────────────────────────────────────

func (srv *server) handleOptimizationsPage(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)

	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		if err := state.refresh(srv.clusterList); err != nil {
			http.Error(w, "scan failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		state.mu.RLock()
		scan = state.scan
		state.mu.RUnlock()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, renderOptimizationsPage(scan, ctx, srv.clusterList))
}

type optimizationsPageData struct {
	ClusterName string
	DashURL     string
	Sidebar     template.HTML
	// RI section
	RIPools    []riPoolRow
	TotalRI1yr string // pre-formatted "$1,234"
	TotalRI3yr string // pre-formatted or ""
	HasRI      bool
	// Waste section
	PVCCount         int
	PVCCostCell      string // pre-formatted HTML
	PVCNote          string
	ZeroReplicaCount int
	AbandonedCount   int
	ZombieCount      int
	IdleCount        int
	WasteTotal       int
	TotalWasteCost   string
	// Right-sizing
	RSCandidates []rsCandidateRow
}

type riPoolRow struct {
	Name      string
	VMSize    string
	NodeCount int
	PayGMo    string // "$9,811"
	RI1yr     string // "$3,200"
	RI3yr     string // "$2,100" or ""
}

type rsCandidateRow struct {
	Name     string
	PodCount int
	CPUCores string // "0.200"
	MemoryGB string // "0.2"
	CostCell string // pre-formatted HTML
}

func renderOptimizationsPage(scan *clusterScan, activeCtx string, clusterList []string) string {
	var pools []models.NodePoolCost
	var nsCosts []models.NamespaceCostInfo
	clusterName := displayName(activeCtx)
	if scan != nil && scan.report != nil {
		pools = scan.report.NodePoolCosts
		nsCosts = scan.report.NamespaceCosts
		clusterName = scan.report.ClusterName
	}

	// RI opportunities
	var riPoolRows []riPoolRow
	var totalRI1yr, totalRI3yr float64
	for _, p := range pools {
		if p.RISavings > 0 {
			ri3yr := ""
			if p.RISavings3yr > 0 {
				ri3yr = "$" + formatMoney(p.RISavings3yr)
			}
			riPoolRows = append(riPoolRows, riPoolRow{
				Name:      p.Name,
				VMSize:    p.VMSize,
				NodeCount: p.NodeCount,
				PayGMo:    "$" + formatMoney(p.TotalMonthly),
				RI1yr:     "$" + formatMoney(p.RISavings),
				RI3yr:     ri3yr,
			})
			totalRI1yr += p.RISavings
			totalRI3yr += p.RISavings3yr
		}
	}
	sort.Slice(riPoolRows, func(i, j int) bool { return riPoolRows[i].RI1yr > riPoolRows[j].RI1yr })

	// Waste counts
	var zombieCount, idleCount, zeroReplicaCount, abandonedCount, pvcCount, pvcStorageGB int
	var pvcCostEst float64
	var wasteTotal int
	if scan != nil && scan.wasteAudit != nil {
		wa := scan.wasteAudit
		for _, p := range wa.StalePods {
			switch p.Kind {
			case analyzer.StalePodZombie:
				zombieCount++
			case analyzer.StalePodIdle:
				idleCount++
			}
		}
		zeroReplicaCount = len(wa.ZeroReplicaWorkloads)
		abandonedCount = len(wa.AbandonedNamespaces)
		pvcCount = len(wa.OrphanedPVCs)
		pvcStorageGB = wa.OrphanedPVCStorageGB
		if wa.EstimatedMonthlyWaste > 0 {
			pvcCostEst = wa.EstimatedMonthlyWaste
		} else if pvcStorageGB > 0 {
			pvcCostEst = float64(pvcStorageGB) * 0.10
		}
		wasteTotal = wa.TotalWasteItems
	}

	pvcNote := fmt.Sprintf("%d GB unattached storage", pvcStorageGB)
	if pvcStorageGB == 0 {
		pvcNote = "no size data"
	}
	pvcCostCell := ""
	if pvcCostEst > 0 {
		pvcCostCell = "$" + formatMoney(pvcCostEst)
	}
	totalWasteCost := ""
	if pvcCostEst > 0 {
		totalWasteCost = "$" + formatMoney(pvcCostEst)
	}

	// Right-sizing
	var rsRows []rsCandidateRow
	for _, ns := range nsCosts {
		if ns.CPUCores < 0.5 && ns.PodCount > 0 {
			costCell := ""
			if ns.EstimatedCost.Best >= 1 {
				costCell = "$" + formatMoney(ns.EstimatedCost.Best)
			} else if ns.EstimatedCost.Best > 0 {
				costCell = "<$1"
			}
			rsRows = append(rsRows, rsCandidateRow{
				Name:     ns.Name,
				PodCount: ns.PodCount,
				CPUCores: fmt.Sprintf("%.3f", ns.CPUCores),
				MemoryGB: fmt.Sprintf("%.1f", ns.MemoryGB),
				CostCell: costCell,
			})
		}
	}
	sort.Slice(rsRows, func(i, j int) bool { return rsRows[i].Name < rsRows[j].Name })
	if len(rsRows) > 10 {
		rsRows = rsRows[:10]
	}

	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}

	data := optimizationsPageData{
		ClusterName:      clusterName,
		DashURL:          "/" + q,
		Sidebar:          template.HTML(buildSidebar("optimizations", activeCtx, clusterName, clusterList, countCriticalIssues(scan))),
		RIPools:          riPoolRows,
		TotalRI1yr:       pvcCostCell,
		HasRI:            len(riPoolRows) > 0,
		PVCCount:         pvcCount,
		PVCCostCell:      pvcCostCell,
		PVCNote:          pvcNote,
		ZeroReplicaCount: zeroReplicaCount,
		AbandonedCount:   abandonedCount,
		ZombieCount:      zombieCount,
		IdleCount:        idleCount,
		WasteTotal:       wasteTotal,
		TotalWasteCost:   totalWasteCost,
		RSCandidates:     rsRows,
	}

	var buf strings.Builder
	if err := getOptimizationsTmpl().Execute(&buf, data); err != nil {
		log.Printf("optimizations template: %v", err)
		return ""
	}
	return buf.String()
}

var getOptimizationsTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("optimizations.html").
			ParseFS(templateFS, "templates/base.html", "templates/optimizations.html"),
	)
})

// ── Security Posture page ─────────────────────────────────────────────────────

type securityPageData struct {
	DashHref      string
	WrHref        string
	CostsHref     string
	InfraHref     string
	WasteHref     string
	SecurityHref  string
	IncidentsHref string
	ActivePage    string
	ClusterName   string
	CriticalCount int
	Clusters      []sidebarCluster

	CISScore              int
	CISScoreColor         string
	TotalChecks           int
	PassedChecks          int
	FailedChecks          int
	Controls              []analyzer.CISControl
	Risks                 models.SecurityRisks
	HasRisks              bool
	TotalPods             int
	PriorityActions       []string
	ScannedAtMs           int64
	ScanAvailable         bool
	ExpectedInfra         int
	RequiresReview        int
	Findings              []securityFindingGroup
	PassedControls        []analyzer.CISControl
	TopFindingSummaries   []string
	UnprotectedNS         []analyzer.NamespaceNetworkStatus
	ProtectedNSCount      int
	NetworkNamespaceTotal int
	NetworkAvailable      bool
	ScanCoverage          string
}

type securityFindingResource struct {
	Namespace string
	Resource  string
	Container string
	Evidence  string
	Expected  bool
	Command   string
}

type securityFindingGroup struct {
	Type          string
	Severity      string
	SeverityClass string
	Name          string
	Count         int
	Unit          string
	ScopeCount    int
	Evidence      string
	Action        string
	ExpectedCount int
	Resources     []securityFindingResource
}

type securityFindingMeta struct {
	Name, Unit, Evidence, Action string
}

var securityFindingMetadata = map[string]securityFindingMeta{
	"host_path_volume":        {"HostPath mounts", "mounts", "Pod specs include hostPath volume mounts.", "Review each mount and verify whether the workload requires host access."},
	"privileged_container":    {"Privileged containers", "containers", "Container security contexts set privileged: true.", "Review which containers require privileged access."},
	"host_network":            {"Host network", "pods", "Pod specs set hostNetwork: true.", "Review host-network requirements and reduce usage where appropriate."},
	"host_pid":                {"Host PID", "pods", "Pod specs set hostPID: true.", "Review host-PID requirements."},
	"host_ipc":                {"Host IPC", "pods", "Pod specs set hostIPC: true.", "Review host-IPC requirements."},
	"default_service_account": {"Default ServiceAccount", "pods", "Pod specs use the default ServiceAccount.", "Review required permissions and whether a dedicated ServiceAccount is appropriate."},
	"running_as_root":         {"Non-root enforcement", "containers", "Pod specs do not explicitly enforce non-root execution.", "Review image requirements and enforce non-root execution where appropriate."},
	"missing_resource_limits": {"Resource limits", "containers", "Container specs omit a CPU limit, memory limit, or both.", "Review containers missing CPU or memory limits."},
	"added_capabilities":      {"Added capabilities", "containers", "Container security contexts add Linux capabilities.", "Review each added capability against workload requirements."},
	"privilege_escalation":    {"Privilege escalation", "containers", "Container specs permit or do not explicitly disable privilege escalation.", "Review requirements and disable privilege escalation where appropriate."},
}

func buildSecurityFindingGroups(issues []models.SecurityIssue) []securityFindingGroup {
	byType := make(map[string]*securityFindingGroup)
	scopes := make(map[string]map[string]struct{})
	for _, issue := range issues {
		meta, ok := securityFindingMetadata[issue.Type]
		if !ok {
			continue
		}
		group := byType[issue.Type]
		if group == nil {
			group = &securityFindingGroup{
				Type: issue.Type, Severity: issue.Severity, SeverityClass: securitySeverityClass(issue.Severity),
				Name: meta.Name, Unit: meta.Unit, Evidence: meta.Evidence, Action: meta.Action,
			}
			byType[issue.Type] = group
			scopes[issue.Type] = make(map[string]struct{})
		}
		if securitySeverityRank(issue.Severity) > securitySeverityRank(group.Severity) {
			group.Severity = issue.Severity
			group.SeverityClass = securitySeverityClass(issue.Severity)
		}
		resource, container := issue.Name, ""
		if issue.Resource == "container" {
			if parts := strings.SplitN(issue.Name, "/", 2); len(parts) == 2 {
				resource, container = parts[0], parts[1]
			}
		}
		expected := strings.Contains(issue.Description, "(expected for this infrastructure component)")
		if expected {
			group.ExpectedCount++
		}
		command := ""
		if issue.Namespace != "" && resource != "" {
			command = fmt.Sprintf("kubectl get pod %s -n %s -o yaml", resource, issue.Namespace)
		}
		group.Resources = append(group.Resources, securityFindingResource{
			Namespace: issue.Namespace, Resource: resource, Container: container,
			Evidence: issue.Description, Expected: expected, Command: command,
		})
		group.Count++
		scopes[issue.Type][issue.Namespace+"/"+resource] = struct{}{}
	}
	groups := make([]securityFindingGroup, 0, len(byType))
	for issueType, group := range byType {
		group.ScopeCount = len(scopes[issueType])
		groups = append(groups, *group)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		ri, rj := securitySeverityRank(groups[i].Severity), securitySeverityRank(groups[j].Severity)
		if ri == rj {
			return groups[i].Count > groups[j].Count
		}
		return ri > rj
	})
	return groups
}

func securitySeverityRank(severity string) int {
	switch severity {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

func securitySeverityClass(severity string) string {
	switch severity {
	case "critical":
		return "critical"
	case "high":
		return "high"
	case "medium":
		return "medium"
	default:
		return "info"
	}
}

var getSecurityTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("security.html").
			Funcs(template.FuncMap{
				"add": func(a, b int) int { return a + b },
			}).
			ParseFS(templateFS,
				"templates/base.html",
				"templates/sidebar.html",
				"templates/security.html"),
	)
})

func (srv *server) handleSecurityPage(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		if err := state.refresh(srv.clusterList); err != nil {
			http.Error(w, "scan failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		state.mu.RLock()
		scan = state.scan
		state.mu.RUnlock()
	}

	q := "?cluster=" + url.QueryEscape(ctx)

	var clusters []sidebarCluster
	if len(srv.clusterList) > 1 {
		for _, c := range srv.clusterList {
			label := displayName(c)
			if len(label) > 22 {
				label = label[:21] + "…"
			}
			clusters = append(clusters, sidebarCluster{
				Href:     "/security?" + url.Values{"cluster": {c}}.Encode(),
				Label:    label,
				IsActive: c == ctx,
			})
		}
	}

	data := securityPageData{
		DashHref:      "/" + q,
		WrHref:        "/warroom" + q,
		CostsHref:     "/costs" + q,
		InfraHref:     "/infrastructure" + q,
		WasteHref:     "/waste" + q,
		SecurityHref:  "/security" + q,
		IncidentsHref: "/incidents" + q,
		ActivePage:    "security",
		ClusterName:   displayName(ctx),
		CriticalCount: countCriticalIssues(scan),
		Clusters:      clusters,
		ScannedAtMs:   time.Now().UnixMilli(),
	}

	data.ScanAvailable = scan.secAudit != nil && scan.cisResult != nil
	if data.ScanAvailable {
		data.CISScore = scan.cisResult.Score
		data.TotalChecks = scan.cisResult.TotalChecks
		data.PassedChecks = scan.cisResult.PassedChecks
		data.FailedChecks = scan.cisResult.FailedChecks
		data.Controls = make([]analyzer.CISControl, len(scan.cisResult.Controls))
		copy(data.Controls, scan.cisResult.Controls)
		sort.SliceStable(data.Controls, func(i, j int) bool {
			return !data.Controls[i].Passed && data.Controls[j].Passed
		})
		for _, control := range data.Controls {
			if control.Passed {
				data.PassedControls = append(data.PassedControls, control)
			}
		}
	}
	if data.ScanAvailable {
		data.Risks = scan.secAudit.Risks
		data.TotalPods = scan.secAudit.TotalPodsAudited
		data.PriorityActions = scan.secAudit.PriorityActions
		data.Findings = buildSecurityFindingGroups(scan.secAudit.Issues)
		for i := 0; i < len(data.Findings) && i < 3; i++ {
			finding := data.Findings[i]
			data.TopFindingSummaries = append(data.TopFindingSummaries,
				fmt.Sprintf("%s on %d %s", finding.Name, finding.Count, finding.Unit))
		}
		for _, issue := range scan.secAudit.Issues {
			if strings.Contains(issue.Description, "(expected for this infrastructure component)") {
				data.ExpectedInfra++
			} else {
				data.RequiresReview++
			}
		}
		r := scan.secAudit.Risks
		data.HasRisks = r.RunningAsRoot > 0 || r.PrivilegedContainers > 0 ||
			r.HostNetwork > 0 || r.HostPID > 0 || r.HostIPC > 0 ||
			r.HostPathVolumes > 0 || r.DefaultServiceAccount > 0 ||
			r.MissingResourceLimits > 0 ||
			r.AddedCapabilities > 0 || r.PrivilegeEscalation > 0
	}
	if scan.netAudit != nil {
		data.NetworkAvailable = true
		data.UnprotectedNS = scan.netAudit.UnprotectedNamespaces
		data.ProtectedNSCount = len(scan.netAudit.ProtectedNamespaces)
		data.NetworkNamespaceTotal = scan.netAudit.TotalNamespaces
		if data.NetworkNamespaceTotal == 0 {
			data.NetworkNamespaceTotal = data.ProtectedNSCount + len(data.UnprotectedNS)
		}
	}
	switch {
	case !data.ScanAvailable:
		data.ScanCoverage = "Unavailable"
	case !data.NetworkAvailable:
		data.ScanCoverage = "Partial"
	default:
		data.ScanCoverage = "Complete"
	}

	switch {
	case data.CISScore >= 70:
		data.CISScoreColor = "green"
	case data.CISScore >= 40:
		data.CISScoreColor = "orange"
	default:
		data.CISScoreColor = "red"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	var buf strings.Builder
	if err := getSecurityTmpl().Execute(&buf, data); err != nil {
		log.Printf("security template: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Write([]byte(buf.String()))
}

// ── Waste & Drift page ────────────────────────────────────────────────────────

type wastePageData struct {
	DashHref      string
	WrHref        string
	CostsHref     string
	InfraHref     string
	WasteHref     string
	SecurityHref  string
	IncidentsHref string
	ActivePage    string
	ClusterName   string
	CriticalCount int
	Clusters      []sidebarCluster

	TotalWasteItems      int
	ResourceCandidates   int
	OrphanedPVCStorageGB int
	ZombieCount          int
	ProbeFailureCount    int
	CrashLoopCount       int
	OtherIncidentCount   int
	IncidentHref         string
	ScanCoverage         string
	ZombiePods           []analyzer.StalePod
	IdlePods             []analyzer.StalePod
	OrphanedPVCs         []analyzer.OrphanedPVC
	ZeroReplicaWorkloads []analyzer.ZeroReplicaWorkload
	AbandonedNamespaces  []analyzer.AbandonedNamespace
	StaleJobs            []analyzer.StaleJob
	OrphanedServices     []analyzer.OrphanedService
	BrokenIngresses      []analyzer.BrokenIngress
	MisconfiguredHPAs    []analyzer.MisconfiguredHPA
	OldReplicaSets       []analyzer.OldReplicaSet
	DetectorWarnings     []analyzer.WasteDetectorWarning
	ScanAvailable        bool
	ScanComplete         bool
	ScannedAtMs          int64
	ResourceRows         []wasteReviewRow
	DriftRows            []wasteReviewRow
	HousekeepingRows     []wasteReviewRow
	TopWasteCategories   []string
}

type wasteReviewRow struct {
	Category, Resource, Namespace, Evidence, Age, Storage, ReviewStatus, Command string
	Score                                                                        float64
}

func wasteCommand(kind, name, namespace string) string {
	if kind == "" || name == "" {
		return ""
	}
	if namespace == "" {
		return fmt.Sprintf("kubectl get %s %s -o yaml", kind, name)
	}
	return fmt.Sprintf("kubectl get %s %s -n %s -o yaml", kind, name, namespace)
}

func buildWasteReviewRows(a *analyzer.WasteAudit, idle []analyzer.StalePod) (resource, drift, housekeeping []wasteReviewRow) {
	for _, item := range a.AbandonedNamespaces {
		resource = append(resource, wasteReviewRow{"Abandoned namespace", item.Name, "cluster-scoped", item.Reason, fmt.Sprintf("%dd", item.AgeDays), "—", "Review candidate", wasteCommand("namespace", item.Name, ""), item.Score})
	}
	for _, item := range idle {
		resource = append(resource, wasteReviewRow{"Idle / unmanaged pod", item.Name, item.Namespace, item.Reason, fmt.Sprintf("%dd", item.AgeDays), "—", "Review candidate", wasteCommand("pod", item.Name, item.Namespace), item.Score})
	}
	for _, item := range a.OrphanedPVCs {
		resource = append(resource, wasteReviewRow{"Unattached PVC candidate", item.Name, item.Namespace, item.Reason, fmt.Sprintf("%dd", item.AgeDays), fmt.Sprintf("%d GB", item.SizeGB), "Ownership review", wasteCommand("pvc", item.Name, item.Namespace), item.Score})
	}
	for _, item := range a.OrphanedServices {
		resource = append(resource, wasteReviewRow{"Orphaned Service candidate", item.Name, item.Namespace, item.Reason, fmt.Sprintf("%dd", item.AgeDays), "—", "Selector evidence", wasteCommand("service", item.Name, item.Namespace), item.Score})
	}
	for _, item := range a.StaleJobs {
		kind := "job"
		if item.IsCronJob {
			kind = "cronjob"
		}
		drift = append(drift, wasteReviewRow{"Stale job history", item.Name, item.Namespace, item.Reason, fmt.Sprintf("%dd", item.AgeDays), "—", "Retention review", wasteCommand(kind, item.Name, item.Namespace), item.Score})
	}
	for _, item := range a.ZeroReplicaWorkloads {
		drift = append(drift, wasteReviewRow{"Zero-replica workload", item.Name, item.Namespace, item.Reason, fmt.Sprintf("%dd", item.AgeDays), "—", "Intent review", wasteCommand(strings.ToLower(item.Kind), item.Name, item.Namespace), item.Score})
	}
	for _, item := range a.BrokenIngresses {
		drift = append(drift, wasteReviewRow{"Broken ingress", item.Name, item.Namespace, item.Reason, fmt.Sprintf("%dd", item.AgeDays), "—", "Configuration review", wasteCommand("ingress", item.Name, item.Namespace), item.Score})
	}
	for _, item := range a.MisconfiguredHPAs {
		drift = append(drift, wasteReviewRow{"Misconfigured HPA", item.Name, item.Namespace, item.Reason, fmt.Sprintf("%dd", item.AgeDays), "—", "Configuration review", wasteCommand("hpa", item.Name, item.Namespace), item.Score})
	}
	for _, item := range a.OldReplicaSets {
		housekeeping = append(housekeeping, wasteReviewRow{"Old ReplicaSet", item.Name, item.Namespace, item.Reason, fmt.Sprintf("%dd", item.AgeDays), "—", "Housekeeping; excluded from total", wasteCommand("replicaset", item.Name, item.Namespace), item.Score})
	}
	sort.SliceStable(resource, func(i, j int) bool { return resource[i].Score > resource[j].Score })
	sort.SliceStable(drift, func(i, j int) bool { return drift[i].Score > drift[j].Score })
	sort.SliceStable(housekeeping, func(i, j int) bool { return housekeeping[i].Score > housekeeping[j].Score })
	return
}

const dashboardWasteMinAgeDays = 7

var getWasteTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("waste.html").
			ParseFS(templateFS,
				"templates/base.html",
				"templates/sidebar.html",
				"templates/waste.html"),
	)
})

func (srv *server) handleWastePage(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		if err := state.refresh(srv.clusterList); err != nil {
			http.Error(w, "scan failed: "+err.Error(), 500)
			return
		}
		state.mu.RLock()
		scan = state.scan
		state.mu.RUnlock()
	}

	q := "?cluster=" + url.QueryEscape(ctx)

	var clusters []sidebarCluster
	if len(srv.clusterList) > 1 {
		for _, c := range srv.clusterList {
			label := displayName(c)
			if len(label) > 22 {
				label = label[:21] + "…"
			}
			clusters = append(clusters, sidebarCluster{
				Href:     "/waste?" + url.Values{"cluster": {c}}.Encode(),
				Label:    label,
				IsActive: c == ctx,
			})
		}
	}

	data := wastePageData{
		DashHref:      "/" + q,
		WrHref:        "/warroom" + q,
		CostsHref:     "/costs" + q,
		InfraHref:     "/infrastructure" + q,
		WasteHref:     "/waste" + q,
		SecurityHref:  "/security" + q,
		IncidentsHref: "/incidents" + q,
		ActivePage:    "waste",
		ClusterName:   displayName(ctx),
		CriticalCount: countCriticalIssues(scan),
		Clusters:      clusters,
		ScannedAtMs:   time.Now().UnixMilli(),
		IncidentHref:  "/incidents" + q + "&status=active",
	}

	if scan.wasteAudit != nil {
		wa := scan.wasteAudit
		data.ScanAvailable = true
		data.ScanComplete = len(wa.DetectorWarnings) == 0
		if data.ScanComplete {
			data.ScanCoverage = "Complete"
		} else {
			data.ScanCoverage = "Incomplete"
		}
		data.TotalWasteItems = wa.TotalWasteItems
		data.OrphanedPVCStorageGB = wa.OrphanedPVCStorageGB
		data.OrphanedPVCs = wa.OrphanedPVCs
		data.ZeroReplicaWorkloads = wa.ZeroReplicaWorkloads
		data.AbandonedNamespaces = wa.AbandonedNamespaces
		data.StaleJobs = wa.StaleJobs
		data.OrphanedServices = wa.OrphanedServices
		data.BrokenIngresses = wa.BrokenIngresses
		data.MisconfiguredHPAs = wa.MisconfiguredHPAs
		data.OldReplicaSets = wa.OldReplicaSets
		data.DetectorWarnings = wa.DetectorWarnings

		for _, p := range wa.StalePods {
			if p.Kind == analyzer.StalePodZombie {
				data.ZombiePods = append(data.ZombiePods, p)
				data.ZombieCount++
				switch p.Status {
				case "ProbeFailure":
					data.ProbeFailureCount++
				case "CrashLoopBackOff":
					data.CrashLoopCount++
				default:
					data.OtherIncidentCount++
				}
			} else {
				data.IdlePods = append(data.IdlePods, p)
			}
		}
		data.ResourceRows, data.DriftRows, data.HousekeepingRows = buildWasteReviewRows(wa, data.IdlePods)
		categoryCounts := []struct {
			name  string
			count int
		}{
			{"Unattached PVC candidates", len(wa.OrphanedPVCs)},
			{"Idle / unmanaged pods", len(data.IdlePods)},
			{"Abandoned namespaces", len(wa.AbandonedNamespaces)},
			{"Orphaned Service candidates", len(wa.OrphanedServices)},
			{"Drift findings", len(data.DriftRows)},
		}
		sort.SliceStable(categoryCounts, func(i, j int) bool { return categoryCounts[i].count > categoryCounts[j].count })
		for _, category := range categoryCounts {
			if category.count > 0 && len(data.TopWasteCategories) < 3 {
				data.TopWasteCategories = append(data.TopWasteCategories, fmt.Sprintf("%s (%d)", category.name, category.count))
			}
		}
		data.ResourceCandidates = len(data.AbandonedNamespaces) +
			len(data.IdlePods) +
			len(data.OrphanedPVCs) +
			len(data.StaleJobs) +
			len(data.ZeroReplicaWorkloads) +
			len(data.OrphanedServices) +
			len(data.BrokenIngresses) +
			len(data.MisconfiguredHPAs)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	var buf strings.Builder
	if err := getWasteTmpl().Execute(&buf, data); err != nil {
		log.Printf("waste template: %v", err)
		http.Error(w, "template error", 500)
		return
	}
	w.Write([]byte(buf.String()))
}

// ── HTML rendering pipeline ───────────────────────────────────────────────────

// costPageData holds all data needed to render the cost overview template.
type costPageData struct {
	ClusterName string
	ActiveCtx   string
	ClusterList []costClusterLink
	DashURL     string
	InfraURL    string
	NSsURL      string
	OptURL      string
	WrURL       string
	RefreshURL  string
	ScannedAtMS int64
	Timestamp   time.Time

	MonthlyCost           float64
	SavingsPotential      float64
	ClusterCount          int
	Provider              string
	DetectedProvider      string
	ProviderDetectionMode string
	ProviderWarning       string
	Region                string
	PricingCoverage       string
	PricingWarnings       []string
	Currency              string
	ScopeExclusions       []string
	LastPriceRefresh      time.Time
	MatchedNodes          int
	TotalNodes            int
	ShowSavings           bool
	ShowRI                bool
	CapacityTypes         string

	AccuracyPct     int
	KnownVMs        int
	UnknownVMs      int
	TotalVMs        int
	ConfidenceLabel string
	ConfRingStyle   template.CSS
	ConfPctStyle    template.CSS

	PoolRows       []costPoolRow
	TotalRISavings float64

	NSRows        []costNSRow
	Scenarios     []models.OptimizationScenario
	Disclaimers   []string
	PricingSource string

	CostsURL     string
	IncidentsURL string
	SecurityURL  string
	WasteURL     string
	ActivePage   string
	Version      string
}

type costPoolRow struct {
	Name           string
	VMSize         string
	NodeCount      int
	TagClass       string
	TagLabel       string
	CPUColor       string
	MemColor       string
	CPUUtilPct     float64
	MemUtilPct     float64
	CPUWidthStyle  template.CSS
	MemWidthStyle  template.CSS
	PricePerNode   string
	PoolTotal      string
	RISavingsFmt   string
	RISavings      float64
	Provider       string
	Region         string
	PriceAvailable bool
	ShowRI         bool
}

type costNSRow struct {
	Name     string
	PodCount int
	CPUCores float64
	MemoryGB float64
	NSShare  float64
	CostFmt  string
	GroupID  string
	Deps     []costDepRow
}

type costDepRow struct {
	Name       string
	Kind       string
	KindTag    string
	CPUCores   float64
	MemoryGB   float64
	Replicas   int
	NSSharePct float64
	CostFmt    string
}

type costWRIssue struct {
	warRoomIssue
	CardClass  string
	BadgeLabel string
	Icon       string
	TypeLabel  string
	AgeLbl     string
	ShortName  string
	ShortMsg   string
}

var getCostTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("cost.html").
			Funcs(template.FuncMap{"money": formatMoney}).
			ParseFS(templateFS, "templates/base.html", "templates/cost.html"))
})

func buildCostPageData(scan *clusterScan, activeCtx string, clusterList []string) costPageData {
	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}
	data := costPageData{
		ActiveCtx: activeCtx, ClusterCount: len(clusterList), DashURL: "/" + q,
		InfraURL: "/infrastructure" + q, NSsURL: "/namespaces" + q,
		OptURL: "/optimizations" + q, WrURL: "/warroom" + q,
		RefreshURL: "/refresh" + q, CostsURL: "/costs" + q,
		IncidentsURL: "/incidents" + q, SecurityURL: "/security" + q,
		WasteURL: "/waste" + q, ActivePage: "costs", Version: Version,
		Currency: "USD", Provider: "Unknown", Region: "Not detected",
		PricingCoverage: "Pricing unavailable", CapacityTypes: "None",
	}

	if scan != nil && scan.report != nil {
		r := scan.report
		data.ClusterName = r.ClusterName
		data.MonthlyCost = r.TotalMonthlyCost
		data.SavingsPotential = r.TotalSavingsPotential.Best
		data.Scenarios = r.OptimizationScenarios
		data.Disclaimers = r.Disclaimers
		data.PricingSource = r.PricingSource
		data.Provider = titleProvider(r.Provider)
		data.DetectedProvider = titleProvider(r.DetectedProvider)
		data.ProviderDetectionMode = r.ProviderDetectionMode
		data.ProviderWarning = r.ProviderWarning
		data.Region = r.Region
		data.PricingCoverage = r.PricingCoverage
		data.PricingWarnings = r.PricingWarnings
		data.Currency = r.Currency
		data.ScopeExclusions = r.ScopeExclusions
		data.LastPriceRefresh = r.LastPriceRefresh
		data.ShowSavings = r.Provider == "azure" && r.TotalSavingsPotential.Best > 0
		data.ShowRI = r.Provider == "azure"
		switch r.Provider {
		case "azure":
			data.CapacityTypes = "Regular and Spot"
		case "aws":
			data.CapacityTypes = "EC2 On-Demand; Spot detected but not priced"
		case "mixed":
			data.CapacityTypes = "Varies by provider"
		}
		data.Timestamp = r.Timestamp
		data.ScannedAtMS = r.Timestamp.UnixMilli()

		known, unknown := 0, 0
		for _, p := range r.NodePoolCosts {
			if p.PricingAvailable {
				known += p.NodeCount
			} else {
				unknown += p.NodeCount
			}
		}
		total := known + unknown
		if total > 0 {
			data.AccuracyPct = known * 100 / total
		}
		data.KnownVMs, data.UnknownVMs, data.TotalVMs = known, unknown, total
		data.MatchedNodes, data.TotalNodes = known, total
		data.ConfidenceLabel = "High"
		if data.AccuracyPct < 80 {
			data.ConfidenceLabel = "Medium"
		}
		if data.AccuracyPct < 50 {
			data.ConfidenceLabel = "Low"
		}
		colorHex := confidenceColorHex(data.AccuracyPct)
		circ := 226
		offset := circ - (circ * data.AccuracyPct / 100)
		data.ConfRingStyle = template.CSS(fmt.Sprintf("stroke:%s;stroke-dasharray:%d;stroke-dashoffset:%d", colorHex, circ, offset))
		data.ConfPctStyle = template.CSS("color:" + colorHex)

		for _, p := range r.NodePoolCosts {
			data.TotalRISavings += p.RISavings
			row := costPoolRow{
				Name:           p.Name,
				VMSize:         p.VMSize,
				NodeCount:      p.NodeCount,
				CPUUtilPct:     p.CPUUtilizationPct,
				MemUtilPct:     p.MemoryUtilizationPct,
				CPUWidthStyle:  template.CSS(fmt.Sprintf("width:%.0f%%", p.CPUUtilizationPct)),
				MemWidthStyle:  template.CSS(fmt.Sprintf("width:%.0f%%", p.MemoryUtilizationPct)),
				PricePerNode:   formatMoney(p.PricePerNodeMonth),
				PoolTotal:      formatMoney(p.TotalMonthly),
				RISavings:      p.RISavings,
				Provider:       titleProvider(p.Provider),
				Region:         p.Region,
				PriceAvailable: p.PricingAvailable,
				ShowRI:         p.Provider == "azure",
			}
			if strings.EqualFold(p.Priority, "spot") {
				row.TagClass, row.TagLabel = "tag-spot", "Spot"
			} else {
				row.TagClass, row.TagLabel = "tag-regular", "On-Demand"
			}
			row.CPUColor = utilColorClass(p.CPUUtilizationPct)
			row.MemColor = utilColorClass(p.MemoryUtilizationPct)
			if p.RISavings > 0 {
				row.RISavingsFmt = "$" + formatMoney(p.RISavings)
			} else {
				row.RISavingsFmt = "—"
			}
			data.PoolRows = append(data.PoolRows, row)
		}

		for i, ns := range r.NamespaceCosts {
			row := costNSRow{
				Name:     ns.Name,
				PodCount: ns.PodCount,
				CPUCores: ns.CPUCores,
				MemoryGB: ns.MemoryGB,
				NSShare:  ns.WeightedShare * 100,
				CostFmt:  fmtCostRange(ns.EstimatedCost),
				GroupID:  fmt.Sprintf("ns-%d", i),
			}
			for _, dep := range ns.Deployments {
				kindTag := "tag-deploy"
				if dep.Kind == "StatefulSet" {
					kindTag = "tag-sts"
				} else if dep.Kind == "DaemonSet" {
					kindTag = "tag-ds"
				}
				row.Deps = append(row.Deps, costDepRow{
					Name:       dep.Name,
					Kind:       dep.Kind,
					KindTag:    kindTag,
					CPUCores:   dep.CPUCores,
					MemoryGB:   dep.MemoryGB,
					Replicas:   dep.Replicas,
					NSSharePct: dep.NSShare * 100,
					CostFmt:    fmtCostRange(dep.EstimatedCost),
				})
			}
			data.NSRows = append(data.NSRows, row)
		}
	} else {
		data.ClusterName = displayName(activeCtx)
		data.Timestamp = time.Now()
		data.ScannedAtMS = data.Timestamp.UnixMilli()
		data.ConfRingStyle = template.CSS("stroke:#64748b;stroke-dasharray:226;stroke-dashoffset:226")
		data.ConfPctStyle = template.CSS("color:#64748b")
	}

	for _, ctx := range clusterList {
		label := displayName(ctx)
		if len(label) > 22 {
			label = label[:21] + "…"
		}
		data.ClusterList = append(data.ClusterList, costClusterLink{
			Ctx:      ctx,
			Label:    label,
			Href:     "/?" + url.Values{"cluster": {ctx}}.Encode(),
			IsActive: ctx == activeCtx,
		})
	}

	return data
}

func titleProvider(provider string) string {
	switch provider {
	case "azure":
		return "Azure"
	case "aws":
		return "AWS"
	case "mixed":
		return "Mixed"
	default:
		return "Unknown"
	}
}

func renderCostPage(scan *clusterScan, activeCtx string, clusterList []string) string {
	data := buildCostPageData(scan, activeCtx, clusterList)
	var buf strings.Builder
	if err := getCostTmpl().Execute(&buf, data); err != nil {
		log.Printf("cost template: %v", err)
		return ""
	}
	return buf.String()
}
