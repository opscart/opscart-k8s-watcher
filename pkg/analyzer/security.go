package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SecurityAuditor performs security analysis on cluster workloads
type SecurityAuditor struct {
	clientset *kubernetes.Clientset
	ctx       context.Context
}

// NewSecurityAuditor creates a new security auditor
func NewSecurityAuditor(clientset *kubernetes.Clientset) *SecurityAuditor {
	return &SecurityAuditor{
		clientset: clientset,
		ctx:       context.Background(),
	}
}

// AuditClusterSecurity performs comprehensive security audit
func (sa *SecurityAuditor) AuditClusterSecurity(namespace string) (*models.SecurityAudit, error) {
	audit := &models.SecurityAudit{
		TotalPodsAudited: 0,
		Risks:            models.SecurityRisks{},
		Issues:           []models.SecurityIssue{},
	}

	// Get all pods
	podList, err := sa.clientset.CoreV1().Pods(namespace).List(sa.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	audit.TotalPodsAudited = len(podList.Items)

	// Audit each pod
	for _, pod := range podList.Items {
		issues := sa.auditPod(pod)
		audit.Issues = append(audit.Issues, issues...)

		// Count risks
		for _, issue := range issues {
			sa.incrementRiskCounter(audit, issue.Type)
		}
	}

	// Generate priority actions
	audit.PriorityActions = sa.generatePriorityActions(audit)

	return audit, nil
}

// auditPod checks a single pod for security issues
func (sa *SecurityAuditor) auditPod(pod corev1.Pod) []models.SecurityIssue {
	var issues []models.SecurityIssue

	// Skip system namespaces for some checks
	isSystemNamespace := pod.Namespace == "kube-system" || pod.Namespace == "istio-system"

	// Check for hostPath volumes (CRITICAL)
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			severity := "high"
			if !isSystemNamespace {
				severity = "critical"
			}
			issues = append(issues, models.SecurityIssue{
				Type:        "host_path_volume",
				Severity:    severity,
				Resource:    "pod",
				Namespace:   pod.Namespace,
				Name:        pod.Name,
				Description: fmt.Sprintf("Pod mounts host path: %s", volume.HostPath.Path),
				Remediation: "Remove hostPath volume - use PersistentVolumeClaims or emptyDir instead",
			})
		}
	}

	// Check for default service account usage
	serviceAccount := pod.Spec.ServiceAccountName
	if serviceAccount == "" || serviceAccount == "default" {
		if !isSystemNamespace {
			issues = append(issues, models.SecurityIssue{
				Type:        "default_service_account",
				Severity:    "medium",
				Resource:    "pod",
				Namespace:   pod.Namespace,
				Name:        pod.Name,
				Description: "Pod uses default service account",
				Remediation: "Create a dedicated ServiceAccount with minimal permissions",
			})
		}
	}

	// Check pod-level security context
	if pod.Spec.SecurityContext != nil {
		// Host network
		if pod.Spec.HostNetwork {
			severity := "high"
			if !isSystemNamespace {
				severity = "critical"
			}
			issues = append(issues, models.SecurityIssue{
				Type:        "host_network",
				Severity:    severity,
				Resource:    "pod",
				Namespace:   pod.Namespace,
				Name:        pod.Name,
				Description: "Pod uses host network namespace",
				Remediation: "Remove hostNetwork: true unless absolutely necessary",
			})
		}

		// Host PID
		if pod.Spec.HostPID {
			issues = append(issues, models.SecurityIssue{
				Type:        "host_pid",
				Severity:    "critical",
				Resource:    "pod",
				Namespace:   pod.Namespace,
				Name:        pod.Name,
				Description: "Pod uses host PID namespace",
				Remediation: "Remove hostPID: true",
			})
		}

		// Host IPC
		if pod.Spec.HostIPC {
			issues = append(issues, models.SecurityIssue{
				Type:        "host_ipc",
				Severity:    "high",
				Resource:    "pod",
				Namespace:   pod.Namespace,
				Name:        pod.Name,
				Description: "Pod uses host IPC namespace",
				Remediation: "Remove hostIPC: true",
			})
		}
	}

	// Check each container
	for _, container := range pod.Spec.Containers {
		containerIssues := sa.auditContainer(pod, container, isSystemNamespace)
		issues = append(issues, containerIssues...)
	}

	return issues
}

