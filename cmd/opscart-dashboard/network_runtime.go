package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
)

// This file is docs/08 Phase 4D.3: Network Policy analysis migrated off
// direct Kubernetes acquisition onto the shared ClusterSnapshot pipeline —
// the fourth analyzer (after Node Optimization Phase 4C, Resource Analyzer
// Phase 4D.1, Node Health Phase 4D.2) driven by the per-cluster
// Coordinator. See acquisition_runtime.go's runCoordinatedAnalysis for
// where all four are invoked from the same coalesced generation.
//
// This migration is DISPLAY-only, exactly like Node Health. Incident
// persistence (UpsertIncidents/ResolveMissing) remains entirely owned by
// the legacy scan cycle (refresh, scan.go) — see
// preserveNewerCoordinatorNetworkAnalysis's doc comment there for exactly
// why (netAudit's HIGH-risk UnprotectedNamespaces feed
// collectWarRoomIssues' "unprotected_namespace" issues, part of that same
// incident batch), and how the two coexist without becoming two
// independent incident writers or making incident aging generation-driven.
//
// The legacy Network call inside runFullScan (analyzer.NewNetworkPolicyAuditor
// + AuditNetworkPolicies/AuditNetworkPoliciesWithPods, server.go step 4) is
// deliberately left in place, not disabled: its result feeds
// CalculateCISScore synchronously in the same function, and Security (which
// owns that score) is out of scope for this slice. Duplicate execution for
// the DISPLAY value is tolerated the same way Phase 4C/4D.1/4D.2
// established: whichever publishes last wins the display, guarded by
// publishNetworkAnalysis's generation check below.
//
// filterNamespace is always "" here, matching the legacy call site exactly:
// server.go passes "" to both AuditNetworkPolicies and
// AuditNetworkPoliciesWithPods regardless of the --namespace flag — that
// flag only decides which Pod-acquisition strategy runFullScan uses
// internally (reuse ResourceAnalyzer's snapshot vs. list live), never
// namespace-scopes the Network audit's own result. skipNamespaces is nil,
// matching that no dashboard call site configures
// NetworkPolicyAuditor.WithSkipNamespaces — shouldSkipNamespace's built-in
// infra-pattern/label strategies are the only skip behavior in play.
func buildNetworkAnalysis(resources clusterstate.ClusterResources) *analyzer.NetworkPolicyAudit {
	namespaces := snapshotResourceCopy(resources.Namespaces)
	pods := snapshotResourceCopy(resources.Pods)
	policies := snapshotResourceCopy(resources.NetworkPolicies)
	return analyzer.AnalyzeNetworkPolicies(namespaces, pods, policies, "", nil)
}

// runNetworkAnalysis is the Coordinator-facing analysis step for one
// cluster: it gates on acquisition trustworthiness, then computes and
// publishes a new Network audit for display. It never touches incident
// persistence — see this file's header comment.
//
// A DEGRADED/RESYNCING/STALE snapshot is not analyzed: state.scan keeps
// showing whatever result (coordinator- or legacy-produced) was last
// published. This is also what keeps an untrustworthy snapshot from ever
// being treated as evidence a namespace became protected or unprotected —
// the function simply does not run, so it cannot feed a false transition
// into anything, incident-related or not.
func runNetworkAnalysis(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) {
	if !snapshot.Trustworthy() {
		return
	}

	state.mu.RLock()
	hasScan := state.scan != nil
	state.mu.RUnlock()
	if !hasScan {
		// No legacy scan has ever published a *clusterScan yet — there is
		// nothing to copy-and-swap this result onto.
		return
	}

	audit := buildNetworkAnalysis(snapshot.Resources())
	publishNetworkAnalysis(state, snapshot.Generation(), audit)
}

// publishNetworkAnalysis replaces state.scan.netAudit via copy-and-swap of
// the whole *clusterScan pointer — the exact mechanism
// publishNodeOptimization/publishResourceAnalysis/publishNodeHealth use,
// for the same reasons: every existing reader treats an already-published
// *clusterScan as immutable, and the generation check is defense-in-depth
// documenting an invariant Coordinator's single-threaded loop already
// guarantees structurally.
func publishNetworkAnalysis(state *dashboardState, generation uint64, audit *analyzer.NetworkPolicyAudit) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.scan == nil || generation <= state.scan.netAuditGeneration {
		return
	}

	updated := *state.scan
	updated.netAudit = audit
	updated.netAuditGeneration = generation
	state.scan = &updated
}
