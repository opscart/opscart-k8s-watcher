package main

import (
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func f64ptr(v float64) *float64 { return &v }

func simulationPassedRecommendation() analyzer.NodeOptimizationRecommendation {
	return analyzer.NodeOptimizationRecommendation{
		Status:                        analyzer.NodeOptimizationRecommendationSimulationPassed,
		Summary:                       "Scheduling feasibility proved under the supported model.",
		PoolKey:                       analyzer.CostPoolKey{PoolName: "workers"},
		CandidateRemovedNode:          "node-3",
		CurrentNodeCount:              6,
		CandidateNodeCount:            5,
		PodsConsidered:                18,
		PodsAssigned:                  18,
		RetainedCPUCapacityMilli:      40000,
		RetainedMemoryCapacityBytes:   80 * 1024 * 1024 * 1024,
		AggregateCPURequestedMilli:    26400,
		AggregateMemoryRequestedBytes: 56 * 1024 * 1024 * 1024,
		CPUHeadroomMilli:              13600,
		MemoryHeadroomBytes:           24 * 1024 * 1024 * 1024,
		CPUHeadroomPercent:            f64ptr(34),
		MemoryHeadroomPercent:         f64ptr(29),
		VerifiedChecks: []analyzer.NodeOptimizationVerifiedCheck{
			analyzer.NodeOptimizationCheckCPUCapacity,
			analyzer.NodeOptimizationCheckMemoryCapacity,
			analyzer.NodeOptimizationCheckPodResourceRequests,
		},
		Assignments: []analyzer.NodeOptimizationRecommendationAssignment{
			{Namespace: "orders", PodName: "orders-api-84bc", SourceNode: "node-3", DestinationNode: "node-4"},
			{Namespace: "payments", PodName: "payments-api-7f8d", SourceNode: "node-3", DestinationNode: "node-1"},
			{Namespace: "payments", PodName: "worker-6f49", SourceNode: "node-3", DestinationNode: "node-2"},
		},
		Caveats:      []string{"read-only simulation; execution safety was not evaluated"},
		NotEvaluated: []string{"drain or eviction execution", "PodDisruptionBudget and disruption timing"},
		Evidence:     analyzer.NodeOptimizationRecommendationEvidence{Complete: true},
	}
}

func blockedRecommendation() analyzer.NodeOptimizationRecommendation {
	return analyzer.NodeOptimizationRecommendation{
		Status:               analyzer.NodeOptimizationRecommendationBlocked,
		Summary:              "The candidate does not satisfy a supported hard constraint under the modeled checks.",
		PoolKey:              analyzer.CostPoolKey{PoolName: "workers"},
		CandidateRemovedNode: "node-3",
		CurrentNodeCount:     6,
		CandidateNodeCount:   5,
		PodsConsidered:       18,
		VerifiedChecks: []analyzer.NodeOptimizationVerifiedCheck{
			analyzer.NodeOptimizationCheckCPUCapacity,
		},
		Blockers: []analyzer.NodeOptimizationRecommendationReason{
			{Code: "insufficient_cpu", Node: "node-3", Message: "insufficient CPU after consolidation"},
			{Code: "no_eligible_destination", Pod: "payments/worker-6f49", Node: "node-3", Message: "no eligible destination for pod payments/worker-6f49"},
		},
		NotEvaluated: []string{"drain or eviction execution"},
		Evidence:     analyzer.NodeOptimizationRecommendationEvidence{Complete: true},
	}
}

func partialRecommendation() analyzer.NodeOptimizationRecommendation {
	return analyzer.NodeOptimizationRecommendation{
		Status:               analyzer.NodeOptimizationRecommendationPartial,
		Summary:              "Scheduling feasibility could not be proved or rejected because required evidence or semantics were incomplete.",
		PoolKey:              analyzer.CostPoolKey{PoolName: "gpu-workers"},
		CandidateRemovedNode: "node-9",
		CurrentNodeCount:     3,
		CandidateNodeCount:   2,
		Blockers: []analyzer.NodeOptimizationRecommendationReason{
			{Code: "pvc_storage_mobility_unproven", Message: "CSI-backed storage mobility not modeled"},
		},
		NotEvaluated: []string{"drain or eviction execution"},
		Evidence:     analyzer.NodeOptimizationRecommendationEvidence{Complete: false, EvidenceIncomplete: true},
	}
}

func observationRecommendation(poolName string) analyzer.NodeOptimizationRecommendation {
	return analyzer.NodeOptimizationRecommendation{
		Status:       analyzer.NodeOptimizationRecommendationObservation,
		Summary:      "No same-shape N-1 candidate was evaluated.",
		PoolKey:      analyzer.CostPoolKey{PoolName: poolName},
		NotEvaluated: []string{"drain or eviction execution"},
		Evidence:     analyzer.NodeOptimizationRecommendationEvidence{Complete: true},
	}
}

func scanWithRecommendations(recs ...analyzer.NodeOptimizationRecommendation) *clusterScan {
	return &clusterScan{
		report:           &models.CloudCostReport{ClusterName: "prod-eastus"},
		nodeOptimization: recs,
	}
}

func scanWithRecommendationsAndSavings(
	recs []analyzer.NodeOptimizationRecommendation,
	savings []analyzer.NodeOptimizationSavingsProjection,
) *clusterScan {
	return &clusterScan{
		report:                  &models.CloudCostReport{ClusterName: "prod-eastus"},
		nodeOptimization:        recs,
		nodeOptimizationSavings: savings,
	}
}

// 1. SIMULATION_PASSED rendering.
func TestNodeOptimizationPage_SimulationPassed(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(simulationPassedRecommendation()), "prod-eastus", []string{"prod-eastus"})
	for _, want := range []string{
		"SIMULATION PASSED",
		"6 nodes",
		"5 nodes",
		"node-3",
		"18 / 18",
		"Scheduling feasibility proved under the supported model.",
		"No cluster changes were made.",
		"34%",
		"29%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("SIMULATION_PASSED page missing %q", want)
		}
	}
}

