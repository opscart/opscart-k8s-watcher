package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// This file is docs/08 Phase 4D.1: Resource Analyzer migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline — the
// second analyzer (after Node Optimization, Phase 4C) driven by the
// per-cluster Coordinator. See acquisition_runtime.go's
// runCoordinatedAnalysis for where this and Node Optimization are both
// invoked from the same coalesced generation, and its doc comment for why
// that combining logic lives there rather than in either analyzer's file.
//
// The legacy Resource Analyzer call inside runFullScan
// (analyzer.NewResourceAnalyzer + AnalyzeClusterResources, server.go step
// after Cost) is deliberately left in place, not disabled: its retained Pod
// snapshot (ResourceAnalyzer.PodSnapshot()) is still the sole Pod-acquisition
// path for Security, Waste, Network, and Node Optimization's legacy
// duplicate execution — none of which migrate in this slice — and its full
// *models.ClusterResourceAnalysis output feeds CostAnalyzer.AnalyzeCosts
// synchronously in the same function; Cost is out of scope here too.
// Disabling the legacy call would break those still-legacy consumers,
// exactly the risk Phase 4C already found for Node Optimization. Duplicate
// execution is tolerated the same way: whichever publishes last wins the
// display, guarded by publishResourceAnalysis's generation check below.

// buildResourceAnalysis runs analyzer.AnalyzeResources — the same resource
// analysis algorithm AnalyzeClusterResources itself now delegates to
// (pkg/analyzer/resources.go) — sourced from one ClusterSnapshot
// generation's Pods/Nodes instead of direct Kubernetes calls.
func buildResourceAnalysis(resources clusterstate.ClusterResources, namespace string) *models.ClusterResourceAnalysis {
	pods := snapshotResourceCopy(resources.Pods)
	nodes := snapshotResourceCopy(resources.Nodes)
	return analyzer.AnalyzeResources(pods, nodes, namespace)
}

// runResourceAnalysis is the Coordinator-facing analysis step for one
// cluster: it gates on acquisition trustworthiness, then computes and
// publishes a new Resource Analyzer result.
//
// Unlike Node Optimization, this has no dependency on a legacy scan's own
// output (no cost/pricing enrichment involved) — only on a *clusterScan
// already existing to copy-and-swap onto (see publishResourceAnalysis).
//
// A DEGRADED/RESYNCING/STALE snapshot is not analyzed — state.scan keeps
// showing whatever result (coordinator- or legacy-produced) was last
// published, matching Node Optimization's identical contract.
func runResourceAnalysis(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) {
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

	analysis := buildResourceAnalysis(snapshot.Resources(), namespace)
	publishResourceAnalysis(state, snapshot.Generation(), analysis.Workloads, analysis.PodWorkloads)
}

// publishResourceAnalysis replaces state.scan's Resource Analyzer fields via
// copy-and-swap of the whole *clusterScan pointer — the exact mechanism
// publishNodeOptimization uses (node_optimization_runtime.go), for the same
// reasons: every existing reader treats an already-published *clusterScan as
// immutable, and the generation check is defense-in-depth documenting an
// invariant Coordinator's single-threaded loop already guarantees
// structurally, staying correct if a legacy scan cycle resets
// resourceAnalysisGeneration to 0 in between coordinator runs.
func publishResourceAnalysis(
	state *dashboardState,
	generation uint64,
	workloads []models.WorkloadRef,
	podWorkloads map[string]models.WorkloadRef,
) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.scan == nil || generation <= state.scan.resourceAnalysisGeneration {
		return
	}

	updated := *state.scan
	updated.AllWorkloads = workloads
	updated.PodWorkloads = podWorkloads
	updated.resourceAnalysisGeneration = generation
	state.scan = &updated
}
