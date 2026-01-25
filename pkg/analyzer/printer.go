package analyzer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// PrintResourceAnalysis displays resource analysis in war room format
func PrintResourceAnalysis(analysis *models.ClusterResourceAnalysis, format string) {
	if format == "json" {
		printResourceJSON(analysis)
		return
	}

	printResourceTable(analysis)
}

// printResourceTable outputs resource analysis as formatted table
func printResourceTable(analysis *models.ClusterResourceAnalysis) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║         CLUSTER RESOURCE USAGE ANALYSIS                    ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("ℹ️  Note: Spot instance savings based on cloud provider published rates (~70-90%)")
	fmt.Println()
	// Cluster summary
	fmt.Printf("Cluster Capacity:  %0.1f CPU cores, %0.1f GB memory\n",
		analysis.TotalCPUCores, analysis.TotalMemoryGB)
	fmt.Printf("Total Requested:   %0.1f CPU cores (%0.1f%%), %0.1f GB memory (%0.1f%%)\n\n",
		analysis.TotalCPURequested, analysis.CPUUtilization,
		analysis.TotalMemoryRequested, analysis.MemoryUtilization)

	// Namespace table
	fmt.Println("NAMESPACE RANKING:")
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tCPU%\tMEMORY%\tPODS\tCPU REQ\tMEM REQ\tFLAGS")
	fmt.Fprintln(w, strings.Repeat("─", 100))

	for _, ns := range analysis.Namespaces {
		flags := ""
		if len(ns.Flags) > 0 {
			flags = strings.Join(ns.Flags, ", ")
		}

		fmt.Fprintf(w, "%s\t%0.1f%%\t%0.1f%%\t%d\t%0.1f\t%0.1f GB\t%s\n",
			ns.Name,
			ns.CPUPercent,
			ns.MemoryPercent,
			ns.PodCount,
			ns.CPUCoresRequested,
			ns.MemoryGBRequested,
			flags)
	}
	w.Flush()
	fmt.Println()

	// Optimization opportunities
	if len(analysis.Optimizations) > 0 {
		fmt.Println("OPTIMIZATION OPPORTUNITIES:")
		fmt.Println()

		highImpact := []models.Optimization{}
		mediumImpact := []models.Optimization{}

		for _, opt := range analysis.Optimizations {
			if opt.Priority == "high" {
				highImpact = append(highImpact, opt)
			} else {
				mediumImpact = append(mediumImpact, opt)
			}
		}

		if len(highImpact) > 0 {
			fmt.Println("🔴 HIGH IMPACT:")
			for _, opt := range highImpact {
				fmt.Printf("  • %s\n", opt.Description)
				if opt.Action != "" {
					fmt.Printf("    └─ Action: %s\n", opt.Action)
				}
				if opt.Impact != "" {
					fmt.Printf("    └─ Impact: %s\n", opt.Impact)
				}
			}
			fmt.Println()
		}

		if len(mediumImpact) > 0 {
			fmt.Println("🟡 MEDIUM IMPACT:")
			for _, opt := range mediumImpact {
				fmt.Printf("  • %s\n", opt.Description)
				if opt.Action != "" {
					fmt.Printf("    └─ Action: %s\n", opt.Action)
				}
				if opt.Impact != "" {
					fmt.Printf("    └─ Impact: %s\n", opt.Impact)
				}
			}
			fmt.Println()
		}

		// Summary
		totalCPUSavings := 0.0
		totalMemSavings := 0.0
		for _, opt := range analysis.Optimizations {
			if opt.Type == "idle_namespace" {
				// Parse savings from description (rough estimate)
				for _, ns := range analysis.Namespaces {
					if ns.Name == opt.Namespace {
						totalCPUSavings += ns.CPUCoresRequested
						totalMemSavings += ns.MemoryGBRequested
					}
				}
			}
		}

		if totalCPUSavings > 0 {
			pctSavings := (totalCPUSavings / analysis.TotalCPURequested) * 100
			fmt.Printf("💡 Total Optimization Potential: %0.1f-%0.1f%% resource reduction\n",
				pctSavings*0.7, pctSavings)
		}
	}
}

// printResourceJSON outputs resource analysis as JSON
func printResourceJSON(analysis *models.ClusterResourceAnalysis) {
	data, err := json.MarshalIndent(analysis, "", "  ")
	if err != nil {
		fmt.Printf("Error formatting JSON: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

// PrintOptimizationSummary shows quick optimization check
func PrintOptimizationSummary(optimizations []models.Optimization) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║         OPTIMIZATION QUICK CHECK                           ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	if len(optimizations) == 0 {
		fmt.Println("✅ No obvious optimizations found. Cluster looks well-configured!")
		return
	}

	highImpact := []models.Optimization{}
	mediumImpact := []models.Optimization{}

	for _, opt := range optimizations {
		if opt.Priority == "high" {
			highImpact = append(highImpact, opt)
		} else {
			mediumImpact = append(mediumImpact, opt)
		}
	}

	if len(highImpact) > 0 {
		fmt.Println("⚡ QUICK WINS (do these first):")
		fmt.Println()
		for i, opt := range highImpact {
			fmt.Printf("%d. %s\n", i+1, opt.Description)
			fmt.Printf("   └─ %s\n", opt.Impact)
			if opt.Action != "" {
				fmt.Printf("   └─ Command: %s\n", opt.Action)
			}
			fmt.Println()
		}
	}

	if len(mediumImpact) > 0 {
		fmt.Println("🎯 ADDITIONAL OPPORTUNITIES:")
		fmt.Println()
		for i, opt := range mediumImpact {
			fmt.Printf("%d. %s\n", i+1, opt.Description)
			fmt.Printf("   └─ %s\n", opt.Impact)
			fmt.Println()
		}
	}
}
