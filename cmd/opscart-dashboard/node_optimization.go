package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
)

// ── Node Optimization page ("Explainable consolidation simulation") ──────────
//
// This page renders analyzer.NodeOptimizationRecommendation values already
// computed during the scan (see runFullScan in server.go). It performs no
// Kubernetes, cloud, or pricing calls, and reimplements no scheduling or
// placement logic — it only projects the analyzer's read-only recommendation
// contract into presentation-only fields for the template.

func (srv *server) handleNodeOptimizationPage(w http.ResponseWriter, r *http.Request) {
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
	fmt.Fprint(w, renderNodeOptimizationPage(scan, ctx, srv.clusterList))
}

// nodeOptimizationPageData is the presentation-only projection handed to the
// template. It never carries analyzer types directly.
type nodeOptimizationPageData struct {
	ClusterName string
	DashURL     string
	Sidebar     template.HTML
	ScannedAtMS int64

	Pools         []nodeOptimizationPoolView
	HasPools      bool
	MultiplePools bool
}

// nodeOptimizationPoolView is one pool's read-only recommendation, projected
// for the template. Numeric/percent fields are pre-formatted here so the
// template contains no derivation logic.
type nodeOptimizationPoolView struct {
	AnchorID string
	PoolName string

	Status      string
	StatusLabel string
	StatusClass string
	Summary     string

	IsSimulationPassed bool
	IsBlocked          bool
	IsPartial          bool
	IsObservation      bool

	HasCandidate         bool
	CandidateRemovedNode string
	CurrentNodeCount     int
	CandidateNodeCount   int
	PodsConsidered       int
	PodsAssigned         int

	HasHeadroom           bool
	CPUHeadroomPercent    string
	MemoryHeadroomPercent string
	RetainedCPUText       string
	RetainedMemoryText    string
	RequestedCPUText      string
	RequestedMemoryText   string

	// SavingsSummaryText is a short value for the multi-pool summary table:
	// either a formatted monthly amount or "Unavailable". It is always set.
	SavingsSummaryText string

	SavingsAvailable         bool
	EstimatedMonthlySavings  string
	HasMonthlyCostTotals     bool
	CurrentMonthlyCostText   string
	CandidateMonthlyCostText string
	PricingUnavailableReason string

	VerifiedChecks    []string
	HasVerifiedChecks bool

	Blockers          []nodeOptimizationReasonView
	HasBlockers       bool
	BlockersTotal     int
	BlockersTruncated bool

	Caveats    []string
	HasCaveats bool

	NotEvaluated []string

	Assignments     []nodeOptimizationAssignmentView
	AssignmentTotal int
}

// nodeOptimizationReasonView is one blocker or evidence-gap reason, stripped
// down to what the template needs to display.
type nodeOptimizationReasonView struct {
	Message string
	Pod     string
	Node    string
}

// nodeOptimizationAssignmentView is one Pod's placement evidence row.
type nodeOptimizationAssignmentView struct {
	Namespace       string
	PodName         string
	SourceNode      string
	DestinationNode string
}

// nodeOptimizationReasonDisplayLimit caps how many blocker/evidence-gap
// reasons render per pool. The analyzer contract can emit one warning per
// affected Pod (e.g. every Pod on a node with unresolved cost-pool identity),
// which would otherwise turn this section into a wall of near-duplicate text.
// The exact total is always shown alongside the capped list.
const nodeOptimizationReasonDisplayLimit = 12

