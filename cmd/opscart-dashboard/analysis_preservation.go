package main

// This file holds refresh's (scan.go) coordinator-vs-legacy publication-
// ordering guards — one preserveNewerCoordinatorX function per Phase
// 4C/4D-migrated analyzer (Node Optimization, Resource Analyzer, Node
// Health, Network, Security). Each guards refresh's wholesale *clusterScan
// replacement against clobbering that analyzer's newer, coordinator-
// published DISPLAY result with the legacy scan's own — always
// generation-less — computation for the same field.
//
// These are deliberately explicit, analyzer-specific functions, not a
// generic registry or reflection-driven loop: each analyzer's fields differ
// (some pair a result with a savings/workload-map sibling, some feed the
// incident batch and some don't), and the guard for a sixth migrated
// analyzer should read as one more paragraph here, not a new abstraction.
//
// Every function here follows the same shape: previous is the *clusterScan
// refresh is about to replace; next is the one legacy runFullScan just
// built and is about to publish. previous may be nil (the very first scan
// for a cluster). Each coordinator's own publishX (see e.g.
// node_optimization_runtime.go) already guards the opposite direction — an
// older coordinator generation can never overwrite a newer one, nor a
// legacy result that arrived after it — so these are the one remaining
// place a newer coordinator result can be lost: refresh does not go
// through any publishX function itself.

// preserveNewerCoordinatorNodeOptimization guards refresh's wholesale
// *clusterScan replacement (below) against clobbering a newer,
// coordinator-published Node Optimization result (docs/08 Phase 4C) with
// the legacy scan's own — always generation-less — step 6 computation.
//
// previous is the *clusterScan refresh is about to replace; next is the
// one legacy runFullScan just built and is about to publish. The legacy
// computation itself is intentionally still run every cycle regardless
// (node_optimization_runtime.go documents why disabling it is unsafe); this
// only decides which result the swap actually publishes. previous may be
// nil (the very first scan for this cluster).
//
// The coordinator's own publishNodeOptimization already guards the
// opposite direction (an older coordinator generation can never overwrite
// a newer one, nor a legacy result that arrived after it — see its
// generation check), so this is the one remaining place a newer result can
// be lost: refresh does not go through publishNodeOptimization at all.
func preserveNewerCoordinatorNodeOptimization(previous, next *clusterScan) {
	if previous == nil || previous.nodeOptimizationGeneration == 0 {
		return
	}
	next.nodeOptimization = previous.nodeOptimization
	next.nodeOptimizationSavings = previous.nodeOptimizationSavings
	next.nodeOptimizationGeneration = previous.nodeOptimizationGeneration
}

// preserveNewerCoordinatorResourceAnalysis is
// preserveNewerCoordinatorNodeOptimization's exact counterpart for Resource
// Analyzer (docs/08 Phase 4D.1): guards refresh's wholesale *clusterScan
// replacement against clobbering a newer, coordinator-published
// AllWorkloads/PodWorkloads result with the legacy scan's own —
// always generation-less — computation. See
// preserveNewerCoordinatorNodeOptimization for the full reasoning; this is
// the same pattern applied to a second, independent analyzer, not a new
// versioning system.
func preserveNewerCoordinatorResourceAnalysis(previous, next *clusterScan) {
	if previous == nil || previous.resourceAnalysisGeneration == 0 {
		return
	}
	next.AllWorkloads = previous.AllWorkloads
	next.PodWorkloads = previous.PodWorkloads
	next.resourceAnalysisGeneration = previous.resourceAnalysisGeneration
}

// preserveNewerCoordinatorNodeHealth is
// preserveNewerCoordinatorNodeOptimization's counterpart for Node Health
// (docs/08 Phase 4D.2): guards refresh's wholesale *clusterScan replacement
// against clobbering a newer, coordinator-published Node Health DISPLAY
// result with the legacy scan's own — always generation-less — computation.
//
// This affects the DISPLAY field only. refresh takes a legacyScan snapshot
// of next BEFORE calling this function (and preserveNewerCoordinatorNetworkAnalysis
// below) specifically so incident persistence
// (calcIncidentScore/collectWarRoomIssues/completeIncidentBatch, further
// down in refresh) always uses this legacy scan cycle's own synchronous
// observation, never a coordinator-sourced one — docs/08 Phase 4D.2's
// incident-lifecycle decision (Option A: analysis-only migration).
//
// Why persistence cannot simply follow the coordinator's fresher value:
// ResolveMissing(cluster, scanID) treats every ACTIVE incident for cluster
// not refreshed by scanID as absent — for every issue type, not just Node
// Health. If a coordinator-timed batch were upserted under its own scanID,
// every other (still-legacy) incident type would incorrectly look
// "missing" to that call and start or advance its own absence clock, on
// the coordinator's cadence rather than the legacy scan's. Keeping exactly
// one incident writer (the legacy scan, using its own evidence) avoids
// that; this function only ever changes what the dashboard shows.
func preserveNewerCoordinatorNodeHealth(previous, next *clusterScan) {
	if previous == nil || previous.nodeHealthGeneration == 0 {
		return
	}
	next.nodeHealth = previous.nodeHealth
	next.nodeHealthGeneration = previous.nodeHealthGeneration
}

// preserveNewerCoordinatorNetworkAnalysis is
// preserveNewerCoordinatorNodeHealth's counterpart for the Network analyzer
// (docs/08 Phase 4D.3): guards refresh's wholesale *clusterScan replacement
// against clobbering a newer, coordinator-published Network audit DISPLAY
// result with the legacy scan's own — always generation-less — computation.
//
// netAudit feeds the incident batch too (collectWarRoomIssues' HIGH-risk
// "unprotected_namespace" issues, further down in refresh), so exactly the
// same DISPLAY-vs-incident-persistence split applies as
// preserveNewerCoordinatorNodeHealth — see its doc comment and refresh's
// legacyScan capture.
func preserveNewerCoordinatorNetworkAnalysis(previous, next *clusterScan) {
	if previous == nil || previous.netAuditGeneration == 0 {
		return
	}
	next.netAudit = previous.netAudit
	next.netAuditGeneration = previous.netAuditGeneration
}

// preserveNewerCoordinatorSecurityAnalysis is
// preserveNewerCoordinatorNetworkAnalysis's counterpart for the Security
// analyzer (docs/08 Phase 4D.4): guards refresh's wholesale *clusterScan
// replacement against clobbering a newer, coordinator-published Security
// audit DISPLAY result with the legacy scan's own — always generation-less
// — computation.
//
// secAudit feeds the incident batch too (collectWarRoomIssues' critical
// "privileged_container" issues, further down in refresh), so exactly the
// same DISPLAY-vs-incident-persistence split applies as
// preserveNewerCoordinatorNetworkAnalysis — see its doc comment and
// refresh's legacyScan capture. It does NOT touch cisResult — see that
// field's doc comment in clusterScan for why CIS scoring stays entirely
// legacy-owned.
func preserveNewerCoordinatorSecurityAnalysis(previous, next *clusterScan) {
	if previous == nil || previous.secAuditGeneration == 0 {
		return
	}
	next.secAudit = previous.secAudit
	next.secAuditGeneration = previous.secAuditGeneration
}
