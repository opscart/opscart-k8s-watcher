package analyzer

import (
	"context"
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
		env := detectEnvironment(pod.Namespace)

		// Check if this is an expected privileged pod
		isExpected := isExpectedPrivileged(pod.Name, pod.Namespace)

		// Only lower severity if it's SYSTEM and expected
		if env != "SYSTEM" {
			severity = "critical"
		} else if !isExpected {
			// System namespace but NOT in expected list - still concerning
			severity = "high"
		} else {
			// System namespace AND in expected list
			severity = "medium"
		}

		description := "Container running in privileged mode"
		if env == "SYSTEM" && isExpected {
			description = "Container running in privileged mode (expected for this infrastructure component)"
		} else if env == "SYSTEM" && !isExpected {
			description = "Container running in privileged mode (unexpected - review required)"
		}

		issues = append(issues, models.SecurityIssue{
			Type:        "privileged_container",
			Severity:    severity,
			Resource:    "container",
			Namespace:   pod.Namespace,
			Name:        fmt.Sprintf("%s/%s", pod.Name, container.Name),
			Description: description,
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

	// System/Infrastructure namespaces (comprehensive list covering major K8s distributions)
	infraPatterns := []string{
		// Core Kubernetes
		"kube-system", "kube-public", "kube-node-lease",

		// CNI (Container Network Interface)
		"calico-system", "tigera-", "cilium", "flannel", "weave", "canal",

		// Service Mesh
		"istio-system", "istio-", "linkerd", "consul", "consul-",

		// Storage
		"longhorn-system", "rook-", "openebs", "csi-", "portworx",

		// GitOps
		"argocd", "argo-", "flux-system", "flux-", "fleet-system",

		// Monitoring & Observability
		"monitoring", "prometheus", "grafana", "datadog", "newrelic",
		"dynatrace", "elastic-system", "splunk-", "jaeger", "tempo",

		// Security & Policy
		"cert-manager", "vault", "sealed-secrets", "gatekeeper-system",
		"falco", "trivy-system", "aqua-", "sysdig-", "neuvector-",

		// Ingress Controllers
		"ingress-nginx", "nginx-ingress", "traefik", "ambassador",
		"contour", "haproxy-",

		// Backup & DR
		"velero", "kasten-io", "stash-",

		// Cloud Provider Specific
		"azure-", "aws-", "gke-", "aks-", "eks-",
		"gmp-system", "config-management-system", // GCP
		"aad-pod-identity",                    // Azure
		"amazon-cloudwatch", "amazon-vpc-cni", // AWS

		// Platform/Distribution
		"openshift-", "rancher-", "cattle-", "tanzu-", "vmware-system-",
		"k3s-", "rke2-",

		// Autoscaling
		"karpenter", "cluster-autoscaler", "descheduler",

		// Service Catalog & Operators
		"operator-", "olm", "marketplace-",

		// Logging
		"logging", "fluentd", "fluent-bit", "loki", "elasticsearch",
	}

	for _, pattern := range infraPatterns {
		if strings.Contains(lower, pattern) {
			return "SYSTEM"
		}
	}

	// Production (check after infrastructure; exclude "prodfix" pattern)
	if strings.Contains(lower, "prod") && !strings.Contains(lower, "prodfix") {
		return "PRODUCTION"
	}

	// Staging/QA
	if strings.Contains(lower, "staging") ||
		strings.Contains(lower, "stage") ||
		strings.Contains(lower, "qa") ||
		strings.Contains(lower, "uat") ||
		strings.Contains(lower, "e2e") ||
		strings.Contains(lower, "perf") {
		return "STAGING"
	}

	// Default to development
	return "DEVELOPMENT"
}

// calculateIssueCounts separates actionable issues from expected infrastructure configurations
func calculateIssueCounts(issues []models.SecurityIssue) (actionable, infrastructure, prodStaging, development, systemUnexpected int) {
	for _, issue := range issues {
		env := detectEnvironment(issue.Namespace)

		// Check if it's an expected system configuration
		isExpectedSystemConfig := false
		if env == "SYSTEM" {
			// For privileged containers, check if expected
			if issue.Type == "privileged_container" {
				if strings.Contains(issue.Description, "expected for this infrastructure") {
					isExpectedSystemConfig = true
				}
			} else {
				// Other system namespace issues are considered expected infrastructure
				isExpectedSystemConfig = true
			}
		}

		if isExpectedSystemConfig {
			infrastructure++
		} else {
			actionable++

			// Count by environment for actionable issues
			switch env {
			case "PRODUCTION", "STAGING":
				prodStaging++
			case "DEVELOPMENT":
				development++
			case "SYSTEM":
				// System but not expected (e.g., unexpected privileged)
				systemUnexpected++
			}
		}
	}

	return
}

// isExpectedPrivileged checks if a pod legitimately needs privileged access
// Only certain system pods require privileged mode for their core functionality
func isExpectedPrivileged(podName, namespace string) bool {
	lower := strings.ToLower(podName)
	lowerNs := strings.ToLower(namespace)

	// CNI pods (need to manipulate network interfaces)
	if strings.Contains(lowerNs, "calico") ||
		strings.Contains(lowerNs, "cilium") ||
		strings.Contains(lowerNs, "flannel") ||
		strings.Contains(lowerNs, "weave") ||
		strings.Contains(lowerNs, "canal") ||
		strings.Contains(lower, "kindnet") {
		return true
	}

	// kube-proxy (manipulates iptables/ipvs)
	if strings.HasPrefix(lower, "kube-proxy") {
		return true
	}

	// Storage CSI drivers (mount/unmount volumes)
	if strings.Contains(lower, "csi-") ||
		strings.Contains(lower, "longhorn") ||
		strings.Contains(lower, "rook") ||
		strings.Contains(lower, "openebs") {
		return true
	}

	// Node-level monitoring agents (need host metrics)
	if strings.Contains(lower, "node-exporter") ||
		strings.Contains(lower, "datadog-agent") ||
		strings.Contains(lower, "newrelic-agent") ||
		strings.Contains(lower, "dynatrace-agent") ||
		strings.Contains(lower, "ama-logs") { // Azure Monitor Agent
		return true
	}

	// Node problem detector
	if strings.Contains(lower, "node-problem-detector") {
		return true
	}

	return false
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
