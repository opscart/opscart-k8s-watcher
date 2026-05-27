package analyzer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// PrintCostAnalysis displays cost analysis in the requested format
func PrintCostAnalysis(estimate *models.CostEstimate, format string) error {
	switch format {
	case "json":
		printCostJSON(estimate)
		return nil
	case "html":
		return GenerateCostsHTML(estimate, "")
	default:
		printCostTable(estimate)
		return nil
	}
}

// printCostTable outputs a FinOps-grade cost analysis table
func printCostTable(estimate *models.CostEstimate) {
	noCost := estimate.TotalClusterCost <= 0

	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	if noCost {
		fmt.Println("║         RESOURCE SHARE ANALYSIS (FinOps)                   ║")
	} else {
		fmt.Println("║         ESTIMATED COST ANALYSIS (FinOps)                   ║")
	}
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	if noCost {
		fmt.Println("💡 TIP: Provide --monthly-cost <amount> to see estimated dollar costs")
		fmt.Println("        Use --format html for a visual FinOps dashboard")
		fmt.Println()
	}

	fmt.Println("⚠️  IMPORTANT DISCLAIMERS:")
	for _, d := range estimate.Disclaimers {
		fmt.Printf("   %s\n", d)
	}
	fmt.Println()

	if !noCost {
		totalAllocated := 0.0
		for _, ns := range estimate.NamespaceCosts {
			totalAllocated += ns.EstimatedCost.Best
		}
		unallocated := estimate.TotalClusterCost - totalAllocated

		fmt.Printf("  Total Cluster Cost (provided):  $%s / month\n", formatCurrency(estimate.TotalClusterCost))
		fmt.Printf("  Allocated to namespaces:        $%s / month  (%.1f%%)\n",
			formatCurrency(totalAllocated), totalAllocated/estimate.TotalClusterCost*100)
		fmt.Printf("  Unallocated / Shared Infra:     $%s / month  (%.1f%%)  ← node OS, reserved, DaemonSets\n",
			formatCurrency(unallocated), unallocated/estimate.TotalClusterCost*100)
		fmt.Printf("  Allocation Method:              %s\n", estimate.Method)
		fmt.Printf("  Overall Confidence:             %s\n", estimate.Confidence)
		fmt.Println()

		top := 3
		if len(estimate.NamespaceCosts) < top {
			top = len(estimate.NamespaceCosts)
		}
		fmt.Println("🔥 TOP COST CONSUMERS:")
		for i := 0; i < top; i++ {
			ns := estimate.NamespaceCosts[i]
			barLen := int(ns.WeightedShare * 100 / 2)
			bar := strings.Repeat("█", barLen)
			fmt.Printf("   %d. %-25s $%6s/mo  %5.1f%%  %s\n",
				i+1, ns.Name,
				formatCurrency(ns.EstimatedCost.Best),
				ns.WeightedShare*100, bar)
		}
		fmt.Println()
	}

	if noCost {
		fmt.Println("NAMESPACE RESOURCE SHARE:")
	} else {
		fmt.Println("NAMESPACE COST ALLOCATION:")
	}
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	if noCost {
		fmt.Fprintln(w, "NAMESPACE\tWGT SHARE\tCPU %\tCPU CORES\tMEM %\tMEM GB\tPODS\tIDLE\tCONFIDENCE")
		fmt.Fprintln(w, strings.Repeat("─", 110))
		totalCPU, totalMem := 0.0, 0.0
		for _, ns := range estimate.NamespaceCosts {
			totalCPU += ns.CPUCores
			totalMem += ns.MemoryGB
			idle := ""
			if ns.IdlePods > 0 {
				idle = fmt.Sprintf("%d ⚠️", ns.IdlePods)
			}
			fmt.Fprintf(w, "%s\t%5.2f%%\t%5.2f%%\t%6.2f\t%5.2f%%\t%6.2f\t%4d\t%s\t%s\n",
				ns.Name,
				ns.WeightedShare*100, ns.CPUShare*100, ns.CPUCores,
				ns.MemoryShare*100, ns.MemoryGB,
				ns.PodCount, idle,
				determineConfidence(ns))
		}
		fmt.Fprintln(w, strings.Repeat("─", 110))
		fmt.Fprintf(w, "%-28s\t%5s\t%5s\t%6.2f\t%5s\t%6.2f\t\t\t\n",
			"TOTAL (all namespaces)", "", "", totalCPU, "", totalMem)
	} else {
		fmt.Fprintln(w, "NAMESPACE\tWGT SHARE\tCPU CORES\tMEM GB\tPODS\tIDLE\tEST. COST/MO\tRANGE (LOW - HIGH)\tCONFIDENCE")
		fmt.Fprintln(w, strings.Repeat("─", 130))

		totalAllocated := 0.0
		totalCPU, totalMem := 0.0, 0.0
		for _, ns := range estimate.NamespaceCosts {
			totalAllocated += ns.EstimatedCost.Best
			totalCPU += ns.CPUCores
			totalMem += ns.MemoryGB
			idle := ""
			if ns.IdlePods > 0 {
				idle = fmt.Sprintf("%d ⚠️", ns.IdlePods)
			}
			wasteTag := ""
			if ns.WasteScore > 70 {
				wasteTag = " 🔴"
			} else if ns.WasteScore > 40 {
				wasteTag = " 🟡"
			}
			fmt.Fprintf(w, "%s%s\t%5.2f%%\t%6.2f\t%6.2f\t%4d\t%s\t$%s\t$%s - $%s\t%s\n",
				ns.Name, wasteTag,
				ns.WeightedShare*100, ns.CPUCores, ns.MemoryGB,
				ns.PodCount, idle,
				formatCurrency(ns.EstimatedCost.Best),
				formatCurrency(ns.EstimatedCost.Low),
				formatCurrency(ns.EstimatedCost.High),
				determineConfidence(ns))

			// Deployment breakdown (if requested)
			for j, d := range ns.Deployments {
				connector := "├──"
				if j == len(ns.Deployments)-1 {
					connector = "└──"
				}
				kindTag := ""
				switch d.Kind {
				case "StatefulSet":
					kindTag = " [STS]"
				case "DaemonSet":
					kindTag = " [DS]"
				}
				costStr := ""
				if d.EstimatedCost.Best > 0 {
					costStr = "$" + formatCurrency(d.EstimatedCost.Best)
				}
				fmt.Fprintf(w, "  %s %-30s\t%5.1f%%\t%6.2f\t%6.2f\t%4d\t\t%s\t\t\n",
					connector, d.Name+kindTag,
					d.NSShare*100, d.CPUCores, d.MemoryGB,
					d.Replicas, costStr)
			}
		}

		unallocated := estimate.TotalClusterCost - totalAllocated
		fmt.Fprintln(w, strings.Repeat("─", 130))
		fmt.Fprintf(w, "%-32s\t%5.2f%%\t%6s\t%6s\t%4s\t%s\t$%s\t%s\t%s\n",
			"Unallocated / Shared Infra",
			unallocated/estimate.TotalClusterCost*100,
			"—", "—", "—", "",
			formatCurrency(unallocated),
			"(node OS, DaemonSets, reserved)", "N/A")
		fmt.Fprintln(w, strings.Repeat("═", 130))
		fmt.Fprintf(w, "%-32s\t%5s\t%6.2f\t%6.2f\t%4s\t%s\t$%s\t%s\t%s\n",
			"TOTAL", "100%", totalCPU, totalMem, "", "",
			formatCurrency(estimate.TotalClusterCost), "", "")
	}

	w.Flush()
	fmt.Println()

	if !noCost {
		fmt.Println("  🔴 = High waste (score >70)   🟡 = Medium waste (score >40)   ⚠️  = Idle pods detected")
		fmt.Println()
	}

	if len(estimate.OptimizationScenarios) > 0 {
		fmt.Println("OPTIMIZATION SCENARIOS:")
		fmt.Println()
		for i, scenario := range estimate.OptimizationScenarios {
			printScenario(i+1, scenario)
		}

		fmt.Println("═══════════════════════════════════════════════════════════")
		if noCost {
			fmt.Println("💰 OPTIMIZATION POTENTIAL: see CPU/memory impact above")
			fmt.Println("   Add --monthly-cost <amount> for dollar savings estimates")
			fmt.Println()
		} else {
			fmt.Printf("💰 TOTAL OPTIMIZATION POTENTIAL: %s / month\n",
				formatCostRange(estimate.TotalSavingsPotential))
			bestCase := estimate.TotalClusterCost - estimate.TotalSavingsPotential.Best
			pct := estimate.TotalSavingsPotential.Best / estimate.TotalClusterCost * 100
			fmt.Printf("   Current:  $%s / month\n", formatCurrency(estimate.TotalClusterCost))
			fmt.Printf("   After:    $%s / month  (save %.0f%%)\n\n", formatCurrency(bestCase), pct)
		}
	} else {
		fmt.Println("✅ No major optimization opportunities found — cluster looks efficient!")
		fmt.Println()
	}

	fmt.Println("ASSUMPTIONS:")
	for i, a := range estimate.Assumptions {
		fmt.Printf("  %d. %s\n", i+1, a)
	}
	fmt.Println()

	fmt.Println("💡 NEXT STEPS:")
	fmt.Println("   1. Review optimization scenarios above")
	fmt.Println("   2. Prioritize by Effort/Risk/Savings ratio")
	fmt.Println("   3. Run with --format html for a shareable FinOps dashboard")
	fmt.Println("   4. Validate actuals via Azure Cost Management (export → compare)")
}

