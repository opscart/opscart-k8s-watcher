package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed templates
var templateFS embed.FS

var (
	port         string
	cluster      string
	clustersFlag string
	region       string
	breakdown    string
	namespace    string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "opscart-dashboard",
		Short: "OpsCart cloud cost dashboard server",
		Long: `Serves a live cloud cost FinOps dashboard for Kubernetes clusters.

Routes:
  GET  /               — HTML dashboard (auto-refreshes every 60s)
  POST /refresh        — trigger an immediate re-scan
  GET  /api/report     — full CloudCostReport as JSON
  GET  /api/overview   — summary KPIs as JSON
  GET  /api/summary    — {monthly_cost, waste_total, security_score, cluster_count, pod_count}
  GET  /api/warroom    — top 5 critical issues (crash pods, privileged containers, unprotected namespaces)
  GET  /warroom        — full War Room page (crash loops, OOMKilled, ImagePullBackOff, unprotected namespaces, orphaned PVCs, zero-replica workloads)
  GET  /healthz        — liveness probe

All data routes accept ?cluster=<context> to target a specific cluster.`,
		RunE: runDashboard,
	}

	rootCmd.Flags().StringVarP(&port, "port", "p", "8080", "Port to listen on")
	rootCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Kubernetes context to scan (default: current context)")
	rootCmd.Flags().StringVar(&clustersFlag, "clusters", "", "Comma-separated Kubernetes contexts for the sidebar selector")
	rootCmd.Flags().StringVar(&region, "region", "", "Azure region for pricing (auto-detected from node labels if empty)")
	rootCmd.Flags().StringVar(&breakdown, "breakdown", "", "Cost breakdown level: '' or 'deployment'")
	rootCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// ── Cluster list ─────────────────────────────────────────────────────────────

func parseClusterList() []string {
	seen := map[string]bool{}
	var list []string
	add := func(ctx string) {
		ctx = strings.TrimSpace(ctx)
		if !seen[ctx] {
			seen[ctx] = true
			list = append(list, ctx)
		}
	}
	add(cluster)
	for _, ctx := range strings.Split(clustersFlag, ",") {
		add(ctx)
	}
	return list
}

func displayName(ctx string) string {
	if ctx == "" {
		return "current-context"
	}
	return ctx
}

// ── Full scan result ──────────────────────────────────────────────────────────

// clusterScan holds results from all analyzers for a single cluster scan.
// Fields other than report may be nil if the audit failed (RBAC, timeout, etc.).
type clusterScan struct {
	report     *models.CloudCostReport
	secAudit   *models.SecurityAudit
	cisResult  *analyzer.CISResult
	wasteAudit *analyzer.WasteAudit
	netAudit   *analyzer.NetworkPolicyAudit
}

func (s *clusterScan) monthlyPodCount() int {
	if s == nil || s.report == nil {
		return 0
	}
	n := 0
	for _, ns := range s.report.NamespaceCosts {
		n += ns.PodCount
	}
	return n
}

func (s *clusterScan) wasteTotal() int {
	if s == nil || s.wasteAudit == nil {
		return -1 // -1 signals "data not available"
	}
	return s.wasteAudit.TotalWasteItems
}

func (s *clusterScan) securityScore() int {
	if s == nil || s.cisResult == nil {
		return -1
	}
	return s.cisResult.Score
}

// ── Per-cluster state ─────────────────────────────────────────────────────────

type dashboardState struct {
	ctx      string
	mu       sync.RWMutex
	scan     *clusterScan
	htmlPage string
	scanning atomic.Bool
}

func (s *dashboardState) refresh(clusterList []string) error {
	if !s.scanning.CompareAndSwap(false, true) {
		return nil
	}
	defer s.scanning.Store(false)

	scan, err := runFullScan(s.ctx)
	if err != nil {
		return err
	}
	page := renderHTML(scan, s.ctx, clusterList)

	s.mu.Lock()
	s.scan = scan
	s.htmlPage = page
	s.mu.Unlock()

	log.Printf("[%s] scan complete — $%.0f/mo  waste=%d  cis=%d",
		displayName(s.ctx), scan.report.TotalMonthlyCost, scan.wasteTotal(), scan.securityScore())
	return nil
}

// ── Server ────────────────────────────────────────────────────────────────────

type server struct {
	clusterList []string
	mu          sync.RWMutex
	states      map[string]*dashboardState
}

func newServer(clusterList []string) *server {
	return &server{clusterList: clusterList, states: make(map[string]*dashboardState)}
}

func (srv *server) getState(ctx string) *dashboardState {
	srv.mu.RLock()
	s := srv.states[ctx]
	srv.mu.RUnlock()
	if s != nil {
		return s
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if s = srv.states[ctx]; s != nil {
		return s
	}
	s = &dashboardState{ctx: ctx}
	srv.states[ctx] = s
	return s
}

func (srv *server) activeCtx(r *http.Request) string {
	if ctx := r.URL.Query().Get("cluster"); ctx != "" {
		return ctx
	}
	return srv.clusterList[0]
}

// startBackgroundRefresh ticks every interval and re-scans every cluster that
// has been visited at least once. Uses the same refresh() pipeline as POST /refresh.
func (srv *server) startBackgroundRefresh(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		srv.mu.RLock()
		states := make([]*dashboardState, 0, len(srv.states))
		for _, s := range srv.states {
			states = append(states, s)
		}
		srv.mu.RUnlock()

		for _, state := range states {
			state.mu.RLock()
			hasData := state.scan != nil
			state.mu.RUnlock()
			if !hasData {
				continue
			}
			if err := state.refresh(srv.clusterList); err != nil {
				log.Printf("[%s] background refresh error: %v", displayName(state.ctx), err)
			}
		}
	}
}

