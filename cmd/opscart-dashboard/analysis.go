package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	corev1 "k8s.io/api/core/v1"
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

	report, nodeInfos := buildCostAnalysis(state.costAnalyzer, state.ctx, resources)
	scan.report = report
	scan.nodeInfos = nodeInfos

	// nodes is the authoritative, already-acquired Kubernetes Node inventory
	// for this pass (informer-backed, zero additional API calls) — the
	// Infrastructure page's Nodes tab reads real node conditions,
	// schedulability, age, and kubelet version directly from these objects.
	// snapshot.Resources() already returns an independent slice (see
	// ClusterResources' doc comment), so no further copy is needed here.
	scan.nodes = resources.Nodes

	// nodePodCounts: pods actually occupying each node, counted once here
	// from this same snapshot's Pods — mirrors the exact Running/Pending
	// phase filter AnalyzeNodePoolCostResultFromResources already uses when
	// summing per-node resource requests (pkg/analyzer/nodepool_costs.go),
	// so a node's reported pod count is never inconsistent with its
	// reported CPU/memory requested evidence.
	scan.nodePodCounts = countPodsByNode(resources.Pods)
	scan.namespacePodCounts = countPodsByNamespace(resources.Pods)

	resourceAnalysis := buildResourceAnalysis(resources, namespace)
	scan.AllWorkloads = resourceAnalysis.Workloads
	scan.PodWorkloads = resourceAnalysis.PodWorkloads

	// namespaceCount/namespaces: the authoritative Kubernetes Namespace
	// inventory from this same snapshot — not Cost Intelligence's
	// NamespaceCosts (a namespace can exist with zero cost-allocation
	// entries, e.g. no priced/allocated workloads, and still be a real
	// namespace). resources.Namespaces is already an independent slice
	// (see ClusterResources' doc comment), so no further copy is needed.
	scan.namespaceCount = len(resources.Namespaces)
	scan.namespaces = resources.Namespaces

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

// countPodsByNode counts, per Node name, the Pods currently occupying it —
// Running or Pending only, matching AnalyzeNodePoolCostResultFromResources'
// own phase filter (pkg/analyzer/nodepool_costs.go) for the exact same
// reason: a Succeeded/Failed pod no longer holds resources on that node.
// Pods with no Spec.NodeName (not yet scheduled) are not counted against
// any node.
func countPodsByNode(pods []*corev1.Pod) map[string]int {
	counts := make(map[string]int, len(pods))
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			continue
		}
		if pod.Spec.NodeName == "" {
			continue
		}
		counts[pod.Spec.NodeName]++
	}
	return counts
}

// countPodsByNamespace counts every Pod in the authoritative snapshot by
// namespace. Unlike cost allocation, this inventory includes Pods regardless
// of whether pricing or workload ownership could be resolved.
func countPodsByNamespace(pods []*corev1.Pod) map[string]int {
	counts := make(map[string]int)
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		counts[pod.Namespace]++
	}
	return counts
}
