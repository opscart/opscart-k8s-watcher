package main

import (
	"embed"
	"encoding/json"
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
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed templates
var templateFS embed.FS

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
}

func newServer(clusterList []string, db store.Store, retentionDays int, dbPersistent bool) *server {
	auth, err := resolveAuthConfig()
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	logAuthConfig(auth)
	return &server{clusterList: clusterList, states: make(map[string]*dashboardState), db: db, retentionDays: retentionDays, dbPersistent: dbPersistent, auth: auth}
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
	if ctx := r.URL.Query().Get("cluster"); ctx != "" {
		return ctx
	}
	return srv.clusterList[0]
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

	data := buildOverviewData(scan, ctx, srv.clusterList, srv.db)

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

func (srv *server) newMux() http.Handler {
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
	mux.HandleFunc("/investigate", srv.handleInvestigationPage)
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
	CriticalCount    int
	SavingsPotential float64
	SecurityScore    int
	SecurityColor    string
	WasteCount       int
	MonthlyCost      float64

	// Incident Score
	IncidentScore      int
	IncidentScoreColor string
	IncidentScoreLabel string

	// Top 5 Things to Fix
	TopIssues []topIssue
	Trend     *store.OverviewTrend

	// War Room featured issue
	FeaturedIssues []warRoomIssue
	HasFeatured    bool

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

func buildOverviewData(scan *clusterScan, activeCtx string, clusterList []string, db store.Store) overviewPageData {

	clusterName := displayName(activeCtx)
	var monthlyCost, savings float64
	var podCount, nsCount, nodePoolCount int
	var cpuUtil, memUtil int
	var wasteCount, securityScore, secFailed int
	var wrIssues []warRoomIssue
	var featuredIssues []warRoomIssue
	var criticalCount int

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

	var trend *store.OverviewTrend
	if db != nil {
		if t, err := db.GetOverviewTrend(activeCtx); err == nil {
			trend = t
		}
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
		ClusterName:        clusterName,
		ActiveCtx:          activeCtx,
		ClusterList:        clusters,
		DashURL:            "/" + q,
		InfraURL:           "/infrastructure" + q,
		NSsURL:             "/namespaces" + q,
		OptURL:             "/optimizations" + q,
		WrURL:              "/warroom" + q,
		CostsURL:           "/costs" + q,
		ScannedAtMS:        time.Now().UnixMilli(),
		CriticalCount:      criticalCount,
		SavingsPotential:   savings,
		SecurityScore:      securityScore,
		WasteCount:         wasteCount,
		MonthlyCost:        monthlyCost,
		TopIssues:          buildTopIssues(scan, wrIssues),
		FeaturedIssues:     featuredIssues,
		HasFeatured:        len(featuredIssues) > 0,
		NodePoolCount:      nodePoolCount,
		PodCount:           podCount,
		CPUUtilization:     cpuUtil,
		MemUtilization:     memUtil,
		NamespaceCount:     nsCount,
		Version:            "v1.3.0",
		DashHref:           "/" + q,
		CostsHref:          "/costs" + q,
		InfraHref:          "/infrastructure" + q,
		NsHref:             "/namespaces" + q,
		OptHref:            "/optimizations" + q,
		WrHref:             "/warroom" + q,
		IncidentsHref:      "/incidents" + q,
		SecurityHref:       "/security" + q,
		WasteHref:          "/waste" + q,
		ActivePage:         "dashboard",
		Clusters:           convertToSidebarClusters(clusterList, activeCtx, "/"),
		IncidentScore:      incidentScore,
		IncidentScoreColor: incidentScoreColor,
		IncidentScoreLabel: incidentScoreLabel,
		Trend:              trend,
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

var getOverviewTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("overview.html").
			Funcs(template.FuncMap{
				"money": formatMoney,
				"sparkHeight": func(score int) int {
					if score <= 0 {
						return 4
					}
					if score >= 100 {
						return 32
					}
					return 3 + (score * 28 / 100)
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
	// Default client-go limits (QPS 5, Burst 10) cause multi-second scans and
	// skipped sub-scans ("context deadline exceeded") on large clusters.
	cfg.QPS = 50
	cfg.Burst = 100
	return kubernetes.NewForConfig(cfg)
}
