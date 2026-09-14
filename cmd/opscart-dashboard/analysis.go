package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
)

// This file is docs/08 Phase 4E/5: buildClusterScan is the one recurring
// analysis pass for a cluster. Phase 4E (formerly runFullScan/
// runLegacyAnalysis here) removed its direct Kubernetes acquisition in
// favor of reading the shared ClusterSnapshot. Phase 5 removed the second,
// duplicate analysis path that used to exist alongside it (the Coordinator
// calling seven independent runX/publishX pairs, one per analyzer, each
// with its own generation-guarded field on clusterScan) — see
// acquisition_runtime.go's runAnalysisPass, the single caller of this
// function now, invoked from the Coordinator (event- or clock-triggered)
// and from refresh's on-demand path (scan.go), never both computing the
// same generation independently.
//
// buildClusterScan calls the exact same buildX functions each
// *_runtime.go file establishes (buildNodeHealth, buildCostAnalysis,
// buildResourceAnalysis, buildSecurityAnalysis, buildWasteAnalysis,
// buildNetworkAnalysis, buildNodeOptimization) — those are unchanged by
// Phase 5; only the orchestration around them (one path instead of two)
// changed.
//
// CIS scoring (docs/08 Phase 4D.4) is derived here, synchronously, from
// this SAME call's own secAudit+netAudit — never from a value read back
// from state.scan — so it is structurally impossible for CIS to combine
// Security and Network results from two different analysis passes/
// generations (docs/08 Phase 5's CIS/Security/Network generation-
// consistency requirement).
//
// namespace scoping: --namespace never scoped Security, Waste, Network, or
// Node Health's result — only Resource Analyzer's AllWorkloads/PodWorkloads
// and Cost's namespace allocation, both of which already apply it (via the
// namespace package var and costPodsInNamespace respectively). See
// buildResourceAnalysis/buildCostAnalysis.
func buildClusterScan(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) *clusterScan {
	resources := snapshot.Resources()
	scan := &clusterScan{generation: snapshot.Generation()}

	scan.nodeHealth = buildNodeHealth(resources)

	report := buildCostAnalysis(state.costAnalyzer, state.ctx, resources)
	scan.report = report

	resourceAnalysis := buildResourceAnalysis(resources, namespace)
	scan.AllWorkloads = resourceAnalysis.Workloads
	scan.PodWorkloads = resourceAnalysis.PodWorkloads

	scan.secAudit = buildSecurityAnalysis(resources)
	scan.wasteAudit = buildWasteAnalysis(resources)
	scan.netAudit = buildNetworkAnalysis(resources)

	if scan.secAudit != nil {
		result := analyzer.CalculateCISScore(scan.secAudit, scan.netAudit)
		scan.cisResult = &result
	}

	recommendations, savings := buildNodeOptimization(resources, report)
	scan.nodeOptimization = recommendations
	scan.nodeOptimizationSavings = savings

	return scan
}
