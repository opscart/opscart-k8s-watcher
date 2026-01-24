package analyzer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// PrintRiskCostAnalysis displays risk-cost analysis
func PrintRiskCostAnalysis(analysis *models.RiskCostAnalysis, format string) {
	if format == "json" {
		printRiskCostJSON(analysis)
		return
	}

	printRiskCostTable(analysis)
}

// printRiskCostTable outputs risk-cost analysis as formatted tables
func printRiskCostTable(analysis *models.RiskCostAnalysis) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║         SECURITY RISK COST ANALYSIS                        ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Executive Summary
	fmt.Println("EXECUTIVE SUMMARY:")
	fmt.Printf("  Security Score: %d/100\n", analysis.SecurityScore)
	fmt.Printf("  Total Risk Exposure: %s\n",
		formatCostRangeK(analysis.TotalRiskExposure))
	fmt.Printf("  Fix Cost: $%s (%.0f engineer-hours)\n",
		formatCurrency(analysis.RemediationPlan.EstimatedCost),
		analysis.RemediationPlan.TotalHours)
	fmt.Printf("  ROI: %.1fx (save $%.0f for every $1 spent)\n",
		analysis.RemediationPlan.ROI,
		analysis.RemediationPlan.ROI)

	if analysis.RemediationPlan.PaybackMonths < 12 {
		fmt.Printf("  Payback: %.1f months\n", analysis.RemediationPlan.PaybackMonths)
	}
	fmt.Println()

	// Risk Categories
	fmt.Println("RISK BREAKDOWN BY CATEGORY:")
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RISK CATEGORY\tCOUNT\tSEVERITY\tEXPOSURE\tPROBABILITY")
	fmt.Fprintln(w, strings.Repeat("─", 100))

	for _, cat := range analysis.RiskCategories {
		severity := formatSeverity(cat.Severity)
		exposure := formatCostRangeK(cat.RiskExposure)

		// Derive probability from risk exposure
		probability := "Medium"
		if cat.RiskExposure.Best > 20000 {
			probability = "High"
		} else if cat.RiskExposure.Best < 5000 {
			probability = "Low"
		}

		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n",
			cat.Name,
			cat.Count,
			severity,
			exposure,
			probability)
	}
	w.Flush()
	fmt.Println()

	// Detailed Risk Analysis
	fmt.Println("DETAILED RISK ANALYSIS:")
	fmt.Println()

	for i, cat := range analysis.RiskCategories {
		if cat.Severity == "critical" || cat.Severity == "high" {
			printRiskCategory(i+1, cat)
		}
	}

	// Remediation Plan
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("REMEDIATION PLAN:")
	fmt.Println()

	fmt.Printf("Total Effort: %.0f hours (%s)\n",
		analysis.RemediationPlan.TotalHours,
		analysis.RemediationPlan.Timeline)
	fmt.Printf("Estimated Cost: $%s\n",
		formatCurrency(analysis.RemediationPlan.EstimatedCost))
	fmt.Printf("Risk Reduction: $%s\n",
		formatCostRangeK(models.CostRange{
			Low:  analysis.RemediationPlan.RiskReduction,
			Best: analysis.RemediationPlan.RiskReduction,
			High: analysis.RemediationPlan.RiskReduction,
		}))
	fmt.Printf("Return on Investment: %.1fx\n\n", analysis.RemediationPlan.ROI)

	// Implementation Phases
	fmt.Println("IMPLEMENTATION PHASES:")
	for i, phase := range analysis.RemediationPlan.Phases {
		if phase.Hours > 0 {
			fmt.Printf("\n%d. %s (%s)\n", i+1, phase.Name, phase.Priority)
			fmt.Printf("   Duration: %s (%.0f hours)\n", phase.Duration, phase.Hours)
			fmt.Printf("   Cost: $%s\n", formatCurrency(phase.Cost))
		}
	}
	fmt.Println()

	// Priority Recommendations
	if len(analysis.PriorityRecommendations) > 0 {
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Println("PRIORITY ACTIONS:")
		for i, rec := range analysis.PriorityRecommendations {
			fmt.Printf("  %d. %s\n", i+1, rec)
		}
		fmt.Println()
	}

	// Call to action
	fmt.Println("💡 BUSINESS CASE:")
	roi := analysis.RemediationPlan.ROI

	if roi > 5 {
		fmt.Println("   ✅ EXCELLENT ROI - Recommend immediate approval")
		fmt.Printf("   Investing $%s reduces risk by $%s (%.1fx return)\n",
			formatCurrency(analysis.RemediationPlan.EstimatedCost),
			formatCurrency(analysis.RemediationPlan.RiskReduction),
			roi)
	} else if roi > 2 {
		fmt.Println("   ✅ STRONG ROI - Recommend approval")
		fmt.Printf("   Fix cost: $%s | Risk reduction: $%s\n",
			formatCurrency(analysis.RemediationPlan.EstimatedCost),
			formatCurrency(analysis.RemediationPlan.RiskReduction))
	} else {
		fmt.Println("   ⚠️  MODERATE ROI - Consider phased approach")
		fmt.Println("   Focus on critical issues first")
	}
}

