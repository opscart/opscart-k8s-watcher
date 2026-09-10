package analyzer

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func workersPoolKey() CostPoolKey {
	return CostPoolKey{
		Provider:     "azure",
		PoolName:     "workers",
		InstanceType: "Standard_D4s_v3",
		CapacityType: "Regular",
		Region:       "eastus",
		OS:           "linux",
		Architecture: "amd64",
	}
}

func passedRecommendationFor(key CostPoolKey, currentNodes int) NodeOptimizationRecommendation {
	return NodeOptimizationRecommendation{
		Status:               NodeOptimizationRecommendationSimulationPassed,
		PoolKey:              key,
		CandidateRemovedNode: "node-3",
		CurrentNodeCount:     currentNodes,
		CandidateNodeCount:   currentNodes - 1,
	}
}

func pricedWorkersPool() models.NodePoolCost {
	return models.NodePoolCost{
		Name:              "workers",
		VMSize:            "Standard_D4s_v3",
		Priority:          "Regular",
		Region:            "eastus",
		OS:                "linux",
		Provider:          "azure",
		NodeCount:         6,
		PricingAvailable:  true,
		PricePerNodeMonth: 182.0,
		TotalMonthly:      1092.0,
	}
}

func TestBuildNodeOptimizationSavingsProjection_ExactPriceWithReconciledTotals(t *testing.T) {
	rec := passedRecommendationFor(workersPoolKey(), 6)
	got := BuildNodeOptimizationSavingsProjection(rec, []models.NodePoolCost{pricedWorkersPool()}, "USD")

	if !got.Available {
		t.Fatalf("Available = false, want true; reason=%q", got.ReasonUnavailable)
	}
	if got.PricingCoverage != NodeOptimizationPricingCoverageExact {
		t.Errorf("PricingCoverage = %q, want %q", got.PricingCoverage, NodeOptimizationPricingCoverageExact)
	}
	if got.EstimatedMonthlySavings == nil || *got.EstimatedMonthlySavings != 182.0 {
		t.Fatalf("EstimatedMonthlySavings = %v, want 182.0", got.EstimatedMonthlySavings)
	}
	if got.CurrentMonthlyCost == nil || *got.CurrentMonthlyCost != 1092.0 {
		t.Fatalf("CurrentMonthlyCost = %v, want 1092.0", got.CurrentMonthlyCost)
	}
	if got.CandidateMonthlyCost == nil || *got.CandidateMonthlyCost != 910.0 {
		t.Fatalf("CandidateMonthlyCost = %v, want 910.0", got.CandidateMonthlyCost)
	}
	if got.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", got.Currency)
	}
}

func TestBuildNodeOptimizationSavingsProjection_SavingsWithoutReconciledTotals(t *testing.T) {
	// The recommendation's observed current node count (5) does not match the
	// priced pool's node count (6): the pool total no longer describes the
	// exact same set of nodes, so totals must stay unavailable even though
	// the single removed node's price is still exact.
	rec := passedRecommendationFor(workersPoolKey(), 5)
	got := BuildNodeOptimizationSavingsProjection(rec, []models.NodePoolCost{pricedWorkersPool()}, "USD")

	if !got.Available {
		t.Fatalf("Available = false, want true; reason=%q", got.ReasonUnavailable)
	}
	if got.EstimatedMonthlySavings == nil || *got.EstimatedMonthlySavings != 182.0 {
		t.Fatalf("EstimatedMonthlySavings = %v, want 182.0", got.EstimatedMonthlySavings)
	}
	if got.CurrentMonthlyCost != nil || got.CandidateMonthlyCost != nil {
		t.Errorf("expected CurrentMonthlyCost/CandidateMonthlyCost to remain nil on node-count mismatch, got %v / %v",
			got.CurrentMonthlyCost, got.CandidateMonthlyCost)
	}
}

