package main

import (
	"context"

	"github.com/opscart/opscart-k8s-watcher/pkg/acquisition"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
)

// This file is docs/08 Phase 4C: the first analyzer migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline built in
// Phases 2-4B. Only Node Optimization moves here — every other analyzer
// (Cost, Node Health, Resource Analyzer, Waste, Security, Network,
// incidents) keeps acquiring Kubernetes state directly via runFullScan
// (server.go), unchanged.
//
// The legacy Node Optimization block inside runFullScan (step 6) is
// deliberately left in place, not disabled: runFullScan's caller (refresh,
// scan.go) replaces dashboardState.scan wholesale every scan cycle, so if
// the legacy block stopped populating nodeOptimization/nodeOptimizationSavings,
// the very next legacy scan would silently wipe out this coordinator's more
// recent result. Avoiding that would require teaching refresh/runFullScan to
// carry coordinator-owned fields forward across an unrelated scan cycle —
// exactly the broad runFullScan refactor this migration slice is scoped to
// avoid. Duplicate execution (legacy scan and coordinator both compute Node
// Optimization) is tolerated instead; whichever publishes last wins the
// display, and publishNodeOptimization's generation guard below prevents an
// older coordinator generation from clobbering a newer one.

// snapshotResourceCopy dereferences a ClusterResources pointer slice into the
// value slice Node Optimization's existing pkg/analyzer functions expect
// (they predate ClusterSnapshot and take []corev1.T, not []*corev1.T). It is
// a one-time, one-way, shallow copy performed at this boundary only — the
// same adaptation pattern pkg/clusterstate's own proof tests use — not a
// general-purpose conversion framework and not evidence that
// ClusterResources itself deep-copies (see snapshot.go's doc comment).
func snapshotResourceCopy[T any](in []*T) []T {
	out := make([]T, len(in))
	for i, v := range in {
		out[i] = *v
	}
	return out
}

// buildNodeInfosFromSnapshot builds one models.NodeInfo per generation-N
// live Node via analyzer.NodeInfoFromNode — the exact per-node extraction
// NodePoolCostAnalyzer.AnalyzeNodePoolCosts itself uses, reused rather than
// reimplemented here. This is the fix for the Phase 4C audit finding that
// filtering a legacy scan's retained NodeInfo by live node name was
// insufficient: CPUCapacity, MemGBCapacity, NodePool, VMSize, Region, Zone,
// OS, Architecture, and Priority are feasibility/pool-grouping evidence,
// and must come from the same ClusterSnapshot generation as the
// Nodes/Pods/PVCs/PVs the rest of buildNodeOptimization already uses — not
// from a legacy scan that can be up to one scan cycle (60s) stale. Building
// this fresh, every call, makes a removed node simply absent and a newly
// added node simply present, with no reconciliation step needed.
//
// The one legitimate exception is a configured manual cloud-provider
// override: that is Cost-analyzer configuration, not Kubernetes-observed
// evidence, and does not go stale the way node topology does. It is
// reapplied here from report — the legacy scan's already-computed
// CloudCostReport — exactly mirroring AnalyzeNodePoolCosts' own identical
// step, so pool identity still joins correctly against report.NodePoolCosts
// in BuildNodeOptimizationSavingsProjections. report may be nil (no legacy
// scan yet); nothing here requires it beyond this override check.
//
// CPURequested/MemGBRequested are deliberately left unset: confirmed by
// inspection that no pkg/analyzer Node Optimization function reads either
// field (only NodePoolCostAnalyzer's own pool-cost math does), so there is
// nothing to source or reconcile for them here.
func buildNodeInfosFromSnapshot(nodes []*corev1.Node, report *models.CloudCostReport) []models.NodeInfo {
	infos := make([]models.NodeInfo, 0, len(nodes))
	for _, node := range nodes {
		info := analyzer.NodeInfoFromNode(*node)
		if report != nil && report.ProviderDetectionMode == "manual" && report.EffectiveProvider != "" {
			info.Provider = report.EffectiveProvider
		}
		infos = append(infos, info)
	}
	return infos
}

