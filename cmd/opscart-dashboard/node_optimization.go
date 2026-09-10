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
// contract into presentation-only fields for the template. Near-duplicate
// blocker/evidence-gap reasons are grouped for display (see
// groupNodeOptimizationReasons) but the underlying analyzer contract and its
// exact counts are never altered.

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

	// PostureSummaryText is a short value for the multi-pool summary table's
	// Headroom/posture column: headroom for a proven candidate, "—" otherwise.
	PostureSummaryText string

	// SavingsSummaryText is a short value for the multi-pool summary table:
	// either a formatted monthly amount or "Unavailable". It is always set.
	SavingsSummaryText string

	SavingsAvailable         bool
	EstimatedMonthlySavings  string
	HasMonthlyCostTotals     bool
	CurrentMonthlyCostText   string
	CandidateMonthlyCostText string
	PricingCoverageText      string
	PricingUnavailableReason string

	VerifiedChecks    []string
	HasVerifiedChecks bool

	// PrimaryBlockerText is the single most useful blocker/evidence-gap
	// message, surfaced in the hero for BLOCKED and PARTIAL pools so the
	// result is not buried under the full diagnostic list below it.
	PrimaryBlockerText string

	Blockers           []nodeOptimizationReasonView
	HasBlockers        bool
	BlockersTotal      int
	BlockersGroupCount int
	BlockersTruncated  bool

	Caveats    []string
	HasCaveats bool

	NotEvaluated []string

	Assignments        []nodeOptimizationAssignmentView
	AssignmentTotal    int
	AssignmentAutoOpen bool
}

// nodeOptimizationReasonView is one blocker or evidence-gap reason (or a
// deduplicated group of near-identical ones), stripped down to what the
// template needs to display.
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
// reason groups render per pool. The analyzer contract can emit one warning
// per affected Pod (e.g. every Pod on a node with unresolved cost-pool
// identity); groupNodeOptimizationReasons collapses those into one line
// before this cap is applied, so the cap is rarely hit in practice. The
// exact total is always shown alongside the capped list.
const nodeOptimizationReasonDisplayLimit = 12

// nodeOptimizationAssignmentAutoOpenLimit is the largest assignment count the
// placement-evidence <details> element auto-expands for. Above this, the
// section renders collapsed by default (native HTML disclosure, no JS) so a
// very large table does not dominate the initial viewport — every row is
// still present in the page, just one click away.
const nodeOptimizationAssignmentAutoOpenLimit = 20

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
	grouped := groupNodeOptimizationReasons(rec.Blockers)
	view.BlockersGroupCount = len(grouped)
	if len(grouped) > 0 {
		view.PrimaryBlockerText = grouped[0].Message
	}
	if len(grouped) > nodeOptimizationReasonDisplayLimit {
		grouped = grouped[:nodeOptimizationReasonDisplayLimit]
		view.BlockersTruncated = true
	}
	view.Blockers = grouped
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

	if view.IsSimulationPassed && view.HasHeadroom {
		view.PostureSummaryText = fmt.Sprintf("%s CPU / %s Mem headroom", view.CPUHeadroomPercent, view.MemoryHeadroomPercent)
	} else {
		view.PostureSummaryText = "—"
	}

	view.SavingsAvailable = savings.Available
	if savings.Available && savings.EstimatedMonthlySavings != nil {
		view.EstimatedMonthlySavings = formatNodeOptimizationMonthly(*savings.EstimatedMonthlySavings)
		view.SavingsSummaryText = view.EstimatedMonthlySavings
		if savings.CurrentMonthlyCost != nil && savings.CandidateMonthlyCost != nil {
			view.HasMonthlyCostTotals = true
			view.CurrentMonthlyCostText = formatNodeOptimizationMonthly(*savings.CurrentMonthlyCost)
			view.CandidateMonthlyCostText = formatNodeOptimizationMonthly(*savings.CandidateMonthlyCost)
			view.PricingCoverageText = fmt.Sprintf("%d of %d nodes priced", rec.CurrentNodeCount, rec.CurrentNodeCount)
		}
	} else {
		view.SavingsSummaryText = "Unavailable"
		view.PricingUnavailableReason = savings.ReasonUnavailable
	}

	view.AssignmentTotal = len(rec.Assignments)
	view.AssignmentAutoOpen = view.AssignmentTotal <= nodeOptimizationAssignmentAutoOpenLimit
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