// auditContainer checks a single container for security issues
func (sa *SecurityAuditor) auditContainer(pod corev1.Pod, container corev1.Container, isSystemNamespace bool) []models.SecurityIssue {
	var issues []models.SecurityIssue

	// Check if running as root
	runAsRoot := true
	if container.SecurityContext != nil && container.SecurityContext.RunAsNonRoot != nil {
		runAsRoot = !*container.SecurityContext.RunAsNonRoot
	} else if pod.Spec.SecurityContext != nil && pod.Spec.SecurityContext.RunAsNonRoot != nil {
		runAsRoot = !*pod.Spec.SecurityContext.RunAsNonRoot
	}

	if runAsRoot && !isSystemNamespace {
		issues = append(issues, models.SecurityIssue{
			Type:        "running_as_root",
			Severity:    "medium",
			Resource:    "container",
			Namespace:   pod.Namespace,
			Name:        fmt.Sprintf("%s/%s", pod.Name, container.Name),
			Description: "Container running as root user",
			Remediation: "Add securityContext.runAsNonRoot: true and runAsUser: <non-zero>",
		})
	}

	// Check privileged containers
	if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		severity := "high"
		if !isSystemNamespace {
			severity = "critical"
		}
		issues = append(issues, models.SecurityIssue{
			Type:        "privileged_container",
			Severity:    severity,
			Resource:    "container",
			Namespace:   pod.Namespace,
			Name:        fmt.Sprintf("%s/%s", pod.Name, container.Name),
			Description: "Container running in privileged mode",
			Remediation: "Remove privileged: true",
		})
	}

	// Check added capabilities
	if container.SecurityContext != nil && container.SecurityContext.Capabilities != nil {
		if len(container.SecurityContext.Capabilities.Add) > 0 {
			capsList := fmt.Sprintf("%v", container.SecurityContext.Capabilities.Add)
			issues = append(issues, models.SecurityIssue{
				Type:        "added_capabilities",
				Severity:    "medium",
				Resource:    "container",
				Namespace:   pod.Namespace,
				Name:        fmt.Sprintf("%s/%s", pod.Name, container.Name),
				Description: fmt.Sprintf("Container adds capabilities: %s", capsList),
				Remediation: "Drop all capabilities and add only required ones",
			})
		}
	}

	// Check resource limits
	if container.Resources.Limits == nil ||
		(container.Resources.Limits.Cpu().IsZero() && container.Resources.Limits.Memory().IsZero()) {
		issues = append(issues, models.SecurityIssue{
			Type:        "missing_resource_limits",
			Severity:    "medium",
			Resource:    "container",
			Namespace:   pod.Namespace,
			Name:        fmt.Sprintf("%s/%s", pod.Name, container.Name),
			Description: "Container missing CPU/memory limits",
			Remediation: "Add resources.limits.cpu and resources.limits.memory",
		})
	}

	// Check for allowPrivilegeEscalation
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil ||
		*container.SecurityContext.AllowPrivilegeEscalation {
		if !isSystemNamespace {
			issues = append(issues, models.SecurityIssue{
				Type:        "privilege_escalation",
				Severity:    "medium",
				Resource:    "container",
				Namespace:   pod.Namespace,
				Name:        fmt.Sprintf("%s/%s", pod.Name, container.Name),
				Description: "Container allows privilege escalation",
				Remediation: "Set securityContext.allowPrivilegeEscalation: false",
			})
		}
	}

	return issues
}

// incrementRiskCounter increments the appropriate risk counter
func (sa *SecurityAuditor) incrementRiskCounter(audit *models.SecurityAudit, issueType string) {
	switch issueType {
	case "running_as_root":
		audit.Risks.RunningAsRoot++
	case "privileged_container":
		audit.Risks.PrivilegedContainers++
	case "host_network":
		audit.Risks.HostNetwork++
	case "host_pid":
		audit.Risks.HostPID++
	case "host_ipc":
		audit.Risks.HostIPC++
	case "host_path_volume":
		audit.Risks.HostPathVolumes++
	case "default_service_account":
		audit.Risks.DefaultServiceAccount++
	case "missing_resource_limits":
		audit.Risks.MissingResourceLimits++
	case "added_capabilities":
		audit.Risks.AddedCapabilities++
	case "privilege_escalation":
		audit.Risks.PrivilegeEscalation++
	}
}

// generatePriorityActions creates a prioritized action list
func (sa *SecurityAuditor) generatePriorityActions(audit *models.SecurityAudit) []string {
	var actions []string

	// Critical actions first
	if audit.Risks.HostPathVolumes > 0 {
		actions = append(actions, "Remove hostPath volumes (critical filesystem access)")
	}
	if audit.Risks.PrivilegedContainers > 0 {
		actions = append(actions, "Fix privileged containers (highest risk)")
	}
	if audit.Risks.HostPID > 0 {
		actions = append(actions, "Remove hostPID usage (critical security risk)")
	}

	// High priority
	if audit.Risks.HostNetwork > 0 {
		actions = append(actions, "Review and minimize hostNetwork usage")
	}
	if audit.Risks.HostIPC > 0 {
		actions = append(actions, "Remove hostIPC usage where not required")
	}
	if audit.Risks.RunningAsRoot > 0 {
		actions = append(actions, "Configure pods to run as non-root user")
	}

	// Medium priority
	if audit.Risks.DefaultServiceAccount > 0 {
		actions = append(actions, "Create dedicated ServiceAccounts with minimal permissions")
	}
	if audit.Risks.MissingResourceLimits > 0 {
		actions = append(actions, "Add resource limits to all pods")
	}
	if audit.Risks.PrivilegeEscalation > 0 {
		actions = append(actions, "Set allowPrivilegeEscalation: false")
	}

	return actions
}