// printRiskCategory prints detailed information about a risk category
func printRiskCategory(num int, cat models.RiskCategory) {
	fmt.Printf("RISK %d: %s (%s)\n", num, cat.Name, strings.ToUpper(cat.Severity))
	fmt.Printf("  Count: %d instances\n", cat.Count)
	fmt.Printf("  Exposure: %s\n", formatCostRangeK(cat.RiskExposure))
	fmt.Printf("  What it means: %s\n", cat.Description)

	if len(cat.TypicalIncidents) > 0 {
		fmt.Printf("  Potential impacts:\n")
		for _, incident := range cat.TypicalIncidents {
			fmt.Printf("    • %s\n", incident)
		}
	}

	if len(cat.IndustryExamples) > 0 {
		fmt.Printf("  Real-world examples:\n")
		for _, example := range cat.IndustryExamples {
			fmt.Printf("    • %s\n", example)
		}
	}
	fmt.Println()
}

// printRiskCostJSON outputs risk-cost analysis as JSON
func printRiskCostJSON(analysis *models.RiskCostAnalysis) {
	data, err := json.MarshalIndent(analysis, "", "  ")
	if err != nil {
		fmt.Printf("Error formatting JSON: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

// formatSeverity formats severity with emoji
func formatSeverity(severity string) string {
	switch severity {
	case "critical":
		return "🔴 CRITICAL"
	case "high":
		return "🟡 HIGH"
	case "medium":
		return "🟠 MEDIUM"
	case "low":
		return "🟢 LOW"
	default:
		return severity
	}
}

// formatCostRangeK formats cost range in thousands
func formatCostRangeK(cr models.CostRange) string {
	if cr.Low == cr.High || cr.High == 0 {
		if cr.Best >= 1000 {
			return fmt.Sprintf("$%.0fK", cr.Best/1000)
		}
		return fmt.Sprintf("$%.0f", cr.Best)
	}

	// Show range
	if cr.Best >= 1000 {
		return fmt.Sprintf("$%.0fK - $%.0fK (best: $%.0fK)",
			cr.Low/1000,
			cr.High/1000,
			cr.Best/1000)
	}

	return fmt.Sprintf("$%.0f - $%.0f (best: $%.0f)",
		cr.Low,
		cr.High,
		cr.Best)
}

// PrintRiskCostSummary shows a quick risk-cost overview
func PrintRiskCostSummary(analysis *models.RiskCostAnalysis) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║         SECURITY RISK SUMMARY                              ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	fmt.Printf("Security Score: %d/100\n", analysis.SecurityScore)
	fmt.Printf("Risk Exposure: %s\n", formatCostRangeK(analysis.TotalRiskExposure))
	fmt.Printf("Fix Cost: $%s\n", formatCurrency(analysis.RemediationPlan.EstimatedCost))
	fmt.Printf("ROI: %.1fx\n\n", analysis.RemediationPlan.ROI)

	// Count critical issues
	criticalCount := 0
	for _, cat := range analysis.RiskCategories {
		if cat.Severity == "critical" {
			criticalCount += cat.Count
		}
	}

	if criticalCount > 0 {
		fmt.Printf("🔴 %d critical security issues require immediate attention\n", criticalCount)
	} else {
		fmt.Println("✅ No critical security issues found")
	}

	fmt.Println()
	fmt.Println("Run 'opscart-scan risk-cost --cluster <n>' for detailed analysis")
}
