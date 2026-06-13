package main

import (
	"encoding/json"
	"fmt"
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
		"monthly_cost":    scan.report.TotalMonthlyCost,
		"waste_total":     scan.wasteTotal(),
		"security_score":  scan.securityScore(),
		"cluster_count":   len(srv.clusterList),
		"pod_count":       scan.monthlyPodCount(),
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

func collectWarRoomIssues(scan *clusterScan, limit int) []warRoomIssue {
	var issues []warRoomIssue

	// 1. Crash-looping / zombie pods
	if scan.wasteAudit != nil {
		for _, pod := range scan.wasteAudit.StalePods {
			if pod.Kind == analyzer.StalePodZombie {
				issues = append(issues, warRoomIssue{
					Severity:  "critical",
					Type:      "crash_loop",
					Namespace: pod.Namespace,
					Resource:  pod.Name,
					Message:   fmt.Sprintf("%s — %d restarts, %d days old", pod.Status, pod.RestartCount, pod.AgeDays),
				})
			}
		}
	}

	// 2. Unexpected privileged containers (non-system or unexpected system ones)
	if scan.secAudit != nil {
		for _, issue := range scan.secAudit.Issues {
			if issue.Type == "privileged_container" && issue.Severity == "critical" {
				issues = append(issues, warRoomIssue{
					Severity:  "critical",
					Type:      "privileged_container",
					Namespace: issue.Namespace,
					Resource:  issue.Name,
					Message:   issue.Description,
				})
			}
		}
	}

	// 3. High-risk unprotected namespaces
	if scan.netAudit != nil {
		for _, ns := range scan.netAudit.UnprotectedNamespaces {
			if ns.RiskLevel == "HIGH" {
				issues = append(issues, warRoomIssue{
					Severity:  "high",
					Type:      "unprotected_namespace",
					Namespace: ns.Name,
					Resource:  "namespace",
					Message:   fmt.Sprintf("No NetworkPolicy — %d pods exposed", ns.PodCount),
				})
			}
		}
	}

	// Sort: critical before high
	severityOrder := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}
	sort.SliceStable(issues, func(i, j int) bool {
		return severityOrder[issues[i].Severity] < severityOrder[issues[j].Severity]
	})

	if len(issues) > limit {
		return issues[:limit]
	}
	return issues
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
	critical, warnings := collectPageWarRoomIssues(scan)

	scannedAt := time.Now()
	clusterName := displayName(activeCtx)
	if scan != nil && scan.report != nil {
		scannedAt = scan.report.Timestamp
		clusterName = scan.report.ClusterName
	}

	dashURL := "/"
	warRoomURL := "/warroom"
	if activeCtx != "" {
		q := url.QueryEscape(activeCtx)
		dashURL = "/?cluster=" + q
		warRoomURL = "/warroom?cluster=" + q
	}

	var sb strings.Builder

	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>War Room — ` + clusterName + `</title>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&display=swap" rel="stylesheet">
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0f172a;--surface:#1e293b;--surface-hover:#334155;--border:#334155;
  --text:#f1f5f9;--text-secondary:#94a3b8;--text-muted:#64748b;
  --primary:#6366f1;--primary-light:#818cf8;--primary-bg:rgba(99,102,241,0.1);
  --success:#10b981;--warning:#f59e0b;--danger:#ef4444;
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
.nav-item.active{background:rgba(239,68,68,0.12);color:#f87171;font-weight:600}
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
.summary-chip{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius-sm);padding:0.75rem 1.25rem;display:flex;align-items:center;gap:0.75rem;min-width:160px}
.summary-chip.c{border-color:rgba(239,68,68,0.35)}
.summary-chip.w{border-color:rgba(245,158,11,0.35)}
.summary-chip.ok{border-color:rgba(16,185,129,0.35)}
.summary-num{font-size:1.6rem;font-weight:800;line-height:1}
.c .summary-num{color:var(--danger)}
.w .summary-num{color:var(--warning)}
.ok .summary-num{color:var(--success)}
.summary-lbl{font-size:0.72rem;color:var(--text-muted);text-transform:uppercase;letter-spacing:0.5px;line-height:1.3}
.wr-section{margin-bottom:2.5rem}
.wr-section-hdr{display:flex;align-items:center;gap:0.75rem;margin-bottom:1rem;padding-bottom:0.75rem;border-bottom:1px solid var(--border)}
.wr-section-hdr h2{font-size:1rem;font-weight:700}
.cnt-chip{padding:3px 10px;border-radius:20px;font-size:0.7rem;font-weight:700}
.cnt-chip.c{background:rgba(239,68,68,0.15);color:#f87171}
.cnt-chip.w{background:rgba(245,158,11,0.15);color:#fbbf24}
.wr-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:1rem}
.wr-card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:1.25rem;display:flex;flex-direction:column;gap:0.55rem;transition:border-color 0.2s;animation:fadeIn 0.3s ease both}
.wr-card.c{border-color:rgba(239,68,68,0.3);background:rgba(239,68,68,0.025)}
.wr-card.w{border-color:rgba(245,158,11,0.25);background:rgba(245,158,11,0.02)}
.wr-card:hover{border-color:var(--primary)}
.wr-top{display:flex;align-items:center;gap:0.5rem;flex-wrap:wrap}
.badge{padding:2px 8px;border-radius:20px;font-size:0.64rem;font-weight:700;letter-spacing:0.5px}
.badge.c{background:rgba(239,68,68,0.15);color:#f87171}
.badge.w{background:rgba(245,158,11,0.15);color:#fbbf24}
.type-lbl{font-size:0.72rem;color:var(--text-muted)}
.age-lbl{margin-left:auto;font-size:0.7rem;color:var(--text-muted);white-space:nowrap;background:rgba(255,255,255,0.05);padding:1px 7px;border-radius:4px}
.wr-name{font-size:0.93rem;font-weight:700;color:var(--text);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.wr-ns{font-size:0.73rem;color:var(--text-muted)}
.wr-reason{font-size:0.78rem;color:var(--text-secondary);line-height:1.55;display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden}
.wr-cmd{display:flex;align-items:center;gap:0.5rem;background:rgba(0,0,0,0.3);border:1px solid var(--border);border-radius:var(--radius-xs);padding:0.5rem 0.75rem;margin-top:auto}
.wr-cmd code{font-family:'Consolas','Monaco',monospace;font-size:0.7rem;color:var(--primary-light);flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.copy-btn{background:transparent;border:1px solid var(--border);border-radius:4px;color:var(--text-muted);cursor:pointer;font-size:0.64rem;padding:2px 7px;white-space:nowrap;transition:all 0.15s;flex-shrink:0}
.copy-btn:hover{background:var(--surface-hover);color:var(--text)}
.all-clear-box{background:var(--surface);border:1px solid rgba(16,185,129,0.25);border-radius:var(--radius);padding:2.5rem;text-align:center;color:var(--text-muted)}
.all-clear-box .icon{font-size:2.5rem;margin-bottom:0.5rem}
.footer{text-align:center;padding:1.5rem 0;color:var(--text-muted);font-size:0.75rem;border-top:1px solid var(--border);margin-top:1rem}
@keyframes fadeIn{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:translateY(0)}}
@media(max-width:1024px){.layout{grid-template-columns:1fr}.sidebar{display:none}}
@media(max-width:640px){.wr-grid{grid-template-columns:1fr}.summary-bar{flex-direction:column}}
</style>
</head>
<body>
<div class="layout">
`)

	// ── Sidebar ────────────────────────────────────────────────────────────────
	sb.WriteString(`<aside class="sidebar">
<div class="logo">
  <div class="logo-icon">⚡</div>
  <div><div class="logo-text">OpsCart</div><div class="logo-sub">FinOps Engine</div></div>
</div>
<div class="nav-section">
  <div class="nav-label">Dashboard</div>
  <a class="nav-item" href="` + dashURL + `">📊 Cost Overview</a>
</div>
<div class="nav-section">
  <div class="nav-label">Ops</div>
  <a class="nav-item active" href="` + warRoomURL + `">🚨 War Room</a>
</div>
`)

	if len(clusterList) > 1 {
		sb.WriteString(`<div class="nav-section"><div class="nav-label">Clusters</div>`)
		for _, ctx := range clusterList {
			cls := "nav-item"
			if ctx == activeCtx {
				cls += " active"
			}
			href := "/warroom?" + url.Values{"cluster": {ctx}}.Encode()
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
<div class="cluster-badge"><h4>Scanned</h4><p id="wr-age-sb">just now</p></div>
</div>
</aside>
`, clusterName))

	// ── Main content ───────────────────────────────────────────────────────────
	sb.WriteString(`<main class="main">`)

	totalIssues := len(critical) + len(warnings)
	statusDesc := "No issues detected"
	if totalIssues > 0 {
		statusDesc = fmt.Sprintf("%d issue(s) detected", totalIssues)
	}

	sb.WriteString(fmt.Sprintf(`<div class="page-hdr">
<div>
  <h1>🚨 War Room</h1>
  <p>%s &mdash; %s</p>
</div>
<div class="page-meta">
  <div>Last scanned: <strong id="wr-age">just now</strong></div>
  <a class="back-link" href="%s">← Back to Dashboard</a>
</div>
</div>
`, clusterName, statusDesc, dashURL))

	critChipClass := "ok"
	if len(critical) > 0 {
		critChipClass = "c"
	}
	warnChipClass := "ok"
	if len(warnings) > 0 {
		warnChipClass = "w"
	}
	sb.WriteString(fmt.Sprintf(`<div class="summary-bar">
<div class="summary-chip %s"><div class="summary-num">%d</div><div class="summary-lbl">Critical<br>Issues</div></div>
<div class="summary-chip %s"><div class="summary-num">%d</div><div class="summary-lbl">Warnings</div></div>
</div>
`, critChipClass, len(critical), warnChipClass, len(warnings)))

	// ── Critical section ───────────────────────────────────────────────────────
	sb.WriteString(`<div class="wr-section">`)
	sb.WriteString(fmt.Sprintf(`<div class="wr-section-hdr"><h2>🔴 Critical Issues</h2><span class="cnt-chip c">%d</span></div>`, len(critical)))
	if len(critical) == 0 {
		sb.WriteString(`<div class="all-clear-box"><div class="icon">✅</div><div>No crash-looping, OOMKilled, or ImagePullBackOff pods detected</div></div>`)
	} else {
		sb.WriteString(`<div class="wr-grid">`)
		for _, issue := range critical {
			sb.WriteString(renderWarRoomCard(issue))
		}
		sb.WriteString(`</div>`)
	}
	sb.WriteString(`</div>`)

	// ── Warnings section ───────────────────────────────────────────────────────
	sb.WriteString(`<div class="wr-section">`)
	sb.WriteString(fmt.Sprintf(`<div class="wr-section-hdr"><h2>🟡 Warnings</h2><span class="cnt-chip w">%d</span></div>`, len(warnings)))
	if len(warnings) == 0 {
		sb.WriteString(`<div class="all-clear-box"><div class="icon">✅</div><div>No unprotected namespaces, orphaned PVCs, or zero-replica workloads detected</div></div>`)
	} else {
		sb.WriteString(`<div class="wr-grid">`)
		for _, issue := range warnings {
			sb.WriteString(renderWarRoomCard(issue))
		}
		sb.WriteString(`</div>`)
	}
	sb.WriteString(`</div>`)

	sb.WriteString(`<div class="footer">OpsCart FinOps Engine &middot; War Room &middot; Suggestions only &mdash; verify with owners before acting</div>`)
	sb.WriteString(`</main>`)
	sb.WriteString(`</div>`) // .layout

	sb.WriteString(fmt.Sprintf(`<script>
(function(){
  var TS=%d,el=document.getElementById('wr-age'),sb=document.getElementById('wr-age-sb');
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

// injectWarRoomSidebarLink injects an "Ops → War Room" nav section into the
// existing dashboard sidebar, before the cluster-info block.
func injectWarRoomSidebarLink(html, activeCtx string) string {
	warRoomURL := "/warroom"
	if activeCtx != "" {
		warRoomURL = "/warroom?cluster=" + url.QueryEscape(activeCtx)
	}
	navSection := `<div class="nav-section">` +
		`<div class="nav-label">Ops</div>` +
		`<a class="nav-item" href="` + warRoomURL + `" style="text-decoration:none">🚨 War Room</a>` +
		`</div>`
	return strings.Replace(html, `<div class="cluster-info">`, navSection+`<div class="cluster-info">`, 1)
}

// ── HTML rendering pipeline ───────────────────────────────────────────────────

func renderHTML(scan *clusterScan, activeCtx string, clusterList []string) string {
	html := analyzer.GenerateCloudCostHTML(scan.report)
	html = injectWarRoomSidebarLink(html, activeCtx)
	if len(clusterList) > 1 {
		html = injectClusterSelector(html, clusterList, activeCtx)
	}
	html = injectDashboardEnhancements(html, scan, len(clusterList))
	return injectAutoRefresh(html, scan.report.Timestamp, activeCtx)
}

// injectClusterSelector adds a "Clusters" nav section to the sidebar.
func injectClusterSelector(html string, clusterList []string, activeCtx string) string {
	var sb strings.Builder
	sb.WriteString(`<div class="nav-section">`)
	sb.WriteString(`<div class="nav-label">Clusters</div>`)
	for _, ctx := range clusterList {
		cls := "nav-item"
		if ctx == activeCtx {
			cls += " active"
		}
		href := "/?" + url.Values{"cluster": {ctx}}.Encode()
		label := displayName(ctx)
		if len(label) > 22 {
			label = label[:21] + "…"
		}
		sb.WriteString(fmt.Sprintf(
			`<a class="%s" href="%s" style="text-decoration:none">🔵 %s</a>`,
			cls, href, label,
		))
	}
	sb.WriteString(`</div>`)
	return strings.Replace(html, `<div class="cluster-info">`, sb.String()+`<div class="cluster-info">`, 1)
}

// injectDashboardEnhancements injects:
//  1. An enhanced 6-metric KPI bar (replacing the existing one)
//  2. A two-column grid layout: left = existing cost sections, right = War Room panel
func injectDashboardEnhancements(html string, scan *clusterScan, clusterCount int) string {
	kpiBar := buildEnhancedKPIBar(scan, clusterCount)
	warRoom := buildWarRoomPanel(scan)

	// Hide the original auto-generated kpi-row; inject our replacement + open grid.
	// The left column of the grid contains all existing sections.
	beforeKPI := `<style>.kpi-row{display:none!important}` +
		`.oc-kpi-row{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:1rem;margin-bottom:1.5rem}` +
		`</style>` +
		kpiBar +
		`<div style="display:grid;grid-template-columns:1fr 340px;gap:1.5rem;align-items:start">` +
		`<div>` // open left column

	html = strings.Replace(html, `<div class="kpi-row">`, beforeKPI+`<div class="kpi-row">`, 1)

	// Close left column, inject war room as sticky right column, close grid.
	beforeDisclaimer := `</div>` + // close left column
		`<div style="position:sticky;top:2rem">` + warRoom + `</div>` +
		`</div>` // close grid

	html = strings.Replace(html, `<div class="disclaimer-bar">`, beforeDisclaimer+`<div class="disclaimer-bar">`, 1)
	return html
}

// buildEnhancedKPIBar renders 6 KPI cards: monthly cost, savings, security
// score, waste items, pod count, and cluster count.
func buildEnhancedKPIBar(scan *clusterScan, clusterCount int) string {
	monthlyCost := 0.0
	savings := 0.0
	if scan != nil && scan.report != nil {
		monthlyCost = scan.report.TotalMonthlyCost
		savings = scan.report.TotalSavingsPotential.Best
	}

	cisScore := scan.securityScore()
	wasteItems := scan.wasteTotal()
	pods := scan.monthlyPodCount()

	cisColor := "green"
	cisDisplay := fmt.Sprintf("%d/100", cisScore)
	if cisScore < 0 {
		cisDisplay = "N/A"
		cisColor = "blue"
	} else if cisScore < 60 {
		cisColor = "red"
	} else if cisScore < 80 {
		cisColor = "yellow"
	}

	wasteColor := "green"
	wasteDisplay := fmt.Sprintf("%d", wasteItems)
	if wasteItems < 0 {
		wasteDisplay = "N/A"
		wasteColor = "blue"
	} else if wasteItems > 10 {
		wasteColor = "red"
	} else if wasteItems > 0 {
		wasteColor = "yellow"
	}

	var sb strings.Builder
	sb.WriteString(`<div class="oc-kpi-row">`)

	// Monthly cost
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon red">💰</div></div>
		<h3 class="money red">$%s</h3><p>Monthly Cost</p></div>`, formatMoney(monthlyCost)))

	// Savings potential
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon green">💡</div><span class="kpi-trend down">↓ Save</span></div>
		<h3 class="money green">$%s</h3><p>Savings Potential</p></div>`, formatMoney(savings)))

	// Security score
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon %s">🔐</div></div>
		<h3>%s</h3><p>Security Score</p></div>`, cisColor, cisDisplay))

	// Waste items
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon %s">🗑️</div></div>
		<h3>%s</h3><p>Waste Items</p></div>`, wasteColor, wasteDisplay))

	// Pod count
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon blue">🐳</div></div>
		<h3>%d</h3><p>Running Pods</p></div>`, pods))

	// Cluster count
	sb.WriteString(fmt.Sprintf(`<div class="kpi">
		<div class="kpi-top"><div class="kpi-icon cyan">☸️</div></div>
		<h3>%d</h3><p>Clusters</p></div>`, clusterCount))

	sb.WriteString(`</div>`)
	return sb.String()
}

