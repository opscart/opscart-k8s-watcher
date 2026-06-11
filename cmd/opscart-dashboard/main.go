package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
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
	cluster      string // single-context shorthand (backwards compat)
	clustersFlag string // comma-separated list for multi-cluster sidebar
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
  GET  /                 — HTML dashboard (auto-refreshes every 60s)
  POST /refresh          — trigger an immediate re-scan
  GET  /api/report       — full CloudCostReport as JSON
  GET  /api/overview     — summary KPIs as JSON
  GET  /healthz          — liveness probe

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

// ── Cluster list ────────────────────────────────────────────────────────────

// parseClusterList merges --cluster and --clusters into an ordered, deduplicated
// slice. An empty string means "use the kubeconfig current context".
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

// ── Per-cluster state ────────────────────────────────────────────────────────

type dashboardState struct {
	ctx      string
	mu       sync.RWMutex
	report   *models.CloudCostReport
	htmlPage string   // rendered HTML with sidebar + auto-refresh injected
	scanning atomic.Bool
}

// refresh runs a cluster scan outside of the lock, then atomically swaps the
// result in. A concurrent refresh call while one is in progress is a no-op.
func (s *dashboardState) refresh(clusterList []string) error {
	if !s.scanning.CompareAndSwap(false, true) {
		return nil
	}
	defer s.scanning.Store(false)

	report, err := buildReport(s.ctx)
	if err != nil {
		return err
	}
	page := renderHTML(report, s.ctx, clusterList)

	s.mu.Lock()
	s.report = report
	s.htmlPage = page
	s.mu.Unlock()

	log.Printf("[%s] scan complete — $%.0f/mo", displayName(s.ctx), report.TotalMonthlyCost)
	return nil
}

// ── Server ───────────────────────────────────────────────────────────────────

type server struct {
	clusterList []string
	mu          sync.RWMutex
	states      map[string]*dashboardState
}

func newServer(clusterList []string) *server {
	return &server{
		clusterList: clusterList,
		states:      make(map[string]*dashboardState),
	}
}

// startBackgroundRefresh ticks every interval and re-scans every cluster that
// has already been visited (report != nil). Newly registered clusters are
// lazily scanned on first HTTP request; this goroutine keeps them fresh
// afterwards. The same dashboardState.refresh pipeline used by POST /refresh
// is called here, so the atomic scan flag prevents overlapping scans.
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
			hasData := state.report != nil
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

// getState returns the dashboardState for ctx, creating it if needed.
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

// activeCtx resolves the target cluster from ?cluster= query param, falling
// back to the first cluster in the list.
func (srv *server) activeCtx(r *http.Request) string {
	if ctx := r.URL.Query().Get("cluster"); ctx != "" {
		return ctx
	}
	return srv.clusterList[0]
}

// ── runDashboard ─────────────────────────────────────────────────────────────

func runDashboard(_ *cobra.Command, _ []string) error {
	cl := parseClusterList()
	srv := newServer(cl)

	log.Printf("Scanning cluster %q ...", displayName(cl[0]))
	if err := srv.getState(cl[0]).refresh(cl); err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleDashboard)
	mux.HandleFunc("/refresh", srv.handleRefresh)
	mux.HandleFunc("/api/report", srv.handleReportJSON)
	mux.HandleFunc("/api/overview", srv.handleOverview)
	mux.HandleFunc("/healthz", handleHealth)

	go srv.startBackgroundRefresh(60 * time.Second)

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

	// Lazy scan: cluster was listed in --clusters but not yet scanned.
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
	if err := srv.getState(ctx).refresh(srv.clusterList); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	srv.getState(ctx).mu.RLock()
	report := srv.getState(ctx).report
	srv.getState(ctx).mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"scanned_at":         report.Timestamp.Format(time.RFC3339),
		"total_monthly_cost": report.TotalMonthlyCost,
	})
}