// ===================================================================
// HELPER FUNCTIONS - FIX #2 and #3
// ===================================================================

// detectEnvironment detects environment type from namespace name
func detectEnvironment(namespace string) string {
	lower := strings.ToLower(namespace)

	// System namespaces
	if strings.Contains(lower, "kube-system") ||
		strings.Contains(lower, "kube-public") ||
		strings.Contains(lower, "istio-system") ||
		strings.Contains(lower, "monitoring") {
		return "SYSTEM"
	}

	// Production
	if strings.Contains(lower, "prod") {
		return "PRODUCTION"
	}

	// Staging/QA
	if strings.Contains(lower, "staging") ||
		strings.Contains(lower, "stage") ||
		strings.Contains(lower, "qa") ||
		strings.Contains(lower, "uat") {
		return "STAGING"
	}

	// Default to development
	return "DEVELOPMENT"
}

// filterIssuesByType returns issues of a specific type
func filterIssuesByType(issues []models.SecurityIssue, issueType string) []models.SecurityIssue {
	var filtered []models.SecurityIssue
	for _, issue := range issues {
		if issue.Type == issueType {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

// getTopIssues returns top N issues of a specific type
func getTopIssues(issues []models.SecurityIssue, issueType string, limit int) []models.SecurityIssue {
	filtered := filterIssuesByType(issues, issueType)

	// Group by environment priority
	production := []models.SecurityIssue{}
	staging := []models.SecurityIssue{}
	development := []models.SecurityIssue{}
	system := []models.SecurityIssue{}

	for _, issue := range filtered {
		env := detectEnvironment(issue.Namespace)
		switch env {
		case "PRODUCTION":
			production = append(production, issue)
		case "STAGING":
			staging = append(staging, issue)
		case "SYSTEM":
			system = append(system, issue)
		default:
			development = append(development, issue)
		}
	}

	// Combine with production first
	result := []models.SecurityIssue{}
	result = append(result, production...)
	result = append(result, staging...)
	result = append(result, development...)
	result = append(result, system...)

	if len(result) > limit {
		return result[:limit]
	}
	return result
}

// countByEnvironment counts issues by environment type
func countByEnvironment(issues []models.SecurityIssue, issueType string) map[string]int {
	counts := map[string]int{
		"PRODUCTION":  0,
		"STAGING":     0,
		"DEVELOPMENT": 0,
		"SYSTEM":      0,
	}

	filtered := filterIssuesByType(issues, issueType)
	for _, issue := range filtered {
		env := detectEnvironment(issue.Namespace)
		counts[env]++
	}

	return counts
}

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
	cisResult := CalculateCISScore(audit)
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
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("CLUSTER SECURITY SUMMARY")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("Pods Scanned: %d\n", audit.TotalPodsAudited)
	fmt.Printf("Issues Found: %d\n", len(audit.Issues))
	fmt.Println()
}

func printDetailedFindings(audit *models.SecurityAudit) {
	risks := audit.Risks

	fmt.Println("\n═══════════════════════════════════════════════════════════")
	fmt.Println("DETAILED SECURITY FINDINGS")
	fmt.Println("═══════════════════════════════════════════════════════════")

	// Critical findings with TOP 5 resources (FIX #2)
	if hasAnyCriticalFindings(risks) {
		fmt.Println("\n🔴 CRITICAL FINDINGS:")

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

	// Show environment breakdown if multiple environments
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

	// Show top 5 specific resources (FIX #2)
	topIssues := getTopIssues(allIssues, issueType, 5)
	if len(topIssues) > 0 {
		fmt.Println("    Top resources:")
		for i, issue := range topIssues {
			env := detectEnvironment(issue.Namespace)
			envLabel := ""
			if env == "PRODUCTION" {
				envLabel = " [PROD]"
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

	// Always show breakdown for transparency
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("ISSUE COUNT BREAKDOWN")
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
	fmt.Printf("  TOTAL:                      %3d\n", totalCounted)

	// Only show warning if there's a discrepancy
	if totalCounted != actualIssues {
		fmt.Println()
		fmt.Printf("⚠️  WARNING: Total (%d) doesn't match Issues Found (%d)\n", totalCounted, actualIssues)
		fmt.Printf("Difference: %d issues not tracked in SecurityRisks\n", actualIssues-totalCounted)
	} else {
		fmt.Printf("\nCount verified: All %d issues accounted for\n", actualIssues)
	}
	fmt.Println()
}

// PrintSecurityAuditJSON outputs security audit in JSON format
func PrintSecurityAuditJSON(audit *models.SecurityAudit) {
	// Calculate CIS score
	cisResult := CalculateCISScore(audit)

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
