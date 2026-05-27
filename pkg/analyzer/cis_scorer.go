// Save this as: pkg/analyzer/cis_scorer.go
package analyzer

import (
	"fmt"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// CISControl represents a CIS Kubernetes benchmark control
type CISControl struct {
	ID          string
	Description string
	Weight      float64
	Passed      bool
	Finding     string
}

// CISResult holds the CIS compliance score
type CISResult struct {
	Score        int
	TotalChecks  int
	PassedChecks int
	FailedChecks int
	Controls     []CISControl
}

// CalculateCISScore evaluates cluster against CIS Kubernetes benchmarks.
// Based on CIS Kubernetes Benchmark v1.8 - Pod Security subset.
// netAudit is optional — pass nil when network policy data is not available
// (5.7.3 will be marked as not evaluated rather than a false pass).
func CalculateCISScore(audit *models.SecurityAudit, netAudit *NetworkPolicyAudit) CISResult {
	// Derive 5.7.3 result from real network audit data
	net573Passed := false
	net573Finding := "Not evaluated — run 'security' command with network audit enabled"
	if netAudit != nil {
		unprotected := len(netAudit.UnprotectedNamespaces)
		total := netAudit.TotalNamespaces
		if unprotected == 0 && total > 0 {
			net573Passed = true
			net573Finding = fmt.Sprintf("All %d namespaces have NetworkPolicies ✅", total)
		} else if total == 0 {
			net573Finding = "No namespaces found to evaluate"
		} else {
			net573Finding = fmt.Sprintf(
				"%d of %d namespaces have no NetworkPolicy (%d high-risk)",
				unprotected, total, netAudit.HighRiskNamespaces,
			)
		}
	}

	controls := []CISControl{
		{
			ID:          "5.2.1",
			Description: "Minimize privileged containers",
			Weight:      10.0,
			Passed:      audit.Risks.PrivilegedContainers == 0,
			Finding:     fmt.Sprintf("%d privileged containers", audit.Risks.PrivilegedContainers),
		},
		{
			ID:          "5.2.2",
			Description: "Minimize host PID namespace sharing",
			Weight:      8.0,
			Passed:      audit.Risks.HostPID == 0,
			Finding:     fmt.Sprintf("%d pods using hostPID", audit.Risks.HostPID),
		},
		{
			ID:          "5.2.3",
			Description: "Minimize host IPC namespace sharing",
			Weight:      7.0,
			Passed:      audit.Risks.HostIPC == 0,
			Finding:     fmt.Sprintf("%d pods using hostIPC", audit.Risks.HostIPC),
		},
		{
			ID:          "5.2.4",
			Description: "Minimize host network namespace sharing",
			Weight:      8.0,
			Passed:      audit.Risks.HostNetwork == 0,
			Finding:     fmt.Sprintf("%d pods using hostNetwork", audit.Risks.HostNetwork),
		},
		{
			ID:          "5.2.6",
			Description: "Minimize containers running as root",
			Weight:      6.0,
			Passed:      audit.Risks.RunningAsRoot == 0,
			Finding:     fmt.Sprintf("%d containers as root", audit.Risks.RunningAsRoot),
		},
		{
			ID:          "5.7.3",
			Description: "Ensure namespaces have network policies",
			Weight:      5.0,
			Passed:      net573Passed,
			Finding:     net573Finding,
		},
		{
			ID:          "RM-1",
			Description: "Ensure containers have resource limits",
			Weight:      4.0,
			Passed:      audit.Risks.MissingResourceLimits == 0,
			Finding:     fmt.Sprintf("%d containers missing limits", audit.Risks.MissingResourceLimits),
		},
	}

	// Calculate score
	totalWeight := 0.0
	earnedWeight := 0.0
	passed := 0
	failed := 0

	for i := range controls {
		totalWeight += controls[i].Weight
		if controls[i].Passed {
			earnedWeight += controls[i].Weight
			passed++
		} else {
			failed++
		}
	}

	score := 0
	if totalWeight > 0 {
		score = int((earnedWeight / totalWeight) * 100)
	}

	return CISResult{
		Score:        score,
		TotalChecks:  len(controls),
		PassedChecks: passed,
		FailedChecks: failed,
		Controls:     controls,
	}
}

// PrintCISResult displays CIS compliance score
func PrintCISResult(result CISResult) {
	fmt.Println("\n" + strings.Repeat("═", 70))
	fmt.Println("CIS KUBERNETES BENCHMARK COMPLIANCE (Pod Security Subset)")
	fmt.Println(strings.Repeat("═", 70))

	// Overall score with color
	scoreColor := ""
	if result.Score >= 80 {
		scoreColor = "\033[32m" // Green
	} else if result.Score >= 60 {
		scoreColor = "\033[33m" // Yellow
	} else {
		scoreColor = "\033[31m" // Red
	}

	fmt.Printf("\nCIS Compliance Score: %s%d/100\033[0m\n", scoreColor, result.Score)
	fmt.Printf("Controls Passed: %d/%d\n", result.PassedChecks, result.TotalChecks)
	fmt.Printf("Controls Failed: %d/%d\n\n", result.FailedChecks, result.TotalChecks)

	// Interpretation
	interpretation := ""
	switch {
	case result.Score >= 90:
		interpretation = "✅ Excellent - Strong security posture"
	case result.Score >= 70:
		interpretation = "✅ Good - Minor improvements needed"
	case result.Score >= 50:
		interpretation = "⚠️  Fair - Several gaps to address"
	case result.Score >= 30:
		interpretation = "⚠️  Poor - Significant issues present"
	default:
		interpretation = "🔴 Critical - Immediate action required"
	}
	fmt.Println(interpretation)
	fmt.Println()

	// Failed controls
	hasFailures := false
	for _, ctrl := range result.Controls {
		if !ctrl.Passed {
			if !hasFailures {
				fmt.Println(strings.Repeat("─", 70))
				fmt.Println("FAILED CONTROLS:")
				fmt.Println(strings.Repeat("─", 70))
				hasFailures = true
			}
			priority := "🔴"
			if ctrl.Weight < 8.0 {
				priority = "🟠"
			}
			if ctrl.Weight < 6.0 {
				priority = "🟡"
			}
			fmt.Printf("%s [%s] %s\n", priority, ctrl.ID, ctrl.Description)
			fmt.Printf("   Finding: %s\n\n", ctrl.Finding)
		}
	}

	// Disclaimer
	fmt.Println(strings.Repeat("─", 70))
	fmt.Println("ℹ️  Notes:")
	fmt.Println("  • Based on CIS Kubernetes Benchmark v1.8")
	fmt.Println("  • Covers pod security controls only (not control plane/nodes)")
	fmt.Println("  • For full compliance, use kube-bench or similar tools")
	fmt.Println("  • Reference: https://www.cisecurity.org/benchmark/kubernetes")
	fmt.Println(strings.Repeat("─", 70))
}