// buildWarRoomPanel renders the right-side critical issues panel.
func buildWarRoomPanel(scan *clusterScan) string {
	issues := collectWarRoomIssues(scan, 5)

	var sb strings.Builder
	sb.WriteString(`<div class="section" style="border-color:rgba(239,68,68,0.4)">`)
	sb.WriteString(`<div class="section-header">`)
	sb.WriteString(`<div class="section-title">🚨 War Room</div>`)

	if len(issues) > 0 {
		sb.WriteString(fmt.Sprintf(
			`<span style="background:rgba(239,68,68,0.12);color:#ef4444;padding:3px 10px;`+
				`border-radius:20px;font-size:0.72rem;font-weight:700">%d issues</span>`,
			len(issues),
		))
	} else {
		sb.WriteString(`<span style="background:rgba(16,185,129,0.12);color:#10b981;padding:3px 10px;` +
			`border-radius:20px;font-size:0.72rem;font-weight:700">All clear</span>`)
	}
	sb.WriteString(`</div>`) // section-header

	if len(issues) == 0 {
		sb.WriteString(`<div style="text-align:center;padding:2rem 1rem;color:#64748b">` +
			`<div style="font-size:2rem;margin-bottom:0.5rem">✅</div>` +
			`<div style="font-size:0.85rem">No critical issues detected</div>` +
			`</div>`)
	} else {
		for _, issue := range issues {
			issueCardColor, issueBg, badgeBg, badgeText := warRoomColors(issue.Severity)
			typeIcon, typeLabel := warRoomTypeLabel(issue.Type)

			sb.WriteString(fmt.Sprintf(
				`<div style="border:1px solid %s;border-radius:8px;padding:0.75rem;margin-bottom:0.75rem;background:%s">`,
				issueCardColor, issueBg,
			))

			// Header row: severity badge + type
			sb.WriteString(fmt.Sprintf(
				`<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:0.4rem">`+
					`<span style="background:%s;color:%s;padding:2px 8px;border-radius:20px;font-size:0.68rem;font-weight:700">%s</span>`+
					`<span style="font-size:0.7rem;color:#64748b">%s %s</span>`+
					`</div>`,
				badgeBg, badgeText, strings.ToUpper(issue.Severity),
				typeIcon, typeLabel,
			))

			// Resource name
			resource := issue.Resource
			if len(resource) > 28 {
				resource = resource[:27] + "…"
			}
			sb.WriteString(fmt.Sprintf(
				`<div style="font-size:0.82rem;font-weight:600;color:#f1f5f9;margin-bottom:0.2rem">%s</div>`,
				resource,
			))

			// Namespace
			sb.WriteString(fmt.Sprintf(
				`<div style="font-size:0.72rem;color:#64748b;margin-bottom:0.35rem">ns: %s</div>`,
				issue.Namespace,
			))

			// Message (truncated)
			msg := issue.Message
			if len(msg) > 80 {
				msg = msg[:79] + "…"
			}
			sb.WriteString(fmt.Sprintf(
				`<div style="font-size:0.75rem;color:#94a3b8">%s</div>`,
				msg,
			))

			sb.WriteString(`</div>`) // issue card
		}
	}

	sb.WriteString(`</div>`) // section
	return sb.String()
}

