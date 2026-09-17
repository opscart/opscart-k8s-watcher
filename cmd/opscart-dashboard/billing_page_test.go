package main

import (
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/billing"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func TestBuildBillingPageDataNotConfigured(t *testing.T) {
	data := buildBillingPageData(billing.Snapshot{}, false)
	if data.Configured {
		t.Error("Configured = true, want false")
	}
	if data.Total != 0 || data.Currency != "" {
		t.Error("unconfigured billing must not report any total/currency")
	}
}

func TestBuildBillingPageDataAvailable(t *testing.T) {
	snap := billing.Snapshot{
		Status: billing.StatusAvailable, Total: 7345, Currency: "USD",
		CostBasis:   billing.CostBasisActualCost,
		PeriodStart: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC),
		Source:      "Azure Cost Management API", Coverage: "resource groups X and Y",
		RetrievedAt: time.Date(2026, 9, 17, 6, 0, 0, 0, time.UTC),
	}
	data := buildBillingPageData(snap, true)
	if !data.Configured || data.StatusLabel != "Live" {
		t.Fatalf("data = %+v", data)
	}
	if data.Total != 7345 || data.Currency != "USD" {
		t.Errorf("Total/Currency = %v %v", data.Total, data.Currency)
	}
	if data.PeriodLabel != "2026-08-17 to 2026-09-15" {
		t.Errorf("PeriodLabel = %q", data.PeriodLabel)
	}
	if data.CostBasisLabel != "Actual cost" {
		t.Errorf("CostBasisLabel = %q", data.CostBasisLabel)
	}
}

func TestBuildBillingPageDataStaleRetainsPriorTotal(t *testing.T) {
	snap := billing.Snapshot{Status: billing.StatusStale, Stale: true, Total: 100, Currency: "USD", UnavailableReason: "throttled"}
	data := buildBillingPageData(snap, true)
	if !data.Stale || data.Total != 100 {
		t.Errorf("data = %+v", data)
	}
	if data.UnavailableReason == "" {
		t.Error("expected UnavailableReason to be surfaced for a stale snapshot")
	}
}

func TestBuildBillingPageDataUnavailableNeverReportsZeroAsCost(t *testing.T) {
	snap := billing.Snapshot{Status: billing.StatusUnavailable, UnavailableReason: "authentication failed"}
	data := buildBillingPageData(snap, true)
	if data.Total != 0 {
		t.Errorf("Total = %v", data.Total)
	}
	// Total is legitimately the zero value here, but StatusLabel must make
	// clear this is "unavailable," not a real $0 result — the cost.html
	// template branches on Status, never on Total, to decide what to show.
	if data.StatusLabel != "Unavailable" {
		t.Errorf("StatusLabel = %q, want Unavailable", data.StatusLabel)
	}
}

func TestRenderCostPageShowsBillingWhenConfiguredAndAvailable(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{
		Timestamp: time.Now(), ClusterName: "rxr-rxp-e2e-01-cus-aks", Provider: "azure", Region: "centralus",
		Currency: "USD",
	}}
	snap := billing.Snapshot{
		Status: billing.StatusAvailable, Total: 7345.12, Currency: "USD",
		CostBasis:   billing.CostBasisActualCost,
		PeriodStart: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC),
		Source:      "Azure Cost Management API (Query - Usage, resource-group scope)",
		Coverage:    "Resource-level Azure billing for resource groups X and Y",
	}
	html := renderCostPage(scan, "", []string{""}, snap, true)
	if !strings.Contains(html, "Azure billing") {
		t.Fatal("billing section not rendered")
	}
	if !strings.Contains(html, "2026-08-17 to 2026-09-15") {
		t.Error("exact billing period dates not rendered")
	}
	if !strings.Contains(html, "Current retail node run-rate (estimate)") {
		t.Error("estimate hero was not relabeled")
	}
	if strings.Contains(html, "Estimate-only mode") {
		t.Error("estimate-only disclosure must not render when billing is configured")
	}
}

func TestRenderCostPageEstimateOnlyModeWhenNotConfigured(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{Timestamp: time.Now(), ClusterName: "aks", Currency: "USD"}}
	html := renderCostPage(scan, "", []string{""}, billing.Snapshot{}, false)
	if !strings.Contains(html, "Estimate-only mode") {
		t.Error("expected the estimate-only disclosure when billing is not configured")
	}
	if strings.Contains(html, `<div class="hero-kicker">Azure billing`) {
		t.Error("billing hero must not render when not configured")
	}
}