// buildNodeOptimization runs the same Node Optimization pipeline runFullScan's
// legacy step 6 runs, sourced entirely from one ClusterSnapshot generation's
// resources plus report — the existing published Cost Intelligence result
// this migration slice does not itself acquire (docs/08 Phase 4C scope;
// Cost stays unmigrated). report may be nil if no legacy scan has completed
// yet; buildNodeInfosFromSnapshot and the savings projection below both
// tolerate that.
func buildNodeOptimization(
	resources clusterstate.ClusterResources,
	report *models.CloudCostReport,
) ([]analyzer.NodeOptimizationRecommendation, []analyzer.NodeOptimizationSavingsProjection) {
	nodes := snapshotResourceCopy(resources.Nodes)
	pods := snapshotResourceCopy(resources.Pods)
	pvcs := snapshotResourceCopy(resources.PersistentVolumeClaims)
	pvs := snapshotResourceCopy(resources.PersistentVolumes)
	nodeInfos := buildNodeInfosFromSnapshot(resources.Nodes, report)

	schedulingEvidence := analyzer.BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	storageEvidence := analyzer.BuildNodeOptimizationStorageEvidence(pvcs, pvs)
	summary := analyzer.BuildNMinusOneNodeOptimizationScenariosWithSchedulingAndStorageEvidence(
		nodeInfos, pods, schedulingEvidence, storageEvidence,
	)
	recommendations := analyzer.BuildNodeOptimizationRecommendations(summary, pods)

	var poolCosts []models.NodePoolCost
	var currency string
	if report != nil {
		poolCosts = report.NodePoolCosts
		currency = report.Currency
	}
	savings := analyzer.BuildNodeOptimizationSavingsProjections(recommendations, poolCosts, currency)
	return recommendations, savings
}

// runNodeOptimization is the Coordinator-facing analysis step for one
// cluster: it gates on acquisition trustworthiness and on a legacy scan
// having published at least once (for report — see buildNodeOptimization),
// then computes and publishes a new Node Optimization result.
//
// A DEGRADED/RESYNCING/STALE snapshot is not analyzed — state.scan keeps
// showing whatever Node Optimization result (coordinator- or legacy-produced)
// was last published, which is the required "continue displaying the last
// trustworthy result" behavior with no second readiness model.
func runNodeOptimization(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) {
	if !snapshot.Trustworthy() {
		return
	}

	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()
	if scan == nil || scan.report == nil {
		// No legacy scan has published a CloudCostReport yet — there is no
		// pool pricing/currency or provider-override decision to join
		// against.
		return
	}

	recommendations, savings := buildNodeOptimization(snapshot.Resources(), scan.report)
	publishNodeOptimization(state, snapshot.Generation(), recommendations, savings)
}

// publishNodeOptimization replaces state.scan's Node Optimization fields via
// copy-and-swap of the whole *clusterScan pointer — never in-place field
// mutation, since every existing reader treats an already-published
// *clusterScan as immutable (see clusterScan's doc comment in scan.go).
//
// The generation <= check is defense-in-depth, not the mechanism that makes
// ordering safe: Coordinator's single-threaded Run loop already makes an
// older generation being analyzed after a newer one structurally impossible
// (pkg/clusterstate/coordinator.go). It exists to document that invariant
// here and to stay correct if a legacy scan cycle resets
// nodeOptimizationGeneration to 0 in between coordinator runs.
func publishNodeOptimization(
	state *dashboardState,
	generation uint64,
	recommendations []analyzer.NodeOptimizationRecommendation,
	savings []analyzer.NodeOptimizationSavingsProjection,
) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.scan == nil || generation <= state.scan.nodeOptimizationGeneration {
		return
	}

	updated := *state.scan
	updated.nodeOptimization = recommendations
	updated.nodeOptimizationSavings = savings
	updated.nodeOptimizationGeneration = generation
	state.scan = &updated
}

// newNodeOptimizationAnalysis adapts runNodeOptimization into the
// clusterstate.AnalysisFunc shape Coordinator calls.
func newNodeOptimizationAnalysis(state *dashboardState) clusterstate.AnalysisFunc {
	return func(snapshot *clusterstate.ClusterSnapshot) {
		runNodeOptimization(state, snapshot)
	}
}

// startNodeOptimizationCoordinator creates and starts this cluster's Phase 4B
// coordinator over rt's ClusterState, wired to the Node Optimization analysis
// above. Exactly one coordinator exists per cluster, matching rt's
// ClusterState one-to-one (docs/08 §13).
func startNodeOptimizationCoordinator(ctx context.Context, state *dashboardState, rt *acquisition.Runtime) {
	coordinator := clusterstate.NewCoordinator(rt.ClusterState(), newNodeOptimizationAnalysis(state))
	state.coordinator = coordinator
	go coordinator.Run(ctx)
}
