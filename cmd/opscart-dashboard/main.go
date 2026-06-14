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
	mux.HandleFunc("/", srv.handleDashboard)
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
	if r.URL.Path != "/" {
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

func renderInfrastructurePage(scan *clusterScan, activeCtx string, clusterList []string) string {
	var pools []models.NodePoolCost
	scannedAt := time.Now()
	clusterName := displayName(activeCtx)
	if scan != nil && scan.report != nil {
		pools = scan.report.NodePoolCosts
		scannedAt = scan.report.Timestamp
		clusterName = scan.report.ClusterName
	}

	// Totals for summary row
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
	dashURL := "/" + q

	var sb strings.Builder

	// ── Head ──────────────────────────────────────────────────────────────────
	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>Infrastructure — ` + clusterName + `</title>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&display=swap" rel="stylesheet">
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0f172a;--surface:#1e293b;--surface-hover:#334155;--border:#334155;
  --text:#f1f5f9;--text-secondary:#94a3b8;--text-muted:#64748b;
  --primary:#6366f1;--primary-light:#818cf8;--primary-bg:rgba(99,102,241,0.1);
  --success:#10b981;--warning:#f59e0b;--danger:#ef4444;--info:#06b6d4;
  --radius:12px;--radius-sm:8px;--radius-xs:6px;
}
body{font-family:'Inter',system-ui,sans-serif;background:var(--bg);color:var(--text);line-height:1.6;min-height:100vh}
.layout{display:grid;grid-template-columns:260px 1fr;min-height:100vh}
.sidebar{background:var(--surface);border-right:1px solid var(--border);padding:2rem 1.5rem;position:sticky;top:0;height:100vh;overflow-y:auto}
.main{padding:2rem 2.5rem;overflow-y:auto}
.logo{display:flex;align-items:center;gap:0.75rem;margin-bottom:2.5rem}
.logo-icon{width:40px;height:40px;background:var(--primary);border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:1.2rem}
.logo-text{font-size:1.1rem;font-weight:700}
.logo-sub{font-size:0.7rem;color:var(--text-muted);text-transform:uppercase;letter-spacing:1px}
.nav-section{margin-bottom:1.5rem}
.nav-label{font-size:0.7rem;text-transform:uppercase;letter-spacing:1.5px;color:var(--text-muted);margin-bottom:0.75rem;padding-left:0.75rem}
.nav-item{display:flex;align-items:center;gap:0.75rem;padding:0.7rem 0.75rem;border-radius:var(--radius-sm);color:var(--text-secondary);font-size:0.9rem;cursor:pointer;transition:all 0.15s;text-decoration:none}
.nav-item:hover{background:var(--surface-hover);color:var(--text)}
.nav-item.active{background:var(--primary-bg);color:var(--primary-light);font-weight:600}
.cluster-info{margin-top:auto;padding-top:2rem;border-top:1px solid var(--border)}
.cluster-badge{background:var(--primary-bg);border:1px solid rgba(99,102,241,0.2);border-radius:var(--radius-sm);padding:0.85rem;margin-bottom:0.75rem}
.cluster-badge h4{font-size:0.7rem;color:var(--primary-light);margin-bottom:0.3rem;text-transform:uppercase;letter-spacing:0.8px}
.cluster-badge p{font-size:0.75rem;color:var(--text-muted);word-break:break-all}
.page-hdr{display:flex;justify-content:space-between;align-items:flex-start;margin-bottom:1.75rem;flex-wrap:wrap;gap:1rem}
.page-hdr h1{font-size:1.75rem;font-weight:800}
.page-hdr p{color:var(--text-muted);font-size:0.85rem;margin-top:0.25rem}
.page-meta{text-align:right;font-size:0.78rem;color:var(--text-muted)}
.page-meta strong{color:var(--text-secondary)}
.back-link{display:block;margin-top:0.5rem;color:var(--primary-light);font-size:0.75rem;text-decoration:none}
.back-link:hover{text-decoration:underline}
.summary-bar{display:flex;gap:1rem;margin-bottom:2rem;flex-wrap:wrap}
.kpi-chip{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius-sm);padding:0.75rem 1.25rem;min-width:140px}
.kpi-chip-val{font-size:1.4rem;font-weight:800;line-height:1;margin-bottom:0.2rem}
.kpi-chip-val.cost{color:var(--danger)}
.kpi-chip-val.save{color:var(--success)}
.kpi-chip-lbl{font-size:0.7rem;color:var(--text-muted);text-transform:uppercase;letter-spacing:0.5px}
.section{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:1.5rem;margin-bottom:1.5rem}
.section-hdr{display:flex;justify-content:space-between;align-items:center;margin-bottom:1.25rem}
.section-title{font-size:1rem;font-weight:700}
.inf-wrap{overflow-x:auto;border:1px solid var(--border);border-radius:var(--radius-sm)}
.inf-table{width:100%;border-collapse:collapse;min-width:860px}
.inf-table th{background:rgba(0,0,0,0.35);padding:0.6rem 0.85rem;text-align:left;font-size:0.67rem;text-transform:uppercase;letter-spacing:0.8px;color:var(--text-muted);font-weight:600;border-bottom:1px solid var(--border);white-space:nowrap}
.inf-table td{padding:0.75rem 0.85rem;border-bottom:1px solid var(--border);font-size:0.83rem;vertical-align:middle}
.inf-table tbody tr:last-child td{border-bottom:none}
.inf-table tbody tr:hover td{background:rgba(99,102,241,0.03)}
.inf-table tfoot td{background:rgba(0,0,0,0.25);font-weight:700;border-top:2px solid var(--border);font-size:0.83rem;padding:0.75rem 0.85rem}
.pool-name{font-weight:600;color:var(--text);margin-bottom:0.3rem}
.badge-row{display:flex;gap:4px;flex-wrap:wrap}
.tag{display:inline-flex;align-items:center;padding:1px 7px;border-radius:20px;font-size:0.63rem;font-weight:600;white-space:nowrap}
.tag-spot{background:#422006;color:#fbbf24;border:1px solid rgba(251,191,36,0.2)}
.tag-regular{background:#172554;color:#60a5fa;border:1px solid rgba(96,165,250,0.2)}
.tag-system{background:rgba(99,102,241,0.15);color:var(--primary-light);border:1px solid rgba(99,102,241,0.25)}
.tag-user{background:rgba(100,116,139,0.12);color:#94a3b8;border:1px solid rgba(100,116,139,0.2)}
.tag-windows{background:rgba(6,182,212,0.1);color:#22d3ee;border:1px solid rgba(6,182,212,0.15)}
.sku{font-family:'Consolas','Monaco',monospace;font-size:0.75rem;color:var(--primary-light)}
.sub{color:var(--text-muted);font-size:0.7rem;margin-top:2px}
.util-cell{min-width:130px}
.util-nums{font-size:0.75rem;color:var(--text-secondary);margin-bottom:3px}
.util-bar{display:flex;align-items:center;gap:6px}
.util-track{flex:1;height:5px;background:rgba(255,255,255,0.06);border-radius:3px;overflow:hidden}
.util-fill{height:100%;border-radius:3px;transition:width 0.4s ease}
.fill-green{background:var(--success)}
.fill-yellow{background:var(--warning)}
.fill-red{background:var(--danger)}
.util-pct{font-size:0.72rem;color:var(--text-secondary);min-width:32px;text-align:right}
.money{font-variant-numeric:tabular-nums;font-weight:600}
.ri-val{color:var(--success);font-weight:600;font-size:0.8rem}
.ri-na{color:var(--text-muted);font-size:0.78rem}
.empty-state{text-align:center;padding:3rem 1rem;color:var(--text-muted)}
.empty-state .icon{font-size:2.5rem;margin-bottom:0.75rem}
.footer{text-align:center;padding:1.5rem 0;color:var(--text-muted);font-size:0.75rem;border-top:1px solid var(--border);margin-top:1rem}
@keyframes fadeIn{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:translateY(0)}}
.section{animation:fadeIn 0.3s ease both}
@media(max-width:1024px){.layout{grid-template-columns:1fr}.sidebar{display:none}}
</style>
</head>
<body>
<div class="layout">
`)

	// ── Sidebar ────────────────────────────────────────────────────────────────
	sb.WriteString(buildSidebar("infrastructure", activeCtx, clusterName, clusterList))

	// ── Main ──────────────────────────────────────────────────────────────────
	sb.WriteString(`<main class="main">`)

	sb.WriteString(fmt.Sprintf(`<div class="page-hdr">
<div>
  <h1>🖥️ Infrastructure</h1>
  <p>%s &mdash; %d node pool(s), %d node(s)</p>
</div>
<div class="page-meta">
  <div>Last scanned: <strong id="infra-age">just now</strong></div>
  <a class="back-link" href="%s">← Back to Dashboard</a>
</div>
</div>
`, clusterName, len(pools), totalNodes, dashURL))

	// Summary KPI chips
	sb.WriteString(`<div class="summary-bar">`)
	sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val">%d</div><div class="kpi-chip-lbl">Node Pools</div></div>`, len(pools)))
	sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val">%d</div><div class="kpi-chip-lbl">Total Nodes</div></div>`, totalNodes))
	sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val">%.0f</div><div class="kpi-chip-lbl">Total vCPUs</div></div>`, totalCores))
	sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val">%.0f GB</div><div class="kpi-chip-lbl">Total Memory</div></div>`, totalMemGB))
	sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val cost">$%s</div><div class="kpi-chip-lbl">Monthly Cost</div></div>`, formatMoney(totalCost)))
	if totalRI1yr > 0 {
		sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val save">$%s</div><div class="kpi-chip-lbl">1yr RI Potential</div></div>`, formatMoney(totalRI1yr)))
	}
	sb.WriteString(`</div>`)

	// Node pool table
	sb.WriteString(`<div class="section">`)
	sb.WriteString(`<div class="section-hdr"><div class="section-title">Node Pools</div></div>`)

	if len(pools) == 0 {
		sb.WriteString(`<div class="empty-state"><div class="icon">🖥️</div><div>No node pool data available</div></div>`)
	} else {
		sb.WriteString(`<div class="inf-wrap"><table class="inf-table">`)
		sb.WriteString(`<thead><tr>
<th>Pool</th>
<th>VM SKU</th>
<th style="text-align:center">Nodes</th>
<th>CPU Utilization</th>
<th>Memory Utilization</th>
<th style="text-align:right">$/Node/mo</th>
<th style="text-align:right">Pool/mo</th>
<th style="text-align:right">1yr RI Save</th>
<th style="text-align:right">3yr RI Save</th>
</tr></thead>`)
		sb.WriteString(`<tbody>`)
		for _, p := range pools {
			sb.WriteString(renderInfraPoolRow(p))
		}
		sb.WriteString(`</tbody>`)

		// Summary / totals footer row
		ri1yrCell := `<span class="ri-na">—</span>`
		if totalRI1yr > 0 {
			ri1yrCell = fmt.Sprintf(`<span class="ri-val">$%s</span>`, formatMoney(totalRI1yr))
		}
		ri3yrCell := `<span class="ri-na">—</span>`
		if totalRI3yr > 0 {
			ri3yrCell = fmt.Sprintf(`<span class="ri-val">$%s</span>`, formatMoney(totalRI3yr))
		}
		sb.WriteString(fmt.Sprintf(`<tfoot><tr>
<td colspan="2" style="color:var(--text-muted);font-size:0.75rem;text-transform:uppercase;letter-spacing:0.5px">Total</td>
<td style="text-align:center">%d</td>
<td><span style="font-size:0.78rem;color:var(--text-muted)">%.1f / %.1f cores</span></td>
<td><span style="font-size:0.78rem;color:var(--text-muted)">%.1f / %.1f GB</span></td>
<td style="text-align:right">—</td>
<td style="text-align:right" class="money">$%s</td>
<td style="text-align:right">%s</td>
<td style="text-align:right">%s</td>
</tr></tfoot>`,
			totalNodes,
			sumCPUReq(pools), totalCores,
			sumMemReq(pools), totalMemGB,
			formatMoney(totalCost),
			ri1yrCell, ri3yrCell,
		))
		sb.WriteString(`</table></div>`)
	}
	sb.WriteString(`</div>`) // section

	sb.WriteString(`<div class="footer">OpsCart FinOps Engine &middot; Infrastructure &middot; Pricing from embedded Azure catalog &mdash; actual costs may vary</div>`)
	sb.WriteString(`</main>`)
	sb.WriteString(`</div>`) // layout

	sb.WriteString(fmt.Sprintf(`<script>
(function(){
  var TS=%d,el=document.getElementById('infra-age'),sb=document.getElementById('infra-age-sb');
  function upd(){
    var s=Math.floor((Date.now()-TS)/1000);
    var t=s<5?'just now':s<60?s+'s ago':Math.floor(s/60)+'m ago';
    if(el)el.textContent=t;if(sb)sb.textContent=t;
  }
  setInterval(upd,1000);upd();
})();
</script>
</body>
</html>`, scannedAt.UnixMilli()))

	return sb.String()
}

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
		Sidebar:          template.HTML(buildSidebar("namespaces", activeCtx, clusterName, clusterList)),
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

