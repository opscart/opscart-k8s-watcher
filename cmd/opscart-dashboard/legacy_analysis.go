package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
)

// This file is docs/08 Phase 4E: refresh's (scan.go) own Kubernetes
// acquisition — formerly server.go's runFullScan, which built its own
// *kubernetes.Clientset and called each analyzer's live-acquisition entry
// point every scan cycle — is replaced by reading the same ClusterSnapshot
// the Coordinator already reads (acquisition_runtime.go's
// runCoordinatedAnalysis). runLegacyAnalysis below calls the exact same
// buildX functions each *_runtime.go file already established for the
// coordinator-driven path; only the acquisition source changes.
//
// This pass itself is NOT removed, and its wall-clock (60s ticker) timing
// is unchanged — see docs/08 Phase 4D.2's incident-lifecycle decision
// (Option A: analysis-only migration) and each *_runtime.go file's header
// comment: incident persistence (UpsertIncidents/ResolveMissing) and CIS
// scoring remain entirely owned by this synchronous pass, never by the
// Coordinator's independently-timed DISPLAY publications. refresh's
// legacyScan capture and analysis_preservation.go's preserveNewerCoordinatorX
// guards are unchanged by this file. Duplicate analyzer EXECUTION between
// this pass and the Coordinator remains tolerated for the DISPLAY fields,
// exactly as already documented — only the duplicate Kubernetes
// ACQUISITION is removed here.
//
// namespace scoping: --namespace never scoped Security, Waste, Network, or
// Node Health's legacy result — server.go's former calls into each always
// passed a literal "" filter namespace (confirmed by inspection: only
// whether they could reuse ResourceAnalyzer's Pod snapshot depended on the
// flag, never the audited scope itself). --namespace only ever scoped
// Resource Analyzer's AllWorkloads/PodWorkloads and Cost's namespace
// allocation, and both buildResourceAnalysis and buildCostAnalysis already
// apply it (via the namespace package var and costPodsInNamespace
// respectively). No new namespace filtering is required here to preserve
// that behavior.
func runLegacyAnalysis(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) *clusterScan {
	resources := snapshot.Resources()
	scan := &clusterScan{}

	scan.nodeHealth = buildNodeHealth(resources)

	report := buildCostAnalysis(state.costAnalyzer, state.ctx, resources)
	scan.report = report

	resourceAnalysis := buildResourceAnalysis(resources, namespace)
	scan.AllWorkloads = resourceAnalysis.Workloads
	scan.PodWorkloads = resourceAnalysis.PodWorkloads

	scan.secAudit = buildSecurityAnalysis(resources)
	scan.wasteAudit = buildWasteAnalysis(resources)
	scan.netAudit = buildNetworkAnalysis(resources)

	// CIS stays legacy-owned (docs/08 Phase 4D.4's cisResult boundary,
	// clusterScan's doc comment in scan.go): derived synchronously from this
	// same pass's own Security+Network results, never from the Coordinator's
	// independently-timed DISPLAY values.
	if scan.secAudit != nil {
		result := analyzer.CalculateCISScore(scan.secAudit, scan.netAudit)
		scan.cisResult = &result
	}

	recommendations, savings := buildNodeOptimization(resources, report)
	scan.nodeOptimization = recommendations
	scan.nodeOptimizationSavings = savings

	return scan
}
