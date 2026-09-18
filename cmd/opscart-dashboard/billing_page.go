package main

import (
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/billing"
)

// billingPageData is the Cost page's Azure billing view, built once per
// render directly from a billing.Snapshot already cached by that cluster's
// billing.Runtime (billing_runtime.go) — this function never triggers a
// network call itself, satisfying "no billing API calls during page
// rendering." Configured is false whenever the cluster has no billing
// configuration at all, in which case the Cost page renders exactly as it
// did before this feature existed.
type billingPageData struct {
	Configured  bool
	Status      string
	StatusLabel string

	Total          float64
	Currency       string
	CostBasisLabel string
	PeriodLabel    string
	Source         string
	Scope          string
	Coverage       string
	Disclosures    []string

	Stale             bool
	UnavailableReason string
	// LastSuccess is when the last SUCCESSFUL refresh completed — zero if
	// billing has never once succeeded. LastAttempt is when the most
	// recent refresh attempt finished, success or failure, so a viewer can
	// tell "billing has been failing for days" from "billing just
	// refreshed" instead of seeing only one ambiguous timestamp.
	LastSuccess time.Time
	LastAttempt time.Time
}

func buildBillingPageData(snapshot billing.Snapshot, configured bool) billingPageData {
	data := billingPageData{Configured: configured, Status: string(snapshot.Status), StatusLabel: billingStatusLabel(snapshot.Status)}
	if !configured {
		return data
	}
	data.Stale = snapshot.Stale
	data.UnavailableReason = snapshot.UnavailableReason
	data.LastSuccess = snapshot.RetrievedAt
	data.LastAttempt = snapshot.LastAttemptedAt

	switch snapshot.Status {
	case billing.StatusAvailable, billing.StatusStale, billing.StatusNoData:
		data.Total = snapshot.Total
		data.Currency = snapshot.Currency
		data.CostBasisLabel = billingCostBasisLabel(snapshot.CostBasis)
		data.PeriodLabel = billingPeriodLabel(snapshot.PeriodStart, snapshot.PeriodEnd)
		data.Source = snapshot.Source
		data.Scope = snapshot.Scope
		data.Coverage = snapshot.Coverage
		data.Disclosures = snapshot.Disclosures
	}
	return data
}

func billingStatusLabel(status billing.Status) string {
	switch status {
	case billing.StatusAvailable:
		return "Live"
	case billing.StatusStale:
		return "Stale"
	case billing.StatusNoData:
		return "No billed usage"
	case billing.StatusUnavailable:
		return "Unavailable"
	default:
		return "Disabled"
	}
}

func billingCostBasisLabel(basis billing.CostBasis) string {
	if basis == billing.CostBasisAmortizedCost {
		return "Amortized cost"
	}
	return "Actual cost"
}

// billingPeriodLabel formats the exact queried dates, never as a "/month"
// figure — a partial-period billing total must never be presented as a
// monthly run rate.
func billingPeriodLabel(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return ""
	}
	return start.Format("2006-01-02") + " to " + end.Format("2006-01-02")
}