func renderOptimizationsPage(scan *clusterScan, activeCtx string, clusterList []string) string {
	// ── Data ─────────────────────────────────────────────────────────────────
	var pools []models.NodePoolCost
	var nsCosts []models.NamespaceCostInfo
	clusterName := displayName(activeCtx)
	if scan != nil && scan.report != nil {
		pools = scan.report.NodePoolCosts
		nsCosts = scan.report.NamespaceCosts
		clusterName = scan.report.ClusterName
	}

	// RI opportunities
	var riPools []models.NodePoolCost
	var totalRI1yr, totalRI3yr float64
	for _, p := range pools {
		if p.RISavings > 0 {
			riPools = append(riPools, p)
			totalRI1yr += p.RISavings
			totalRI3yr += p.RISavings3yr
		}
	}
	sort.Slice(riPools, func(i, j int) bool { return riPools[i].RISavings > riPools[j].RISavings })

	// Waste counts — same data path as collectWarRoomIssues
	var zombieCount, idleCount, zeroReplicaCount, abandonedCount, pvcCount, pvcStorageGB int
	var pvcCostEst float64
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
	}

	// Right-sizing: namespaces with < 0.5 CPU cores requested but running pods
	var rsNS []models.NamespaceCostInfo
	for _, ns := range nsCosts {
		if ns.CPUCores < 0.5 && ns.PodCount > 0 {
			rsNS = append(rsNS, ns)
		}
	}
	sort.Slice(rsNS, func(i, j int) bool {
		return rsNS[i].EstimatedCost.Best > rsNS[j].EstimatedCost.Best
	})
	if len(rsNS) > 10 {
		rsNS = rsNS[:10]
	}

	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}
	dashURL := "/" + q

	var sb strings.Builder

	// ── Head ─────────────────────────────────────────────────────────────────
	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>Optimizations — ` + clusterName + `</title>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&display=swap" rel="stylesheet">
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0f172a;--surface:#1e293b;--surface-hover:#334155;--border:#334155;
  --text:#f1f5f9;--text-secondary:#94a3b8;--text-muted:#64748b;
  --primary:#6366f1;--primary-light:#818cf8;--primary-bg:rgba(99,102,241,0.1);
  --success:#10b981;--warning:#f59e0b;--danger:#ef4444;--info:#06b6d4;
  --radius:12px;--radius-sm:8px;--radius-xs:6px;
}
body{font-family:'Inter',system-ui,sans-serif;background:var(--bg);color:var(--text);line-height:1.6;min-height:100vh}
.layout{display:grid;grid-template-columns:260px 1fr;min-height:100vh}
.sidebar{background:var(--surface);border-right:1px solid var(--border);padding:2rem 1.5rem;position:sticky;top:0;height:100vh;overflow-y:auto}
.main{padding:2rem 2.5rem;overflow-y:auto}
.logo{display:flex;align-items:center;gap:0.75rem;margin-bottom:2.5rem}
.logo-icon{width:40px;height:40px;background:var(--primary);border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:1.2rem}
.logo-text{font-size:1.1rem;font-weight:700}
.logo-sub{font-size:0.7rem;color:var(--text-muted);text-transform:uppercase;letter-spacing:1px}
.nav-section{margin-bottom:1.5rem}
.nav-label{font-size:0.7rem;text-transform:uppercase;letter-spacing:1.5px;color:var(--text-muted);margin-bottom:0.75rem;padding-left:0.75rem}
.nav-item{display:flex;align-items:center;gap:0.75rem;padding:0.7rem 0.75rem;border-radius:var(--radius-sm);color:var(--text-secondary);font-size:0.9rem;cursor:pointer;transition:all 0.15s;text-decoration:none}
.nav-item:hover{background:var(--surface-hover);color:var(--text)}
.nav-item.active{background:var(--primary-bg);color:var(--primary-light);font-weight:600}
.cluster-info{margin-top:auto;padding-top:2rem;border-top:1px solid var(--border)}
.cluster-badge{background:var(--primary-bg);border:1px solid rgba(99,102,241,0.2);border-radius:var(--radius-sm);padding:0.85rem;margin-bottom:0.75rem}
.cluster-badge h4{font-size:0.7rem;color:var(--primary-light);margin-bottom:0.3rem;text-transform:uppercase;letter-spacing:0.8px}
.cluster-badge p{font-size:0.75rem;color:var(--text-muted);word-break:break-all}
.page-hdr{display:flex;justify-content:space-between;align-items:flex-start;margin-bottom:1.75rem;flex-wrap:wrap;gap:1rem}
.page-hdr h1{font-size:1.75rem;font-weight:800}
.page-hdr p{color:var(--text-muted);font-size:0.85rem;margin-top:0.25rem}
.page-meta{text-align:right;font-size:0.78rem;color:var(--text-muted)}
.back-link{display:block;margin-top:0.5rem;color:var(--primary-light);font-size:0.75rem;text-decoration:none}
.back-link:hover{text-decoration:underline}
.summary-bar{display:flex;gap:1rem;margin-bottom:2rem;flex-wrap:wrap}
.kpi-chip{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius-sm);padding:0.75rem 1.25rem;min-width:140px}
.kpi-chip-val{font-size:1.4rem;font-weight:800;line-height:1;margin-bottom:0.2rem}
.kpi-chip-val.save{color:var(--success)}
.kpi-chip-val.warn{color:var(--warning)}
.kpi-chip-lbl{font-size:0.7rem;color:var(--text-muted);text-transform:uppercase;letter-spacing:0.5px}
.section{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:1.5rem;margin-bottom:1.5rem}
.section-hdr{display:flex;justify-content:space-between;align-items:center;margin-bottom:1.25rem}
.section-title{font-size:1rem;font-weight:700}
.section-sub{font-size:0.78rem;color:var(--text-muted)}
.opt-wrap{overflow-x:auto;border:1px solid var(--border);border-radius:var(--radius-sm)}
.opt-table{width:100%;border-collapse:collapse}
.opt-table th{background:rgba(0,0,0,0.35);padding:0.6rem 0.85rem;text-align:left;font-size:0.67rem;text-transform:uppercase;letter-spacing:0.8px;color:var(--text-muted);font-weight:600;border-bottom:1px solid var(--border);white-space:nowrap}
.opt-table td{padding:0.75rem 0.85rem;border-bottom:1px solid var(--border);font-size:0.83rem;vertical-align:middle}
.opt-table tbody tr:last-child td{border-bottom:none}
.opt-table tbody tr:hover td{background:rgba(99,102,241,0.03)}
.opt-table tfoot td{background:rgba(0,0,0,0.25);font-weight:700;border-top:2px solid var(--border);font-size:0.83rem;padding:0.75rem 0.85rem}
.sku{font-family:'Consolas','Monaco',monospace;font-size:0.75rem;color:var(--primary-light)}
.money{font-variant-numeric:tabular-nums;font-weight:600}
.save{color:var(--success);font-weight:600}
.muted{color:var(--text-muted)}
.badge{display:inline-flex;align-items:center;padding:2px 9px;border-radius:20px;font-size:0.7rem;font-weight:600;white-space:nowrap}
.badge-warn{background:rgba(245,158,11,0.12);color:#fbbf24;border:1px solid rgba(245,158,11,0.2)}
.empty-state{text-align:center;padding:2.5rem 1rem;color:var(--text-muted)}
.empty-state .icon{font-size:2rem;margin-bottom:0.5rem}
@keyframes fadeIn{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:translateY(0)}}
.section{animation:fadeIn 0.3s ease both}
@media(max-width:1024px){.layout{grid-template-columns:1fr}.sidebar{display:none}}
</style>
</head>
<body>
<div class="layout">
`)

	// ── Sidebar ───────────────────────────────────────────────────────────────
	sb.WriteString(buildSidebar("optimizations", activeCtx, clusterName, clusterList))

	// ── Main ─────────────────────────────────────────────────────────────────
	sb.WriteString(`<main class="main">`)

	sb.WriteString(fmt.Sprintf(`<div class="page-hdr">
