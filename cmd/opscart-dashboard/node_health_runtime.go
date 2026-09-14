package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
)

// This file is docs/08 Phase 4D.2: Node Health migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline — the
// third analyzer (after Node Optimization Phase 4C, Resource Analyzer
// Phase 4D.1) driven by the per-cluster Coordinator. See
// acquisition_runtime.go's runCoordinatedAnalysis for where this and the
// other two are invoked from the same coalesced generation.
//
// This migration is DISPLAY-only. Incident persistence
// (UpsertIncidents/ResolveMissing) remains entirely owned by the legacy
// scan cycle (refresh, scan.go) — see preserveNewerCoordinatorNodeHealth's
// doc comment there for exactly why, and how the two coexist without
// becoming two independent incident writers or making incident aging
// generation-driven. In short: ResolveMissing treats every active incident
// for a cluster not refreshed by the given scanID as absent, regardless of
// issue type — a coordinator-only batch would incorrectly start or advance
// the absence clock on every other, still-legacy incident type. Keeping
// exactly one writer (the legacy scan, using its own synchronous
// observation) avoids that; this file only ever updates what the dashboard
// displays.
//
// The legacy Node Health call inside runFullScan (scanner.FindNodeHealthConditions)
// is deliberately left in place, not disabled: it is the sole writer of the
// incident batch's Node Health evidence, and its retained Node snapshot
// (Scanner.NodeSnapshot()) is still the sole Node source for Node
// Optimization's legacy duplicate execution — neither migrates in this
// slice. Duplicate execution for the DISPLAY value is tolerated the same
// way Phase 4C/4D.1 established: whichever publishes last wins the
// display, guarded by publishNodeHealth's generation check below.

// buildNodeHealth runs the same detection+correlation algorithm
// FindNodeHealthConditions uses (scanner.AnalyzeNodeHealth), sourced from
// one ClusterSnapshot generation's Nodes/Pods/Jobs instead of direct
// Kubernetes calls. Jobs supply Job -> CronJob owner resolution purely from
// each Job's own OwnerReferences — no separate CronJob read is needed.
func buildNodeHealth(resources clusterstate.ClusterResources) []models.NodeConditionFinding {
	nodes := snapshotResourceCopy(resources.Nodes)
	pods := snapshotResourceCopy(resources.Pods)
	jobs := snapshotResourceCopy(resources.Jobs)
	return scanner.AnalyzeNodeHealth(nodes, pods, jobs)
}

// runNodeHealth is the Coordinator-facing analysis step for one cluster: it
// gates on acquisition trustworthiness, then computes and publishes a new
// Node Health result for display. It never touches incident persistence —
// see this file's header comment.
//
// A DEGRADED/RESYNCING/STALE snapshot is not analyzed: state.scan keeps
// showing whatever result (coordinator- or legacy-produced) was last
// published. This is also what keeps an untrustworthy snapshot's absence of
// a finding from ever being treated as evidence a condition cleared — the
// function simply does not run, so it cannot feed absence into anything.
func runNodeHealth(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) {
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

	findings := buildNodeHealth(snapshot.Resources())
	publishNodeHealth(state, snapshot.Generation(), findings)
}

// publishNodeHealth replaces state.scan.nodeHealth via copy-and-swap of the
// whole *clusterScan pointer — the exact mechanism publishNodeOptimization/
// publishResourceAnalysis use, for the same reasons. It updates only the
// display field; incident persistence never reads through this path (see
// this file's header comment and preserveNewerCoordinatorNodeHealth in
// scan.go).
func publishNodeHealth(state *dashboardState, generation uint64, findings []models.NodeConditionFinding) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.scan == nil || generation <= state.scan.nodeHealthGeneration {
		return
	}

	updated := *state.scan
	updated.nodeHealth = findings
	updated.nodeHealthGeneration = generation
	state.scan = &updated
}