func TestBuildNodeOptimizationSavingsProjection_NotSimulationPassedNeverPriced(t *testing.T) {
	for _, status := range []NodeOptimizationRecommendationStatus{
		NodeOptimizationRecommendationBlocked,
		NodeOptimizationRecommendationPartial,
		NodeOptimizationRecommendationObservation,
		NodeOptimizationRecommendationCandidate,
		NodeOptimizationRecommendationPreCheckPassed,
	} {
		rec := passedRecommendationFor(workersPoolKey(), 6)
		rec.Status = status
		got := BuildNodeOptimizationSavingsProjection(rec, []models.NodePoolCost{pricedWorkersPool()}, "USD")
		if got.Available {
			t.Errorf("status %q: Available = true, want false", status)
		}
		if got.EstimatedMonthlySavings != nil {
			t.Errorf("status %q: EstimatedMonthlySavings = %v, want nil", status, got.EstimatedMonthlySavings)
		}
		if got.ReasonUnavailable == "" {
			t.Errorf("status %q: expected a non-empty ReasonUnavailable", status)
		}
	}
}

func TestBuildNodeOptimizationSavingsProjection_NeverZeroOnMissingPrice(t *testing.T) {
	unpriced := pricedWorkersPool()
	unpriced.PricingAvailable = false
	unpriced.PricePerNodeMonth = 0
	unpriced.TotalMonthly = 0
	unpriced.PricingWarning = "pricing is unavailable for provider azure"

	rec := passedRecommendationFor(workersPoolKey(), 6)
	got := BuildNodeOptimizationSavingsProjection(rec, []models.NodePoolCost{unpriced}, "USD")

	if got.Available {
		t.Fatal("Available = true, want false for an unpriced pool")
	}
	if got.EstimatedMonthlySavings != nil {
		t.Errorf("EstimatedMonthlySavings = %v, want nil (never fabricate $0)", got.EstimatedMonthlySavings)
	}
	if got.ReasonUnavailable != "pricing is unavailable for provider azure" {
		t.Errorf("ReasonUnavailable = %q, want the pool's PricingWarning to be surfaced verbatim", got.ReasonUnavailable)
	}
}

func TestBuildNodeOptimizationSavingsProjection_NoMatchingPricedPool(t *testing.T) {
	rec := passedRecommendationFor(workersPoolKey(), 6)
	other := pricedWorkersPool()
	other.VMSize = "Standard_D8s_v3" // different instance type: no join
	got := BuildNodeOptimizationSavingsProjection(rec, []models.NodePoolCost{other}, "USD")

	if got.Available {
		t.Fatal("Available = true, want false when no priced pool matches this pool's identity")
	}
	if got.ReasonUnavailable == "" {
		t.Error("expected a non-empty ReasonUnavailable")
	}
}

func TestBuildNodeOptimizationSavingsProjection_AmbiguousMatchIsUnavailable(t *testing.T) {
	rec := passedRecommendationFor(workersPoolKey(), 6)
	duplicate := pricedWorkersPool()
	got := BuildNodeOptimizationSavingsProjection(rec, []models.NodePoolCost{pricedWorkersPool(), duplicate}, "USD")

	if got.Available {
		t.Fatal("Available = true, want false when the pool identity matches more than one priced pool")
	}
}

func TestBuildNodeOptimizationSavingsProjection_IncompletePoolKeyIsUnavailable(t *testing.T) {
	rec := passedRecommendationFor(CostPoolKey{PoolName: "workers"}, 6)
	got := BuildNodeOptimizationSavingsProjection(rec, []models.NodePoolCost{pricedWorkersPool()}, "USD")

	if got.Available {
		t.Fatal("Available = true, want false when the pool identity is incomplete")
	}
}

func TestBuildNodeOptimizationSavingsProjections_AlignedByIndex(t *testing.T) {
	recs := []NodeOptimizationRecommendation{
		passedRecommendationFor(workersPoolKey(), 6),
		{Status: NodeOptimizationRecommendationBlocked, PoolKey: CostPoolKey{PoolName: "gpu-workers"}},
	}
	got := BuildNodeOptimizationSavingsProjections(recs, []models.NodePoolCost{pricedWorkersPool()}, "USD")

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if !got[0].Available {
		t.Error("projection[0] (workers, SIMULATION_PASSED) should be Available")
	}
	if got[1].Available {
		t.Error("projection[1] (gpu-workers, BLOCKED) should not be Available")
	}
}