func (srv *server) handleOverviewPage(w http.ResponseWriter, r *http.Request) {
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

	data := buildOverviewData(scan, ctx, srv.clusterList)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		log.Printf("overview template: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Write([]byte(buf.String()))
}

// ── runDashboard ──────────────────────────────────────────────────────────────

func runDashboard(_ *cobra.Command, _ []string) error {
	cl := parseClusterList()
	srv := newServer(cl)

	log.Printf("Scanning cluster %q ...", displayName(cl[0]))
	if err := srv.getState(cl[0]).refresh(cl); err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}

	go srv.startBackgroundRefresh(60 * time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleOverviewPage)
	mux.HandleFunc("/costs", srv.handleDashboard)
	mux.HandleFunc("/refresh", srv.handleRefresh)
	mux.HandleFunc("/api/report", srv.handleReportJSON)
	mux.HandleFunc("/api/overview", srv.handleOverview)
	mux.HandleFunc("/api/summary", srv.handleSummary)
	mux.HandleFunc("/api/warroom", srv.handleWarRoom)
	mux.HandleFunc("/warroom", srv.handleWarRoomPage)
	mux.HandleFunc("/infrastructure", srv.handleInfrastructurePage)
	mux.HandleFunc("/namespaces", srv.handleNamespacesPage)
	mux.HandleFunc("/optimizations", srv.handleOptimizationsPage)
	mux.HandleFunc("/healthz", handleHealth)

	addr := ":" + port
	log.Printf("Dashboard ready at http://localhost%s", addr)
	return http.ListenAndServe(addr, mux)
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

func (srv *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/costs" {
		http.NotFound(w, r)
		return
	}
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)

	state.mu.RLock()
	page := state.htmlPage
	state.mu.RUnlock()

	if page == "" {
		if err := state.refresh(srv.clusterList); err != nil {
			http.Error(w, "scan failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		state.mu.RLock()
		page = state.htmlPage
		state.mu.RUnlock()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	fmt.Fprint(w, page)
}

func (srv *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST /refresh", http.StatusMethodNotAllowed)
		return
	}
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	if err := state.refresh(srv.clusterList); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"scanned_at":         scan.report.Timestamp.Format(time.RFC3339),
		"total_monthly_cost": scan.report.TotalMonthlyCost,
	})
}

func (srv *server) handleReportJSON(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		if err := state.refresh(srv.clusterList); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		state.mu.RLock()
		scan = state.scan
		state.mu.RUnlock()
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(scan.report); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

// handleOverview returns lightweight KPI JSON for monitoring systems.
//
//	GET /api/overview[?cluster=<ctx>]
//	{"total_monthly_cost":..., "node_pool_count":..., "namespace_count":..., "last_scanned":...}
func (srv *server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		http.Error(w, "no data — POST /refresh first", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total_monthly_cost": scan.report.TotalMonthlyCost,
		"node_pool_count":    len(scan.report.NodePoolCosts),
		"namespace_count":    len(scan.report.NamespaceCosts),
		"last_scanned":       scan.report.Timestamp.Format(time.RFC3339),
	})
}

// handleSummary returns the six top-line metrics shown in the KPI bar.
//
//	GET /api/summary[?cluster=<ctx>]
//	{"monthly_cost":..., "waste_total":..., "security_score":..., "cluster_count":..., "pod_count":...}
func (srv *server) handleSummary(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		http.Error(w, "no data — POST /refresh first", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"monthly_cost":   scan.report.TotalMonthlyCost,
		"waste_total":    scan.wasteTotal(),
		"security_score": scan.securityScore(),
		"cluster_count":  len(srv.clusterList),
		"pod_count":      scan.monthlyPodCount(),
	})
}

