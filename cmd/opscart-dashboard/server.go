package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awspricing "github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

var getAWSPricingProvider = sync.OnceValue(func() analyzer.PricingProvider {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion("us-east-1"))
	if err != nil {
		log.Printf("AWS pricing configuration unavailable: %v", err)
		return nil
	}
	return analyzer.NewAWSPricingProvider(awspricing.NewFromConfig(awsCfg), 24*time.Hour)
})

func displayName(ctx string) string {
	if ctx == "" {
		return "current-context"
	}
	return ctx
}

// ── Full scan result ──────────────────────────────────────────────────────────

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

// ── Server ────────────────────────────────────────────────────────────────────

type server struct {
	clusterList   []string
	mu            sync.RWMutex
	states        map[string]*dashboardState
	db            store.Store
	retentionDays int
	dbPersistent  bool
	auth          *authConfig
	refreshState  func(*dashboardState, []string) error
	backgroundWG  sync.WaitGroup
	logsEnabled   bool
	kubeClientFor kubeClientFactory
	podLogReader  podLogReaderFunc
}

func newServer(clusterList []string, db store.Store, retentionDays int, dbPersistent bool) *server {
	auth, err := resolveAuthConfig()
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	logAuthConfig(auth)
	return &server{
		clusterList:   clusterList,
		states:        make(map[string]*dashboardState),
		db:            db,
		retentionDays: retentionDays,
		dbPersistent:  dbPersistent,
		auth:          auth,
		logsEnabled:   logPreviewEnabledFromEnv(),
		kubeClientFor: func(ctx string) (kubernetes.Interface, error) { return kubeClient(ctx) },
		podLogReader:  readPodLogs,
		refreshState: func(state *dashboardState, clusters []string) error {
			return state.refresh(clusters)
		},
	}
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
	s = &dashboardState{ctx: ctx, db: srv.db, retentionDays: srv.retentionDays}
	srv.states[ctx] = s
	return s
}

func (srv *server) activeCtx(r *http.Request) string {
	if ctx := r.URL.Query().Get("cluster"); ctx != "" && ctx != displayName("") {
		return ctx
	}
	return srv.clusterList[0]
}

// lastViewedCookieName holds the Unix-seconds timestamp of the last time
// this browser loaded the Overview page, used as the cursor for the
// "what's changed since last scan" feed.
const lastViewedCookieName = "opscart_last_viewed"

// lastViewedMaxAge is how long the cursor cookie persists — long enough to
// survive a browser restart, since it's a "last visit" marker, not a
// session token.
const lastViewedMaxAge = 30 * 24 * time.Hour

// readLastViewedCursor reads the lastViewedCookieName cookie and returns the
// time it encodes. A missing or unparseable cookie (first-ever visit, or a
// tampered/stale value) defaults to 24h ago so first-time visitors see a
// day of history instead of nothing.
func readLastViewedCursor(r *http.Request) time.Time {
	c, err := r.Cookie(lastViewedCookieName)
	if err != nil {
		return time.Now().Add(-24 * time.Hour)
	}
	sec, err := strconv.ParseInt(c.Value, 10, 64)
	if err != nil {
		return time.Now().Add(-24 * time.Hour)
	}
	return time.Unix(sec, 0)
}

func setLastViewedCursor(w http.ResponseWriter, at time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:   lastViewedCookieName,
		Value:  strconv.FormatInt(at.Unix(), 10),
		Path:   "/",
		MaxAge: int(lastViewedMaxAge.Seconds()),
	})
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

	since := readLastViewedCursor(r)
	data := buildOverviewData(scan, ctx, srv.clusterList, srv.db, since)
	if srv.db != nil {
		if earliest, err := store.GetEarliestRetainedObservation(srv.db, ctx); err == nil && !earliest.IsZero() {
			data.EarliestRetainedObservation = earliest.Local().Format("2006-01-02 15:04 MST")
		}
	}
	if srv.retentionDays <= 0 {
		data.ConfiguredRetention = "Forever"
	} else {
		data.ConfiguredRetention = fmt.Sprintf("%d days", srv.retentionDays)
	}
	if srv.dbPersistent {
		data.StorageMode = "Persistent"
	} else {
		data.StorageMode = "Ephemeral"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		log.Printf("overview template: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	// Set the cursor to now only after using the previous value to query
	// changes above — otherwise this request's own changes would never be
	// visible as "new".
	setLastViewedCursor(w, time.Now())

	w.Write([]byte(buf.String()))
}

func (srv *server) newMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleOverviewPage)
	mux.Handle("/static/", http.FileServer(http.FS(staticFS)))
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
	mux.HandleFunc("/investigate", srv.handleInvestigationPage)
	mux.HandleFunc("/api/investigation/logs", srv.handleInvestigationLogs)
	mux.HandleFunc("/incidents", srv.handleIncidentsPage)
	mux.HandleFunc("/security", srv.handleSecurityPage)
	mux.HandleFunc("/waste", srv.handleWastePage)
	mux.HandleFunc("/settings", srv.handleStubPage("settings", "Settings"))

	// /healthz is registered on the unwrapped top-level mux so kubelet
	// liveness/readiness probes succeed without credentials; every other
	// route is served through the authenticated sub-mux.
	top := http.NewServeMux()
	top.HandleFunc("/healthz", srv.handleHealth)
	top.Handle("/", basicAuthMiddleware(srv.auth, mux))
	return top
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

