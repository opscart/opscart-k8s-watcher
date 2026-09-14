package main

import (
	"fmt"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
)

// This file is docs/08 Phase 4D.6: Cost's own Kubernetes acquisition
// (Nodes+Pods, previously two unconditional LISTs every legacy scan cycle)
// migrated off direct Kubernetes acquisition onto the shared ClusterSnapshot
// pipeline. buildCostAnalysis is called from analysis.go's buildClusterScan
// — the one analysis path per cluster (docs/08 Phase 5) — not from a
// separate Coordinator-facing entry point: Phase 5 removed the
// runCostAnalysis/publishCostAnalysis split (and the per-analyzer
// costGeneration field it guarded) once there was no longer a second,
// independently-timed analysis path to reconcile against. See
// acquisition_runtime.go's runAnalysisPass (Cost-before-Node-Optimization
// ordering preserved by direct sequential calls, not a gate) and
// publishScan for the single ordering guard that replaced it.
//
// The full models.CloudCostReport is built once per pass and threaded
// directly into buildNodeOptimization (node_optimization_runtime.go) by
// runAnalysisPass — the same-generation join Node Optimization needs is now
// structural (one Go value passed between two sequential calls in the same
// function), not a cross-analyzer generation check.
//
// costAnalyzer is dashboardState's one persistent, per-cluster
// analyzer.NodePoolCostAnalyzer (constructed once in getState), reused by
// every analysis pass rather than each reconstructing its own — this is
// the fix for the known Azure pricing-provider lifetime defect (docs/08
// Phase 0 finding): a fresh analyzer would mean a fresh, empty-cache Azure
// provider every pass, so its 24h TTL could never actually be reached.
// Reusing one persistent analyzer instance lets that cache — and the AWS
// provider's pre-existing process-global cache — survive across
// generations and across clock-triggered re-analysis of an unchanged
// generation alike. See NodePoolCostAnalyzer's own doc comment
// (pkg/analyzer/nodepool_costs.go) for the concurrency-safety consequence
// of that sharing.
//
// Pricing TTL expiry, Cost's Pod-age/idle classifications, and Waste's age
// thresholds are clock-driven rather than generation-driven (docs/08
// §10-11): each provider's own cache checks its TTL lazily on every call,
// making an external HTTP refresh (never a Kubernetes call) whenever it's
// actually due. docs/08 Phase 5's Coordinator clock trigger
// (pkg/clusterstate/coordinator.go's clockInterval) is what guarantees this
// file gets called periodically even on an otherwise-static cluster, so a
// stale price is never served indefinitely just because nothing in
// Kubernetes changed.

// buildCostAnalysis adapts one ClusterSnapshot generation's Nodes/Pods into
// the complete report pipeline. npa is this cluster's persistent Cost runtime
// (dashboardState.costAnalyzer), so provider caches retain their independent
// wall-clock TTL across generations.
func buildCostAnalysis(npa *analyzer.NodePoolCostAnalyzer, ctx string, resources clusterstate.ClusterResources) *models.CloudCostReport {
	nodes := snapshotResourceCopy(resources.Nodes)
	pods := snapshotResourceCopy(resources.Pods)
	result := npa.AnalyzeNodePoolCostResultFromResources(nodes, pods)
	resourceAnalysis := analyzer.AnalyzeResources(pods, nodes, namespace)

	return buildCloudCostReport(ctx, result, resourceAnalysis, costPodsInNamespace(pods, namespace))
}

