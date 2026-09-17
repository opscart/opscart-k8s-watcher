// Package billing defines a provider-neutral contract for retrieving actual
// cloud billing data for a Kubernetes cluster, separate from
// pkg/analyzer's list-price PricingProvider. A BillingProvider reports what a
// cloud billing API says was actually charged; PricingProvider reports what a
// node currently costs at public/list rates. The two are never conflated.
package billing

import (
	"context"
	"time"
)

// Status distinguishes every outcome a Snapshot can represent. Callers must
// switch on Status rather than infer availability from Total, since a valid
// zero-cost period and an unavailable/no-data period are different facts.
type Status string

const (
	// StatusDisabled means billing is not configured/enabled for this
	// cluster. No refresh has ever been attempted.
	StatusDisabled Status = "disabled"
	// StatusUnavailable means the most recent refresh failed and no prior
	// successful snapshot exists to fall back to.
	StatusUnavailable Status = "unavailable"
	// StatusStale means a refresh failed but a prior successful snapshot is
	// being retained and shown, marked stale.
	StatusStale Status = "stale"
	// StatusNoData means the billing API was queried successfully but
	// returned zero rows for the configured scope and period — distinct
	// from a valid zero cost, where rows were returned summing to zero.
	StatusNoData Status = "no_data"
	// StatusAvailable means the most recent refresh succeeded and returned
	// at least one cost row. Total may still legitimately be zero (e.g. a
	// free-tier meter, or credits exactly offsetting charges).
	StatusAvailable Status = "available"
)

// CostBasis selects which Azure Cost Management query type is used.
type CostBasis string

const (
	CostBasisActualCost    CostBasis = "ActualCost"
	CostBasisAmortizedCost CostBasis = "AmortizedCost"
)

// ResourceCost is one attributed line item: a single Azure resource's summed
// cost for the queried period, from a single query scope (cluster resource or
// node resource group).
type ResourceCost struct {
	ResourceID    string
	ResourceGroup string
	Cost          float64
	Currency      string
}

// Request describes one billing retrieval for a configured cluster.
type Request struct {
	PeriodStart time.Time
	PeriodEnd   time.Time
	CostBasis   CostBasis
}

// Result is what a Provider implementation returns for one successful
// retrieval. It carries enough evidence for the caller to render exact
// dates, currency, cost basis, source, and scope without recomputing them.
type Result struct {
	Total       float64
	Currency    string
	CostBasis   CostBasis
	PeriodStart time.Time
	PeriodEnd   time.Time
	RetrievedAt time.Time

	// Source is a human-readable description of the API/provider used.
	Source string
	// Scope is a human-readable description of what billing scope was
	// queried (e.g. the cluster resource group and node resource group).
	Scope string
	// Coverage explicitly describes what this Result does and does not
	// attribute, so it is never presented as identical to a full invoice.
	Coverage string
	// Disclosures lists shared/external costs or known gaps that could not
	// be attributed to this cluster from the queried scope.
	Disclosures []string

	// Lines is the resource-level breakdown supported by the API. RowCount
	// distinguishes "zero rows returned" (StatusNoData) from "rows returned
	// summing to zero" (StatusAvailable, Total == 0) — RowCount can exceed
	// len(Lines) when duplicate resource IDs across scopes were merged.
	Lines    []ResourceCost
	RowCount int
}

// Provider is the provider-neutral billing contract. Azure is the first
// implementation (AzureProvider); it is deliberately separate from
// pkg/analyzer.PricingProvider.
type Provider interface {
	Name() string
	FetchBilling(ctx context.Context, req Request) (Result, error)
}

// Snapshot is the read-only, in-memory-cached view a dashboard renders. It is
// produced by Runtime from the last successful (or stale) Result and never
// triggers a network call itself.
type Snapshot struct {
	Status Status

	Total       float64
	Currency    string
	CostBasis   CostBasis
	PeriodStart time.Time
	PeriodEnd   time.Time

	Source      string
	Scope       string
	Coverage    string
	Disclosures []string
	RowCount    int

	RetrievedAt       time.Time
	Stale             bool
	UnavailableReason string
}