func renderNodeOptimizationPage(scan *clusterScan, activeCtx string, clusterList []string) string {
	clusterName := displayName(activeCtx)
	var recommendations []analyzer.NodeOptimizationRecommendation
	var scannedAtMS int64
	if scan != nil {
		if scan.report != nil {
			clusterName = scan.report.ClusterName
			scannedAtMS = scan.report.Timestamp.UnixMilli()
		}
		recommendations = scan.nodeOptimization
	}

	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}

	var savings []analyzer.NodeOptimizationSavingsProjection
	if scan != nil {
		savings = scan.nodeOptimizationSavings
	}

	pools := make([]nodeOptimizationPoolView, 0, len(recommendations))
	for i, rec := range recommendations {
		var projection analyzer.NodeOptimizationSavingsProjection
		if i < len(savings) {
			projection = savings[i]
		}
		pools = append(pools, buildNodeOptimizationPoolView(i, rec, projection))
	}

	data := nodeOptimizationPageData{
		ClusterName:   clusterName,
		DashURL:       "/" + q,
		Sidebar:       template.HTML(buildSidebar("node-optimization", activeCtx, clusterName, clusterList, countCriticalIssues(scan))),
		ScannedAtMS:   scannedAtMS,
		Pools:         pools,
		HasPools:      len(pools) > 0,
		MultiplePools: len(pools) > 1,
	}

	var buf strings.Builder
	if err := getNodeOptimizationTmpl().Execute(&buf, data); err != nil {
		log.Printf("node optimization template: %v", err)
		return ""
	}
	return buf.String()
}

var getNodeOptimizationTmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("node_optimization.html").
			ParseFS(templateFS, "templates/base.html", "templates/node_optimization.html"),
	)
})

func buildNodeOptimizationPoolView(
	index int,
	rec analyzer.NodeOptimizationRecommendation,
	savings analyzer.NodeOptimizationSavingsProjection,
) nodeOptimizationPoolView {
	label, class := nodeOptimizationStatusView(rec.Status)

	view := nodeOptimizationPoolView{
		AnchorID:    fmt.Sprintf("pool-%d", index),
		PoolName:    nodeOptimizationPoolDisplayName(rec.PoolKey),
		Status:      string(rec.Status),
		StatusLabel: label,
		StatusClass: class,
		Summary:     rec.Summary,

		IsSimulationPassed: rec.Status == analyzer.NodeOptimizationRecommendationSimulationPassed,
		IsBlocked:          rec.Status == analyzer.NodeOptimizationRecommendationBlocked,
		IsPartial:          rec.Status == analyzer.NodeOptimizationRecommendationPartial,
		IsObservation:      rec.Status == analyzer.NodeOptimizationRecommendationObservation,

		HasCandidate:         rec.CurrentNodeCount > 0 && rec.CandidateNodeCount > 0,
		CandidateRemovedNode: rec.CandidateRemovedNode,
		CurrentNodeCount:     rec.CurrentNodeCount,
		CandidateNodeCount:   rec.CandidateNodeCount,
		PodsConsidered:       rec.PodsConsidered,
		PodsAssigned:         rec.PodsAssigned,

		NotEvaluated: rec.NotEvaluated,
	}

	for _, check := range rec.VerifiedChecks {
		view.VerifiedChecks = append(view.VerifiedChecks, nodeOptimizationCheckLabel(check))
	}
	view.HasVerifiedChecks = len(view.VerifiedChecks) > 0

	view.BlockersTotal = len(rec.Blockers)
	blockers := rec.Blockers
	if len(blockers) > nodeOptimizationReasonDisplayLimit {
		blockers = blockers[:nodeOptimizationReasonDisplayLimit]
		view.BlockersTruncated = true
	}
	for _, blocker := range blockers {
		view.Blockers = append(view.Blockers, nodeOptimizationReasonView{
			Message: blocker.Message,
			Pod:     blocker.Pod,
			Node:    blocker.Node,
		})
	}
	view.HasBlockers = len(view.Blockers) > 0

	view.Caveats = rec.Caveats
	view.HasCaveats = len(view.Caveats) > 0

	if rec.Status == analyzer.NodeOptimizationRecommendationSimulationPassed &&
		rec.CPUHeadroomPercent != nil && rec.MemoryHeadroomPercent != nil {
		view.HasHeadroom = true
		view.CPUHeadroomPercent = fmt.Sprintf("%.0f%%", *rec.CPUHeadroomPercent)
		view.MemoryHeadroomPercent = fmt.Sprintf("%.0f%%", *rec.MemoryHeadroomPercent)
		view.RetainedCPUText = formatNodeOptimizationCPU(rec.RetainedCPUCapacityMilli)
		view.RetainedMemoryText = formatNodeOptimizationMemoryGB(rec.RetainedMemoryCapacityBytes)
		view.RequestedCPUText = formatNodeOptimizationCPU(rec.AggregateCPURequestedMilli)
		view.RequestedMemoryText = formatNodeOptimizationMemoryGB(rec.AggregateMemoryRequestedBytes)
	}

	view.SavingsAvailable = savings.Available
	if savings.Available && savings.EstimatedMonthlySavings != nil {
		view.EstimatedMonthlySavings = fmt.Sprintf("$%s/mo", formatMoney(*savings.EstimatedMonthlySavings))
		view.SavingsSummaryText = view.EstimatedMonthlySavings
		if savings.CurrentMonthlyCost != nil && savings.CandidateMonthlyCost != nil {
			view.HasMonthlyCostTotals = true
			view.CurrentMonthlyCostText = fmt.Sprintf("$%s/mo", formatMoney(*savings.CurrentMonthlyCost))
			view.CandidateMonthlyCostText = fmt.Sprintf("$%s/mo", formatMoney(*savings.CandidateMonthlyCost))
		}
	} else {
		view.SavingsSummaryText = "Unavailable"
		view.PricingUnavailableReason = savings.ReasonUnavailable
	}

	view.AssignmentTotal = len(rec.Assignments)
	for _, a := range rec.Assignments {
		view.Assignments = append(view.Assignments, nodeOptimizationAssignmentView{
			Namespace:       a.Namespace,
			PodName:         a.PodName,
			SourceNode:      a.SourceNode,
			DestinationNode: a.DestinationNode,
		})
	}

	return view
}