func warRoomColors(severity string) (border, bg, badgeBg, badgeText string) {
	switch severity {
	case "critical":
		return "rgba(239,68,68,0.35)", "rgba(239,68,68,0.04)",
			"rgba(239,68,68,0.15)", "#ef4444"
	case "high":
		return "rgba(245,158,11,0.35)", "rgba(245,158,11,0.04)",
			"rgba(245,158,11,0.15)", "#f59e0b"
	default:
		return "rgba(99,102,241,0.35)", "rgba(99,102,241,0.04)",
			"rgba(99,102,241,0.15)", "#818cf8"
	}
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

// collectPageWarRoomIssues returns all critical and warning issues for the full
// War Room page. Unlike collectWarRoomIssues it has no cap and covers all types.
func collectPageWarRoomIssues(scan *clusterScan) (critical, warnings []warRoomIssue) {
	if scan == nil {
		return nil, nil
	}

	// Critical: zombie pods split by actual status
	if scan.wasteAudit != nil {
		for _, pod := range scan.wasteAudit.StalePods {
			if pod.Kind != analyzer.StalePodZombie {
				continue
			}
			itype := zombieTypeForStatus(pod.Status)
			critical = append(critical, warRoomIssue{
				Severity:   "critical",
				Type:       itype,
				Namespace:  pod.Namespace,
				Resource:   pod.Name,
				Message:    pod.Reason,
				AgeDays:    pod.AgeDays,
				KubectlCmd: kubectlCmdForIssue(itype, pod.Name, pod.Namespace),
			})
		}
	}

	// Warnings: unprotected namespaces (all risk levels)
	if scan.netAudit != nil {
		for _, ns := range scan.netAudit.UnprotectedNamespaces {
			warnings = append(warnings, warRoomIssue{
				Severity:   "warning",
				Type:       "unprotected_namespace",
				Namespace:  ns.Name,
				Resource:   "namespace",
				Message:    ns.RiskReason,
				KubectlCmd: fmt.Sprintf("kubectl get networkpolicies -n %s", ns.Name),
			})
		}
	}

	if scan.wasteAudit != nil {
		// Warnings: orphaned PVCs with cost estimate
		for _, pvc := range scan.wasteAudit.OrphanedPVCs {
			msg := pvc.Reason
			if pvc.SizeGB > 0 {
				msg = fmt.Sprintf("%dGB idle (~$%.0f/mo). %s", pvc.SizeGB, float64(pvc.SizeGB)*0.10, pvc.Reason)
			}
			warnings = append(warnings, warRoomIssue{
				Severity:   "warning",
				Type:       "orphaned_pvc",
				Namespace:  pvc.Namespace,
				Resource:   pvc.Name,
				Message:    msg,
				AgeDays:    pvc.AgeDays,
				KubectlCmd: fmt.Sprintf("kubectl describe pvc %s -n %s", pvc.Name, pvc.Namespace),
			})
		}

		// Warnings: zero-replica workloads
		for _, wrk := range scan.wasteAudit.ZeroReplicaWorkloads {
			kind := strings.ToLower(wrk.Kind)
			warnings = append(warnings, warRoomIssue{
				Severity:   "warning",
				Type:       "zero_replica",
				Namespace:  wrk.Namespace,
				Resource:   wrk.Name,
				Message:    wrk.Reason,
				AgeDays:    wrk.AgeDays,
				KubectlCmd: fmt.Sprintf("kubectl get %s %s -n %s", kind, wrk.Name, wrk.Namespace),
			})
		}
	}

	return critical, warnings
}

// injectAutoRefresh appends the live-update badge and 60s auto-refresh script.
func injectAutoRefresh(html string, scannedAt time.Time, activeCtx string) string {
	refreshURL := "/refresh"
	if activeCtx != "" {
		refreshURL = "/refresh?cluster=" + url.QueryEscape(activeCtx)
	}

	script := fmt.Sprintf(`
<style>
#oc-refresh-badge {
  position:fixed;bottom:1.25rem;right:1.25rem;
  background:#1e293b;border:1px solid #334155;border-radius:10px;
  padding:0.5rem 1rem;
  font-family:'Inter',system-ui,sans-serif;font-size:0.75rem;color:#94a3b8;
  z-index:9999;display:flex;align-items:center;gap:0.5rem;
  box-shadow:0 4px 12px rgba(0,0,0,0.4);transition:border-color 0.3s;
}
#oc-refresh-badge.refreshing{border-color:#6366f1}
#oc-dot{width:8px;height:8px;border-radius:50%%;background:#10b981;flex-shrink:0;transition:background 0.3s}
#oc-refresh-badge.refreshing #oc-dot{background:#6366f1;animation:oc-pulse 1s infinite}
@keyframes oc-pulse{0%%,100%%{opacity:1}50%%{opacity:0.3}}
</style>
<div id="oc-refresh-badge">
  <span id="oc-dot"></span>
  <span>Last updated: <strong id="oc-age">just now</strong></span>
</div>
<script>
(function(){
  var SCAN_TS=%d, REFRESH_URL=%q, REFRESH_INTERVAL=60000;
  var badge=document.getElementById('oc-refresh-badge');
  var ageEl=document.getElementById('oc-age');
  function updateAge(){
    var s=Math.floor((Date.now()-SCAN_TS)/1000);
    if(s<5) ageEl.textContent='just now';
    else if(s<60) ageEl.textContent=s+'s ago';
    else ageEl.textContent=Math.floor(s/60)+'m ago';
  }
  setInterval(updateAge,1000); updateAge();
  function doRefresh(){
    badge.classList.add('refreshing'); ageEl.textContent='refreshing…';
    fetch(REFRESH_URL,{method:'POST'})
      .then(function(){location.reload();})
      .catch(function(){badge.classList.remove('refreshing');updateAge();setTimeout(doRefresh,10000);});
  }
  setTimeout(doRefresh,REFRESH_INTERVAL);
})();
</script>`, scannedAt.UnixMilli(), refreshURL)

	return strings.Replace(html, "</body>", script+"</body>", 1)
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