// handleWarRoom returns up to 5 critical issues: crash-looping pods, unexpected
// privileged containers, and high-risk unprotected namespaces.
//
//	GET /api/warroom[?cluster=<ctx>]
//	[{"severity":"critical","type":"crash_loop","namespace":"...","resource":"...","message":"..."}]
func (srv *server) handleWarRoom(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	issues := collectWarRoomIssues(scan, 5)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(issues)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// ── War Room helpers ──────────────────────────────────────────────────────────

type warRoomIssue struct {
	Severity   string `json:"severity"`
	Type       string `json:"type"`
	Namespace  string `json:"namespace"`
	Resource   string `json:"resource"`
	Message    string `json:"message"`
	AgeDays    int    `json:"age_days,omitempty"`
	KubectlCmd string `json:"kubectl_cmd,omitempty"`
}

type warRoomPageData struct {
	ClusterName   string
	StatusDesc    string
	DashURL       string
	CritChipClass string
	WarnChipClass string
	Critical      []warRoomIssue
	Warnings      []warRoomIssue
	ScannedAtMs   int64
	Sidebar       template.HTML
}

func collectWarRoomIssues(scan *clusterScan, limit int) []warRoomIssue {
	var issues []warRoomIssue

	// 1. Crash-looping / zombie pods
	if scan.wasteAudit != nil {
		for _, pod := range scan.wasteAudit.StalePods {
			if pod.Kind == analyzer.StalePodZombie {
				itype := zombieTypeForStatus(pod.Status)
				issues = append(issues, warRoomIssue{
					Severity:   "critical",
					Type:       itype,
					Namespace:  pod.Namespace,
					Resource:   pod.Name,
					Message:    fmt.Sprintf("%s — %d restarts, %d days old", pod.Status, pod.RestartCount, pod.AgeDays),
					AgeDays:    pod.AgeDays,
					KubectlCmd: kubectlCmdForIssue(itype, pod.Name, pod.Namespace),
				})
			}
		}
	}

	// 2. Unexpected privileged containers (non-system or unexpected system ones)
	if scan.secAudit != nil {
		for _, issue := range scan.secAudit.Issues {
			if issue.Type == "privileged_container" && issue.Severity == "critical" {
				issues = append(issues, warRoomIssue{
					Severity:   "critical",
					Type:       "privileged_container",
					Namespace:  issue.Namespace,
					Resource:   issue.Name,
					Message:    issue.Description,
					KubectlCmd: fmt.Sprintf("kubectl describe pod %s -n %s", issue.Name, issue.Namespace),
				})
			}
		}
	}

	// 3. High-risk unprotected namespaces
	if scan.netAudit != nil {
		for _, ns := range scan.netAudit.UnprotectedNamespaces {
			if ns.RiskLevel == "HIGH" {
				issues = append(issues, warRoomIssue{
					Severity:   "high",
					Type:       "unprotected_namespace",
					Namespace:  ns.Name,
					Resource:   "namespace",
					Message:    fmt.Sprintf("No NetworkPolicy — %d pods exposed", ns.PodCount),
					KubectlCmd: fmt.Sprintf("kubectl get networkpolicies -n %s", ns.Name),
				})
			}
		}
	}

	// Sort: critical before high
	severityOrder := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}
	sort.SliceStable(issues, func(i, j int) bool {
		return severityOrder[issues[i].Severity] < severityOrder[issues[j].Severity]
	})

	// limit=0 means no cap (used by the HTML page to show all issues)
	if limit > 0 && len(issues) > limit {
		return issues[:limit]
	}
	return issues
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

type sidebarData struct {
	DashHref      string
	CostsHref     string
	InfraHref     string
	NsHref        string
	OptHref       string
	WrHref        string
	ActivePage    string
	ClusterName   string
	Clusters      []sidebarCluster
	CriticalCount int
}

type sidebarCluster struct {
	Href     string
	Label    string
	IsActive bool
}

// countCriticalIssues returns the number of critical issues in War Room data.
// Used by buildSidebar to display the red badge on the War Room nav item.
func countCriticalIssues(scan *clusterScan) int {
	if scan == nil {
		return 0
	}
	issues := collectWarRoomIssues(scan, 0)
	count := 0
	for _, issue := range issues {
		if issue.Severity == "critical" {
			count++
		}
	}
	return count
}

// buildSidebar returns a complete <aside>…</aside> sidebar, shared by all sub-pages.
// activePage is one of: "dashboard", "infrastructure", "namespaces", "optimizations", "warroom".
func buildSidebar(activePage, activeCtx, clusterName string, clusterList []string, criticalCount int) string {

	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}

	basePath := "/"
	switch activePage {
	case "infrastructure":
		basePath = "/infrastructure"
	case "namespaces":
		basePath = "/namespaces"
	case "optimizations":
		basePath = "/optimizations"
	case "warroom":
		basePath = "/warroom"
	}

	var clusters []sidebarCluster
	if len(clusterList) > 1 {
		for _, ctx := range clusterList {
			label := displayName(ctx)
			if len(label) > 22 {
				label = label[:21] + "…"
			}
			clusters = append(clusters, sidebarCluster{
				Href:     basePath + "?" + url.Values{"cluster": {ctx}}.Encode(),
				Label:    label,
				IsActive: ctx == activeCtx,
			})
		}
	}

	data := sidebarData{
		DashHref:      "/" + q,
		CostsHref:     "/costs" + q,
		InfraHref:     "/infrastructure" + q,
		NsHref:        "/namespaces" + q,
		OptHref:       "/optimizations" + q,
		WrHref:        "/warroom" + q,
		ActivePage:    activePage,
		ClusterName:   clusterName,
		Clusters:      clusters,
		CriticalCount: criticalCount,
	}

	var buf strings.Builder
	if err := getSidebarTmpl().Execute(&buf, data); err != nil {
		log.Printf("sidebar template: %v", err)
		return ""
	}
	return buf.String()
}

var getSidebarTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("sidebar.html").
			ParseFS(templateFS, "templates/sidebar.html"),
	)
})

// ── War Room page ─────────────────────────────────────────────────────────────

func (srv *server) handleWarRoomPage(w http.ResponseWriter, r *http.Request) {
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
	fmt.Fprint(w, renderWarRoomPage(scan, ctx, srv.clusterList))
}

func renderWarRoomPage(scan *clusterScan, activeCtx string, clusterList []string) string {
	allIssues := collectWarRoomIssues(scan, 0)
	var critical, warnings []warRoomIssue
	for _, issue := range allIssues {
		if issue.Severity == "critical" {
			critical = append(critical, issue)
		} else {
			warnings = append(warnings, issue)
		}
	}

	scannedAt := time.Now()
	clusterName := displayName(activeCtx)
	if scan != nil && scan.report != nil {
		scannedAt = scan.report.Timestamp
		clusterName = scan.report.ClusterName
	}

	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}

	critChipClass := "ok"
	if len(critical) > 0 {
		critChipClass = "c"
	}
	warnChipClass := "ok"
	if len(warnings) > 0 {
		warnChipClass = "w"
	}

	data := warRoomPageData{
		ClusterName:   clusterName,
		StatusDesc:    fmt.Sprintf("%d issue(s) detected", len(critical)+len(warnings)),
		DashURL:       "/" + q,
		CritChipClass: critChipClass,
		WarnChipClass: warnChipClass,
		Critical:      critical,
		Warnings:      warnings,
		ScannedAtMs:   scannedAt.UnixMilli(),
		Sidebar:       template.HTML(buildSidebar("warroom", activeCtx, clusterName, clusterList, countCriticalIssues(scan))),
	}

	var buf strings.Builder
	if err := getWarRoomTmpl().Execute(&buf, data); err != nil {
		log.Printf("warroom template: %v", err)
		return ""
	}
	return buf.String()
}