func (srv *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	persistence := "ephemeral"
	if srv.dbPersistent {
		persistence = "persistent"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","persistence":"%s"}`, persistence)
}

type sidebarData struct {
	DashHref      string
	CostsHref     string
	InfraHref     string
	NsHref        string
	OptHref       string
	WasteHref     string
	SecurityHref  string
	IncidentsHref string
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

// calcIncidentScore returns 0-100 score. Lower = worse.
// 100 = perfect, 0 = critical failure.
func calcIncidentScore(scan *clusterScan) (score int, color string, label string) {
	if scan == nil {
		return 100, "green", "No Data"
	}

	penalties := 0

	// War Room issues
	issues := collectWarRoomIssues(scan, 0)
	crashLoops := 0
	imagePulls := 0
	oomKills := 0
	unprotectedNS := 0

	for _, issue := range issues {
		switch issue.Type {
		case "crash_loop":
			crashLoops++
		case "image_pull":
			imagePulls++
		case "oom_killed":
			oomKills++
		case "unprotected_namespace":
			unprotectedNS++
		}
	}

	// Crash loops: -8 each, cap at -40
	crashPenalty := crashLoops * 8
	if crashPenalty > 40 {
		crashPenalty = 40
	}
	penalties += crashPenalty

	// Image pull failures: -5 each, cap at -20
	imagePenalty := imagePulls * 5
	if imagePenalty > 20 {
		imagePenalty = 20
	}
	penalties += imagePenalty

	// OOM kills: -5 each, cap at -15
	oomPenalty := oomKills * 5
	if oomPenalty > 15 {
		oomPenalty = 15
	}
	penalties += oomPenalty

	// Unprotected namespaces: -4 each, cap at -12
	nsPenalty := unprotectedNS * 4
	if nsPenalty > 12 {
		nsPenalty = 12
	}
	penalties += nsPenalty

	// Waste: orphaned PVCs -1 each, cap at -5
	if scan.wasteAudit != nil {
		pvcPenalty := len(scan.wasteAudit.OrphanedPVCs)
		if pvcPenalty > 5 {
			pvcPenalty = 5
		}
		penalties += pvcPenalty
	}

	// CIS security score penalty
	if scan.cisResult != nil {
		if scan.cisResult.Score < 30 {
			penalties += 20
		} else if scan.cisResult.Score < 50 {
			penalties += 10
		} else if scan.cisResult.Score < 70 {
			penalties += 5
		}
	}

	score = 100 - penalties
	if score < 0 {
		score = 0
	}

	// Color + label bands
	switch {
	case score >= 80:
		return score, "green", "Healthy"
	case score >= 60:
		return score, "yellow", "Needs Attention"
	case score >= 40:
		return score, "orange", "Degraded"
	default:
		return score, "red", "Critical"
	}
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
		IncidentsHref: "/incidents" + q,
		SecurityHref:  "/security" + q,
		WasteHref:     "/waste" + q,
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

// ── Overview page (executive summary) ─────────────────────────────────────────

type overviewPageData struct {
	ClusterName  string
	ActiveCtx    string
	ClusterList  []costClusterLink
	DashURL      string
	InfraURL     string
	NSsURL       string
	OptURL       string
	WrURL        string
	CostsURL     string
	ScannedAtMS  int64
	CostsHref    string
	VerdictLine1 string
	VerdictLine2 string

	// Sidebar aliases (matches sidebar.html template)
	DashHref      string
	InfraHref     string
	NsHref        string
	OptHref       string
	WrHref        string
	IncidentsHref string
	SecurityHref  string
	WasteHref     string
	ActivePage    string
	Clusters      []sidebarCluster

	// KPI bar
	CriticalCount                int
	SavingsPotential             float64
	SecurityScore                int
	SecurityColor                string
	WasteCount                   int
	MonthlyCost                  float64
	PrivilegedContainers         int
	NonRootNotExplicitlyEnforced int
	UnprotectedNamespaceCount    int

	// Incident Score
	IncidentScore      int
	IncidentScoreColor string
	IncidentScoreLabel string

	// Top 5 Things to Fix
	TopIssues []topIssue
	Trend     *store.OverviewTrend

	// "If you only fix one thing today" banner + Situation Briefing
	// "Highest Priority" fact — mirrors TopIssues[0] after enrichment.
	HasTopIssue    bool
	TopIssueName   string // resource name, or Namespace for unprotected_namespace/idle_namespace
	TopIssueNS     string
	TopIssueTrend  string
	TopIssueReopen int

	// Trend deltas formatted for template display: "+N"/"-N", or "" when
	// there's no history or no change.
	CostDeltaText          string
	IncidentScoreDeltaText string
	SecurityScoreDeltaText string

	// Memory scoreboard + cluster-wide recent activity
	Scoreboard                  *store.MemoryScoreboard
	RecentEvents                []changeLine
	EarliestRetainedObservation string
	ConfiguredRetention         string
	StorageMode                 string

	// What's changed since this browser last loaded the Overview page
	ChangesSinceLastView []changeLine
	LastViewedLabel      string

	// Namespace Health + Workload Health grid
	NamespaceHealthList []namespaceHealth
	WorkloadHealthGrid  []workloadHealthCell

	// War Room featured issue
	FeaturedIssues []warRoomIssue
	HasFeatured    bool

	// Cluster Summary
	NodePoolCount     int
	PodCount          int
	CPUUtilization    int
	MemUtilization    int
	UtilizationStatus string
	NamespaceCount    int

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
	ButtonLabel string // "Fix now →" | "View all N →" | "View →" (aggregated rows)
	GroupSize   int    // number of underlying pods/incidents this row represents; 0 for aggregated rows

	// Matching key for cross-referencing this row against the incident
	// registry (db.QueryIncidents) in enrichTopIssues. Empty for rows with
	// no single corresponding incident — the aggregated orphaned-PVCs/
	// security-score/zero-replica rows below aren't tied to one resource.
	Namespace string
	Resource  string
	IssueType string

	// Enriched from the incident registry by enrichTopIssues, matched via
	// Namespace+Resource+IssueType above. Zero-valued when no match was
	// found (including for rows with no matching key at all).
	FirstDetectedLabel string
	ReopenCountVal     int // named _Val to read distinctly from store.IncidentSummary.ReopenCount
	TrendVal           string
	MemoryLine         string // "First detected 7d ago · accelerating"
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

func buildOverviewData(scan *clusterScan, activeCtx string, clusterList []string, db store.Store, since time.Time) overviewPageData {

	clusterName := displayName(activeCtx)
	var monthlyCost, savings float64
	var podCount, nsCount, nodePoolCount int
	var cpuUtil, memUtil int
	var wasteCount, securityScore, secFailed int
	var wrIssues []warRoomIssue
	var featuredIssues []warRoomIssue
	var criticalCount int
	var privilegedContainers, runningAsRoot, unprotectedNamespaces int
	if scan != nil {
		wrIssues = collectWarRoomIssues(scan, 0)
		for _, w := range wrIssues {
			if w.Severity == "critical" {
				criticalCount++
				if len(featuredIssues) < 4 {
					featuredIssues = append(featuredIssues, w)
				}
			}
		}
		// If no critical, feature up to 4 highest-severity warnings
		if len(featuredIssues) == 0 && len(wrIssues) > 0 {
			limit := 4
			if len(wrIssues) < limit {
				limit = len(wrIssues)
			}
			featuredIssues = wrIssues[:limit]
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
			privilegedContainers = scan.secAudit.Risks.PrivilegedContainers
			runningAsRoot = scan.secAudit.Risks.RunningAsRoot
		}

		if scan.netAudit != nil {
			unprotectedNamespaces = scan.netAudit.HighRiskNamespaces
		}

		if scan.cisResult != nil {
			securityScore = scan.cisResult.Score
			secFailed = scan.cisResult.FailedChecks
		}

		if scan.wasteAudit != nil {
			wasteCount = scan.wasteAudit.TotalWasteItems
		}
	}
	incidentScore, incidentScoreColor, incidentScoreLabel := calcIncidentScore(scan)
	_ = secFailed // reserved for future use

	topIssues := buildTopIssues(scan, wrIssues, activeCtx)

	var trend *store.OverviewTrend
	var scoreboard *store.MemoryScoreboard
	var recentEvents []changeLine
	var changesSinceLastView []changeLine
	if db != nil {
		if t, err := db.GetOverviewTrend(activeCtx); err == nil {
			trend = t
		}
		if sb, err := db.GetMemoryScoreboard(activeCtx); err == nil {
			scoreboard = sb
		}
		if events, err := db.GetRecentEvents(activeCtx, time.Now().Add(-recentEventsWindow), recentEventsLimit); err == nil {
			recentEvents = formatChangeLines(events)
		}
		if changes, err := db.GetChangesSince(activeCtx, since, overviewRecentEventsLimit); err == nil {
			changesSinceLastView = formatChangeLines(changes)
		}
		topIssues = enrichTopIssues(topIssues, db, activeCtx)
	}
	// Same topIssues slice Top 5 and the fix-one-thing banner render — one
	// source of truth for "the worst issue" (see buildOverviewVerdict).
	// resolvedSinceCursor reuses changesSinceLastView (already fetched via
	// GetChangesSince above) rather than issuing a second query.
	resolvedSinceCursor := countResolvedSince(changesSinceLastView)
	verdictLine1, verdictLine2 := buildOverviewVerdict(topIssues, resolvedSinceCursor, since)
	lastViewedLabel := humanAge(time.Since(since)) + " ago"

	hasTopIssue, topIssueName, topIssueNS, topIssueTrend, topIssueReopen := deriveTopIssueSummary(topIssues)

	var costDeltaText, incidentScoreDeltaText, securityScoreDeltaText string
	if trend != nil {
		costDeltaText = formatCostDelta(trend.CostDelta, trend.HasHistory)
		incidentScoreDeltaText = formatIntDelta(trend.IncidentScore.Current, trend.IncidentScore.Previous, trend.HasHistory)
		securityScoreDeltaText = formatIntDelta(trend.SecurityScore.Current, trend.SecurityScore.Previous, trend.HasHistory)
	}
	if trend == nil {
		log.Printf("overview: trend is nil")
	} else {
		log.Printf("overview: hasHistory=%v incidentScore current=%d previous=%d text=%q",
			trend.HasHistory, trend.IncidentScore.Current, trend.IncidentScore.Previous, incidentScoreDeltaText)
	}

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
		ClusterName:                  clusterName,
		ActiveCtx:                    activeCtx,
		ClusterList:                  clusters,
		DashURL:                      "/" + q,
		InfraURL:                     "/infrastructure" + q,
		NSsURL:                       "/namespaces" + q,
		OptURL:                       "/optimizations" + q,
		WrURL:                        "/warroom" + q,
		CostsURL:                     "/costs" + q,
		ScannedAtMS:                  time.Now().UnixMilli(),
		CriticalCount:                criticalCount,
		SavingsPotential:             savings,
		SecurityScore:                securityScore,
		PrivilegedContainers:         privilegedContainers,
		NonRootNotExplicitlyEnforced: runningAsRoot,
		UnprotectedNamespaceCount:    unprotectedNamespaces,
		WasteCount:                   wasteCount,
		MonthlyCost:                  monthlyCost,
		TopIssues:                    topIssues,
		HasTopIssue:                  hasTopIssue,
		TopIssueName:                 topIssueName,
		TopIssueNS:                   topIssueNS,
		TopIssueTrend:                topIssueTrend,
		TopIssueReopen:               topIssueReopen,
		FeaturedIssues:               featuredIssues,
		HasFeatured:                  len(featuredIssues) > 0,
		NodePoolCount:                nodePoolCount,
		PodCount:                     podCount,
		CPUUtilization:               cpuUtil,
		MemUtilization:               memUtil,
		UtilizationStatus:            utilizationStatus(cpuUtil, memUtil),
		NamespaceCount:               nsCount,
		Version:                      Version,
		DashHref:                     "/" + q,
		CostsHref:                    "/costs" + q,
		InfraHref:                    "/infrastructure" + q,
		NsHref:                       "/namespaces" + q,
		OptHref:                      "/optimizations" + q,
		WrHref:                       "/warroom" + q,
		IncidentsHref:                "/incidents" + q,
		SecurityHref:                 "/security" + q,
		WasteHref:                    "/waste" + q,
		ActivePage:                   "dashboard",
		Clusters:                     convertToSidebarClusters(clusterList, activeCtx, "/"),
		IncidentScore:                incidentScore,
		IncidentScoreColor:           incidentScoreColor,
		IncidentScoreLabel:           incidentScoreLabel,
		Trend:                        trend,

		CostDeltaText:          costDeltaText,
		IncidentScoreDeltaText: incidentScoreDeltaText,
		SecurityScoreDeltaText: securityScoreDeltaText,

		Scoreboard:   scoreboard,
		RecentEvents: recentEvents,
		VerdictLine1: verdictLine1,
		VerdictLine2: verdictLine2,

		ChangesSinceLastView: changesSinceLastView,
		LastViewedLabel:      lastViewedLabel,

		NamespaceHealthList: buildNamespaceHealth(scan),
		WorkloadHealthGrid:  buildWorkloadHealthGrid(scan),
	}
}

// overviewRecentEventsLimit caps the cursor-scoped "what's changed since
// last visit" feed (GetChangesSince).
const overviewRecentEventsLimit = 15

// recentEventsWindow/recentEventsLimit govern the cluster-wide Recent
// Events feed (GetRecentEvents) — a fixed 7-day lookback independent of
// the opscart_last_viewed cursor, so it reliably shows more/older activity
// than the cursor-scoped ChangesSinceLastView even on a quiet day. Capped
// higher than overviewRecentEventsLimit since a real week of activity
// needs more rows to feel substantive than a single visit's worth of change.
const recentEventsWindow = 7 * 24 * time.Hour
const recentEventsLimit = 20

// formatIntDelta renders the change between two integer metric readings as
// "+N"/"-N" for template display. Returns "" when there's no history to
// compare against or the value hasn't changed, so the template never claims
// a change that didn't happen.
func formatIntDelta(current, previous int, hasHistory bool) string {
	if !hasHistory || current == previous {
		return ""
	}
	if current > previous {
		return fmt.Sprintf("+%d", current-previous)
	}
	return fmt.Sprintf("-%d", previous-current)
}

// formatCostDelta renders a monthly-cost delta as "+$45"/"-$12" for template
// display. Returns "" when there's no history to compare against or the
// cost hasn't changed.
func formatCostDelta(delta float64, hasHistory bool) string {
	if !hasHistory || delta == 0 {
		return ""
	}
	if delta > 0 {
		return fmt.Sprintf("+$%.0f", delta)
	}
	return fmt.Sprintf("-$%.0f", -delta)
}

// countResolvedSince counts changesSinceLastView entries representing a
// Resolved event, for the verdict's "N incidents resolved ..." clause.
// Reuses the changeLine slice buildOverviewData already fetched via
// GetChangesSince — never a second query.
func countResolvedSince(changes []changeLine) int {
	count := 0
	for _, c := range changes {
		if c.EventReason == "Resolved" {
			count++
		}
	}
	return count
}

// sinceCursorPhrase anchors the verdict's resolved-since clause to how old
// the visitor's opscart_last_viewed cursor actually is. "since yesterday"
// only reads honestly when the cursor is roughly a day old — the default
// for a first-time visitor (readLastViewedCursor: now-24h) and for anyone
// who checks back about once a day. A cursor meaningfully more recent
// (checking every few minutes) or older (checking every few weeks) uses
// the always-accurate "since your last visit" instead.
func sinceCursorPhrase(elapsed time.Duration) string {
	if elapsed >= 18*time.Hour && elapsed <= 36*time.Hour {
		return "since yesterday"
	}
	return "since your last visit"
}

// buildOverviewVerdict produces the two-sentence cluster assessment shown
// at the top of the Overview page. line1's priority: accelerating >
// reopened > stable-but-critical > all-clear, built from topIssues[0] —
// the SAME row the "if you only fix one thing today" banner and the Top 5
// list's first entry already show, so there's exactly one source of truth
// for "the worst issue" (previously this ran its own independent
// db.QueryIncidents call and could disagree with what Top 5 displayed).
// line2 is the resolved-since-cursor clause, or "" if nothing resolved.
func buildOverviewVerdict(topIssues []topIssue, resolvedSinceCursor int, cursorSince time.Time) (line1, line2 string) {
	if resolvedSinceCursor > 0 {
		line2 = fmt.Sprintf("%d incident%s resolved %s.",
			resolvedSinceCursor, pluralS(resolvedSinceCursor), sinceCursorPhrase(time.Since(cursorSince)))
	}

	if len(topIssues) == 0 {
		return "No active incidents detected.", line2
	}

	worst := topIssues[0]

	// Aggregated rows (orphaned PVCs, security score, zero-replica
	// workloads) have no single Namespace/Resource/IssueType to build a
	// resource/trend sentence from — fall back to the row's own title,
	// which is always meaningful on its own.
	if worst.Namespace == "" || worst.Resource == "" || worst.IssueType == "" {
		return fmt.Sprintf("Cluster needs attention: %s.", worst.Title), line2
	}

	total := worst.GroupSize
	if total < 1 {
		total = 1
	}
	workloadWord := "workload has an active incident"
	if total > 1 {
		workloadWord = "workloads have active incidents"
	}

	issueLabel := humanizeIssueType(worst.IssueType)

	// unprotected_namespace/idle_namespace incidents store the literal
	// string "namespace" as Resource (there's no pod/deployment involved);
	// the incident's real identifying context is its Namespace.
	resourceLabel := topIssueResourceLabel(worst.Resource, worst.Namespace, worst.IssueType)

	firstDetected := worst.FirstDetectedLabel
	if firstDetected == "" {
		firstDetected = "recently"
	}

	switch {
	case worst.TrendVal == "accelerating":
		line1 = fmt.Sprintf(
			"%d %s. %s has been %s, first detected %s, and its restart rate is accelerating.",
			total, workloadWord, resourceLabel, issueLabel, firstDetected)
	default:
		line1 = fmt.Sprintf(
			"%d %s. %s has been %s, first detected %s.",
			total, workloadWord, resourceLabel, issueLabel, firstDetected)
	}

	return line1, line2
}

func humanizeIssueType(issueType string) string {
	switch issueType {
	case "crash_loop":
		return "crash-looping"
	case "probe_failure":
		return "failing its startup/liveness probe"
	case "image_pull_backoff":
		return "unable to pull its image"
	case "oomkilled":
		return "being OOM-killed"
	case "privileged_container":
		return "running privileged"
	case "unprotected_namespace", "idle_namespace":
		return "flagged"
	default:
		return "failing"
	}
}

// Recent Events feeds: a short human phrase plus the EventReason driving
// its colored dot (reusing the exact Detected/Resolved/Reopened/
// RestartMilestone/SeverityChanged categories and casing the CSS already
// defines for .change-dot — see overview.html).
type changeLine struct {
	Phrase      string
	EventReason string
	OccurredAt  time.Time
}

// formatChangeLine turns a raw store.RecentEvent — which carries only
// Resource/EventReason/OccurredAt, nothing else — into a short readable
// phrase. Some illustrative designs for this feed show extra detail (a
// restart-rate percentage, an incident's total active duration, a trend
// judgment); none of that is available on RecentEvent without an extra
// per-event query, which would be an N+1 pattern this function
// deliberately avoids — so those clauses are omitted rather than
// fabricated.
func formatChangeLine(e store.RecentEvent) changeLine {
	var phrase string
	switch e.EventReason {
	case "Detected":
		phrase = fmt.Sprintf("New: %s", e.Resource)
	case "Resolved":
		phrase = fmt.Sprintf("%s recovered", e.Resource)
	case "Reopened":
		phrase = fmt.Sprintf("%s reopened", e.Resource)
	case "RestartMilestone":
		// Not "...restart rate accelerating" — RecentEvent carries no
		// restart count or trend, so there's nothing to justify a trend
		// judgment. "Milestone reached" is the honest description of what
		// this event actually records (see insertIncidentEvent's
		// RestartMilestone case in pkg/store/sqlite.go).
		phrase = fmt.Sprintf("%s restart milestone reached", e.Resource)
	case "SeverityChanged":
		phrase = fmt.Sprintf("%s severity changed", e.Resource)
	default:
		phrase = e.Resource
	}
	return changeLine{Phrase: phrase, EventReason: e.EventReason, OccurredAt: e.OccurredAt}
}

// formatChangeLines maps formatChangeLine over a whole feed.
func formatChangeLines(events []store.RecentEvent) []changeLine {
	out := make([]changeLine, 0, len(events))

	// Collapse consecutive RestartMilestone events for the same resource
	// into a single row. A pod crossing several thresholds in a row
	// (10 → 50 → 100 …) is one story, not four — without this, a single
	// noisy workload crowds every other event out of the feed.
	var lastResource, lastReason string
	milestoneRun := 0

	flush := func() {
		if milestoneRun > 1 {
			// Rewrite the most recent milestone row to note the run.
			last := &out[len(out)-1]
			last.Phrase = fmt.Sprintf("%s (%d restart milestones)", last.Phrase, milestoneRun)
		}
		milestoneRun = 0
	}

	for _, e := range events {
		if e.EventReason == "RestartMilestone" && e.Resource == lastResource && lastReason == "RestartMilestone" {
			milestoneRun++
			continue // skip — folded into the row already emitted
		}
		flush()

		out = append(out, formatChangeLine(e))
		lastResource, lastReason = e.Resource, e.EventReason
		if e.EventReason == "RestartMilestone" {
			milestoneRun = 1
		}
	}
	flush()

	return out
}

// ── Namespace Health + Workload Health grid ────────────────────────────────

// namespaceHealth is one row of the Namespace Health panel: how many pods
// in a namespace are healthy vs. total.
type namespaceHealth struct {
	Name  string
	Ready int
	Total int
}

// workloadHealthCell is one cell of the Workload Health grid: a
// Deployment/StatefulSet/DaemonSet colored by whether it currently has an
// active incident (per collectWarRoomIssues) and at what severity.
type workloadHealthCell struct {
	Name     string
	Severity string // "critical" | "high" | "medium" | "" (empty = healthy)
}

// workloadSeverityRank orders workloadHealthCell.Severity values from most
// to least urgent; "" (healthy) always ranks last.
var workloadSeverityRank = map[string]int{"critical": 3, "high": 2, "medium": 1}

// buildNamespaceHealth groups pods by namespace using the same per-namespace
// pod counts already computed for cost allocation (scan.report.NamespaceCosts
// — populated on every scan, no new clientset call), and treats pods the
// waste auditor already flagged as crash-looping/OOMKilled zombies
// (scan.wasteAudit.StalePods) as not-ready. Sorted worst-health first,
// matching the risk-first / pod-count-descending convention used for
// NetworkPolicyAudit.UnprotectedNamespaces.
func buildNamespaceHealth(scan *clusterScan) []namespaceHealth {
	if scan == nil || scan.report == nil {
		return nil
	}

	unhealthyByNS := map[string]int{}
	if scan.wasteAudit != nil {
		for _, pod := range scan.wasteAudit.StalePods {
			if pod.Kind == analyzer.StalePodZombie {
				unhealthyByNS[pod.Namespace]++
			}
		}
	}

	list := make([]namespaceHealth, 0, len(scan.report.NamespaceCosts))
	for _, ns := range scan.report.NamespaceCosts {
		total := ns.PodCount
		unhealthy := unhealthyByNS[ns.Name]
		if unhealthy > total {
			unhealthy = total
		}
		list = append(list, namespaceHealth{Name: ns.Name, Ready: total - unhealthy, Total: total})
	}

	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := namespaceHealthRatio(list[i]), namespaceHealthRatio(list[j])
		if ri != rj {
			return ri < rj // worst health (lowest ready/total) first
		}
		return list[i].Total > list[j].Total
	})
	return list
}

// namespaceHealthRatio returns Ready/Total, treating an empty namespace
// (Total == 0) as perfectly healthy so it sorts after any namespace with
// at least one unhealthy pod.
func namespaceHealthRatio(n namespaceHealth) float64 {
	if n.Total == 0 {
		return 1
	}
	return float64(n.Ready) / float64(n.Total)
}

// buildWorkloadHealthGrid maps every workload the scan currently knows
// about to a severity, by reusing collectWarRoomIssues' output — never a
// second incident query. Workload identity for a pod-level issue is
// resolved with store.OwnerNameFromPod (pure string parsing, matching the
// same owner-name convention used for incident fingerprints, not a new
// clientset call). The workload universe itself comes from scan.AllWorkloads
// — every Deployment/StatefulSet/DaemonSet the scan observed while
// enumerating pods for cost allocation, retained regardless of the
// --breakdown flag — so healthy workloads with no active issue are always
// represented, not just the ones an issue happens to name.
func buildWorkloadHealthGrid(scan *clusterScan) []workloadHealthCell {
	if scan == nil {
		return nil
	}

	type workloadKey struct{ namespace, name string }
	severityOf := map[workloadKey]string{}

	for _, issue := range collectWarRoomIssues(scan, 0) {
		if issue.Namespace == "" || issue.Resource == "" || issue.Resource == "namespace" {
			continue // namespace-level issues (e.g. unprotected_namespace) aren't workloads
		}
		key := workloadKey{namespace: issue.Namespace, name: store.OwnerNameFromPod(issue.Resource)}
		if existing, ok := severityOf[key]; !ok || workloadSeverityRank[issue.Severity] > workloadSeverityRank[existing] {
			severityOf[key] = issue.Severity
		}
	}

	seen := map[workloadKey]bool{}
	var grid []workloadHealthCell

	for _, w := range scan.AllWorkloads {
		key := workloadKey{namespace: w.Namespace, name: w.Name}
		if seen[key] {
			continue
		}
		seen[key] = true
		grid = append(grid, workloadHealthCell{Name: w.Name, Severity: severityOf[key]})
	}

	// Always include workloads implied by an active issue, even when
	// AllWorkloads didn't independently observe them (e.g. a test fixture
	// or a partial scan).
	for key, sev := range severityOf {
		if seen[key] {
			continue
		}
		seen[key] = true
		grid = append(grid, workloadHealthCell{Name: key.name, Severity: sev})
	}

	sort.SliceStable(grid, func(i, j int) bool {
		si, sj := workloadSeverityRank[grid[i].Severity], workloadSeverityRank[grid[j].Severity]
		if si != sj {
			return si > sj // most severe first
		}
		return grid[i].Name < grid[j].Name
	})
	return grid
}

// investigateURL builds a deep link to the Investigation page for a single
// incident, matching the exact query params handleInvestigationPage reads
// (pod, ns, type, cluster, from). Falls back to "/warroom" when namespace/
// resource/issueType aren't all known — the aggregated rows built later in
// buildTopIssues (orphaned PVCs, security score, zero-replica workloads)
// have no single corresponding incident to deep-link to.
func investigateURL(namespace, resource, issueType, activeCtx string) string {
	if namespace == "" || issueType == "" {
		return "/warroom"
	}
	clusterParam := ""
	if activeCtx != "" {
		clusterParam = "&cluster=" + url.QueryEscape(activeCtx)
	}
	if issueType == "unprotected_namespace" || issueType == "idle_namespace" {
		return fmt.Sprintf("/investigate?ns=%s&type=%s%s&from=warroom",
			url.QueryEscape(namespace), url.QueryEscape(issueType), clusterParam)
	}
	if resource == "" {
		return "/warroom"
	}
	return fmt.Sprintf("/investigate?pod=%s&ns=%s&type=%s%s&from=warroom",
		url.QueryEscape(resource), url.QueryEscape(namespace),
		url.QueryEscape(issueType), clusterParam)
}

func buildTopIssues(scan *clusterScan, wrIssues []warRoomIssue, activeCtx string) []topIssue {
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

		// grp preserves collectWarRoomIssues' severity/restart-sorted
		// order, so grp[0] is this group's highest-priority representative
		// — the natural incident to cross-reference in enrichTopIssues. A
		// group of exactly one pod deep-links straight to that pod's
		// Investigation page; a group of several pods deep-links to the
		// Incidents registry filtered to this issue type instead of
		// arbitrarily picking grp[0]'s pod as "the" incident.
		issueURL := investigateURL(grp[0].Namespace, grp[0].Resource, k.issueTyp, activeCtx)
		buttonLabel := "Fix now →"
		if count > 1 {
			issueURL = fmt.Sprintf("/incidents?cluster=%s&type=%s&status=active",
				url.QueryEscape(activeCtx), url.QueryEscape(k.issueTyp))
			buttonLabel = fmt.Sprintf("View all %d →", count)
		}

		issues = append(issues, topIssue{
			Title:       title,
			Subtitle:    subtitle,
			Action:      action,
			Severity:    k.severity,
			SeverityLbl: sevLbl,
			CountText:   countText,
			URL:         issueURL,
			ButtonLabel: buttonLabel,
			GroupSize:   count,
			Namespace:   grp[0].Namespace,
			Resource:    grp[0].Resource,
			IssueType:   k.issueTyp,
		})
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
				GroupSize:   len(wa.OrphanedPVCs),
				ButtonLabel: fmt.Sprintf("View all %d →", len(wa.OrphanedPVCs)),
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
			ButtonLabel: "View →",
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
			ButtonLabel: "View →",
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

// postureOnlyIssueTypes are issue types with no restart-based trajectory —
// static findings rather than something that "accelerates" — so
// buildMemoryLine omits the trend clause for them.
var postureOnlyIssueTypes = map[string]bool{
	"privileged_container":  true,
	"unprotected_namespace": true,
	"idle_namespace":        true,
}

// enrichTopIssues cross-references each topIssue's matching key
// (Namespace+Resource+IssueType, set in buildTopIssues) against the active
// incident registry, filling in FirstDetectedLabel/ReopenCountVal/TrendVal/
// MemoryLine for rows with a match. Rows with no matching key (the
// aggregated orphaned-PVCs/security-score/zero-replica rows) or no
// corresponding active incident are left at their zero values.
func enrichTopIssues(issues []topIssue, db store.Store, cluster string) []topIssue {
	if db == nil || len(issues) == 0 {
		return issues
	}
	items, _, err := db.QueryIncidents(store.IncidentFilter{
		Cluster: cluster,
		Status:  "active",
		PerPage: 200,
	})
	if err != nil || len(items) == 0 {
		return issues
	}

	type matchKey struct{ namespace, resource, issueType string }
	byKey := make(map[matchKey]store.IncidentSummary, len(items))
	for _, it := range items {
		byKey[matchKey{it.Namespace, it.Resource, it.IssueType}] = it
	}

	for i := range issues {
		if issues[i].Namespace == "" || issues[i].Resource == "" || issues[i].IssueType == "" {
			continue
		}
		match, ok := byKey[matchKey{issues[i].Namespace, issues[i].Resource, issues[i].IssueType}]
		if !ok {
			continue
		}
		issues[i].FirstDetectedLabel = humanAge(time.Since(match.FirstSeen)) + " ago"
		issues[i].ReopenCountVal = match.ReopenCount
		issues[i].TrendVal = match.Trend
		issues[i].MemoryLine = buildMemoryLine(issues[i].FirstDetectedLabel, match.ReopenCount, match.Trend, issues[i].IssueType)
	}
	return issues
}

// buildMemoryLine composes the small muted operational-memory line.
// The trend clause is omitted for postureOnlyIssueTypes, where a
// restart-based trend doesn't apply.
func buildMemoryLine(firstDetectedLabel string, _ int, trend, issueType string) string {
	parts := []string{"First detected " + firstDetectedLabel}
	if trend == "accelerating" && !postureOnlyIssueTypes[issueType] {
		parts = append(parts, "accelerating")
	}
	return strings.Join(parts, " · ")
}

func utilizationStatus(cpuPercent, memoryPercent int) string {
	highest := cpuPercent
	if memoryPercent > highest {
		highest = memoryPercent
	}
	switch {
	case highest >= 90:
		return "Critical"
	case highest >= 80:
		return "High"
	case highest >= 60:
		return "Elevated"
	default:
		return "Nominal"
	}
}

// topIssueResourceLabel mirrors buildOverviewVerdict's resourceLabel
// substitution: unprotected_namespace/idle_namespace incidents store the
// literal string "namespace" as Resource, so the real identifying context
// is the incident's Namespace instead.
func topIssueResourceLabel(resource, namespace, issueType string) string {
	if issueType == "unprotected_namespace" || issueType == "idle_namespace" {
		return namespace
	}
	return resource
}

// deriveTopIssueSummary promotes TopIssues[0] (after enrichTopIssues) into
// the overviewPageData fields the "if you only fix one thing today" banner
// and the Situation Briefing's "Highest Priority" fact use — same data,
// rendered a second time, not a separate computation. Returns the zero
// value for every field when issues is empty.
func deriveTopIssueSummary(issues []topIssue) (has bool, name, namespace, trend string, reopenCount int) {
	if len(issues) == 0 {
		return false, "", "", "", 0
	}
	top := issues[0]
	return true, topIssueResourceLabel(top.Resource, top.Namespace, top.IssueType), top.Namespace, top.TrendVal, top.ReopenCountVal
}

type costClusterLink struct {
	Ctx      string
	Label    string
	Href     string
	IsActive bool
}

var getOverviewTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("overview.html").
			Funcs(template.FuncMap{
				"money":     formatMoney,
				"hasPrefix": strings.HasPrefix,
				"sparkHeight": func(score int) int {
					if score <= 0 {
						return 4
					}
					if score >= 100 {
						return 32
					}
					return 3 + (score * 28 / 100)
				},
				// eventAge renders a RecentEvent/ChangesSinceLastView
				// timestamp the same way incidents.html's firstDetected/
				// lastSeen render IncidentSummary timestamps.
				"eventAge": func(t time.Time) string {
					if t.IsZero() {
						return "—"
					}
					d := time.Since(t)
					switch {
					case d < time.Minute:
						return "just now"
					case d < time.Hour:
						return fmt.Sprintf("%dm ago", int(d.Minutes()))
					case d < 24*time.Hour:
						return fmt.Sprintf("%dh ago", int(d.Hours()))
					case d < 48*time.Hour:
						return "yesterday"
					default:
						return fmt.Sprintf("%d days ago", int(d.Hours()/24))
					}
				},
				// healthColor classifies a namespaceHealth ready/total pair
				// into "ok"/"warn"/"danger" for badge coloring.
				"healthColor": func(ready, total int) string {
					if total == 0 {
						return "ok"
					}
					ratio := float64(ready) / float64(total)
					switch {
					case ratio >= 1:
						return "ok"
					case ratio >= 0.5:
						return "warn"
					default:
						return "danger"
					}
				},
				// limitSlice caps a changeLine feed's DISPLAY length —
				// GetRecentEvents/GetChangesSince already cap what's
				// fetched (overviewRecentEventsLimit); this only trims how
				// many of those rows the page renders.
				"limitSlice": func(items []changeLine, n int) []changeLine {
					if len(items) <= n {
						return items
					}
					return items[:n]
				},
				// limitNamespaceHealth/limitWorkloadHealth cap the two
				// list-based cards in the Cluster/Namespace Health row the
				// same way limitSlice caps the feed cards above. Go's
				// text/template FuncMap dispatches by exact reflected
				// argument type, so limitSlice's []store.RecentEvent
				// signature can't be reused for these — same pattern,
				// separate typed helpers.
				"limitNamespaceHealth": func(items []namespaceHealth, n int) []namespaceHealth {
					if len(items) <= n {
						return items
					}
					return items[:n]
				},
				"limitWorkloadHealth": func(items []workloadHealthCell, n int) []workloadHealthCell {
					if len(items) <= n {
						return items
					}
					return items[:n]
				},
			}).
			ParseFS(templateFS, "templates/base.html", "templates/sidebar.html", "templates/overview.html"),
	)
})

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