// nodeOptimizationReasonGroupLabels gives plural-safe operator language for
// blocker/evidence-gap categories that repeat once per affected Pod or node.
// Only the display label is generated here; the underlying Code, exact
// per-reason count, and total are all sourced from the analyzer contract
// unchanged.
var nodeOptimizationReasonGroupLabels = map[string]string{
	"insufficient_cpu":                       "candidates lacked sufficient CPU after consolidation",
	"insufficient_memory":                    "candidates lacked sufficient memory after consolidation",
	"pod_exceeds_node_capacity":              "Pods exceed single-node capacity",
	"no_eligible_destination":                "Pods had no eligible destination node",
	"topology_spread_conflict":               "Pods hit a topology spread conflict",
	"required_pod_affinity_unsatisfied":      "Pods had unsatisfied required Pod affinity",
	"required_pod_anti_affinity_conflict":    "Pods had a required Pod anti-affinity conflict",
	"simulation_inconclusive":                "candidates were inconclusive under the placement heuristic",
	"simulation_blocked":                     "candidates were blocked",
	"simulation_evidence_inconsistent":       "candidates had inconsistent simulation evidence",
	"assignment_evidence_incomplete":         "candidates had incomplete assignment evidence",
	"headroom_evidence_incomplete":           "candidates had incomplete headroom evidence",
	"pvc_storage_mobility_unproven":          "Pods rely on CSI storage mobility that is not modeled",
	"unsupported_hard_scheduling_constraint": "Pods use an unsupported hard scheduling constraint",
	"incomplete_scheduling_evidence":         "Pods could not be mapped to a canonical node pool",
	"incomplete_or_unsupported_evidence":     "pool evidence was incomplete or unsupported",
}

// groupNodeOptimizationReasons collapses near-duplicate blocker/evidence-gap
// reasons (the analyzer can emit one per affected Pod) into one line per
// distinct Code, with an exact occurrence count. A reason with an empty Code
// groups by its exact Message instead. Pod/Node scope is only shown for a
// group of exactly one, since it would misrepresent the other members of a
// larger group. This is presentation-only grouping: it never changes the
// analyzer's Blockers slice, its Code taxonomy, or its counts — the raw
// length of the input is always recoverable by the caller.
func groupNodeOptimizationReasons(reasons []analyzer.NodeOptimizationRecommendationReason) []nodeOptimizationReasonView {
	type group struct {
		message string
		pod     string
		node    string
		count   int
	}
	order := make([]string, 0, len(reasons))
	groups := make(map[string]*group, len(reasons))
	for _, r := range reasons {
		key := r.Code
		if key == "" {
			key = r.Message
		}
		g, ok := groups[key]
		if !ok {
			g = &group{message: r.Message, pod: r.Pod, node: r.Node}
			groups[key] = g
			order = append(order, key)
		} else {
			if g.pod != r.Pod {
				g.pod = ""
			}
			if g.node != r.Node {
				g.node = ""
			}
		}
		g.count++
	}

	views := make([]nodeOptimizationReasonView, 0, len(order))
	for _, key := range order {
		g := groups[key]
		message, pod, node := g.message, g.pod, g.node
		if g.count > 1 {
			if label, ok := nodeOptimizationReasonGroupLabels[key]; ok {
				message = fmt.Sprintf("%d %s", g.count, label)
			} else {
				message = fmt.Sprintf("%s (×%d)", g.message, g.count)
			}
			pod, node = "", ""
		}
		views = append(views, nodeOptimizationReasonView{Message: message, Pod: pod, Node: node})
	}
	return views
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

// formatNodeOptimizationMonthly formats an already-known exact monthly
// dollar amount. It is never called with a missing/unavailable value — the
// caller only invokes it once analyzer.NodeOptimizationSavingsProjection has
// confirmed the figure is available, so this never renders a fabricated $0.
func formatNodeOptimizationMonthly(amount float64) string {
	return fmt.Sprintf("$%s/mo", formatMoney(amount))
}
