package analyzer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// ===================================================================
// OUTPUT FUNCTIONS
// ===================================================================

// PrintSecurityAudit displays security audit results with CIS compliance
func PrintSecurityAudit(audit *models.SecurityAudit, format string) {
	if format == "json" {
		PrintSecurityAuditJSON(audit)
		return
	}

	// Print disclaimer
	printSecurityDisclaimer()

	// Print cluster summary
	printClusterSummary(audit)

	// Calculate and print CIS score
	cisResult := CalculateCISScore(audit, nil)
	PrintCISResult(cisResult)

	// Print detailed findings with specific resources
	printDetailedFindings(audit)

	// Print recommendations
	printRecommendations(audit)

	// FIX #1: Validate counting
	validateCounting(audit)
}

func printSecurityDisclaimer() {
	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    ⚠️  DISCLAIMER ⚠️                        ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Println("║  • SECURITY AWARENESS TOOL - NOT FOR COMPLIANCE AUDITS     ║")
	fmt.Println("║  • CIS scoring based on Pod Security subset only           ║")
	fmt.Println("║  • Use kube-bench for complete CIS compliance assessment   ║")
	fmt.Println("║  • Consult security professionals for production decisions ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()
}

func printClusterSummary(audit *models.SecurityAudit) {
	// Calculate actionable vs infrastructure counts
	actionable, infrastructure, prodStaging, development, systemUnexpected := calculateIssueCounts(audit.Issues)

	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("CLUSTER SECURITY SUMMARY")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("Pods Scanned: %d\n", audit.TotalPodsAudited)
	fmt.Println()

	fmt.Printf("SECURITY ISSUES REQUIRING ACTION: %d\n", actionable)
	if prodStaging > 0 {
		fmt.Printf("  └─ Production/Staging:    %d (⚠️  IMMEDIATE ATTENTION)\n", prodStaging)
	}
	if development > 0 {
		fmt.Printf("  └─ Development:          %d (lower priority, monitor)\n", development)
	}
	if systemUnexpected > 0 {
		fmt.Printf("  └─ System (unexpected):  %d (⚠️  REVIEW REQUIRED)\n", systemUnexpected)
	}

	if infrastructure > 0 {
		fmt.Println()
		fmt.Printf("Infrastructure Configurations: %d (expected for system components)\n", infrastructure)
	}

	fmt.Println()
}

func printDetailedFindings(audit *models.SecurityAudit) {
	risks := audit.Risks

	fmt.Println("\n═══════════════════════════════════════════════════════════")
	fmt.Println("DETAILED SECURITY FINDINGS")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println()
	fmt.Println("ℹ️  Note: Findings marked as 'SYSTEM (expected)' are normal infrastructure")
	fmt.Println("   configurations. Focus on Production/Staging/Development and")
	fmt.Println("   'SYSTEM (unexpected)' findings for remediation.")
	fmt.Println()

	// Critical findings with TOP 5 resources (FIX #2)
	if hasAnyCriticalFindings(risks) {
		fmt.Println("🔴 CRITICAL FINDINGS:")

		if risks.PrivilegedContainers > 0 {
			printFindingWithResources("Privileged containers", risks.PrivilegedContainers,
				"Container escape risk", audit.Issues, "privileged_container")
		}

		if risks.HostPID > 0 {
			printFindingWithResources("Host PID namespace", risks.HostPID,
				"Process visibility risk", audit.Issues, "host_pid")
		}

		if risks.HostPathVolumes > 0 {
			printFindingWithResources("Host path volumes", risks.HostPathVolumes,
				"Host filesystem access", audit.Issues, "host_path_volume")
		}
	}

	// High findings with TOP 5 resources
	if hasAnyHighFindings(risks) {
		fmt.Println("\n🟠 HIGH PRIORITY FINDINGS:")

		if risks.HostIPC > 0 {
			printFindingWithResources("Host IPC namespace", risks.HostIPC,
				"Inter-process communication risk", audit.Issues, "host_ipc")
		}

		if risks.HostNetwork > 0 {
			printFindingWithResources("Host network", risks.HostNetwork,
				"Network isolation bypass", audit.Issues, "host_network")
		}
	}

	// Medium findings with TOP 5 resources
	if hasAnyMediumFindings(risks) {
		fmt.Println("\n🟡 MEDIUM PRIORITY FINDINGS:")

		if risks.RunningAsRoot > 0 {
			printFindingWithResources("Containers running as root", risks.RunningAsRoot,
				"Unnecessary privileges", audit.Issues, "running_as_root")
		}

		if risks.PrivilegeEscalation > 0 {
			printFindingWithResources("Privilege escalation allowed", risks.PrivilegeEscalation,
				"Escalation risk", audit.Issues, "privilege_escalation")
		}

		if risks.AddedCapabilities > 0 {
			printFindingWithResources("Added capabilities", risks.AddedCapabilities,
				"Unnecessary capabilities", audit.Issues, "added_capabilities")
		}

		if risks.MissingResourceLimits > 0 {
			printFindingWithResources("Missing resource limits", risks.MissingResourceLimits,
				"Resource exhaustion risk", audit.Issues, "missing_resource_limits")
		}

		if risks.DefaultServiceAccount > 0 {
			printFindingWithResources("Default service account", risks.DefaultServiceAccount,
				"Overly permissive", audit.Issues, "default_service_account")
		}
	}

	fmt.Println()
}

// FIX #2 and #3: Print finding with top resources and environment context
func printFindingWithResources(name string, count int, risk string, allIssues []models.SecurityIssue, issueType string) {
	if count == 0 {
		return
	}

	// Get environment breakdown (FIX #3)
	envCounts := countByEnvironment(allIssues, issueType)

	// Print summary with environment context
	fmt.Printf("  • %s: %d (%s)\n", name, count, risk)

	// Special handling for privileged containers - show expected vs unexpected
	if issueType == "privileged_container" {
		systemIssues := filterIssuesByType(allIssues, issueType)
		expectedCount := 0
		unexpectedCount := 0
		unexpectedPods := []string{}

		for _, issue := range systemIssues {
			if detectEnvironment(issue.Namespace) == "SYSTEM" {
				if strings.Contains(issue.Description, "expected for this infrastructure") {
					expectedCount++
				} else if strings.Contains(issue.Description, "unexpected - review required") {
					unexpectedCount++
					unexpectedPods = append(unexpectedPods, issue.Name)
				}
			}
		}

		// Show breakdown
		if envCounts["PRODUCTION"] > 0 {
			fmt.Printf("    └─ PRODUCTION: %d (⚠️  REQUIRES IMMEDIATE ACTION)\n", envCounts["PRODUCTION"])
		}
		if envCounts["STAGING"] > 0 {
			fmt.Printf("    └─ STAGING: %d (should fix before prod)\n", envCounts["STAGING"])
		}
		if envCounts["DEVELOPMENT"] > 0 {
			fmt.Printf("    └─ DEVELOPMENT: %d (acceptable for dev, monitor)\n", envCounts["DEVELOPMENT"])
		}
		if expectedCount > 0 {
			fmt.Printf("    └─ SYSTEM (expected): %d (CNI/storage/monitoring infrastructure)\n", expectedCount)
		}
		if unexpectedCount > 0 {
			fmt.Printf("    └─ SYSTEM (unexpected): %d (⚠️  REVIEW REQUIRED - not in expected list)\n", unexpectedCount)

			// Show which pods are unexpected (limit to 5)
			if len(unexpectedPods) > 0 {
				fmt.Println("       Unexpected pods to review:")
				limit := 5
				if len(unexpectedPods) < limit {
					limit = len(unexpectedPods)
				}
				for i := 0; i < limit; i++ {
					fmt.Printf("       • %s\n", unexpectedPods[i])
				}
				if len(unexpectedPods) > 5 {
					fmt.Printf("       • ... and %d more\n", len(unexpectedPods)-5)
				}
			}
		}
	} else {
		// Standard environment breakdown for other issue types
		if envCounts["PRODUCTION"] > 0 {
			fmt.Printf("    └─ PRODUCTION: %d (⚠️  REQUIRES IMMEDIATE ACTION)\n", envCounts["PRODUCTION"])
		}
		if envCounts["STAGING"] > 0 {
			fmt.Printf("    └─ STAGING: %d (should fix before prod)\n", envCounts["STAGING"])
		}
		if envCounts["DEVELOPMENT"] > 0 {
			fmt.Printf("    └─ DEVELOPMENT: %d (acceptable for dev, monitor)\n", envCounts["DEVELOPMENT"])
		}
		if envCounts["SYSTEM"] > 0 {
			fmt.Printf("    └─ SYSTEM: %d (expected for infrastructure)\n", envCounts["SYSTEM"])
		}
	}

	// Show top 5 specific resources (FIX #2)
	topIssues := getTopIssues(allIssues, issueType, 5)
	if len(topIssues) > 0 {
		fmt.Println("    Top resources:")
		for i, issue := range topIssues {
			env := detectEnvironment(issue.Namespace)
			envLabel := ""
			if env == "PRODUCTION" {
				envLabel = " [PROD]"
			} else if issueType == "privileged_container" && env == "SYSTEM" {
				if strings.Contains(issue.Description, "unexpected - review required") {
					envLabel = " [⚠️ UNEXPECTED]"
				}
			}
			fmt.Printf("      %d. %s in namespace %s%s\n",
				i+1, issue.Name, issue.Namespace, envLabel)
		}
		if count > 5 {
			fmt.Printf("      ... and %d more\n", count-5)
		}
	}
}

func printFinding(name string, count int, risk string) {
	if count > 0 {
		fmt.Printf("  • %s: %d (%s)\n", name, count, risk)
	}
}

func hasAnyCriticalFindings(r models.SecurityRisks) bool {
	return r.PrivilegedContainers > 0 || r.HostPID > 0 || r.HostPathVolumes > 0
}

func hasAnyHighFindings(r models.SecurityRisks) bool {
	return r.HostIPC > 0 || r.HostNetwork > 0
}

func hasAnyMediumFindings(r models.SecurityRisks) bool {
	return r.RunningAsRoot > 0 || r.PrivilegeEscalation > 0 || r.AddedCapabilities > 0 ||
		r.MissingResourceLimits > 0 || r.DefaultServiceAccount > 0
}

func printRecommendations(audit *models.SecurityAudit) {
	if len(audit.PriorityActions) == 0 {
		return
	}

	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("RECOMMENDED ACTIONS (Priority Order)")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println()

	for i, action := range audit.PriorityActions {
		fmt.Printf("%d. %s\n", i+1, action)
	}

	fmt.Println()
	fmt.Println("─────────────────────────────────────────────────────────")
	fmt.Println("VALIDATION STEPS")
	fmt.Println("─────────────────────────────────────────────────────────")
	fmt.Println("1. Test fixes in staging environment first")
	fmt.Println("2. Verify application functionality after changes")
	fmt.Println("3. Run kube-bench for complete CIS assessment")
	fmt.Println("4. Re-scan cluster after remediation")
	fmt.Println()
}

// FIX #1: Validate issue counting and show breakdown
func validateCounting(audit *models.SecurityAudit) {
	// Calculate total from risk counters
	totalCounted := audit.Risks.PrivilegedContainers +
		audit.Risks.HostPID +
		audit.Risks.HostIPC +
		audit.Risks.HostNetwork +
		audit.Risks.HostPathVolumes +
		audit.Risks.RunningAsRoot +
		audit.Risks.PrivilegeEscalation +
		audit.Risks.AddedCapabilities +
		audit.Risks.MissingResourceLimits +
		audit.Risks.DefaultServiceAccount

	actualIssues := len(audit.Issues)

	// Calculate actionable vs infrastructure
	actionable, infrastructure, _, _, _ := calculateIssueCounts(audit.Issues)

	// Always show breakdown for transparency
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("DETAILED ISSUE COUNT BREAKDOWN")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("  Privileged containers:      %3d\n", audit.Risks.PrivilegedContainers)
	fmt.Printf("  Host PID:                   %3d\n", audit.Risks.HostPID)
	fmt.Printf("  Host IPC:                   %3d\n", audit.Risks.HostIPC)
	fmt.Printf("  Host network:               %3d\n", audit.Risks.HostNetwork)
	fmt.Printf("  Host path volumes:          %3d\n", audit.Risks.HostPathVolumes)
	fmt.Printf("  Running as root:            %3d\n", audit.Risks.RunningAsRoot)
	fmt.Printf("  Privilege escalation:       %3d\n", audit.Risks.PrivilegeEscalation)
	fmt.Printf("  Added capabilities:         %3d\n", audit.Risks.AddedCapabilities)
	fmt.Printf("  Missing resource limits:    %3d\n", audit.Risks.MissingResourceLimits)
	fmt.Printf("  Default service account:    %3d\n", audit.Risks.DefaultServiceAccount)
	fmt.Println("  ─────────────────────────────────")
	fmt.Printf("  TOTAL FINDINGS:             %3d\n", totalCounted)
	fmt.Println()
	fmt.Printf("  Actionable Issues:          %3d (require remediation)\n", actionable)
	fmt.Printf("  Infrastructure Expected:    %3d (system components functioning normally)\n", infrastructure)

	// Only show warning if there's a discrepancy
	if totalCounted != actualIssues {
		fmt.Println()
		fmt.Printf("⚠️  WARNING: Total (%d) doesn't match Issues Found (%d)\n", totalCounted, actualIssues)
		fmt.Printf("Difference: %d issues not tracked in SecurityRisks\n", actualIssues-totalCounted)
	} else {
		fmt.Printf("\nCount verified: All %d findings accounted for\n", actualIssues)
	}
	fmt.Println()
}

// PrintSecurityAuditJSON outputs security audit in JSON format
func PrintSecurityAuditJSON(audit *models.SecurityAudit) {
	// Calculate CIS score
	cisResult := CalculateCISScore(audit, nil)

	output := struct {
		Disclaimer  string                 `json:"disclaimer"`
		PodsScanned int                    `json:"pods_scanned"`
		IssuesFound int                    `json:"issues_found"`
		CISScore    int                    `json:"cis_score"`
		CISPassed   int                    `json:"cis_passed"`
		CISFailed   int                    `json:"cis_failed"`
		Risks       models.SecurityRisks   `json:"risks"`
		Issues      []models.SecurityIssue `json:"issues"`
		Actions     []string               `json:"priority_actions"`
	}{
		Disclaimer:  "Security awareness tool - not for compliance auditing. Use kube-bench for complete CIS assessment.",
		PodsScanned: audit.TotalPodsAudited,
		IssuesFound: len(audit.Issues),
		CISScore:    cisResult.Score,
		CISPassed:   cisResult.PassedChecks,
		CISFailed:   cisResult.FailedChecks,
		Risks:       audit.Risks,
		Issues:      audit.Issues,
		Actions:     audit.PriorityActions,
	}

	jsonData, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(jsonData))
}