// 2. BLOCKED rendering.
func TestNodeOptimizationPage_Blocked(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(blockedRecommendation()), "prod-eastus", []string{"prod-eastus"})
	for _, want := range []string{
		"BLOCKED",
		"OpsCart found a supported hard constraint that prevents this consolidation candidate.",
		"insufficient CPU after consolidation",
		"no eligible destination for pod payments/worker-6f49",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("BLOCKED page missing %q", want)
		}
	}
	if strings.Contains(out, `class="no-status-badge no-status-passed"`) {
		t.Error("BLOCKED page unexpectedly rendered a passed status badge")
	}
}

// 3. PARTIAL rendering.
func TestNodeOptimizationPage_Partial(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(partialRecommendation()), "prod-eastus", []string{"prod-eastus"})
	for _, want := range []string{
		"PARTIAL",
		"OpsCart cannot prove this consolidation candidate with the available evidence.",
		"CSI-backed storage mobility not modeled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PARTIAL page missing %q", want)
		}
	}
	if strings.Contains(out, `class="no-hero no-hero-blocked"`) {
		t.Error("PARTIAL page should not use the blocked/danger visual treatment")
	}
}

// 4. OBSERVATION rendering.
func TestNodeOptimizationPage_Observation(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(observationRecommendation("system")), "prod-eastus", []string{"prod-eastus"})
	if !strings.Contains(out, "No same-shape N-1 candidate was evaluated.") {
		t.Error("OBSERVATION page missing calm summary text")
	}
	for _, forbidden := range []string{"error", "Error", "failed"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("OBSERVATION page should not read as an error state, found %q", forbidden)
		}
	}
}

// 5. Verified checks render only when present.
func TestNodeOptimizationPage_VerifiedChecksOnlyWhenPresent(t *testing.T) {
	withChecks := simulationPassedRecommendation()
	withoutChecks := observationRecommendation("system")
	withoutChecks.VerifiedChecks = nil

	out := renderNodeOptimizationPage(scanWithRecommendations(withChecks, withoutChecks), "prod-eastus", []string{"prod-eastus"})
	if !strings.Contains(out, "CPU capacity") {
		t.Error("expected verified check label to render for the pool that reported it")
	}

	soloOut := renderNodeOptimizationPage(scanWithRecommendations(withoutChecks), "prod-eastus", []string{"prod-eastus"})
	if strings.Contains(soloOut, "What OpsCart verified") {
		t.Error("verified-checks section should not render when the backend reported no checks")
	}
}

// 6. NotEvaluated always renders.
func TestNodeOptimizationPage_NotEvaluatedAlwaysRenders(t *testing.T) {
	for name, rec := range map[string]analyzer.NodeOptimizationRecommendation{
		"passed":      simulationPassedRecommendation(),
		"blocked":     blockedRecommendation(),
		"partial":     partialRecommendation(),
		"observation": observationRecommendation("system"),
	} {
		out := renderNodeOptimizationPage(scanWithRecommendations(rec), "prod-eastus", []string{"prod-eastus"})
		if !strings.Contains(out, "Not evaluated") || !strings.Contains(out, "drain or eviction execution") {
			t.Errorf("%s: expected Not Evaluated section to always render", name)
		}
	}
}

