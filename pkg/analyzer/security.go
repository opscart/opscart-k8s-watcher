package analyzer

import (
	"context"
	"fmt"

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

	// Calculate security score
	audit.SecurityScore = sa.calculateSecurityScore(audit)

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
				Remediation: "Remove hostPath volume - use PersistentVolumeClaims or emptyDir instead. hostPath gives direct access to host filesystem.",
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
				Remediation: "Create a dedicated ServiceAccount with minimal required permissions. Default SA may have excessive privileges.",
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
				Remediation: "Remove hostNetwork: true unless absolutely necessary for CNI plugins",
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
				Remediation: "Remove hostPID: true - rarely needed",
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
				Remediation: "Remove hostIPC: true unless required for shared memory",
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
			Remediation: "Remove privileged: true - grants all capabilities",
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
				Remediation: "Review if added capabilities are necessary. Drop all and add only required ones",
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

	// Check health probes
	hasLiveness := container.LivenessProbe != nil
	hasReadiness := container.ReadinessProbe != nil

	if !hasLiveness && !hasReadiness {
		issues = append(issues, models.SecurityIssue{
			Type:        "missing_probes",
			Severity:    "low",
			Resource:    "container",
			Namespace:   pod.Namespace,
			Name:        fmt.Sprintf("%s/%s", pod.Name, container.Name),
			Description: "Container missing health probes",
			Remediation: "Add livenessProbe and readinessProbe for better reliability",
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

	// Check read-only root filesystem
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem {
		// This is a best practice but not always feasible, so low severity
		if !isSystemNamespace {
			issues = append(issues, models.SecurityIssue{
				Type:        "writable_filesystem",
				Severity:    "low",
				Resource:    "container",
				Namespace:   pod.Namespace,
				Name:        fmt.Sprintf("%s/%s", pod.Name, container.Name),
				Description: "Container filesystem is writable",
				Remediation: "Consider setting securityContext.readOnlyRootFilesystem: true",
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
	case "missing_probes":
		audit.Risks.MissingProbes++
	case "added_capabilities":
		audit.Risks.AddedCapabilities++
	case "privilege_escalation":
		audit.Risks.PrivilegeEscalation++
	case "writable_filesystem":
		audit.Risks.WritableFilesystem++
	}
}

// calculateSecurityScore calculates a 0-100 security score with exponential penalties
func (sa *SecurityAuditor) calculateSecurityScore(audit *models.SecurityAudit) int {
	if audit.TotalPodsAudited == 0 {
		return 100
	}

	score := 100.0

	// CRITICAL ISSUES - Exponential penalties (these are severe!)
	// Privileged containers: Each one is exponentially worse
	if audit.Risks.PrivilegedContainers > 0 {
		penalty := float64(audit.Risks.PrivilegedContainers) * float64(audit.Risks.PrivilegedContainers) * 2.0
		score -= penalty
	}

	// HostPath volumes: Direct host filesystem access (exponential)
	if audit.Risks.HostPathVolumes > 0 {
		penalty := float64(audit.Risks.HostPathVolumes) * float64(audit.Risks.HostPathVolumes) * 2.0
		score -= penalty
	}

	// Host PID: Can see all host processes (exponential)
	if audit.Risks.HostPID > 0 {
		penalty := float64(audit.Risks.HostPID) * float64(audit.Risks.HostPID) * 1.5
		score -= penalty
	}

	// HIGH SEVERITY - Aggressive linear penalties
	// Host Network: Bypasses network policies
	score -= float64(audit.Risks.HostNetwork) * 4.0

	// Host IPC: Shared memory risks
	score -= float64(audit.Risks.HostIPC) * 4.0

	// MEDIUM SEVERITY - Moderate penalties with scaling
	// Running as root: Bad practice, scales with count
	if audit.Risks.RunningAsRoot > 0 {
		// First 10: 0.5 each, after that: 1.0 each
		if audit.Risks.RunningAsRoot <= 10 {
			score -= float64(audit.Risks.RunningAsRoot) * 0.5
		} else {
			score -= 5.0 + float64(audit.Risks.RunningAsRoot-10)*1.0
		}
	}

	// Missing resource limits: Can DoS cluster
	if audit.Risks.MissingResourceLimits > 0 {
		// First 10: 0.5 each, after that: 1.0 each
		if audit.Risks.MissingResourceLimits <= 10 {
			score -= float64(audit.Risks.MissingResourceLimits) * 0.5
		} else {
			score -= 5.0 + float64(audit.Risks.MissingResourceLimits-10)*1.0
		}
	}

	// Privilege escalation allowed
	score -= float64(audit.Risks.PrivilegeEscalation) * 1.0

	// Added capabilities
	score -= float64(audit.Risks.AddedCapabilities) * 1.5

	// Default service account usage
	score -= float64(audit.Risks.DefaultServiceAccount) * 0.8

	// LOW SEVERITY - Small penalties
	score -= float64(audit.Risks.MissingProbes) * 0.2
	score -= float64(audit.Risks.WritableFilesystem) * 0.1

	// Ensure score stays in 0-100 range
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	return int(score)
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

	// Best practices
	if audit.Risks.MissingProbes > 0 {
		actions = append(actions, "Implement health probes for better reliability")
	}

	return actions
}