func warRoomTypeLabel(t string) (iconClass, label string) {
	switch t {
	case "crash_loop":
		return "wri-crash", "CrashLoop"
	case "oomkilled":
		return "wri-oom", "OOMKilled"
	case "image_pull_backoff":
		return "wri-image", "ImagePullBackOff"
	case "privileged_container":
		return "wri-priv", "Privileged"
	case "unprotected_namespace":
		return "wri-netpol", "No NetPolicy"
	case "orphaned_pvc":
		return "wri-pvc", "Orphaned PVC"
	case "zero_replica":
		return "wri-zero", "Zero Replicas"
	default:
		return "wri-default", t
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
	providerOverride, _ := analyzer.ParseCloudProviderOverride(cloudProvider)
	npCostAnalyzer.SetCloudProviderOverride(providerOverride)
	if pricingSource == "aws-api" {
		npCostAnalyzer.SetPricingProvider(getAWSPricingProvider())
	}
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
	// Retained regardless of --breakdown — this is the same pod enumeration
	// already fetched above, not a second cluster call.
	scan.AllWorkloads = resourceAnalysis.Workloads

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
		detectedRegion = npCostAnalyzer.Region()
	}
	if region == "" && npCostAnalyzer.Provider() == analyzer.CloudProviderMixed {
		detectedRegion = "Multiple regions"
	}
	if detectedRegion == "" {
		detectedRegion = "Not detected"
	}
	provider := string(npCostAnalyzer.Provider())
	matchedNodes, totalNodes := 0, 0
	for _, pool := range poolCosts {
		totalNodes += pool.NodeCount
		if pool.PricingAvailable {
			matchedNodes += pool.NodeCount
		}
	}
	coverage := fmt.Sprintf("%d of %d nodes priced", matchedNodes, totalNodes)
	source := "Pricing unavailable"
	switch npCostAnalyzer.Provider() {
	case analyzer.CloudProviderAzure:
		source = analyzer.NewAzurePricingProvider().SourceDescription()
	case analyzer.CloudProviderAWS:
		if pricingSource == "aws-api" {
			source = "AWS Price List Query API public EC2 On-Demand pricing"
		}
	case analyzer.CloudProviderMixed:
		source = "Provider-specific sources; unsupported nodes remain unavailable"
	}
	if npCostAnalyzer.Provider() != analyzer.CloudProviderAzure {
		costEstimate.OptimizationScenarios = nil
		costEstimate.TotalSavingsPotential = models.CostRange{}
	}
	exclusions := providerScopeExclusions(npCostAnalyzer.Provider())

	scan.report = &models.CloudCostReport{
		Timestamp:             time.Now(),
		ClusterName:           displayName(ctx),
		Region:                detectedRegion,
		Provider:              provider,
		DetectedProvider:      string(npCostAnalyzer.DetectedProvider()),
		EffectiveProvider:     provider,
		ProviderDetectionMode: npCostAnalyzer.ProviderDetectionMode(),
		ProviderWarning:       npCostAnalyzer.ProviderWarning(),
		NodePoolCosts:         poolCosts,
		TotalNodeCost:         totalNodeCost,
		NamespaceCosts:        nsCosts,
		TotalMonthlyCost:      totalNodeCost,
		TotalAnnualCost:       totalNodeCost * 12,
		CostBreakdown:         models.CostBreakdown{Compute: totalNodeCost},
		OptimizationScenarios: costEstimate.OptimizationScenarios,
		TotalSavingsPotential: costEstimate.TotalSavingsPotential,
		PricingSource:         source,
		PricingCoverage:       coverage,
		PricingWarnings:       pricingCoverageWarnings(npCostAnalyzer.PricingWarnings(), matchedNodes, totalNodes),
		Currency:              "USD",
		ScopeExclusions:       exclusions,
		LastPriceRefresh:      npCostAnalyzer.LastPriceRefresh(),
		Assumptions:           []string{"Cost allocation uses a weighted average of CPU and memory requests."},
		Disclaimers:           []string{"Public/list pricing estimates are not invoice values."},
	}

	// ── 2. Security audit (best effort) ──────────────────────────────
	sa := analyzer.NewSecurityAuditor(clientset)
	if secAudit, err := sa.AuditClusterSecurity(""); err == nil {
		scan.secAudit = secAudit
	} else {
		log.Printf("[%s] security audit skipped: %v", displayName(ctx), err)
	}

	// ── 3. Waste audit (best effort) ──────────────────────────────────
	wasteAuditor, cancel := analyzer.NewWasteAuditor(clientset, dashboardWasteMinAgeDays)
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

func providerScopeExclusions(provider analyzer.CloudProvider) []string {
	switch provider {
	case analyzer.CloudProviderAWS:
		return []string{
			"Includes only matched EC2 Linux worker-node On-Demand compute at public USD list prices.",
			"Excludes EKS control-plane charges, EBS volumes and snapshots, load balancers, NAT gateways, data transfer, support charges, and taxes.",
			"Excludes account-specific discounts, Savings Plans, and Reserved Instance commitments. Spot nodes are detected but excluded until Spot pricing is integrated.",
		}
	case analyzer.CloudProviderAzure:
		return []string{
			"Includes worker-node VM compute from the embedded Azure public pricing catalog.",
			"Excludes storage, network egress, monitoring, security services, taxes, and negotiated account pricing.",
		}
	case analyzer.CloudProviderMixed:
		return []string{"Each node is priced only by its detected provider; unsupported or disabled provider pricing is excluded from totals."}
	default:
		return []string{"No supported provider evidence was detected, so worker-node pricing is unavailable and no Azure fallback was applied."}
	}
}

func pricingCoverageWarnings(warnings []string, matched, total int) []string {
	result := append([]string(nil), warnings...)
	if matched < total {
		result = append(result, fmt.Sprintf("Estimate includes only %d of %d nodes with resolved pricing.", matched, total))
	}
	return result
}

func kubeClient(ctx string) (*kubernetes.Clientset, error) {
	// In-cluster deployments have no context name and typically no
	// kubeconfig file at all — authenticate via the mounted ServiceAccount
	// instead of trying to resolve a named context.
	if ctx == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			cfg.QPS = 50
			cfg.Burst = 100
			return kubernetes.NewForConfig(cfg)
		}
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: ctx}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	cfg, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig for %q: %w", displayName(ctx), err)
	}
	// Default client-go limits (QPS 5, Burst 10) cause multi-second scans and
	// skipped sub-scans ("context deadline exceeded") on large clusters.
	cfg.QPS = 50
	cfg.Burst = 100
	return kubernetes.NewForConfig(cfg)
}