var warRoomTmplOnce sync.Once
var warRoomTmpl *template.Template

func getWarRoomTmpl() *template.Template {
	warRoomTmplOnce.Do(func() {
		warRoomTmpl = template.Must(
			template.New("warroom.html").Funcs(template.FuncMap{
				"renderCard": func(issue warRoomIssue) template.HTML {
					return template.HTML(renderWarRoomCard(issue))
				},
			}).ParseFS(templateFS, "templates/base.html", "templates/warroom.html"),
		)
	})
	return warRoomTmpl
}

func renderWarRoomCard(issue warRoomIssue) string {
	sc := "c"
	bl := "CRITICAL"
	if issue.Severity == "warning" {
		sc = "w"
		bl = "WARNING"
	}

	icon, label := warRoomTypeLabel(issue.Type)

	age := ""
	if issue.AgeDays > 0 {
		age = fmt.Sprintf(`<span class="age-lbl">%dd</span>`, issue.AgeDays)
	}

	name := issue.Resource
	if len(name) > 38 {
		name = name[:37] + "…"
	}

	reason := issue.Message
	if len(reason) > 320 {
		reason = reason[:317] + "…"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`<div class="wr-card %s">`, sc))
	sb.WriteString(fmt.Sprintf(`<div class="wr-top"><span class="badge %s">%s</span><span class="type-lbl">%s %s</span>%s</div>`,
		sc, bl, icon, label, age))
	sb.WriteString(fmt.Sprintf(`<div class="wr-name">%s</div>`, name))
	sb.WriteString(fmt.Sprintf(`<div class="wr-ns">ns: %s</div>`, issue.Namespace))
	sb.WriteString(fmt.Sprintf(`<div class="wr-reason">%s</div>`, reason))
	sb.WriteString(`<div class="wr-cmd">`)
	sb.WriteString(fmt.Sprintf(`<code>%s</code>`, issue.KubectlCmd))
	sb.WriteString(`<button class="copy-btn" onclick="var b=this,c=this.previousElementSibling.textContent;navigator.clipboard.writeText(c).then(function(){b.textContent='✓';setTimeout(function(){b.textContent='Copy'},1500)})">Copy</button>`)
	sb.WriteString(`</div>`)
	sb.WriteString(`</div>`)
	return sb.String()
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

	MonthlyCost      float64
	SavingsPotential float64
	SecurityColor    string
	SecurityDisplay  string
	WasteColor       string
	WasteDisplay     string
	PodCount         int
	ClusterCount     int

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

	WRIssues      []costWRIssue
	CriticalCount int
	CostsURL      string
}

// ── Overview page (executive summary) ─────────────────────────────────────────

type overviewPageData struct {
	ClusterName string
	ActiveCtx   string
	ClusterList []costClusterLink
	DashURL     string
	InfraURL    string
	NSsURL      string
	OptURL      string
	WrURL       string
	CostsURL    string
	ScannedAtMS int64
	CostsHref   string

	// Sidebar aliases (matches sidebar.html template)
	DashHref   string
	InfraHref  string
	NsHref     string
	OptHref    string
	WrHref     string
	ActivePage string
	Clusters   []sidebarCluster

	// KPI bar
	CriticalCount    int
	SavingsPotential float64
	SecurityScore    int
	SecurityColor    string
	WasteCount       int
	MonthlyCost      float64

	// Top 5 Things to Fix
	TopIssues []topIssue

	// War Room featured issue
	FeaturedIssue *warRoomIssue
	HasFeatured   bool

	// Cluster Summary
	NodePoolCount  int
	PodCount       int
	CPUUtilization int
	MemUtilization int
	NamespaceCount int

	// Version + branding
	Version string
}

type topIssue struct {
	Rank        int
	Title       string
	Subtitle    string
	Action      string
	Severity    string // "critical" | "high" | "medium" | "low"
	SeverityLbl string // "CRITICAL" | "HIGH" | "MEDIUM" | "LOW"
	CountText   string // "8 pods", "~$40/mo", "14 checks"
	URL         string
}

func convertToSidebarClusters(clusterList []string, activeCtx, basePath string) []sidebarCluster {
	if len(clusterList) <= 1 {
		return nil
	}
	var out []sidebarCluster
	for _, ctx := range clusterList {
		label := displayName(ctx)
		if len(label) > 22 {
			label = label[:21] + "…"
		}
		out = append(out, sidebarCluster{
			Href:     basePath + "?" + url.Values{"cluster": {ctx}}.Encode(),
			Label:    label,
			IsActive: ctx == activeCtx,
		})
	}
	return out
}

