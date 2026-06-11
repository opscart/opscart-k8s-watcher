package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	port      string
	cluster   string
	region    string
	breakdown string
	namespace string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "opscart-dashboard",
		Short: "OpsCart cloud cost dashboard server",
		Long: `Serves a live cloud cost FinOps dashboard for Kubernetes clusters.

Routes:
  GET  /           — HTML dashboard (auto-refreshes every 60s)
  POST /refresh    — trigger an immediate re-scan
  GET  /api/report — current CloudCostReport as JSON
  GET  /healthz    — liveness probe`,
		RunE: runDashboard,
	}

	rootCmd.Flags().StringVarP(&port, "port", "p", "8080", "Port to listen on")
	rootCmd.Flags().StringVarP(&cluster, "cluster", "c", "", "Kubernetes context to scan (default: current context)")
	rootCmd.Flags().StringVar(&region, "region", "", "Azure region for pricing (auto-detected from node labels if empty)")
	rootCmd.Flags().StringVar(&breakdown, "breakdown", "", "Cost breakdown level: '' or 'deployment'")
	rootCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace to analyze (default: all)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// dashboardState holds the cached report and rendered HTML under a read-write lock.
// scan is an atomic flag that prevents concurrent re-scans from piling up.
type dashboardState struct {
	mu         sync.RWMutex
	report     *models.CloudCostReport
	cachedHTML string
	scanning   atomic.Bool
}

// refresh runs a fresh cluster scan outside of the lock, then swaps the result in.
// If a scan is already running the call is a no-op (returns nil).
func (s *dashboardState) refresh() error {
	if !s.scanning.CompareAndSwap(false, true) {
		return nil
	}
	defer s.scanning.Store(false)

	report, err := buildReport()
	if err != nil {
		return err
	}
	html := injectAutoRefresh(analyzer.GenerateCloudCostHTML(report), report.Timestamp)

	s.mu.Lock()
	s.report = report
	s.cachedHTML = html
	s.mu.Unlock()

	log.Printf("Scan complete — monthly estimate $%.0f", report.TotalMonthlyCost)
	return nil
}

func runDashboard(_ *cobra.Command, _ []string) error {
	log.Printf("Scanning cluster %q ...", resolveClusterName())
	state := &dashboardState{}
	if err := state.refresh(); err != nil {
		return fmt.Errorf("initial cluster scan: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", state.handleDashboard)
	mux.HandleFunc("/refresh", state.handleRefresh)
	mux.HandleFunc("/api/report", state.handleReportJSON)
	mux.HandleFunc("/healthz", handleHealth)

	addr := ":" + port
	log.Printf("Dashboard ready at http://localhost%s", addr)
	return http.ListenAndServe(addr, mux)
}

func (s *dashboardState) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	html := s.cachedHTML
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, html)
}

func (s *dashboardState) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST /refresh", http.StatusMethodNotAllowed)
		return
	}
	if err := s.refresh(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.RLock()
	report := s.report
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"scanned_at":         report.Timestamp.Format(time.RFC3339),
		"total_monthly_cost": report.TotalMonthlyCost,
	})
}

func (s *dashboardState) handleReportJSON(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	report := s.report
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// injectAutoRefresh appends a script block before </body> that:
//   - shows a fixed "Last updated: X seconds ago" badge
//   - auto-refreshes by POST /refresh then location.reload() every 60 seconds
func injectAutoRefresh(html string, scannedAt time.Time) string {
	script := fmt.Sprintf(`
<style>
#oc-refresh-badge {
  position: fixed;
  bottom: 1.25rem;
  right: 1.25rem;
  background: #1e293b;
  border: 1px solid #334155;
  border-radius: 10px;
  padding: 0.5rem 1rem;
  font-family: 'Inter', system-ui, sans-serif;
  font-size: 0.75rem;
  color: #94a3b8;
  z-index: 9999;
  display: flex;
  align-items: center;
  gap: 0.5rem;
  box-shadow: 0 4px 12px rgba(0,0,0,0.4);
  transition: border-color 0.3s;
}
#oc-refresh-badge.refreshing { border-color: #6366f1; }
#oc-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%%;
  background: #10b981;
  flex-shrink: 0;
  transition: background 0.3s;
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
  var REFRESH_INTERVAL = 60000;
  var badge = document.getElementById('oc-refresh-badge');
  var ageEl  = document.getElementById('oc-age');

  function updateAge() {
    var secs = Math.floor((Date.now() - SCAN_TS) / 1000);
    if (secs < 5)        ageEl.textContent = 'just now';
    else if (secs < 60)  ageEl.textContent = secs + 's ago';
    else                 ageEl.textContent = Math.floor(secs / 60) + 'm ago';
  }

  setInterval(updateAge, 1000);
  updateAge();

  function doRefresh() {
    badge.classList.add('refreshing');
    ageEl.textContent = 'refreshing…';
    fetch('/refresh', { method: 'POST' })
      .then(function () { location.reload(); })
      .catch(function () {
        badge.classList.remove('refreshing');
        updateAge();
        setTimeout(doRefresh, 10000);
      });
  }

  setTimeout(doRefresh, REFRESH_INTERVAL);
})();
</script>
`, scannedAt.UnixMilli())

	return strings.Replace(html, "</body>", script+"</body>", 1)
}

func buildReport() (*models.CloudCostReport, error) {
	clientset, err := kubeClient()
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
		ClusterName:           resolveClusterName(),
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

func kubeClient() (*kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: cluster}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	cfg, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

func resolveClusterName() string {
	if cluster != "" {
		return cluster
	}
	return "current-context"
}
