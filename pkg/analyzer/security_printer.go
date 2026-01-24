package analyzer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// PrintSecurityAudit displays security audit results
func PrintSecurityAudit(audit *models.SecurityAudit, format string) {
	if format == "json" {
		printSecurityJSON(audit)
		return
	}

	printSecurityTable(audit)
}

// printSecurityTable outputs security audit as formatted table
func printSecurityTable(audit *models.SecurityAudit) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║              SECURITY POSTURE AUDIT                        ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Security score with color coding
	scoreIcon := "✅"
	scoreLabel := "EXCELLENT"
	if audit.SecurityScore < 80 {
		scoreIcon = "⚠️ "
		scoreLabel = "NEEDS IMPROVEMENT"
	}
	if audit.SecurityScore < 60 {
		scoreIcon = "🔴"
		scoreLabel = "CRITICAL - IMMEDIATE ACTION REQUIRED"
	}

	fmt.Printf("Security Score: %d/100 %s %s\n", audit.SecurityScore, scoreIcon, scoreLabel)
	fmt.Printf("Total Pods Audited: %d\n\n", audit.TotalPodsAudited)

	// Risk summary
	fmt.Println("SECURITY RISKS DETECTED:")
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RISK TYPE\tCOUNT\tSEVERITY\tPRIORITY")
	fmt.Fprintln(w, strings.Repeat("─", 80))

	// Critical risks
	if audit.Risks.HostPathVolumes > 0 {
		fmt.Fprintf(w, "Host Path Volumes\t%d\t🔴 CRITICAL\tREMOVE IMMEDIATELY\n", audit.Risks.HostPathVolumes)
	}
	if audit.Risks.PrivilegedContainers > 0 {
		fmt.Fprintf(w, "Privileged Containers\t%d\t🔴 CRITICAL\tFIX IMMEDIATELY\n", audit.Risks.PrivilegedContainers)
	}
	if audit.Risks.HostPID > 0 {
		fmt.Fprintf(w, "Host PID Namespace\t%d\t🔴 CRITICAL\tFIX IMMEDIATELY\n", audit.Risks.HostPID)
	}

	// High risks
	if audit.Risks.HostNetwork > 0 {
		fmt.Fprintf(w, "Host Network\t%d\t🟡 HIGH\tFIX SOON\n", audit.Risks.HostNetwork)
	}
	if audit.Risks.HostIPC > 0 {
		fmt.Fprintf(w, "Host IPC Namespace\t%d\t🟡 HIGH\tFIX SOON\n", audit.Risks.HostIPC)
	}
	if audit.Risks.RunningAsRoot > 0 {
		fmt.Fprintf(w, "Running as Root\t%d\t🟡 HIGH\tREVIEW\n", audit.Risks.RunningAsRoot)
	}

	// Medium risks
	if audit.Risks.DefaultServiceAccount > 0 {
		fmt.Fprintf(w, "Default Service Account\t%d\t🟠 MEDIUM\tCREATE DEDICATED SA\n", audit.Risks.DefaultServiceAccount)
	}
	if audit.Risks.MissingResourceLimits > 0 {
		fmt.Fprintf(w, "Missing Resource Limits\t%d\t🟠 MEDIUM\tADD LIMITS\n", audit.Risks.MissingResourceLimits)
	}
	if audit.Risks.PrivilegeEscalation > 0 {
		fmt.Fprintf(w, "Privilege Escalation Allowed\t%d\t🟠 MEDIUM\tDISABLE\n", audit.Risks.PrivilegeEscalation)
	}
	if audit.Risks.AddedCapabilities > 0 {
		fmt.Fprintf(w, "Added Capabilities\t%d\t🟠 MEDIUM\tREVIEW\n", audit.Risks.AddedCapabilities)
	}

	// Low risks
	if audit.Risks.MissingProbes > 0 {
		fmt.Fprintf(w, "Missing Health Probes\t%d\t🟢 LOW\tBEST PRACTICE\n", audit.Risks.MissingProbes)
	}
	if audit.Risks.WritableFilesystem > 0 {
		fmt.Fprintf(w, "Writable Root Filesystem\t%d\t🟢 LOW\tBEST PRACTICE\n", audit.Risks.WritableFilesystem)
	}

	w.Flush()
	fmt.Println()

	// Priority actions
	if len(audit.PriorityActions) > 0 {
		fmt.Println("PRIORITY ACTIONS:")
		fmt.Println()
		for i, action := range audit.PriorityActions {
			fmt.Printf("  %d. %s\n", i+1, action)
		}
		fmt.Println()
	}

	// Show sample issues for critical/high risks
	criticalIssues := []models.SecurityIssue{}
	highIssues := []models.SecurityIssue{}

	for _, issue := range audit.Issues {
		if issue.Severity == "critical" && len(criticalIssues) < 3 {
			criticalIssues = append(criticalIssues, issue)
		} else if issue.Severity == "high" && len(highIssues) < 3 {
			highIssues = append(highIssues, issue)
		}
	}

	if len(criticalIssues) > 0 {
		fmt.Println("CRITICAL ISSUES (sample):")
		fmt.Println()
		for _, issue := range criticalIssues {
			fmt.Printf("  🔴 %s/%s\n", issue.Namespace, issue.Name)
			fmt.Printf("     └─ %s\n", issue.Description)
			fmt.Printf("     └─ Fix: %s\n\n", issue.Remediation)
		}
	}

	if len(highIssues) > 0 {
		fmt.Println("HIGH PRIORITY ISSUES (sample):")
		fmt.Println()
		for _, issue := range highIssues {
			fmt.Printf("  🟡 %s/%s\n", issue.Namespace, issue.Name)
			fmt.Printf("     └─ %s\n", issue.Description)
			fmt.Printf("     └─ Fix: %s\n\n", issue.Remediation)
		}
	}

	// Summary recommendation
	if audit.SecurityScore < 60 {
		fmt.Println("⚠️  RECOMMENDATION: Address critical and high severity issues immediately")
		fmt.Println("   This cluster has significant security risks that should be remediated.")
	} else if audit.SecurityScore < 80 {
		fmt.Println("💡 RECOMMENDATION: Work on improving security posture")
		fmt.Println("   Focus on the priority actions listed above.")
	} else {
		fmt.Println("✅ GOOD: Security posture is solid. Continue following best practices.")
	}
}

// printSecurityJSON outputs security audit as JSON
func printSecurityJSON(audit *models.SecurityAudit) {
	data, err := json.MarshalIndent(audit, "", "  ")
	if err != nil {
		fmt.Printf("Error formatting JSON: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

// PrintSecuritySummary shows a quick security overview
func PrintSecuritySummary(audit *models.SecurityAudit) {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║           SECURITY QUICK CHECK                             ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	fmt.Printf("Security Score: %d/100\n\n", audit.SecurityScore)

	criticalCount := audit.Risks.PrivilegedContainers + audit.Risks.HostPID
	highCount := audit.Risks.HostNetwork + audit.Risks.HostIPC + audit.Risks.RunningAsRoot

	if criticalCount > 0 {
		fmt.Printf("🔴 CRITICAL: %d issues require immediate attention\n", criticalCount)
	}
	if highCount > 0 {
		fmt.Printf("🟡 HIGH: %d issues should be fixed soon\n", highCount)
	}
	if criticalCount == 0 && highCount == 0 {
		fmt.Println("✅ No critical or high severity issues found")
	}

	fmt.Println()
	fmt.Println("Run 'opscart-scan security --cluster <name>' for detailed analysis")
}