func buildOverviewData(scan *clusterScan, activeCtx string, clusterList []string) overviewPageData {
	clusterName := displayName(activeCtx)
	var monthlyCost, savings float64
	var podCount, nsCount, nodePoolCount int
	var cpuUtil, memUtil int
	var wasteCount, securityScore, secFailed int
	var wrIssues []warRoomIssue
	var featured *warRoomIssue
	var criticalCount int

	if scan != nil {
		wrIssues = collectWarRoomIssues(scan, 0)
		for _, w := range wrIssues {
			if w.Severity == "critical" {
				criticalCount++
				if featured == nil {
					tmp := w
					featured = &tmp
				}
			}
		}
		// If no critical, feature highest-severity warning
		if featured == nil && len(wrIssues) > 0 {
			tmp := wrIssues[0]
			featured = &tmp
		}

		if scan.report != nil {
			monthlyCost = scan.report.TotalMonthlyCost
			savings = scan.report.TotalSavingsPotential.Best
			clusterName = scan.report.ClusterName
			nodePoolCount = len(scan.report.NodePoolCosts)
			nsCount = len(scan.report.NamespaceCosts)

			// Aggregate CPU/Mem utilization across pools
			var totalCPU, usedCPU, totalMem, usedMem float64
			for _, p := range scan.report.NodePoolCosts {
				totalCPU += p.TotalCPUCapacity
				usedCPU += p.CPURequested
				totalMem += p.TotalMemoryCapacity
				usedMem += p.MemoryRequested
				podCount += p.NodeCount // fallback; real pod count below
			}
			if totalCPU > 0 {
				cpuUtil = int((usedCPU / totalCPU) * 100)
			}
			if totalMem > 0 {
				memUtil = int((usedMem / totalMem) * 100)
			}
		}

		if scan.secAudit != nil {
			podCount = scan.secAudit.TotalPodsAudited
		}

		if scan.cisResult != nil {
			securityScore = scan.cisResult.Score
			secFailed = scan.cisResult.FailedChecks
		}

		if scan.wasteAudit != nil {
			wasteCount = scan.wasteAudit.TotalWasteItems
		}
	}
	_ = secFailed // reserved for future use

	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}

	var clusters []costClusterLink
	if len(clusterList) > 1 {
		for _, ctx := range clusterList {
			label := displayName(ctx)
			if len(label) > 22 {
				label = label[:21] + "…"
			}
			clusters = append(clusters, costClusterLink{
				Href:     "/?" + url.Values{"cluster": {ctx}}.Encode(),
				Label:    label,
				IsActive: ctx == activeCtx,
			})
		}
	}

	return overviewPageData{
		ClusterName:      clusterName,
		ActiveCtx:        activeCtx,
		ClusterList:      clusters,
		DashURL:          "/" + q,
		InfraURL:         "/infrastructure" + q,
		NSsURL:           "/namespaces" + q,
		OptURL:           "/optimizations" + q,
		WrURL:            "/warroom" + q,
		CostsURL:         "/costs" + q,
		ScannedAtMS:      time.Now().UnixMilli(),
		CriticalCount:    criticalCount,
		SavingsPotential: savings,
		SecurityScore:    securityScore,
		WasteCount:       wasteCount,
		MonthlyCost:      monthlyCost,
		TopIssues:        buildTopIssues(scan, wrIssues),
		FeaturedIssue:    featured,
		HasFeatured:      featured != nil,
		NodePoolCount:    nodePoolCount,
		PodCount:         podCount,
		CPUUtilization:   cpuUtil,
		MemUtilization:   memUtil,
		NamespaceCount:   nsCount,
		Version:          "v0.9.3",
		DashHref:         "/" + q,
		CostsHref:        "/costs" + q,
		InfraHref:        "/infrastructure" + q,
		NsHref:           "/namespaces" + q,
		OptHref:          "/optimizations" + q,
		WrHref:           "/warroom" + q,
		ActivePage:       "dashboard",
		Clusters:         convertToSidebarClusters(clusterList, activeCtx, "/"),
	}
}

func buildTopIssues(scan *clusterScan, wrIssues []warRoomIssue) []topIssue {
	var issues []topIssue

	// Group war room issues by Severity + Type
	type groupKey struct {
		severity string
		issueTyp string
	}
	groups := make(map[groupKey][]warRoomIssue)
	keyOrder := []groupKey{} // preserve insertion order
	for _, wr := range wrIssues {
		k := groupKey{severity: wr.Severity, issueTyp: wr.Type}
		if _, exists := groups[k]; !exists {
			keyOrder = append(keyOrder, k)
		}
		groups[k] = append(groups[k], wr)
	}

	// Severity rank for sorting
	sevRank := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "warning": 2}
	sort.Slice(keyOrder, func(i, j int) bool {
		if sevRank[keyOrder[i].severity] != sevRank[keyOrder[j].severity] {
			return sevRank[keyOrder[i].severity] < sevRank[keyOrder[j].severity]
		}
		// Same severity → larger group first
		return len(groups[keyOrder[i]]) > len(groups[keyOrder[j]])
	})

	// Convert grouped war room issues into topIssue rows
	for _, k := range keyOrder {
		if len(issues) >= 5 {
			break
		}
		grp := groups[k]
		count := len(grp)
		sevLbl := strings.ToUpper(k.severity)

		title, subtitle, countText, action := formatGroupedIssue(k.issueTyp, k.severity, grp)

		issues = append(issues, topIssue{
			Title:       title,
			Subtitle:    subtitle,
			Action:      action,
			Severity:    k.severity,
			SeverityLbl: sevLbl,
			CountText:   countText,
			URL:         "/warroom",
		})
		_ = count
	}

	// Waste: orphaned PVCs (single aggregated row)
	if scan != nil && scan.wasteAudit != nil && len(issues) < 5 {
		wa := scan.wasteAudit
		if len(wa.OrphanedPVCs) > 0 {
			cost := ""
			if wa.EstimatedMonthlyWaste > 0 {
				cost = fmt.Sprintf("~$%.0f/mo", wa.EstimatedMonthlyWaste)
			} else {
				cost = fmt.Sprintf("%d items", len(wa.OrphanedPVCs))
			}
			issues = append(issues, topIssue{
				Title:       fmt.Sprintf("%d orphaned PVCs wasting money", len(wa.OrphanedPVCs)),
				Subtitle:    "Unused PVCs costing you money each month",
				Severity:    "medium",
				SeverityLbl: "MEDIUM",
				CountText:   cost,
				URL:         "/optimizations",
			})
		}
	}

	// Security score row
	if scan != nil && scan.cisResult != nil && scan.cisResult.Score < 70 && len(issues) < 5 {
		issues = append(issues, topIssue{
			Title:       "Security score below recommended threshold",
			Subtitle:    fmt.Sprintf("%d of %d CIS Pod Security checks failed", scan.cisResult.FailedChecks, scan.cisResult.TotalChecks),
			Severity:    "medium",
			SeverityLbl: "MEDIUM",
			CountText:   fmt.Sprintf("%d/%d failed", scan.cisResult.FailedChecks, scan.cisResult.TotalChecks),
			URL:         "/warroom",
		})
	}

	// Zero-replica workloads (aggregated)
	if scan != nil && scan.wasteAudit != nil && len(scan.wasteAudit.ZeroReplicaWorkloads) > 0 && len(issues) < 5 {
		count := len(scan.wasteAudit.ZeroReplicaWorkloads)
		issues = append(issues, topIssue{
			Title:       fmt.Sprintf("%d unused deployment%s scaled to zero", count, pluralS(count)),
			Subtitle:    "Workloads with 0 replicas — clean up or restore",
			Severity:    "low",
			SeverityLbl: "LOW",
			CountText:   fmt.Sprintf("%d workload%s", count, pluralS(count)),
			URL:         "/optimizations",
		})
	}

	// Number them
	for i := range issues {
		issues[i].Rank = i + 1
	}
	return issues
}

