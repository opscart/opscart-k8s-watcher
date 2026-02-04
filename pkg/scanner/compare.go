package scanner

import (
	"fmt"
	"strings"
)

// CompareResult holds the diff between two clusters
type CompareResult struct {
	ClusterA string
	ClusterB string

	// Counts per category
	OnlyInA    []string // Issues only in cluster A
	OnlyInB    []string // Issues only in cluster B
	InBoth     []string // Issues in both clusters
	MatchCount int
}

// CompareClusters takes two ClusterResults and produces a diff
func CompareClusters(a, b ClusterResult) *CompareResult {
	result := &CompareResult{
		ClusterA: a.ClusterName,
		ClusterB: b.ClusterName,
	}

	// Build issue sets from each cluster
	// You will extend this as you wire in real scan results.
	// For now this is the framework — plugs into security/emergency/resource scans.
	issuesA := extractIssues(a)
	issuesB := extractIssues(b)

	setA := toSet(issuesA)
	setB := toSet(issuesB)

	// Only in A
	for _, issue := range issuesA {
		if !setB[issue] {
			result.OnlyInA = append(result.OnlyInA, issue)
		}
	}

	// Only in B
	for _, issue := range issuesB {
		if !setA[issue] {
			result.OnlyInB = append(result.OnlyInB, issue)
		}
	}

	// In both
	for _, issue := range issuesA {
		if setB[issue] {
			result.InBoth = append(result.InBoth, issue)
			result.MatchCount++
		}
	}

	return result
}

// extractIssues pulls issue strings from a ClusterResult
// EXTEND THIS as you wire in real scan results
func extractIssues(r ClusterResult) []string {
	var issues []string
	// Placeholder — wire in real data when integrating with security/emergency commands
	// Example when wired:
	//   for _, finding := range r.SecurityResult.Findings {
	//       issues = append(issues, finding.Type + ":" + finding.Resource)
	//   }
	_ = r
	return issues
}

// toSet converts a slice to a map for O(1) lookup
func toSet(items []string) map[string]bool {
	set := make(map[string]bool)
	for _, item := range items {
		set[item] = true
	}
	return set
}

// PrintCompare displays the comparison result as a side-by-side table
func PrintCompare(result *CompareResult) {
	fmt.Println()
	fmt.Println("╔═══════════════════════════════════════════════════════════╗")
	fmt.Println("║           CLUSTER COMPARISON                              ║")
	fmt.Println("╚═══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Header
	nameA := truncate(result.ClusterA, 24)
	nameB := truncate(result.ClusterB, 24)
	fmt.Printf("  %-26s  vs  %-26s\n", nameA, nameB)
	fmt.Println("  ─────────────────────────────────────────────────────────")
	fmt.Println()

	// Summary counts
	fmt.Printf("  ✅ Issues in BOTH:           %d\n", result.MatchCount)
	fmt.Printf("  🔴 Only in %-20s %d\n", result.ClusterA+":", len(result.OnlyInA))
	fmt.Printf("  🔵 Only in %-20s %d\n", result.ClusterB+":", len(result.OnlyInB))
	fmt.Println()

	// Only in A
	if len(result.OnlyInA) > 0 {
		fmt.Printf("  🔴 Only in %s:\n", result.ClusterA)
		fmt.Println("  ─────────────────────────────────────────────")
		for _, issue := range result.OnlyInA {
			fmt.Printf("    • %s\n", issue)
		}
		fmt.Println()
	}

	// Only in B
	if len(result.OnlyInB) > 0 {
		fmt.Printf("  🔵 Only in %s:\n", result.ClusterB)
		fmt.Println("  ─────────────────────────────────────────────")
		for _, issue := range result.OnlyInB {
			fmt.Printf("    • %s\n", issue)
		}
		fmt.Println()
	}

	// In both
	if len(result.InBoth) > 0 {
		fmt.Printf("  ✅ Present in both clusters:\n")
		fmt.Println("  ─────────────────────────────────────────────")
		for _, issue := range result.InBoth {
			fmt.Printf("    • %s\n", issue)
		}
		fmt.Println()
	}

	// If no issues extracted yet (before wiring)
	if len(result.OnlyInA) == 0 && len(result.OnlyInB) == 0 && len(result.InBoth) == 0 {
		fmt.Println("  📝 Comparison framework ready.")
		fmt.Println("     Wire in scan results to see diff output.")
	}

	fmt.Println("  ─────────────────────────────────────────────────────────")
	fmt.Println()
}

// truncate shortens a string for display
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// PrintCompareHeader prints header before running compare scans
func PrintCompareHeader(clusterA, clusterB string) {
	fmt.Println()
	fmt.Println("╔═══════════════════════════════════════════════════════════╗")
	fmt.Println("║           CLUSTER COMPARISON                              ║")
	fmt.Println("╚═══════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("  Comparing:\n")
	fmt.Printf("    A → %s\n", clusterA)
	fmt.Printf("    B → %s\n", clusterB)
	fmt.Println()
	fmt.Println("  ─────────────────────────────────────────────")

	// Visual legend
	fmt.Println()
	fmt.Println("  Legend:")
	fmt.Printf("    🔴  Issues only in %-20s\n", clusterA)
	fmt.Printf("    🔵  Issues only in %-20s\n", clusterB)
	fmt.Println("    ✅  Issues present in both")
	fmt.Println()
	fmt.Println("  Scanning...")
	fmt.Println("  ─────────────────────────────────────────────")

	// Suppress unused import warning
	_ = strings.Join
}
