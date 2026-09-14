package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// This file is docs/08 Phase 4D.4: Security analysis migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline — the
// fifth analyzer (after Node Optimization Phase 4C, Resource Analyzer
// Phase 4D.1, Node Health Phase 4D.2, Network Phase 4D.3) driven by the
// per-cluster Coordinator. See acquisition_runtime.go's runCoordinatedAnalysis
// for where all five are invoked from the same coalesced generation.
//
// This migration is DISPLAY-only, exactly like Node Health and Network.
// Incident persistence (UpsertIncidents/ResolveMissing) remains entirely
// owned by the legacy scan cycle (refresh, scan.go) — see
// preserveNewerCoordinatorSecurityAnalysis's doc comment there for exactly
// why (secAudit's critical "privileged_container" issues feed
// collectWarRoomIssues, part of that same incident batch), and how the two
// coexist without becoming two independent incident writers.
//
// The legacy scan cycle (legacy_analysis.go's runLegacyAnalysis, since
// docs/08 Phase 4E) calls this same buildSecurityAnalysis directly — not
// disabled: its result feeds CalculateCISScore synchronously in that same
// pass (see clusterScan.cisResult's doc comment in scan.go for the full CIS
// boundary), and CIS scoring stays entirely legacy-owned in this slice.
// Duplicate execution for the DISPLAY value is tolerated the same way Phase
// 4C/4D.1/4D.2/4D.3 established: whichever publishes last wins the display,
// guarded by publishSecurityAnalysis's generation check below. Before Phase
// 4E, the legacy pass called analyzer.NewSecurityAuditor(clientset) +
// AuditClusterSecurityWithPodSnapshot directly; it now shares this file's
// Kubernetes-free implementation instead.
func buildSecurityAnalysis(resources clusterstate.ClusterResources) *models.SecurityAudit {
	pods := snapshotResourceCopy(resources.Pods)
	return analyzer.AnalyzeSecurity(pods)
}

// runSecurityAnalysis is the Coordinator-facing analysis step for one
// cluster: it gates on acquisition trustworthiness, then computes and
// publishes a new Security audit for display. It never touches incident
// persistence or CIS scoring — see this file's header comment.
//
// A DEGRADED/RESYNCING/STALE snapshot is not analyzed: state.scan keeps
// showing whatever result (coordinator- or legacy-produced) was last
// published. This is also what keeps an untrustworthy snapshot from ever
// being treated as evidence a security issue disappeared — the function
// simply does not run, so it cannot feed a false transition into anything,
// incident-related or not.
func runSecurityAnalysis(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) {
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

	audit := buildSecurityAnalysis(snapshot.Resources())
	publishSecurityAnalysis(state, snapshot.Generation(), audit)
}

// publishSecurityAnalysis replaces state.scan.secAudit via copy-and-swap of
// the whole *clusterScan pointer — the exact mechanism
// publishNodeOptimization/publishResourceAnalysis/publishNodeHealth/
// publishNetworkAnalysis use, for the same reasons: every existing reader
// treats an already-published *clusterScan as immutable, and the
// generation check is defense-in-depth documenting an invariant
// Coordinator's single-threaded loop already guarantees structurally.
func publishSecurityAnalysis(state *dashboardState, generation uint64, audit *models.SecurityAudit) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.scan == nil || generation <= state.scan.secAuditGeneration {
		return
	}

	updated := *state.scan
	updated.secAudit = audit
	updated.secAuditGeneration = generation
	state.scan = &updated
}