// formatGroupedIssue produces title/subtitle/count for a grouped batch of issues.
func formatGroupedIssue(issueType, severity string, grp []warRoomIssue) (title, subtitle, countText, action string) {
	count := len(grp)
	// Collect first 3 sample resources for subtitle
	samples := []string{}
	namespaces := map[string]bool{}
	for i, wr := range grp {
		if i < 3 {
			samples = append(samples, wr.Resource)
		}
		namespaces[wr.Namespace] = true
	}
	nsCount := len(namespaces)
	moreThan3 := count - 3

	switch issueType {
	case "crash_loop":
		title = fmt.Sprintf("%d pod%s crash-looping", count, pluralS(count))
		if count == 1 {
			subtitle = fmt.Sprintf("%s in %s", samples[0], grp[0].Namespace)
		} else if moreThan3 > 0 {
			subtitle = fmt.Sprintf("%s, %s, %s + %d more across %d namespace%s",
				samples[0], samples[1], samples[2], moreThan3, nsCount, pluralS(nsCount))
		} else {
			subtitle = fmt.Sprintf("Across %d namespace%s", nsCount, pluralS(nsCount))
		}
		countText = fmt.Sprintf("%d pod%s", count, pluralS(count))
		action = fmt.Sprintf("kubectl logs %s -n %s", samples[0], grp[0].Namespace)
	case "oom_killed":
		title = fmt.Sprintf("%d pod%s OOMKilled", count, pluralS(count))
		subtitle = fmt.Sprintf("Out of memory across %d namespace%s", nsCount, pluralS(nsCount))
		countText = fmt.Sprintf("%d pod%s", count, pluralS(count))
	case "image_pull":
		title = fmt.Sprintf("%d ImagePullBackOff failure%s", count, pluralS(count))
		subtitle = fmt.Sprintf("Image pull failures across %d namespace%s", nsCount, pluralS(nsCount))
		countText = fmt.Sprintf("%d pod%s", count, pluralS(count))
		action = fmt.Sprintf("kubectl describe pod %s -n %s", samples[0], grp[0].Namespace)
	case "unprotected_namespace":
		title = fmt.Sprintf("%d namespace%s missing NetworkPolicy", count, pluralS(count))
		if count == 1 {
			subtitle = fmt.Sprintf("%s has no network isolation", grp[0].Namespace)
		} else {
			nsList := []string{}
			for ns := range namespaces {
				nsList = append(nsList, ns)
				if len(nsList) >= 3 {
					break
				}
			}
			subtitle = fmt.Sprintf("Including %s", strings.Join(nsList, ", "))
		}
		countText = fmt.Sprintf("%d ns", count)
		action = "kubectl apply default-deny NetworkPolicy"
	default:
		title = fmt.Sprintf("%d %s issue%s", count, humanizeWRType(issueType), pluralS(count))
		subtitle = fmt.Sprintf("Across %d namespace%s", nsCount, pluralS(nsCount))
		countText = fmt.Sprintf("%d item%s", count, pluralS(count))
	}
	return
}

