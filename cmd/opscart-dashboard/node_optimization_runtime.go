package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
)

// This file is docs/08 Phase 4C: the first analyzer migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline built in
// Phases 2-4B. Only Node Optimization's own analysis logic lives here —
// every analyzer this pipeline drives (Cost, Resource Analyzer, Node
// Health, Waste, Security, Network) has its own similarly-named
// *_runtime.go file. buildNodeOptimization is called from analysis.go's
// buildClusterScan — the one analysis path per cluster (docs/08 Phase 5) —
// not from a separate Coordinator-facing entry point: Phase 5 removed the
// runNodeOptimization/publishNodeOptimization split (and the per-analyzer
// nodeOptimizationGeneration field, and the scan.costGeneration ==
// snapshot.Generation() cross-analyzer check it used) once Cost's report
// became a plain Go value threaded directly into this function within the
// same buildClusterScan call, rather than something read back from
// state.scan across two independently-timed publications. See
// acquisition_runtime.go's runAnalysisPass and publishScan for the single
// ordering guard that replaced both.

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
// reapplied here from report — the coordinator's own same-generation Cost
// result (docs/08 Phase 4D.6; see cost_runtime.go) —
// exactly mirroring AnalyzeNodePoolCosts' own identical step, so pool
// identity still joins correctly against report.NodePoolCosts in
// BuildNodeOptimizationSavingsProjections. report may be nil (no coordinator
// Cost result published yet); nothing here requires it beyond this override
// check.
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

// buildNodeOptimization runs the Node Optimization pipeline both the
// Coordinator and the legacy scan cycle (legacy_analysis.go) call, sourced
// entirely from one ClusterSnapshot generation's resources plus report —
// the same-generation full Cost result (docs/08 Phase 4D.6; see
// runCoordinatedAnalysis's Cost-before-Node-Optimization ordering,
// acquisition_runtime.go). report may be nil if Cost has not published one
// for this cluster yet; both helpers below tolerate that.
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