func (srv *server) handleReportJSON(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	report := state.report
	state.mu.RUnlock()

	if report == nil {
		if err := state.refresh(srv.clusterList); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		state.mu.RLock()
		report = state.report
		state.mu.RUnlock()
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

// handleOverview returns a lightweight summary suitable for dashboards and
// monitoring systems that don't need the full CloudCostReport payload.
//
//	GET /api/overview[?cluster=<ctx>]
//	{
//	  "total_monthly_cost": 10371.84,
//	  "node_pool_count":    2,
//	  "namespace_count":    32,
//	  "last_scanned":       "2026-06-11T10:30:00Z"
//	}
func (srv *server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	report := state.report
	state.mu.RUnlock()

	if report == nil {
		http.Error(w, "no data yet — POST /refresh first", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total_monthly_cost": report.TotalMonthlyCost,
		"node_pool_count":    len(report.NodePoolCosts),
		"namespace_count":    len(report.NamespaceCosts),
		"last_scanned":       report.Timestamp.Format(time.RFC3339),
	})
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// ── HTML rendering pipeline ───────────────────────────────────────────────────

func renderHTML(report *models.CloudCostReport, activeCtx string, clusterList []string) string {
	html := analyzer.GenerateCloudCostHTML(report)
	if len(clusterList) > 1 {
		html = injectClusterSelector(html, clusterList, activeCtx)
	}
	return injectAutoRefresh(html, report.Timestamp, activeCtx)
}

// injectClusterSelector adds a "Clusters" nav section to the sidebar so users
// can switch between contexts with a single click. Each item navigates to
// /?cluster=<ctx> (a full-page reload with the target cluster's cached data).
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
		// Truncate long context names so they fit the sidebar width.
		if len(label) > 22 {
			label = label[:21] + "…"
		}
		sb.WriteString(fmt.Sprintf(
			`<a class="%s" href="%s" style="text-decoration:none">🔵 %s</a>`,
			cls, href, label,
		))
	}
	sb.WriteString(`</div>`)

	// Insert just before the cluster-info block at the bottom of the sidebar.
	return strings.Replace(html, `<div class="cluster-info">`, sb.String()+`<div class="cluster-info">`, 1)
}

// injectAutoRefresh appends CSS + JS before </body> that:
//   - renders a fixed "Last updated: X seconds ago" badge (bottom-right)
//   - every 60s calls POST /refresh?cluster=<ctx> then reloads the page
func injectAutoRefresh(html string, scannedAt time.Time, activeCtx string) string {
	// Build the refresh URL: include ?cluster= only when a named context is set.
	refreshURL := "/refresh"
	if activeCtx != "" {
		refreshURL = "/refresh?cluster=" + url.QueryEscape(activeCtx)
	}

	script := fmt.Sprintf(`
<style>
#oc-refresh-badge {
  position: fixed; bottom: 1.25rem; right: 1.25rem;
  background: #1e293b; border: 1px solid #334155; border-radius: 10px;
  padding: 0.5rem 1rem;
  font-family: 'Inter', system-ui, sans-serif; font-size: 0.75rem; color: #94a3b8;
  z-index: 9999; display: flex; align-items: center; gap: 0.5rem;
  box-shadow: 0 4px 12px rgba(0,0,0,0.4); transition: border-color 0.3s;
}
#oc-refresh-badge.refreshing { border-color: #6366f1; }
#oc-dot {
  width: 8px; height: 8px; border-radius: 50%%; background: #10b981;
  flex-shrink: 0; transition: background 0.3s;
}
#oc-refresh-badge.refreshing #oc-dot { background: #6366f1; animation: oc-pulse 1s infinite; }
@keyframes oc-pulse { 0%%,100%%{opacity:1} 50%%{opacity:0.3} }
</style>
<div id="oc-refresh-badge">
  <span id="oc-dot"></span>
  <span>Last updated: <strong id="oc-age">just now</strong></span>
</div>
<script>
(function () {
  var SCAN_TS = %d;
  var REFRESH_URL = %q;
  var REFRESH_INTERVAL = 60000;
  var badge = document.getElementById('oc-refresh-badge');
  var ageEl = document.getElementById('oc-age');

  function updateAge() {
    var secs = Math.floor((Date.now() - SCAN_TS) / 1000);
    if (secs < 5)       ageEl.textContent = 'just now';
    else if (secs < 60) ageEl.textContent = secs + 's ago';
    else                ageEl.textContent = Math.floor(secs / 60) + 'm ago';
  }

  setInterval(updateAge, 1000);
  updateAge();

  function doRefresh() {
    badge.classList.add('refreshing');
    ageEl.textContent = 'refreshing…';
    fetch(REFRESH_URL, { method: 'POST' })
      .then(function () { location.reload(); })
      .catch(function () {
        badge.classList.remove('refreshing');
        updateAge();
        setTimeout(doRefresh, 10000); // retry after 10s on network error
      });
  }

  setTimeout(doRefresh, REFRESH_INTERVAL);
})();
</script>
`, scannedAt.UnixMilli(), refreshURL)

	return strings.Replace(html, "</body>", script+"</body>", 1)
}

// ── Cluster scan ──────────────────────────────────────────────────────────────

func buildReport(ctx string) (*models.CloudCostReport, error) {
	clientset, err := kubeClient(ctx)
	if err != nil {
		return nil, err
	}

	npa := analyzer.NewNodePoolCostAnalyzer(clientset, region)
	poolCosts, _, err := npa.AnalyzeNodePoolCosts()
	if err != nil {
		return nil, fmt.Errorf("analyzing node pools: %w", err)
	}
	totalNodeCost := analyzer.TotalClusterCostFromPools(poolCosts)

	ra := analyzer.NewResourceAnalyzer(clientset)
	resourceAnalysis, err := ra.AnalyzeClusterResources(namespace)
	if err != nil {
		return nil, fmt.Errorf("analyzing resources: %w", err)
	}

	nsCosts := npa.AllocateNamespaceCosts(
		poolCosts,
		resourceAnalysis.Namespaces,
		resourceAnalysis.TotalCPUCores,
		resourceAnalysis.TotalMemoryGB,
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

	return &models.CloudCostReport{
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
	}, nil
}

func kubeClient(ctx string) (*kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// Empty ctx means "use kubeconfig's current context".
	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: ctx}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	cfg, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig for %q: %w", displayName(ctx), err)
	}
	return kubernetes.NewForConfig(cfg)
}