// buildCloudCostReport is shared by the snapshot and temporary legacy paths
// so both preserve one field-for-field report contract during coexistence.
// Every input is already acquired; only the pricing result may contain
// independently cached external-provider evidence.
func buildCloudCostReport(
	ctx string,
	result analyzer.NodePoolCostResult,
	resourceAnalysis *models.ClusterResourceAnalysis,
	pods []corev1.Pod,
) *models.CloudCostReport {
	totalNodeCost := analyzer.TotalClusterCostFromPools(result.PoolCosts)
	canonicalAllocation := analyzer.BuildCanonicalAllocationFromSnapshots(
		result.PoolCosts,
		result.NodeInfos,
		pods,
	)
	costEstimate, _ := analyzer.NewCostAnalyzer(resourceAnalysis).AnalyzeCosts(totalNodeCost)

	detectedRegion := region
	if detectedRegion == "" {
		detectedRegion = result.Region
	}
	if region == "" && result.Provider == analyzer.CloudProviderMixed {
		detectedRegion = "Multiple regions"
	}
	if detectedRegion == "" {
		detectedRegion = "Not detected"
	}

	matchedNodes, totalNodes := 0, 0
	for _, pool := range result.PoolCosts {
		totalNodes += pool.NodeCount
		if pool.PricingAvailable {
			matchedNodes += pool.NodeCount
		}
	}
	coverage := fmt.Sprintf("%d of %d nodes priced", matchedNodes, totalNodes)
	source := "Pricing unavailable"
	switch result.Provider {
	case analyzer.CloudProviderAzure:
		source = analyzer.NewAzurePricingProvider().SourceDescription()
	case analyzer.CloudProviderAWS:
		if pricingSource == "aws-api" {
			source = "AWS Price List Query API public EC2 On-Demand pricing"
		}
	case analyzer.CloudProviderMixed:
		source = "Provider-specific sources; unsupported nodes remain unavailable"
	}
	if result.Provider != analyzer.CloudProviderAzure {
		costEstimate.OptimizationScenarios = nil
		costEstimate.TotalSavingsPotential = models.CostRange{}
	}

	provider := string(result.Provider)
	return &models.CloudCostReport{
		Timestamp:                time.Now(),
		ClusterName:              displayName(ctx),
		Region:                   detectedRegion,
		Provider:                 provider,
		DetectedProvider:         string(result.DetectedProvider),
		EffectiveProvider:        provider,
		ProviderDetectionMode:    result.ProviderDetectionMode,
		ProviderWarning:          result.ProviderWarning,
		NodePoolCosts:            result.PoolCosts,
		TotalNodeCost:            totalNodeCost,
		NamespaceCosts:           canonicalAllocation.Namespaces,
		AllocatedNodeCost:        canonicalAllocation.AllocatedMonthly,
		IdleNodeCost:             canonicalAllocation.IdleMonthly,
		UnallocatedNodeCost:      canonicalAllocation.UnallocatedMonthly,
		AllocationExcludedPods:   canonicalAllocation.ExcludedPodCount,
		AllocationUnresolvedPods: canonicalAllocation.UnresolvedPodCount,
		TotalMonthlyCost:         totalNodeCost,
		TotalAnnualCost:          totalNodeCost * 12,
		CostBreakdown:            models.CostBreakdown{Compute: totalNodeCost},
		OptimizationScenarios:    costEstimate.OptimizationScenarios,
		TotalSavingsPotential:    costEstimate.TotalSavingsPotential,
		PricingSource:            source,
		PricingCoverage:          coverage,
		PricingWarnings: pricingCoverageWarnings(
			append(append([]string(nil), result.PricingWarnings...), canonicalAllocation.Warnings...),
			matchedNodes, totalNodes),
		PricingCapabilities: result.PricingCapabilities,
		Currency:            "USD",
		ScopeExclusions:     providerScopeExclusions(result.Provider),
		LastPriceRefresh:    result.LastPriceRefresh,
		Assumptions:         []string{"Resolved worker-node compute is allocated from observed Pod CPU/memory requests; unused capacity remains explicit idle cost."},
		Disclaimers:         []string{"Public/list pricing estimates are not invoice values."},
	}
}

func costPodsInNamespace(pods []corev1.Pod, namespace string) []corev1.Pod {
	if namespace == "" {
		return pods
	}
	scoped := make([]corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if pod.Namespace == namespace {
			scoped = append(scoped, pod)
		}
	}
	return scoped
}
