package main

import (
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func TestBuildCostOptimizationSignalsUsesEvidenceAndSimulationGate(t *testing.T) {
	report := &models.CloudCostReport{
		TotalMonthlyCost: 1000,
		IdleNodeCost:     350,
		NodePoolCosts: []models.NodePoolCost{{
			Name: "userpool", Mode: "User", NodeCount: 4,
			PricingAvailable: true, TotalMonthly: 900, PricePerNodeMonth: 225,
			CPUUtilizationPct: 60, MemoryUtilizationPct: 30,
		}},
	}

	signals := buildCostOptimizationSignals(report)
	if len(signals) < 4 {
		t.Fatalf("got %d signals, want idle, dominant-pool, imbalance and simulation-gated N-1 signals", len(signals))
	}

	var sawSimulation bool
	for _, signal := range signals {
		if signal.BadgeLabel == "SIMULATION REQUIRED" {
			sawSimulation = true
			if !strings.Contains(signal.Action, "bin-packing") {
				t.Fatalf("simulation-gated signal lacks bin-packing action: %+v", signal)
			}
		}
	}
	if !sawSimulation {
		t.Fatal("expected aggregate N-1 pre-check to remain simulation-gated")
	}
}

func TestBuildCostOptimizationSignalsDoesNotClaimNMinusOneWhenCPUEnvelopeFails(t *testing.T) {
	report := &models.CloudCostReport{
		TotalMonthlyCost: 1000,
		NodePoolCosts: []models.NodePoolCost{{
			Name: "userpool", Mode: "User", NodeCount: 7,
			PricingAvailable: true, TotalMonthly: 1000, PricePerNodeMonth: 142.86,
			CPUUtilizationPct: 87, MemoryUtilizationPct: 43,
		}},
	}

	for _, signal := range buildCostOptimizationSignals(report) {
		if signal.BadgeLabel == "SIMULATION REQUIRED" && strings.Contains(signal.Title, "N-1") {
			t.Fatalf("87%% CPU requests exceed the 6/7 aggregate capacity pre-check; got %+v", signal)
		}
	}
}

func TestCostPercentClampsAndHandlesUnavailable(t *testing.T) {
	if got := costPercent(35, 100); got != 35 {
		t.Fatalf("costPercent(35,100)=%v", got)
	}
	if got := costPercent(1, 0); got != 0 {
		t.Fatalf("costPercent with unavailable denominator=%v, want 0", got)
	}
	if got := costPercent(120, 100); got != 100 {
		t.Fatalf("costPercent should clamp display width at 100, got %v", got)
	}
}