// 7. Assignments render deterministically (order preserved, no duplicates, exact total shown).
func TestNodeOptimizationPage_AssignmentsRenderDeterministically(t *testing.T) {
	rec := simulationPassedRecommendation()
	out := renderNodeOptimizationPage(scanWithRecommendations(rec), "prod-eastus", []string{"prod-eastus"})

	idxOrders := strings.Index(out, "orders-api-84bc")
	idxPaymentsAPI := strings.Index(out, "payments-api-7f8d")
	idxWorker := strings.Index(out, "worker-6f49")
	if idxOrders == -1 || idxPaymentsAPI == -1 || idxWorker == -1 {
		t.Fatalf("expected all assignment rows to render, got: %s", out)
	}
	if !(idxOrders < idxPaymentsAPI && idxPaymentsAPI < idxWorker) {
		t.Errorf("assignment rows did not render in the analyzer's deterministic order")
	}
	if !strings.Contains(out, "Placement evidence (3)") {
		t.Error("expected exact assignment total to be shown")
	}
	// Rendering twice must be byte-identical (deterministic, no map iteration leakage).
	out2 := renderNodeOptimizationPage(scanWithRecommendations(simulationPassedRecommendation()), "prod-eastus", []string{"prod-eastus"})
	if out != out2 {
		t.Error("expected identical renders for identical recommendation data")
	}
}

// 8. No ACTIONABLE/safe-to-remove wording anywhere on the page, in any state.
func TestNodeOptimizationPage_NoActionableWording(t *testing.T) {
	forbidden := []string{
		"ACTIONABLE", "Actionable", "Safe to remove", "safe to remove",
		"Ready to delete", "Terminate now", "Guaranteed", "Migration ready",
	}
	for name, scan := range map[string]*clusterScan{
		"passed":      scanWithRecommendations(simulationPassedRecommendation()),
		"blocked":     scanWithRecommendations(blockedRecommendation()),
		"partial":     scanWithRecommendations(partialRecommendation()),
		"observation": scanWithRecommendations(observationRecommendation("system")),
	} {
		out := renderNodeOptimizationPage(scan, "prod-eastus", []string{"prod-eastus"})
		for _, word := range forbidden {
			if strings.Contains(out, word) {
				t.Errorf("%s: page contains forbidden wording %q", name, word)
			}
		}
	}
}

// 9. No fabricated monetary value rendered without connected pricing data;
// the page must say pricing is unavailable rather than show a dollar figure
// (never $0, never any invented amount) when no NodeOptimizationSavingsProjection
// is wired in for a pool.
func TestNodeOptimizationPage_NoFabricatedMonetaryValueWithoutPricing(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(simulationPassedRecommendation(), blockedRecommendation(), partialRecommendation()), "prod-eastus", []string{"prod-eastus"})
	for _, forbidden := range []string{"$0", "$", "/mo"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("page rendered monetary value %q without connected pricing data", forbidden)
		}
	}
	if !strings.Contains(out, "Pricing unavailable") {
		t.Error("expected the page to state pricing is unavailable rather than omit it silently")
	}
	if !strings.Contains(out, "Unavailable") {
		t.Error("expected the multi-pool summary table to show Unavailable savings rather than omit the column")
	}
}

// 9b. Estimated savings render when a NodeOptimizationSavingsProjection is
// connected for a SIMULATION_PASSED pool, and stay "Unavailable" for a pool
// with no connected projection — proving pricing is joined per-pool, not
// applied blanket across the page.
func TestNodeOptimizationPage_RendersConnectedSavings(t *testing.T) {
	savings := f64ptr(182)
	current := f64ptr(1092)
	candidate := f64ptr(910)
	unpricedPassed := simulationPassedRecommendation()
	unpricedPassed.PoolKey = analyzer.CostPoolKey{PoolName: "system"}
	scan := scanWithRecommendationsAndSavings(
		[]analyzer.NodeOptimizationRecommendation{simulationPassedRecommendation(), unpricedPassed},
		[]analyzer.NodeOptimizationSavingsProjection{
			{
				Available:               true,
				EstimatedMonthlySavings: savings,
				CurrentMonthlyCost:      current,
				CandidateMonthlyCost:    candidate,
				Currency:                "USD",
				PricingCoverage:         analyzer.NodeOptimizationPricingCoverageExact,
				Provider:                "azure",
			},
			{
				PricingCoverage:   analyzer.NodeOptimizationPricingCoverageUnavailable,
				ReasonUnavailable: "no priced Cost Intelligence pool matches this pool identity",
			},
		},
	)
	out := renderNodeOptimizationPage(scan, "prod-eastus", []string{"prod-eastus"})

	for _, want := range []string{"$182/mo", "$1,092/mo", "$910/mo"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected connected savings pool to render %q", want)
		}
	}
	if !strings.Contains(out, "no priced Cost Intelligence pool matches this pool identity") {
		t.Error("expected the unpriced pool to surface its pricing-unavailable reason")
	}
}

