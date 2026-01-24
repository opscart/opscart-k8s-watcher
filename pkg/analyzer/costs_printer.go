package analyzer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// PrintCostAnalysis displays cost analysis in professional format
func PrintCostAnalysis(estimate *models.CostEstimate, format string) {
	if format == "json" {
		printCostJSON(estimate)
		return
	}

	printCostTable(estimate)
}

// printCostTable outputs cost analysis as formatted tables
func printCostTable(estimate *models.CostEstimate) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║         ESTIMATED COST ANALYSIS                            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Disclaimers first (very important!)
	fmt.Println("⚠️  IMPORTANT DISCLAIMERS:")
	for _, disclaimer := range estimate.Disclaimers {
		fmt.Printf("   %s\n", disclaimer)
	}
	fmt.Println()

	// Cluster cost
	fmt.Printf("Total Cluster Cost (provided): $%s/month\n", formatCurrency(estimate.TotalClusterCost))
	fmt.Printf("Allocation Method: %s\n", estimate.Method)
	fmt.Printf("Confidence Level: %s\n\n", estimate.Confidence)

	// Namespace cost allocation
	fmt.Println("NAMESPACE COST ALLOCATION:")
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tEST. COST/MONTH\tBASIS\tCONFIDENCE")
	fmt.Fprintln(w, strings.Repeat("─", 100))

	for _, nsCost := range estimate.NamespaceCosts {
		costRange := formatCostRange(nsCost.EstimatedCost)
		basis := fmt.Sprintf("%.1f%% share", nsCost.WeightedShare*100)
		confidence := determineConfidence(nsCost)

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			nsCost.Name,
			costRange,
			basis,
			confidence)
	}
	w.Flush()
	fmt.Println()

	// Optimization scenarios
	if len(estimate.OptimizationScenarios) > 0 {
		fmt.Println("OPTIMIZATION SCENARIOS:")
		fmt.Println()

		for i, scenario := range estimate.OptimizationScenarios {
			printScenario(i+1, scenario)
		}

		// Total savings summary
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Printf("💰 TOTAL OPTIMIZATION POTENTIAL: %s/month\n", formatCostRange(estimate.TotalSavingsPotential))

		bestCase := estimate.TotalClusterCost - estimate.TotalSavingsPotential.Best
		pctSavings := (estimate.TotalSavingsPotential.Best / estimate.TotalClusterCost) * 100

		fmt.Printf("   If all optimizations implemented:\n")
		fmt.Printf("   Current: $%s/month\n", formatCurrency(estimate.TotalClusterCost))
		fmt.Printf("   After:   $%s/month (save %.0f%%)\n\n", formatCurrency(bestCase), pctSavings)
	} else {
		fmt.Println("✅ No major optimization opportunities found - cluster looks efficient!\n")
	}

	// Assumptions
	fmt.Println("ASSUMPTIONS:")
	for i, assumption := range estimate.Assumptions {
		fmt.Printf("  %d. %s\n", i+1, assumption)
	}
	fmt.Println()

	// Call to action
	fmt.Println("💡 NEXT STEPS:")
	fmt.Println("   1. Review optimization scenarios above")
	fmt.Println("   2. Prioritize by Effort/Risk/Savings ratio")
	fmt.Println("   3. Start with 'Low Effort, Low Risk' scenarios")
	fmt.Println("   4. Track actual savings with Azure Cost Management")
}

// printScenario prints a single optimization scenario
func printScenario(num int, scenario models.OptimizationScenario) {
	fmt.Printf("SCENARIO %d: %s\n", num, scenario.Name)
	fmt.Printf("  Description: %s\n", scenario.Description)
	fmt.Printf("  💰 Savings:   %s/month\n", formatCostRange(scenario.Savings))
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

// formatCurrency formats a float as currency
func formatCurrency(amount float64) string {
	if amount < 10 {
		return fmt.Sprintf("%.2f", amount)
	}
	return fmt.Sprintf("%.0f", amount)
}

// formatCostRange formats a cost range for display
func formatCostRange(cr models.CostRange) string {
	if cr.Low == cr.High {
		return fmt.Sprintf("$%s", formatCurrency(cr.Best))
	}

	// Show range with best estimate
	return fmt.Sprintf("$%s - $%s (best: $%s)",
		formatCurrency(cr.Low),
		formatCurrency(cr.High),
		formatCurrency(cr.Best))
}

// determineConfidence determines confidence level based on namespace characteristics
func determineConfidence(nsCost models.NamespaceCostInfo) string {
	// High confidence if reasonable share
	if nsCost.WeightedShare > 0.10 {
		return "Medium"
	}
	// Low confidence for very small namespaces
	if nsCost.WeightedShare < 0.02 {
		return "Low"
	}
	return "Medium"
}

// PrintCostSummary shows a quick cost overview
func PrintCostSummary(estimate *models.CostEstimate) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║              COST ANALYSIS SUMMARY                         ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	fmt.Printf("Cluster Cost: $%s/month\n\n", formatCurrency(estimate.TotalClusterCost))

	// Top 3 most expensive namespaces
	fmt.Println("Top Cost Consumers:")
	count := 3
	if len(estimate.NamespaceCosts) < 3 {
		count = len(estimate.NamespaceCosts)
	}

	for i := 0; i < count; i++ {
		ns := estimate.NamespaceCosts[i]
		fmt.Printf("  %d. %s - $%s/month (%.0f%% of cluster)\n",
			i+1,
			ns.Name,
			formatCurrency(ns.EstimatedCost.Best),
			ns.WeightedShare*100)
	}
	fmt.Println()

	// Optimization potential
	if len(estimate.OptimizationScenarios) > 0 {
		fmt.Printf("Optimization Potential: $%s/month\n",
			formatCurrency(estimate.TotalSavingsPotential.Best))
		fmt.Printf("Available Scenarios: %d\n", len(estimate.OptimizationScenarios))
	} else {
		fmt.Println("Optimization Potential: Minimal - cluster looks efficient")
	}

	fmt.Println()
	fmt.Println("Run 'opscart-scan costs --cluster <name> --monthly-cost <amount>' for detailed analysis")
}