<div>
  <h1>💡 Optimizations</h1>
  <p>%s &mdash; cost-saving opportunities</p>
</div>
<div class="page-meta">
  <a class="back-link" href="%s">← Back to Dashboard</a>
</div>
</div>
`, clusterName, dashURL))

	// Summary KPI chips
	sb.WriteString(`<div class="summary-bar">`)
	if totalRI1yr > 0 {
		sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val save">$%s</div><div class="kpi-chip-lbl">1yr RI Savings/mo</div></div>`, formatMoney(totalRI1yr)))
	}
	if totalRI3yr > 0 {
		sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val save">$%s</div><div class="kpi-chip-lbl">3yr RI Savings/mo</div></div>`, formatMoney(totalRI3yr)))
	}
	if pvcCostEst > 0 {
		sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val warn">$%s</div><div class="kpi-chip-lbl">PVC Waste/mo</div></div>`, formatMoney(pvcCostEst)))
	}
	wasteTotal := 0
	if scan != nil && scan.wasteAudit != nil {
		wasteTotal = scan.wasteAudit.TotalWasteItems
	}
	sb.WriteString(fmt.Sprintf(`<div class="kpi-chip"><div class="kpi-chip-val">%d</div><div class="kpi-chip-lbl">Total Waste Items</div></div>`, wasteTotal))
	sb.WriteString(`</div>`)

	// ── Section 1: Reserved Instance Opportunities ────────────────────────────
	sb.WriteString(`<div class="section">`)
	sb.WriteString(`<div class="section-hdr">
<div class="section-title">💰 Reserved Instance Opportunities</div>
<div class="section-sub">Savings vs. Pay-As-You-Go for On-Demand pools</div>
</div>`)
	if len(riPools) == 0 {
		sb.WriteString(`<div class="empty-state"><div class="icon">💰</div><div>No RI opportunities — all pools are Spot or pricing data unavailable</div></div>`)
	} else {
		sb.WriteString(`<div class="opt-wrap"><table class="opt-table">
<thead><tr>
<th>Pool</th>
<th class="sku">VM SKU</th>
<th style="text-align:center">Nodes</th>
<th style="text-align:right">PAYG/mo</th>
<th style="text-align:right">1yr RI Save/mo</th>
<th style="text-align:right">3yr RI Save/mo</th>
</tr></thead>
<tbody>`)
		for _, p := range riPools {
			ri3Cell := `<span class="muted">—</span>`
			if p.RISavings3yr > 0 {
				ri3Cell = fmt.Sprintf(`<span class="save">$%s</span>`, formatMoney(p.RISavings3yr))
			}
			sb.WriteString(fmt.Sprintf(`<tr>
<td><strong>%s</strong></td>
<td><span class="sku">%s</span></td>
<td style="text-align:center">%d</td>
<td style="text-align:right"><span class="money">$%s</span></td>
<td style="text-align:right"><span class="save">$%s</span></td>
<td style="text-align:right">%s</td>
</tr>`, p.Name, p.VMSize, p.NodeCount, formatMoney(p.TotalMonthly), formatMoney(p.RISavings), ri3Cell))
		}
		sb.WriteString(`</tbody>`)
		ri3TotalCell := `<span class="muted">—</span>`
		if totalRI3yr > 0 {
			ri3TotalCell = fmt.Sprintf(`<span class="save">$%s</span>`, formatMoney(totalRI3yr))
		}
		sb.WriteString(fmt.Sprintf(`<tfoot><tr>
<td colspan="4" style="color:var(--text-muted);font-size:0.75rem;text-transform:uppercase;letter-spacing:0.5px">Total potential savings</td>
<td style="text-align:right"><span class="save">$%s</span></td>
<td style="text-align:right">%s</td>
</tr></tfoot>`, formatMoney(totalRI1yr), ri3TotalCell))
		sb.WriteString(`</table></div>`)
	}
	sb.WriteString(`</div>`)

	// ── Section 2: Waste Items by Category ───────────────────────────────────
	sb.WriteString(`<div class="section">`)
	sb.WriteString(`<div class="section-hdr">
<div class="section-title">🗑️ Waste Items by Category</div>
<div class="section-sub">Resources consuming cost or capacity without business value</div>
</div>`)
	sb.WriteString(`<div class="opt-wrap"><table class="opt-table">
<thead><tr>
<th>Category</th>
<th style="text-align:center">Count</th>
<th style="text-align:right">Est. $/mo</th>
<th>Notes</th>
</tr></thead>
<tbody>`)

	pvcCostCell := `<span class="muted">—</span>`
	if pvcCostEst > 0 {
		pvcCostCell = fmt.Sprintf(`<span class="save">$%s</span>`, formatMoney(pvcCostEst))
	}
	pvcNote := fmt.Sprintf("%d GB unattached storage", pvcStorageGB)
	if pvcStorageGB == 0 {
		pvcNote = "no size data"
	}
	sb.WriteString(fmt.Sprintf(`<tr>
<td><strong>Orphaned PVCs</strong></td>
<td style="text-align:center">%d</td>
<td style="text-align:right">%s</td>
<td class="muted">%s</td>
</tr>`, pvcCount, pvcCostCell, pvcNote))

	sb.WriteString(fmt.Sprintf(`<tr>
<td><strong>Zero-Replica Workloads</strong></td>
<td style="text-align:center">%d</td>
<td style="text-align:right"><span class="muted">—</span></td>
<td class="muted">Deployments/StatefulSets scaled to 0</td>
</tr>`, zeroReplicaCount))

	sb.WriteString(fmt.Sprintf(`<tr>
<td><strong>Abandoned Namespaces</strong></td>
<td style="text-align:center">%d</td>
<td style="text-align:right"><span class="muted">—</span></td>
<td class="muted">All pods idle, no recent activity</td>
</tr>`, abandonedCount))

	sb.WriteString(fmt.Sprintf(`<tr>
<td><strong>Zombie Pods</strong></td>
<td style="text-align:center">%d</td>
<td style="text-align:right"><span class="muted">—</span></td>
<td class="muted">CrashLoopBackOff / OOMKilled consuming node capacity</td>
</tr>`, zombieCount))

	sb.WriteString(fmt.Sprintf(`<tr>
<td><strong>Idle Pods</strong></td>
<td style="text-align:center">%d</td>
<td style="text-align:right"><span class="muted">—</span></td>
<td class="muted">Running but no activity for extended period</td>
</tr>`, idleCount))

	sb.WriteString(`</tbody>`)
	totalWasteCostCell := `<span class="muted">—</span>`
	if pvcCostEst > 0 {
		totalWasteCostCell = fmt.Sprintf(`<span class="save">$%s</span>`, formatMoney(pvcCostEst))
	}
	sb.WriteString(fmt.Sprintf(`<tfoot><tr>
<td colspan="2" style="color:var(--text-muted);font-size:0.75rem;text-transform:uppercase;letter-spacing:0.5px">Total potential savings</td>
<td style="text-align:right">%s</td>
<td></td>
</tr></tfoot>`, totalWasteCostCell))
	sb.WriteString(`</table></div>`)
	sb.WriteString(`</div>`)

	// ── Section 3: Right-sizing ───────────────────────────────────────────────
	sb.WriteString(`<div class="section">`)
	sb.WriteString(`<div class="section-hdr">
<div class="section-title">📐 Right-Sizing Candidates</div>
<div class="section-sub">Namespaces with pods running but &lt;0.5 CPU cores requested — potential over-provisioning</div>
</div>`)
	if len(rsNS) == 0 {
		sb.WriteString(`<div class="empty-state"><div class="icon">📐</div><div>No right-sizing candidates identified</div></div>`)
	} else {
		sb.WriteString(`<div class="opt-wrap"><table class="opt-table">
<thead><tr>
<th>Namespace</th>
<th style="text-align:center">Pods</th>
<th style="text-align:right">CPU Cores</th>
<th style="text-align:right">Memory (GB)</th>
<th style="text-align:right">Est. Cost/mo</th>
</tr></thead>
<tbody>`)
		for _, ns := range rsNS {
			costCell := `<span class="muted">—</span>`
			if ns.EstimatedCost.Best >= 1 {
				costCell = fmt.Sprintf(`<span class="money">$%s</span>`, formatMoney(ns.EstimatedCost.Best))
			} else if ns.EstimatedCost.Best > 0 {
				costCell = `<span class="money">&lt;$1</span>`
			}
			sb.WriteString(fmt.Sprintf(`<tr>
<td><strong>%s</strong></td>
<td style="text-align:center">%d</td>
<td style="text-align:right">%.3f</td>
<td style="text-align:right">%.1f</td>
<td style="text-align:right">%s</td>
</tr>`, ns.Name, ns.PodCount, ns.CPUCores, ns.MemoryGB, costCell))
		}
		sb.WriteString(`</tbody></table></div>`)
	}
	sb.WriteString(`</div>`)

	sb.WriteString(`</main></div></body></html>`)
	return sb.String()
}