// 10. Multiple pools render independently, including PARTIAL pools (never hidden).
func TestNodeOptimizationPage_MultiplePoolsIndependent(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(
		simulationPassedRecommendation(),
		partialRecommendation(),
		observationRecommendation("system"),
	), "prod-eastus", []string{"prod-eastus"})

	for _, want := range []string{"workers", "gpu-workers", "system", "SIMULATION PASSED", "PARTIAL", "OBSERVATION"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected multi-pool page to contain %q", want)
		}
	}
	if !strings.Contains(out, `id="pool-0"`) || !strings.Contains(out, `id="pool-1"`) || !strings.Contains(out, `id="pool-2"`) {
		t.Error("expected each pool to render its own anchored section")
	}
}

// 11. Cluster query propagation on Node Optimization links.
func TestNodeOptimizationPage_ClusterQueryPropagation(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(simulationPassedRecommendation()), "prod-eastus", []string{"prod-eastus"})
	if !strings.Contains(out, `href="/node-optimization?cluster=prod-eastus"`) {
		t.Error("expected the Node Optimization sidebar link to propagate the active cluster")
	}
}

// 12. Empty state does not panic.
func TestNodeOptimizationPage_EmptyStateNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("empty state panicked: %v", r)
		}
	}()
	out := renderNodeOptimizationPage(nil, "prod-eastus", []string{"prod-eastus"})
	if !strings.Contains(out, "No proven consolidation candidate is available for this pool.") {
		t.Error("expected calm empty state for a nil scan")
	}

	out2 := renderNodeOptimizationPage(&clusterScan{}, "prod-eastus", []string{"prod-eastus"})
	if !strings.Contains(out2, "No proven consolidation candidate is available for this pool.") {
		t.Error("expected calm empty state for a scan with no recommendations")
	}
}

// 13. Template handles nil/empty optional slices without panicking or leaving artifacts.
func TestNodeOptimizationPage_HandlesNilOptionalSlices(t *testing.T) {
	rec := analyzer.NodeOptimizationRecommendation{
		Status:  analyzer.NodeOptimizationRecommendationSimulationPassed,
		Summary: "Scheduling feasibility proved under the supported model.",
		PoolKey: analyzer.CostPoolKey{PoolName: "workers"},
		// Assignments, VerifiedChecks, Blockers, Caveats, NotEvaluated left nil.
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil optional slices panicked: %v", r)
		}
	}()
	out := renderNodeOptimizationPage(scanWithRecommendations(rec), "prod-eastus", []string{"prod-eastus"})
	if strings.Contains(out, "Placement evidence") {
		t.Error("assignment evidence section should not render when there are no assignments")
	}
	if strings.Contains(out, "What OpsCart verified") {
		t.Error("verified-checks section should not render when nil")
	}
	if strings.Contains(out, "Caveats") {
		t.Error("caveats section should not render when nil")
	}
}

// 14. Node Optimization Overview card updates only if real data/page is connected.
func TestOverviewNodeOptimizationCard_UpdatesOnlyWhenConnected(t *testing.T) {
	t.Run("no recommendation data leaves the stub copy", func(t *testing.T) {
		data := fullyPopulatedOverviewData()
		data.NodeOptimizationAvailable = false
		var buf strings.Builder
		if err := getOverviewTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("template execution failed: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "In development") {
			t.Error("expected conservative copy when no recommendation data is connected")
		}
		if strings.Contains(out, "Open simulation") {
			t.Error("should not advertise the simulation workflow without connected data")
		}
	})

	t.Run("real recommendation data unlocks the connected copy", func(t *testing.T) {
		data := fullyPopulatedOverviewData()
		data.NodeOptimizationAvailable = true
		var buf strings.Builder
		if err := getOverviewTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("template execution failed: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"Available", "Read-only consolidation simulation", "Open simulation"} {
			if !strings.Contains(out, want) {
				t.Errorf("expected connected Node Optimization card to contain %q", want)
			}
		}
		if strings.Contains(out, "No operator action yet") {
			t.Error("stub copy should not remain once real data is connected")
		}
	})

	t.Run("buildOverviewData reflects whether the scan produced recommendations", func(t *testing.T) {
		empty := buildOverviewData(&clusterScan{report: &models.CloudCostReport{}}, "test", []string{"test"}, nil, time.Now())
		if empty.NodeOptimizationAvailable {
			t.Error("expected NodeOptimizationAvailable=false when the scan has no recommendations")
		}
		connected := buildOverviewData(scanWithRecommendations(observationRecommendation("system")), "test", []string{"test"}, nil, time.Now())
		if !connected.NodeOptimizationAvailable {
			t.Error("expected NodeOptimizationAvailable=true when the scan produced recommendation data")
		}
	})
}