func humanizeWRType(t string) string {
	switch t {
	case "crash_loop":
		return "Pod crash looping"
	case "oom_killed":
		return "Pod OOMKilled"
	case "image_pull":
		return "Image pull failure"
	case "unprotected_namespace":
		return "Namespace"
	default:
		return t
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

type costClusterLink struct {
	Ctx      string
	Label    string
	Href     string
	IsActive bool
}

type costPoolRow struct {
	Name          string
	VMSize        string
	NodeCount     int
	TagClass      string
	TagLabel      string
	CPUColor      string
	MemColor      string
	CPUUtilPct    float64
	MemUtilPct    float64
	CPUWidthStyle template.CSS
	MemWidthStyle template.CSS
	PricePerNode  string
	PoolTotal     string
	RISavingsFmt  string
	RISavings     float64
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

var getOverviewTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("overview.html").
			Funcs(template.FuncMap{"money": formatMoney}).
			ParseFS(templateFS, "templates/base.html", "templates/sidebar.html", "templates/overview.html"),
	)
})

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
		ActiveCtx:     activeCtx,
		ClusterCount:  len(clusterList),
		DashURL:       "/" + q,
		InfraURL:      "/infrastructure" + q,
		NSsURL:        "/namespaces" + q,
		OptURL:        "/optimizations" + q,
		WrURL:         "/warroom" + q,
		RefreshURL:    "/refresh" + q,
		CriticalCount: countCriticalIssues(scan),
		CostsURL:      "/costs" + q,
	}

	if scan != nil && scan.report != nil {
		r := scan.report
		data.ClusterName = r.ClusterName
		data.MonthlyCost = r.TotalMonthlyCost
		data.SavingsPotential = r.TotalSavingsPotential.Best
		data.Scenarios = r.OptimizationScenarios
		data.Disclaimers = r.Disclaimers
		data.PricingSource = r.PricingSource
		data.Timestamp = r.Timestamp
		data.ScannedAtMS = r.Timestamp.UnixMilli()

		known, unknown := 0, 0
		for _, p := range r.NodePoolCosts {
			if p.PricePerNodeMonth > 0 {
				known++
			} else {
				unknown++
			}
		}
		total := known + unknown
		if total > 0 {
			data.AccuracyPct = known * 100 / total
		}
		data.KnownVMs, data.UnknownVMs, data.TotalVMs = known, unknown, total
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
				Name:          p.Name,
				VMSize:        p.VMSize,
				NodeCount:     p.NodeCount,
				CPUUtilPct:    p.CPUUtilizationPct,
				MemUtilPct:    p.MemoryUtilizationPct,
				CPUWidthStyle: template.CSS(fmt.Sprintf("width:%.0f%%", p.CPUUtilizationPct)),
				MemWidthStyle: template.CSS(fmt.Sprintf("width:%.0f%%", p.MemoryUtilizationPct)),
				PricePerNode:  formatMoney(p.PricePerNodeMonth),
				PoolTotal:     formatMoney(p.TotalMonthly),
				RISavings:     p.RISavings,
			}
			if strings.EqualFold(p.Priority, "spot") {
				row.TagClass, row.TagLabel = "tag-spot", "⚡ Spot"
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

	cisScore := scan.securityScore()
	data.SecurityColor = "green"
	if cisScore < 0 {
		data.SecurityDisplay, data.SecurityColor = "N/A", "blue"
	} else {
		data.SecurityDisplay = fmt.Sprintf("%d/100", cisScore)
		if cisScore < 60 {
			data.SecurityColor = "red"
		} else if cisScore < 80 {
			data.SecurityColor = "yellow"
		}
	}

	wasteItems := scan.wasteTotal()
	data.WasteColor = "green"
	if wasteItems < 0 {
		data.WasteDisplay, data.WasteColor = "N/A", "blue"
	} else {
		data.WasteDisplay = fmt.Sprintf("%d", wasteItems)
		if wasteItems > 10 {
			data.WasteColor = "red"
		} else if wasteItems > 0 {
			data.WasteColor = "yellow"
		}
	}

	data.PodCount = scan.monthlyPodCount()

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

	for _, issue := range collectWarRoomIssues(scan, 5) {
		wi := costWRIssue{warRoomIssue: issue}
		if issue.Severity == "critical" {
			wi.CardClass, wi.BadgeLabel = "c", "CRITICAL"
		} else {
			wi.CardClass, wi.BadgeLabel = "w", strings.ToUpper(issue.Severity)
		}
		wi.Icon, wi.TypeLabel = warRoomTypeLabel(issue.Type)
		if issue.AgeDays > 0 {
			wi.AgeLbl = fmt.Sprintf("%dd", issue.AgeDays)
		}
		wi.ShortName = issue.Resource
		if len(wi.ShortName) > 28 {
			wi.ShortName = wi.ShortName[:27] + "…"
		}
		wi.ShortMsg = issue.Message
		if len(wi.ShortMsg) > 80 {
			wi.ShortMsg = wi.ShortMsg[:79] + "…"
		}
		data.WRIssues = append(data.WRIssues, wi)
	}

	return data
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

func renderHTML(scan *clusterScan, activeCtx string, clusterList []string) string {
	return renderCostPage(scan, activeCtx, clusterList)
}

func confidenceColorHex(pct int) string {
	if pct < 50 {
		return "#ef4444"
	}
	if pct < 80 {
		return "#f59e0b"
	}
	return "#10b981"
}

func utilColorClass(pct float64) string {
	if pct > 85 {
		return "red"
	}
	if pct > 65 {
		return "yellow"
	}
	return "green"
}

func fmtCostRange(cr models.CostRange) string {
	if cr.Best == 0 {
		return "—"
	}
	return fmt.Sprintf("$%.0f", cr.Best)
}

func warRoomTypeLabel(t string) (icon, label string) {
	switch t {
	case "crash_loop":
		return "💀", "CrashLoop"
	case "oomkilled":
		return "🧠", "OOMKilled"
	case "image_pull_backoff":
		return "📦", "ImagePullBackOff"
	case "privileged_container":
		return "⚠️", "Privileged"
	case "unprotected_namespace":
		return "🔓", "No NetPolicy"
	case "orphaned_pvc":
		return "💾", "Orphaned PVC"
	case "zero_replica":
		return "⏸️", "Zero Replicas"
	default:
		return "⚠️", t
	}
}

// ── War Room page helpers ──────────────────────────────────────────────────────

func zombieTypeForStatus(status string) string {
	switch status {
	case "OOMKilled":
		return "oomkilled"
	case "ImagePullBackOff":
		return "image_pull_backoff"
	default:
		return "crash_loop"
	}
}

func kubectlCmdForIssue(issueType, resource, namespace string) string {
	switch issueType {
	case "crash_loop":
		return fmt.Sprintf("kubectl logs %s -n %s --previous", resource, namespace)
	default:
		return fmt.Sprintf("kubectl describe pod %s -n %s", resource, namespace)
	}
}

// formatMoney formats a float as comma-grouped integer string.
func formatMoney(amount float64) string {
	s := fmt.Sprintf("%.0f", amount)
	if len(s) <= 3 {
		return s
	}
	var result strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result.WriteByte(',')
		}
		result.WriteRune(c)
	}
	return result.String()
}