// buildSidebar returns a complete <aside>…</aside> sidebar, shared by all sub-pages.
// activePage is one of: "dashboard", "infrastructure", "namespaces", "optimizations", "warroom".
func buildSidebar(activePage, activeCtx, clusterName string, clusterList []string) string {
	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}
	dashHref := "/" + q
	infraHref := "/infrastructure" + q
	nsHref := "/namespaces" + q
	optHref := "/optimizations" + q
	wrHref := "/warroom" + q

	active := func(page string) string {
		if page == activePage {
			return " active"
		}
		return ""
	}

	// Cluster switcher links stay on the current page
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

	var sb strings.Builder
	sb.WriteString(`<aside class="sidebar">
<div class="logo">
  <div class="logo-icon">⚡</div>
  <div><div class="logo-text">OpsCart</div><div class="logo-sub">FinOps Engine</div></div>
</div>
<div class="nav-section">
  <div class="nav-label">Analysis</div>
  <a class="nav-item` + active("dashboard") + `" href="` + dashHref + `">📊 Cost Overview</a>
  <a class="nav-item` + active("infrastructure") + `" href="` + infraHref + `">🖥️ Infrastructure</a>
  <a class="nav-item` + active("namespaces") + `" href="` + nsHref + `">📦 Namespaces</a>
  <a class="nav-item` + active("optimizations") + `" href="` + optHref + `">💡 Optimizations</a>
</div>
<div class="nav-section">
  <div class="nav-label">Ops</div>
  <a class="nav-item` + active("warroom") + `" href="` + wrHref + `">🚨 War Room</a>
</div>
`)

	if len(clusterList) > 1 {
		sb.WriteString(`<div class="nav-section"><div class="nav-label">Clusters</div>`)
		for _, ctx := range clusterList {
			cls := "nav-item"
			if ctx == activeCtx {
				cls += " active"
			}
			href := basePath + "?" + url.Values{"cluster": {ctx}}.Encode()
			label := displayName(ctx)
			if len(label) > 22 {
				label = label[:21] + "…"
			}
			sb.WriteString(fmt.Sprintf(`<a class="%s" href="%s">🔵 %s</a>`, cls, href, label))
		}
		sb.WriteString(`</div>`)
	}

	sb.WriteString(fmt.Sprintf(`<div class="cluster-info">
<div class="cluster-badge"><h4>Cluster</h4><p>%s</p></div>
</div>
</aside>
`, clusterName))

	return sb.String()
}

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
		Sidebar:       template.HTML(buildSidebar("warroom", activeCtx, clusterName, clusterList)),
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

	WRIssues []costWRIssue
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
		ActiveCtx:    activeCtx,
		ClusterCount: len(clusterList),
		DashURL:      "/" + q,
		InfraURL:     "/infrastructure" + q,
		NSsURL:       "/namespaces" + q,
		OptURL:       "/optimizations" + q,
		WrURL:        "/warroom" + q,
		RefreshURL:   "/refresh" + q,
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
