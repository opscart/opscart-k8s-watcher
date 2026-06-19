package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/report"
)

func runSecurityScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)

	// Default to table if not specified
	if securityFormat == "" {
		securityFormat = "table"
	}

	// Check if HTML report requested
	if securityFormat == "html" {
		return generateSecurityReport(clusterContext)
	}

	// Terminal output
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	sa := analyzer.NewSecurityAuditor(clientset)
	audit, err := sa.AuditClusterSecurity(namespace)
	if err != nil {
		return fmt.Errorf("auditing security: %w", err)
	}

	analyzer.PrintSecurityAudit(audit, securityFormat)
	return nil
}

func generateSecurityReport(clusterContext string) error {
	fmt.Println("📊 Generating security report...")

	// Get clientset and run audit
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	sa := analyzer.NewSecurityAuditor(clientset)
	audit, err := sa.AuditClusterSecurity(namespace)
	if err != nil {
		return fmt.Errorf("auditing security: %w", err)
	}

	// Run network policy audit for CIS 5.7.3 — real coverage data
	npa := analyzer.NewNetworkPolicyAuditor(clientset)
	netAudit, netErr := npa.AuditNetworkPolicies(namespace)
	if netErr != nil {
		netAudit = nil
	}

	// Calculate CIS score with real network data
	cisResult := analyzer.CalculateCISScore(audit, netAudit)

	// Build report data with REAL values
	reportData := &report.ReportData{
		ClusterName:    clusterContext,
		GeneratedAt:    time.Now(),
		CISScore:       cisResult.Score,
		SecurityScore:  cisResult.Score,
		ControlsPassed: cisResult.PassedChecks,
		ControlsFailed: cisResult.FailedChecks,
		PodCount:       audit.TotalPodsAudited,
		NamespaceCount: len(audit.Issues),
	}

	risks := audit.Risks

	// Add critical issues with details
	if risks.PrivilegedContainers > 0 {
		details := extractResourceNames(audit.Issues, "privileged_container", 5)
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d privileged containers detected", risks.PrivilegedContainers),
			Description: "Containers with elevated privileges can escape containment and compromise the host",
			Count:       risks.PrivilegedContainers,
			Details:     details,
		})
	}

	if risks.HostPID > 0 {
		details := extractResourceNames(audit.Issues, "host_pid", 5)
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d containers sharing host PID namespace", risks.HostPID),
			Description: "Host PID namespace sharing allows container processes to see all host processes",
			Count:       risks.HostPID,
			Details:     details,
		})
	}

	if risks.HostPathVolumes > 0 {
		details := extractResourceNames(audit.Issues, "host_path_volume", 5)
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d pods mounting host paths", risks.HostPathVolumes),
			Description: "Host path volumes provide direct access to host filesystem",
			Count:       risks.HostPathVolumes,
			Details:     details,
		})
	}

	// Add warnings with details
	if risks.HostIPC > 0 {
		details := extractResourceNames(audit.Issues, "host_ipc", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers sharing host IPC namespace", risks.HostIPC),
			Description: "Host IPC namespace sharing can leak sensitive information",
			Count:       risks.HostIPC,
			Details:     details,
		})
	}

	if risks.RunningAsRoot > 0 {
		details := extractResourceNames(audit.Issues, "running_as_root", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers running as root", risks.RunningAsRoot),
			Description: "Running as root increases attack surface",
			Count:       risks.RunningAsRoot,
			Details:     details,
		})
	}

	if risks.MissingResourceLimits > 0 {
		details := extractResourceNames(audit.Issues, "missing_resource_limits", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers missing resource limits", risks.MissingResourceLimits),
			Description: "Missing resource limits can lead to resource exhaustion",
			Count:       risks.MissingResourceLimits,
			Details:     details,
		})
	}

	if risks.HostNetwork > 0 {
		details := extractResourceNames(audit.Issues, "host_network", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers using host network", risks.HostNetwork),
			Description: "Host network access bypasses network policies",
			Count:       risks.HostNetwork,
			Details:     details,
		})
	}

	if risks.PrivilegeEscalation > 0 {
		details := extractResourceNames(audit.Issues, "privilege_escalation", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers allowing privilege escalation", risks.PrivilegeEscalation),
			Description: "Privilege escalation can lead to container breakout",
			Count:       risks.PrivilegeEscalation,
			Details:     details,
		})
	}

	if risks.AddedCapabilities > 0 {
		details := extractResourceNames(audit.Issues, "added_capabilities", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers with added capabilities", risks.AddedCapabilities),
			Description: "Unnecessary capabilities increase attack surface",
			Count:       risks.AddedCapabilities,
			Details:     details,
		})
	}

	if risks.DefaultServiceAccount > 0 {
		details := extractResourceNames(audit.Issues, "default_service_account", 5)
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d pods using default service account", risks.DefaultServiceAccount),
			Description: "Default service account may have excessive permissions",
			Count:       risks.DefaultServiceAccount,
			Details:     details,
		})
	}

	// Generate HTML report
	generator := report.NewGenerator(report.FormatHTML, "")
	outputPath, err := generator.GenerateSecurityHTML(reportData)
	if err != nil {
		return fmt.Errorf("generating report: %w", err)
	}

	fmt.Printf("\n✅ Security report generated: %s\n", outputPath)
	fmt.Printf("🌐 Open in browser: file://%s\n", outputPath)
	fmt.Printf("\n📊 Summary: CIS Score %d/100 | %d Critical | %d Warnings | %d Total Issues\n",
		cisResult.Score, len(reportData.CriticalIssues), len(reportData.WarningIssues), len(audit.Issues))

	return nil
}

// extractResourceNames gets top N resource names (deduplicated with counts)
func extractResourceNames(issues []models.SecurityIssue, issueType string, limit int) []string {
	podCounts := make(map[string]int)

	for _, issue := range issues {
		if issue.Type == issueType {
			key := issue.Namespace + "/" + issue.Name
			podCounts[key]++
		}
	}

	type podInfo struct {
		key   string
		count int
	}
	var pods []podInfo
	for key, count := range podCounts {
		pods = append(pods, podInfo{key, count})
	}

	// Sort by count descending
	for i := 0; i < len(pods)-1; i++ {
		for j := i + 1; j < len(pods); j++ {
			if pods[j].count > pods[i].count {
				pods[i], pods[j] = pods[j], pods[i]
			}
		}
	}

	var resources []string
	for i := 0; i < len(pods) && i < limit; i++ {
		parts := strings.Split(pods[i].key, "/")
		podName := parts[1]
		namespace := parts[0]

		if pods[i].count > 1 {
			resources = append(resources, fmt.Sprintf("%s in namespace %s (%d issues)", podName, namespace, pods[i].count))
		} else {
			resources = append(resources, fmt.Sprintf("%s in namespace %s", podName, namespace))
		}
	}

	remaining := len(pods) - limit
	if remaining > 0 {
		resources = append(resources, fmt.Sprintf("... and %d more pods", remaining))
	}

	return resources
}
