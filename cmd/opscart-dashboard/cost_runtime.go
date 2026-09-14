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
// (Nodes+Pods, previously two unconditional LISTs inside
// analyzer.NodePoolCostAnalyzer.AnalyzeNodePoolCosts every legacy scan
// cycle) migrated off direct Kubernetes acquisition onto the shared
// ClusterSnapshot pipeline — the seventh and final Phase 4D analyzer driven
// by the per-cluster Coordinator. See acquisition_runtime.go's
// runCoordinatedAnalysis for where all seven are invoked from the same
// coalesced generation, and node_optimization_runtime.go for why Cost must
// run before Node Optimization there.
//
// The full models.CloudCostReport is coordinator-owned: pool pricing,
// canonical namespace allocation, and optimization scenarios are all derived
// from the same snapshot generation and published atomically. The legacy
// scan cycle (legacy_analysis.go's runLegacyAnalysis, since docs/08 Phase
// 4E) still builds the same report by calling buildCostAnalysis directly —
// the same function this file exposes to the Coordinator — but refresh's
// generation-preservation guard prevents that legacy result from replacing
// a coordinator-published one. Phase 4E removed the legacy path's own
// Kubernetes acquisition (it used to call
// analyzer.NodePoolCostAnalyzer.AnalyzeNodePoolCostResult(clientset)
// directly) without requiring another Cost analyzer migration, exactly as
// anticipated when this file was written.
//
// Both the legacy and coordinator paths call into the SAME persistent,
// per-cluster analyzer.NodePoolCostAnalyzer (dashboardState.costAnalyzer,
// constructed once in getState) rather than each constructing their own —
// this is also the fix for the known Azure pricing-provider lifetime defect
// (docs/08 Phase 0 finding): a fresh analyzer previously meant a fresh,
// empty-cache Azure provider every legacy scan cycle, so its 24h TTL could
// never actually be reached. Reusing one persistent analyzer instance lets
// that cache — and the AWS provider's pre-existing process-global cache —
// survive across both scan cycles and coordinator generations. See
// NodePoolCostAnalyzer's own doc comment (pkg/analyzer/nodepool_costs.go)
// for the concurrency-safety consequence of that sharing.
//
// Pricing TTL expiry, Cost's Pod-age/idle classifications, and Waste's age
// thresholds are clock-driven rather than generation-driven (docs/08
// §10-11). The legacy timer currently reevaluates them even without a
// Kubernetes event. Once that timer disappears, a later phase must provide a
// clock-driven analysis trigger over the existing snapshot. That trigger must
// not perform Kubernetes LIST acquisition and is deliberately outside 4D.6b.

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

// runCostAnalysis is the Coordinator-facing analysis step for one cluster:
// it gates on acquisition trustworthiness, then computes and publishes a new
// complete Cost report. It never touches incident persistence.
//
// A DEGRADED/RESYNCING/STALE snapshot is not analyzed: state.scan keeps
// showing whatever result (coordinator- or legacy-produced) was last
// published. This is also what keeps an untrustworthy snapshot from ever
// being treated as evidence a Cost result changed — the function simply
// does not run, so it cannot feed a false transition into anything.
func runCostAnalysis(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) {
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

	result := buildCostAnalysis(state.costAnalyzer, state.ctx, snapshot.Resources())
	publishCostAnalysis(state, snapshot.Generation(), result)
}

// publishCostAnalysis replaces state.scan.report via copy-and-swap of the
// whole *clusterScan pointer — the exact mechanism the other six publishers
// use, for the same reasons: every existing reader treats an
// already-published *clusterScan as immutable, and the generation check is
// defense-in-depth documenting an invariant Coordinator's single-threaded
// loop already guarantees structurally.
func publishCostAnalysis(state *dashboardState, generation uint64, report *models.CloudCostReport) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.scan == nil || generation <= state.scan.costGeneration {
		return
	}

	updated := *state.scan
	updated.report = report
	updated.costGeneration = generation
	state.scan = &updated
}
