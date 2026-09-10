package main

import (
	"fmt"
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

func nodeOptimizationPoolCard(t *testing.T, html string, index int) string {
	t.Helper()
	startMarker := fmt.Sprintf(`<div class="no-pool-card" id="pool-%d">`, index)
	start := strings.Index(html, startMarker)
	if start < 0 {
		t.Fatalf("pool %d card was not rendered", index)
	}
	card := html[start:]
	nextMarker := fmt.Sprintf(`<div class="no-pool-card" id="pool-%d">`, index+1)
	if next := strings.Index(card, nextMarker); next >= 0 {
		card = card[:next]
	}
	return card
}

func nodeOptimizationVisibleReasonBody(t *testing.T, card string) string {
	t.Helper()
	details := strings.Index(card, `<details class="no-evidence no-reason-evidence">`)
	if details < 0 {
		t.Fatal("detailed evidence disclosure was not rendered")
	}
	return card[:details]
}

// 1. SIMULATION_PASSED rendering.
func TestNodeOptimizationPage_SimulationPassed(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(simulationPassedRecommendation()), "prod-eastus", []string{"prod-eastus"})
	for _, want := range []string{
		"SIMULATION PASSED",
		`<div class="no-metric-val">6</div><div class="no-metric-lbl">Current nodes</div>`,
		`<div class="no-metric-val">5</div><div class="no-metric-lbl">Candidate nodes</div>`,
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
		"A supported hard constraint prevents this consolidation candidate.",
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
	if !strings.Contains(out, "No proven N-1 consolidation candidate is available for this pool.") {
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

// 6b. Caveats (limitations of this specific proof) and Not Evaluated
// (execution concerns intentionally outside the read-only simulator) render
// as visually and textually distinct sections, never merged.
func TestNodeOptimizationPage_CaveatsDistinctFromNotEvaluated(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(simulationPassedRecommendation()), "prod-eastus", []string{"prod-eastus"})

	caveatsIdx := strings.Index(out, `<div class="no-section-title">Caveats</div>`)
	notEvaluatedIdx := strings.Index(out, `<div class="no-section-title">Not evaluated</div>`)
	if caveatsIdx == -1 || notEvaluatedIdx == -1 {
		t.Fatalf("expected both a Caveats and a Not evaluated section title, got caveatsIdx=%d notEvaluatedIdx=%d", caveatsIdx, notEvaluatedIdx)
	}
	if !strings.Contains(out, "read-only simulation; execution safety was not evaluated") {
		t.Error("expected the analyzer-reported caveat to render under Caveats")
	}
	if !strings.Contains(out, "drain or eviction execution") {
		t.Error("expected drain/eviction execution to render under Not evaluated")
	}
	// The caveat text must not appear inside the Not Evaluated list, and vice
	// versa — they are conceptually separate and must stay in their own box.
	notEvaluatedBox := out[notEvaluatedIdx:]
	if strings.Contains(notEvaluatedBox[:min(len(notEvaluatedBox), 400)], "read-only simulation; execution safety was not evaluated") {
		t.Error("caveat text leaked into the Not Evaluated box")
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

// 14b. The Overview card's optional proven-candidate count and aggregate
// savings only read already-computed Status/SavingsProjection fields, and
// the aggregate is withheld the moment any proven pool lacks exact pricing.
func TestOverviewNodeOptimizationCard_ProvenCandidateAndAggregateSavings(t *testing.T) {
	t.Run("proven candidate count and aggregate savings shown when every proven pool is priced", func(t *testing.T) {
		scan := scanWithRecommendationsAndSavings(
			[]analyzer.NodeOptimizationRecommendation{simulationPassedRecommendation(), simulationPassedRecommendation(), partialRecommendation()},
			[]analyzer.NodeOptimizationSavingsProjection{
				{Available: true, EstimatedMonthlySavings: f64ptr(182)},
				{Available: true, EstimatedMonthlySavings: f64ptr(50)},
				{},
			},
		)
		data := buildOverviewData(scan, "test", []string{"test"}, nil, time.Now())
		if data.NodeOptimizationProvenCount != 2 {
			t.Fatalf("NodeOptimizationProvenCount = %d, want 2", data.NodeOptimizationProvenCount)
		}
		if !data.NodeOptimizationHasAggregateSavings {
			t.Fatal("expected aggregate savings to be shown when every proven pool is priced")
		}
		if !strings.Contains(data.NodeOptimizationAggregateSavingsText, "232") {
			t.Errorf("NodeOptimizationAggregateSavingsText = %q, want it to reflect 182+50=232", data.NodeOptimizationAggregateSavingsText)
		}
	})

	t.Run("aggregate savings withheld when any proven pool is unpriced", func(t *testing.T) {
		scan := scanWithRecommendationsAndSavings(
			[]analyzer.NodeOptimizationRecommendation{simulationPassedRecommendation(), simulationPassedRecommendation()},
			[]analyzer.NodeOptimizationSavingsProjection{
				{Available: true, EstimatedMonthlySavings: f64ptr(182)},
				{}, // second proven pool has no exact price
			},
		)
		data := buildOverviewData(scan, "test", []string{"test"}, nil, time.Now())
		if data.NodeOptimizationProvenCount != 2 {
			t.Fatalf("NodeOptimizationProvenCount = %d, want 2", data.NodeOptimizationProvenCount)
		}
		if data.NodeOptimizationHasAggregateSavings {
			t.Error("expected aggregate savings to be withheld when not every proven pool is priced")
		}
	})
}

// 15. A large number of near-duplicate evidence-gap reasons is grouped for
// display (one line per distinct Code, with an exact occurrence count), the
// exact raw total is preserved, and a materially distinct reason is never
// folded into that group.
func TestNodeOptimizationPage_LargeReasonListGroupedWithExactTotal(t *testing.T) {
	rec := partialRecommendation()
	var blockers []analyzer.NodeOptimizationRecommendationReason
	for i := 0; i < 38; i++ {
		blockers = append(blockers, analyzer.NodeOptimizationRecommendationReason{
			Code:    "incomplete_scheduling_evidence",
			Message: fmt.Sprintf("Pod ns/pod-%d could not be mapped to a canonical node pool", i),
		})
	}
	blockers = append(blockers, analyzer.NodeOptimizationRecommendationReason{
		Code:    "pvc_storage_mobility_unproven",
		Message: "CSI storage mobility not modeled",
	})
	rec.Blockers = blockers

	out := renderNodeOptimizationPage(scanWithRecommendations(rec), "prod-eastus", []string{"prod-eastus"})

	if !strings.Contains(out, "38 Pods could not be mapped to a canonical node pool") {
		t.Error("expected the 38 near-duplicate reasons to be grouped into one summarized line")
	}
	if !strings.Contains(out, "CSI storage mobility not modeled") {
		t.Error("expected the materially distinct reason to remain visible, not hidden by grouping")
	}
	card := nodeOptimizationPoolCard(t, out, 0)
	visible := nodeOptimizationVisibleReasonBody(t, card)
	if strings.Contains(visible, "pod-37") {
		t.Error("expected individual pod identifiers to be absent from the default-visible grouped explanation")
	}
	if !strings.Contains(visible, "39 reasons total") {
		t.Error("expected the exact raw total (39) to be preserved alongside the grouped explanation")
	}
	if !strings.Contains(card[strings.Index(card, `<details class="no-evidence no-reason-evidence">`):], "pod-37") {
		t.Error("expected raw per-Pod evidence to remain inside the collapsed disclosure")
	}
}

// 16. BLOCKED and PARTIAL pools keep reason text out of the hero so evidence
// has one default-visible home in the grouped Why section.
func TestNodeOptimizationPage_BlockerNotDuplicatedInHero(t *testing.T) {
	blocked := renderNodeOptimizationPage(scanWithRecommendations(blockedRecommendation()), "prod-eastus", []string{"prod-eastus"})
	if strings.Contains(blocked, "Most useful blocker:") {
		t.Error("BLOCKED hero should not duplicate blocker evidence")
	}

	partial := renderNodeOptimizationPage(scanWithRecommendations(partialRecommendation()), "prod-eastus", []string{"prod-eastus"})
	if strings.Contains(partial, `<div class="no-hero-primary-blocker">`) {
		t.Error("PARTIAL hero should not duplicate evidence-gap text")
	}
}

// 16b. Real analyzer warnings can arrive as one pool-level string containing
// dozens of semicolon-separated per-Pod reasons. Every PARTIAL pool must show
// only semantic category counts by default, retain the exact total, and keep
// all raw reasons in native collapsed evidence. BLOCKED uses the same path.
func TestNodeOptimizationPage_ConcatenatedEvidenceGroupedPerPool(t *testing.T) {
	makeRecommendation := func(pool string, status analyzer.NodeOptimizationRecommendationStatus, affinityCount, storageCount int) analyzer.NodeOptimizationRecommendation {
		reasons := make([]string, 0, affinityCount+storageCount)
		for i := 0; i < affinityCount; i++ {
			reasons = append(reasons, fmt.Sprintf("DaemonSet pod kube-system/affinity-%d: required node affinity matchFields are not modeled yet", i))
		}
		for i := 0; i < storageCount; i++ {
			reasons = append(reasons, fmt.Sprintf("pod apps/storage-%d: PersistentVolume pv-%d uses a volume source whose attachment and driver-specific scheduling constraints are not modeled", i, i))
		}
		rawSummary := strings.Join(reasons, "; ")
		return analyzer.NodeOptimizationRecommendation{
			Status:       status,
			Summary:      rawSummary,
			PoolKey:      analyzer.CostPoolKey{PoolName: pool},
			Blockers:     []analyzer.NodeOptimizationRecommendationReason{{Code: "unsupported_hard_scheduling_constraint", Message: "pool " + pool + " skipped because scheduling constraints are not fully modeled: " + rawSummary}},
			NotEvaluated: []string{"drain or eviction execution"},
		}
	}

	recommendations := []analyzer.NodeOptimizationRecommendation{
		makeRecommendation("systempool", analyzer.NodeOptimizationRecommendationPartial, 38, 22),
		makeRecommendation("userpool", analyzer.NodeOptimizationRecommendationPartial, 31, 20),
		makeRecommendation("blockedpool", analyzer.NodeOptimizationRecommendationBlocked, 30, 21),
	}
	out := renderNodeOptimizationPage(scanWithRecommendations(recommendations...), "prod-eastus", []string{"prod-eastus"})

	for index, counts := range []struct {
		affinity int
		storage  int
	}{{38, 22}, {31, 20}, {30, 21}} {
		card := nodeOptimizationPoolCard(t, out, index)
		visible := nodeOptimizationVisibleReasonBody(t, card)
		details := card[len(visible):]
		total := counts.affinity + counts.storage

		for _, want := range []string{
			fmt.Sprintf("%d Pods use required node affinity matchFields, which are not modeled", counts.affinity),
			fmt.Sprintf("%d Pods use volume attachment/driver scheduling semantics that are not modeled", counts.storage),
			fmt.Sprintf("%d reasons total", total),
		} {
			if !strings.Contains(visible, want) {
				t.Errorf("pool %d default-visible body missing %q", index, want)
			}
		}
		if !strings.Contains(details, fmt.Sprintf("View detailed evidence (%d)", total)) {
			t.Errorf("pool %d missing exact detailed-evidence count", index)
		}
		for _, raw := range []string{"affinity-0: required node affinity matchFields are not modeled yet", "storage-0: PersistentVolume pv-0 uses a volume source whose attachment and driver-specific scheduling constraints are not modeled"} {
			if strings.Contains(visible, raw) {
				t.Errorf("pool %d leaked raw evidence into the default-visible body: %q", index, raw)
			}
			if !strings.Contains(details, raw) {
				t.Errorf("pool %d did not retain raw evidence inside details: %q", index, raw)
			}
		}
		if strings.Contains(visible, "pool "+recommendations[index].PoolKey.PoolName+" skipped because") {
			t.Errorf("pool %d leaked the concatenated backend warning envelope into the visible body", index)
		}
		if strings.Contains(card, recommendations[index].Summary) {
			t.Errorf("pool %d rendered the raw recommendation Summary", index)
		}
	}
}

// 17. A large placement-evidence table renders collapsed by default (native
// <details>, no JS) so it does not dominate the initial viewport, while a
// small one still auto-expands and every row remains present in the page.
func TestNodeOptimizationPage_LargeAssignmentTableCollapsedByDefault(t *testing.T) {
	rec := simulationPassedRecommendation()
	rec.Assignments = nil
	rec.PodsAssigned = 0
	for i := 0; i < nodeOptimizationAssignmentAutoOpenLimit+5; i++ {
		rec.Assignments = append(rec.Assignments, analyzer.NodeOptimizationRecommendationAssignment{
			Namespace: "orders", PodName: fmt.Sprintf("pod-%d", i), SourceNode: "node-3", DestinationNode: "node-1",
		})
		rec.PodsAssigned++
	}
	out := renderNodeOptimizationPage(scanWithRecommendations(rec), "prod-eastus", []string{"prod-eastus"})

	if !strings.Contains(out, fmt.Sprintf("Placement evidence (%d)", nodeOptimizationAssignmentAutoOpenLimit+5)) {
		t.Error("expected the exact assignment total to remain visible even when collapsed")
	}
	if strings.Contains(out, `<details class="no-evidence" open>`) {
		t.Error("expected a large assignment table to render collapsed by default")
	}
	if !strings.Contains(out, "pod-24") {
		t.Error("expected every assignment row to remain present in the page, not truncated")
	}

	small := renderNodeOptimizationPage(scanWithRecommendations(simulationPassedRecommendation()), "prod-eastus", []string{"prod-eastus"})
	if !strings.Contains(small, `<details class="no-evidence" open>`) {
		t.Error("expected a small assignment table to remain auto-expanded")
	}
}

// 18. The multi-pool summary table's Headroom/posture column reflects a
// proven candidate's headroom and shows a neutral placeholder otherwise.
func TestNodeOptimizationPage_PostureColumnReflectsHeadroom(t *testing.T) {
	out := renderNodeOptimizationPage(scanWithRecommendations(
		simulationPassedRecommendation(),
		partialRecommendation(),
	), "prod-eastus", []string{"prod-eastus"})

	if !strings.Contains(out, "34% CPU / 29% Mem headroom") {
		t.Error("expected the posture column to show headroom for the proven candidate")
	}
}

// 19. /optimizations is a separate, preserved page (RI/waste/right-sizing)
// that this Node Optimization UX pass must not regress.
func TestOptimizationsPage_StillRendersAlongsideNodeOptimization(t *testing.T) {
	scan := scanWithRecommendations(simulationPassedRecommendation())
	out := renderOptimizationsPage(scan, "prod-eastus", []string{"prod-eastus"})
	if out == "" {
		t.Fatal("expected /optimizations to render non-empty output")
	}
	if !strings.Contains(out, "<html") {
		t.Error("expected /optimizations to render a full HTML page")
	}
}