// ── Full scan pipeline ────────────────────────────────────────────────────────

// runFullScan runs all five analyzers against a cluster. The cost analysis is
// required (returns error on failure). Security, waste, network, and CIS are
// best-effort — failures are logged and leave the corresponding field nil.
func runFullScan(ctx string) (*clusterScan, error) {
	clientset, err := kubeClient(ctx)
	if err != nil {
		return nil, err
	}

	scan := &clusterScan{}

	// ── 1. Cost analysis (required) ───────────────────────────────────
	npCostAnalyzer := analyzer.NewNodePoolCostAnalyzer(clientset, region)
	poolCosts, _, err := npCostAnalyzer.AnalyzeNodePoolCosts()
	if err != nil {
		return nil, fmt.Errorf("node pool analysis: %w", err)
	}
	totalNodeCost := analyzer.TotalClusterCostFromPools(poolCosts)

	ra := analyzer.NewResourceAnalyzer(clientset)
	resourceAnalysis, err := ra.AnalyzeClusterResources(namespace)
	if err != nil {
		return nil, fmt.Errorf("resource analysis: %w", err)
	}

	nsCosts := npCostAnalyzer.AllocateNamespaceCosts(
		poolCosts, resourceAnalysis.Namespaces,
		resourceAnalysis.TotalCPUCores, resourceAnalysis.TotalMemoryGB,
	)

	if breakdown == "deployment" {
		da := analyzer.NewDeploymentCostAnalyzer(clientset)
		if enriched, err := da.EnrichWithDeployments(nsCosts); err == nil {
			nsCosts = enriched
		}
	}

	ca := analyzer.NewCostAnalyzer(resourceAnalysis)
	costEstimate, _ := ca.AnalyzeCosts(totalNodeCost)

	detectedRegion := region
	if detectedRegion == "" {
		detectedRegion = "auto-detected"
	}

	scan.report = &models.CloudCostReport{
		Timestamp:             time.Now(),
		ClusterName:           displayName(ctx),
		Region:                detectedRegion,
		Provider:              "azure",
		NodePoolCosts:         poolCosts,
		TotalNodeCost:         totalNodeCost,
		NamespaceCosts:        nsCosts,
		TotalMonthlyCost:      totalNodeCost,
		TotalAnnualCost:       totalNodeCost * 12,
		CostBreakdown:         models.CostBreakdown{Compute: totalNodeCost},
		OptimizationScenarios: costEstimate.OptimizationScenarios,
		TotalSavingsPotential: costEstimate.TotalSavingsPotential,
		PricingSource:         "embedded-catalog",
		Assumptions: []string{
			"VM pricing from embedded Azure retail price catalog (East US 2 baseline)",
			"Cost allocation: weighted average of CPU + Memory resource requests",
			"Node pool costs = Pay-As-You-Go unless spot label detected",
			"Does NOT include: disk I/O, network egress, Log Analytics, Defender for Cloud",
		},
		Disclaimers: []string{
			"Prices are approximate — based on Azure public pricing as of 2026",
			"Actual costs depend on Enterprise Agreement, MACC commitments, and negotiated rates",
			"Use Azure Cost Management + Billing for exact billing data",
			"Reserved Instance savings shown are potential — requires commitment purchase",
		},
	}

	// ── 2. Security audit (best effort) ──────────────────────────────
	sa := analyzer.NewSecurityAuditor(clientset)
	if secAudit, err := sa.AuditClusterSecurity(""); err == nil {
		scan.secAudit = secAudit
	} else {
		log.Printf("[%s] security audit skipped: %v", displayName(ctx), err)
	}

	// ── 3. Waste audit (best effort) ──────────────────────────────────
	wasteAuditor, cancel := analyzer.NewWasteAuditor(clientset, 0)
	defer cancel()
	if wasteAudit, err := wasteAuditor.AuditWaste(""); err == nil {
		scan.wasteAudit = wasteAudit
	} else {
		log.Printf("[%s] waste audit skipped: %v", displayName(ctx), err)
	}

	// ── 4. Network policy audit (best effort) ─────────────────────────
	netAuditor := analyzer.NewNetworkPolicyAuditor(clientset)
	if netAudit, err := netAuditor.AuditNetworkPolicies(""); err == nil {
		scan.netAudit = netAudit
	} else {
		log.Printf("[%s] network audit skipped: %v", displayName(ctx), err)
	}

	// ── 5. CIS score (derived from security + network audits) ─────────
	if scan.secAudit != nil {
		result := analyzer.CalculateCISScore(scan.secAudit, scan.netAudit)
		scan.cisResult = &result
	}

	return scan, nil
}

func kubeClient(ctx string) (*kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: ctx}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	cfg, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig for %q: %w", displayName(ctx), err)
	}
	return kubernetes.NewForConfig(cfg)
}
