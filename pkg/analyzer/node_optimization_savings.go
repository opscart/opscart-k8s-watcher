package analyzer

import (
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// NodeOptimizationPricingCoverage records why a savings projection does or
// does not carry a monetary value.
type NodeOptimizationPricingCoverage string

const (
	// NodeOptimizationPricingCoverageExact means the projection's monthly
	// savings figure is the exact provider-backed price of one removed node,
	// already computed by Cost Intelligence during this scan.
	NodeOptimizationPricingCoverageExact NodeOptimizationPricingCoverage = "exact_provider_price"
	// NodeOptimizationPricingCoverageUnavailable means no monetary value is
	// shown. It never means the savings are zero.
	NodeOptimizationPricingCoverageUnavailable NodeOptimizationPricingCoverage = "unavailable"
)

// NodeOptimizationSavingsProjection is a read-only join between one pool's
// N-1 recommendation and Cost Intelligence's already-computed provider
// pricing for that pool. It performs no scheduling, Kubernetes, or cloud/
// pricing-provider API work of its own; it only reads pricing evidence that
// Cost Intelligence already produced during this scan.
//
// A missing or incomplete price is never converted to zero: Available is
// false and ReasonUnavailable explains why, while EstimatedMonthlySavings
// and the cost totals stay nil.
type NodeOptimizationSavingsProjection struct {
	Available bool

	CurrentMonthlyCost      *float64
	CandidateMonthlyCost    *float64
	EstimatedMonthlySavings *float64

	Currency string

	PricingCoverage NodeOptimizationPricingCoverage
	Provider        string

	ReasonUnavailable string
}

// nodeOptimizationPricingMatchKey is the canonical join tuple between a
// recommendation's CostPoolKey and a priced models.NodePoolCost. It mirrors
// the matching rules already used by BuildCanonicalAllocationFromSnapshots
// (cost_allocation_live.go): Architecture is intentionally excluded because
// Cost Intelligence pricing is not resolved per architecture, pool name and
// instance type match verbatim, and provider/capacity type/region/OS match
// case-insensitively.
type nodeOptimizationPricingMatchKey struct {
	provider, pool, instance, capacity, region, os string
}

func nodeOptimizationPricingKeyFromCostPoolKey(key CostPoolKey) nodeOptimizationPricingMatchKey {
	return nodeOptimizationPricingMatchKey{
		provider: strings.ToLower(strings.TrimSpace(key.Provider)),
		pool:     strings.TrimSpace(key.PoolName),
		instance: strings.TrimSpace(key.InstanceType),
		capacity: strings.ToLower(strings.TrimSpace(key.CapacityType)),
		region:   strings.ToLower(strings.TrimSpace(key.Region)),
		os:       strings.ToLower(strings.TrimSpace(key.OS)),
	}
}

func nodeOptimizationPricingKeyFromPoolCost(pool models.NodePoolCost) nodeOptimizationPricingMatchKey {
	return nodeOptimizationPricingMatchKey{
		provider: strings.ToLower(strings.TrimSpace(pool.Provider)),
		pool:     strings.TrimSpace(pool.Name),
		instance: strings.TrimSpace(pool.VMSize),
		capacity: strings.ToLower(strings.TrimSpace(pool.Priority)),
		region:   strings.ToLower(strings.TrimSpace(pool.Region)),
		os:       strings.ToLower(strings.TrimSpace(pool.OS)),
	}
}

// BuildNodeOptimizationSavingsProjections joins each recommendation's pool
// identity to already-priced Cost Intelligence pool evidence (poolCosts, as
// produced by NodePoolCostAnalyzer.AnalyzeNodePoolCosts during this same
// scan) and returns one projection per recommendation, aligned by index. It
// performs no Kubernetes, cloud, or pricing-provider API calls, never
// derives a price from node count alone, never averages partially priced
// nodes, and never substitutes a missing price with zero or a static
// fallback table.
func BuildNodeOptimizationSavingsProjections(
	recommendations []NodeOptimizationRecommendation,
	poolCosts []models.NodePoolCost,
	currency string,
) []NodeOptimizationSavingsProjection {
	projections := make([]NodeOptimizationSavingsProjection, len(recommendations))
	for i, rec := range recommendations {
		projections[i] = BuildNodeOptimizationSavingsProjection(rec, poolCosts, currency)
	}
	return projections
}

// BuildNodeOptimizationSavingsProjection projects the monthly savings of one
// recommendation. SIMULATION_PASSED does not require pricing: when an exact
// provider-backed price cannot be joined, the recommendation itself remains
// unaffected and only the returned projection reports Available: false.
func BuildNodeOptimizationSavingsProjection(
	recommendation NodeOptimizationRecommendation,
	poolCosts []models.NodePoolCost,
	currency string,
) NodeOptimizationSavingsProjection {
	unavailable := func(reason string) NodeOptimizationSavingsProjection {
		return NodeOptimizationSavingsProjection{
			PricingCoverage:   NodeOptimizationPricingCoverageUnavailable,
			Provider:          recommendation.PoolKey.Provider,
			ReasonUnavailable: reason,
		}
	}

	if recommendation.Status != NodeOptimizationRecommendationSimulationPassed {
		return unavailable("estimated savings are only projected for a SIMULATION_PASSED recommendation")
	}
	if recommendation.CandidateRemovedNode == "" {
		return unavailable("candidate removed node identity is missing")
	}
	if missing := missingCostPoolKeyFields(recommendation.PoolKey); len(missing) > 0 {
		return unavailable("pool identity is incomplete: " + strings.Join(missing, ", "))
	}

	matches := matchingNodePoolCosts(recommendation.PoolKey, poolCosts)
	if len(matches) == 0 {
		return unavailable("no priced Cost Intelligence pool matches this pool's provider, instance type, capacity type, region, and operating system")
	}
	if len(matches) > 1 {
		return unavailable("pool identity matched more than one priced Cost Intelligence pool; the join is ambiguous")
	}

	pool := matches[0]
	if !pool.PricingAvailable || pool.PricePerNodeMonth <= 0 {
		reason := pool.PricingWarning
		if reason == "" {
			reason = "the matched Cost Intelligence pool has no exact provider price"
		}
		return unavailable(reason)
	}

	savings := pool.PricePerNodeMonth
	projection := NodeOptimizationSavingsProjection{
		Available:               true,
		EstimatedMonthlySavings: &savings,
		Currency:                currency,
		PricingCoverage:         NodeOptimizationPricingCoverageExact,
		Provider:                recommendation.PoolKey.Provider,
	}

	// The full current/candidate pool total is only meaningful when the
	// priced pool's node count reconciles with the node count this
	// recommendation actually simulated. A mismatch means the priced pool
	// total does not describe the same nodes being simulated, so the totals
	// stay unavailable even though the single removed node's price is exact.
	if pool.NodeCount > 0 && pool.NodeCount == recommendation.CurrentNodeCount {
		current := pool.TotalMonthly
		candidate := current - savings
		projection.CurrentMonthlyCost = &current
		projection.CandidateMonthlyCost = &candidate
	}

	return projection
}

func matchingNodePoolCosts(poolKey CostPoolKey, poolCosts []models.NodePoolCost) []models.NodePoolCost {
	target := nodeOptimizationPricingKeyFromCostPoolKey(poolKey)
	var matches []models.NodePoolCost
	for _, pool := range poolCosts {
		if nodeOptimizationPricingKeyFromPoolCost(pool) == target {
			matches = append(matches, pool)
		}
	}
	return matches
}