// printScenario prints a single optimization scenario
func printScenario(num int, scenario models.OptimizationScenario) {
	fmt.Printf("SCENARIO %d: %s\n", num, scenario.Name)
	fmt.Printf("  Description: %s\n", scenario.Description)
	if scenario.Savings.Best > 0 {
		fmt.Printf("  💰 Savings:   %s / month\n", formatCostRange(scenario.Savings))
	}
	fmt.Printf("  📊 Impact:    %s\n", scenario.Impact)
	fmt.Printf("  ⚡ Effort:    %s | Risk: %s | Timeline: %s\n",
		scenario.Effort, scenario.Risk, scenario.Timeline)
	if len(scenario.Actions) > 0 {
		fmt.Printf("  📝 Actions:\n")
		for _, action := range scenario.Actions {
			fmt.Printf("     • %s\n", action)
		}
	}
	fmt.Println()
}

// printCostJSON outputs cost analysis as JSON
func printCostJSON(estimate *models.CostEstimate) {
	data, err := json.MarshalIndent(estimate, "", "  ")
	if err != nil {
		fmt.Printf("Error formatting JSON: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

// formatCurrency formats a float as currency string
func formatCurrency(amount float64) string {
	if amount < 10 {
		return fmt.Sprintf("%.2f", amount)
	}
	return fmt.Sprintf("%.0f", amount)
}

// formatCostRange formats a CostRange for display
func formatCostRange(cr models.CostRange) string {
	if cr.Low == cr.High || (cr.Low == 0 && cr.High == 0) {
		return fmt.Sprintf("$%s", formatCurrency(cr.Best))
	}
	return fmt.Sprintf("$%s - $%s (best: $%s)",
		formatCurrency(cr.Low),
		formatCurrency(cr.High),
		formatCurrency(cr.Best))
}

// determineConfidence returns a confidence label based on share size, pod count, and waste
func determineConfidence(nsCost models.NamespaceCostInfo) string {
	score := 0
	// Share size: larger share = more stable/predictable
	if nsCost.WeightedShare >= 0.10 {
		score += 3
	} else if nsCost.WeightedShare >= 0.03 {
		score += 2
	} else {
		score += 1
	}
	// Pod count: more pods = more predictable
	if nsCost.PodCount >= 10 {
		score += 2
	} else if nsCost.PodCount >= 3 {
		score += 1
	}
	// Waste: high waste = less predictable
	if nsCost.WasteScore > 60 {
		score -= 2
	} else if nsCost.WasteScore > 35 {
		score -= 1
	}
	if score >= 4 {
		return "High"
	}
	if score >= 2 {
		return "Medium"
	}
	return "Low"
}

// PrintCostSummary shows a quick cost overview (used by report command)
func PrintCostSummary(estimate *models.CostEstimate) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║              COST ANALYSIS SUMMARY                         ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("Cluster Cost: $%s/month\n\n", formatCurrency(estimate.TotalClusterCost))

	top := 3
	if len(estimate.NamespaceCosts) < top {
		top = len(estimate.NamespaceCosts)
	}
	fmt.Println("Top Cost Consumers:")
	for i := 0; i < top; i++ {
		ns := estimate.NamespaceCosts[i]
		fmt.Printf("  %d. %s — $%s/month (%.1f%%)\n",
			i+1, ns.Name, formatCurrency(ns.EstimatedCost.Best), ns.WeightedShare*100)
	}
	fmt.Println()

	if len(estimate.OptimizationScenarios) > 0 {
		fmt.Printf("Optimization Potential: $%s/month\n",
			formatCurrency(estimate.TotalSavingsPotential.Best))
		fmt.Printf("Available Scenarios: %d\n", len(estimate.OptimizationScenarios))
	} else {
		fmt.Println("Optimization Potential: Minimal — cluster looks efficient")
	}
	fmt.Println()
	fmt.Println("Run with --format html for a full FinOps dashboard report")
}