func nodeOptimizationPoolDisplayName(key analyzer.CostPoolKey) string {
	if key.PoolName != "" {
		return key.PoolName
	}
	return defaultNodePoolDisplayName
}

func nodeOptimizationStatusView(status analyzer.NodeOptimizationRecommendationStatus) (label, class string) {
	switch status {
	case analyzer.NodeOptimizationRecommendationSimulationPassed:
		return "SIMULATION PASSED", "passed"
	case analyzer.NodeOptimizationRecommendationBlocked:
		return "BLOCKED", "blocked"
	case analyzer.NodeOptimizationRecommendationPartial:
		return "PARTIAL", "partial"
	case analyzer.NodeOptimizationRecommendationPreCheckPassed:
		return "PRE-CHECK PASSED", "precheck"
	case analyzer.NodeOptimizationRecommendationCandidate:
		return "CANDIDATE", "candidate"
	case analyzer.NodeOptimizationRecommendationObservation:
		return "OBSERVATION", "observation"
	default:
		return string(status), "observation"
	}
}

var nodeOptimizationCheckLabels = map[analyzer.NodeOptimizationVerifiedCheck]string{
	analyzer.NodeOptimizationCheckCPUCapacity:             "CPU capacity",
	analyzer.NodeOptimizationCheckMemoryCapacity:          "Memory capacity",
	analyzer.NodeOptimizationCheckPodResourceRequests:     "Pod resource requests",
	analyzer.NodeOptimizationCheckNodeSelector:            "nodeSelector",
	analyzer.NodeOptimizationCheckRequiredNodeAffinity:    "Required node affinity",
	analyzer.NodeOptimizationCheckTaintsTolerations:       "Taints/tolerations",
	analyzer.NodeOptimizationCheckCordonState:             "Cordon state",
	analyzer.NodeOptimizationCheckDaemonSetOverhead:       "DaemonSet overhead",
	analyzer.NodeOptimizationCheckTopologySpread:          "Topology spread",
	analyzer.NodeOptimizationCheckRequiredPodAffinity:     "Required Pod affinity",
	analyzer.NodeOptimizationCheckRequiredPodAntiAffinity: "Required Pod anti-affinity",
	analyzer.NodeOptimizationCheckPersistentVolume:        "PVC/PV storage topology",
}

func nodeOptimizationCheckLabel(check analyzer.NodeOptimizationVerifiedCheck) string {
	if label, ok := nodeOptimizationCheckLabels[check]; ok {
		return label
	}
	return string(check)
}

func formatNodeOptimizationCPU(milli int64) string {
	return fmt.Sprintf("%.1f cores", float64(milli)/1000)
}

func formatNodeOptimizationMemoryGB(bytes int64) string {
	const gb = 1024 * 1024 * 1024
	return fmt.Sprintf("%.1f GB", float64(bytes)/gb)
}
