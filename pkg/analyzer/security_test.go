package analyzer

import (
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestDetectEnvironment(t *testing.T) {
	tests := []struct {
		namespace string
		want      string
	}{
		// Core system namespaces
		{"kube-system", "SYSTEM"},
		{"kube-public", "SYSTEM"},
		{"kube-node-lease", "SYSTEM"},
		// CNI / network
		{"calico-system", "SYSTEM"},
		{"istio-system", "SYSTEM"},
		// Monitoring
		{"monitoring", "SYSTEM"},
		{"prometheus", "SYSTEM"},
		// GitOps
		{"argocd", "SYSTEM"},
		{"flux-system", "SYSTEM"},
		// Security / ingress
		{"cert-manager", "SYSTEM"},
		{"ingress-nginx", "SYSTEM"},
		// Cloud provider prefix
		{"azure-arc", "SYSTEM"},
		{"gke-system", "SYSTEM"},
		// Production
		{"production", "PRODUCTION"},
		{"payments-prod", "PRODUCTION"},
		{"api-prod", "PRODUCTION"},
		// prodfix is intentionally NOT production
		{"prodfix", "DEVELOPMENT"},
		// Staging / QA
		{"staging", "STAGING"},
		{"qa-testing", "STAGING"},
		{"uat-services", "STAGING"},
		{"e2e-tests", "STAGING"},
		// Development / default
		{"default", "DEVELOPMENT"},
		{"my-app", "DEVELOPMENT"},
		{"team-alpha", "DEVELOPMENT"},
	}
	for _, tc := range tests {
		t.Run(tc.namespace, func(t *testing.T) {
			got := detectEnvironment(tc.namespace)
			if got != tc.want {
				t.Errorf("detectEnvironment(%q) = %q, want %q", tc.namespace, got, tc.want)
			}
		})
	}
}

func TestAuditContainerUsesPodSpecEvidenceWording(t *testing.T) {
	sa := &SecurityAuditor{}
	pod := corev1.Pod{}
	pod.Name = "api"
	pod.Namespace = "app"
	container := corev1.Container{Name: "main"}

	issues := sa.auditContainer(pod, container, false)
	var nonRoot models.SecurityIssue
	for _, issue := range issues {
		if issue.Type == "running_as_root" {
			nonRoot = issue
		}
	}
	if nonRoot.Description != "Non-root execution not explicitly enforced in the pod spec" {
		t.Fatalf("description = %q", nonRoot.Description)
	}
	if strings.Contains(strings.ToLower(nonRoot.Description), "running as root") {
		t.Fatalf("description makes an unsupported runtime claim: %q", nonRoot.Description)
	}
}

func TestAuditContainerDistinguishesMissingResourceLimits(t *testing.T) {
	tests := []struct {
		name   string
		limits corev1.ResourceList
		want   string
	}{
		{name: "both", want: "missing CPU and memory limits"},
		{name: "cpu", limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}, want: "missing a CPU limit"},
		{name: "memory", limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, want: "missing a memory limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sa := &SecurityAuditor{}
			pod := corev1.Pod{}
			pod.Name = "api"
			pod.Namespace = "app"
			container := corev1.Container{
				Name:      "main",
				Resources: corev1.ResourceRequirements{Limits: tt.limits},
			}
			issues := sa.auditContainer(pod, container, false)
			for _, issue := range issues {
				if issue.Type == "missing_resource_limits" {
					if !strings.Contains(issue.Description, tt.want) {
						t.Fatalf("description = %q, want substring %q", issue.Description, tt.want)
					}
					return
				}
			}
			t.Fatal("missing_resource_limits issue not found")
		})
	}
}

func TestPriorityActionsUseReviewLanguage(t *testing.T) {
	sa := &SecurityAuditor{}
	audit := &models.SecurityAudit{Risks: models.SecurityRisks{
		HostPathVolumes:       1,
		MissingResourceLimits: 1,
	}}
	actions := strings.Join(sa.generatePriorityActions(audit), "\n")
	for _, want := range []string{
		"Review hostPath mounts and verify which workloads require host access",
		"Review containers missing CPU or memory limits",
	} {
		if !strings.Contains(actions, want) {
			t.Errorf("missing softened action %q in %q", want, actions)
		}
	}
	for _, forbidden := range []string{"Remove hostPath volumes", "Add resource limits to all pods"} {
		if strings.Contains(actions, forbidden) {
			t.Errorf("contains unconditional action %q", forbidden)
		}
	}
}

func TestHostPathFindingPreservesEvidenceWithoutCriticalClaim(t *testing.T) {
	sa := &SecurityAuditor{}
	pod := corev1.Pod{}
	pod.Name = "api"
	pod.Namespace = "app"
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "host",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/app"},
		},
	}}
	issues := sa.auditPod(pod)
	for _, issue := range issues {
		if issue.Type == "host_path_volume" {
			if issue.Severity == "critical" {
				t.Fatalf("hostPath occurrence was unconditionally critical: %+v", issue)
			}
			if !strings.Contains(issue.Description, "/var/lib/app") {
				t.Fatalf("hostPath evidence missing: %+v", issue)
			}
			if !strings.Contains(issue.Remediation, "Review") {
				t.Fatalf("hostPath recommendation is not review-oriented: %+v", issue)
			}
			return
		}
	}
	t.Fatal("hostPath finding not produced")
}

func TestIsExpectedPrivileged(t *testing.T) {
	tests := []struct {
		podName   string
		namespace string
		want      bool
	}{
		// kube-proxy
		{"kube-proxy-abc123", "kube-system", true},
		// Azure Monitor Agent
		{"ama-logs-xyz", "kube-system", true},
		// Node exporter
		{"node-exporter-daemonset-q8f", "monitoring", true},
		// calico CNI (namespace match)
		{"calico-node-k7p", "calico-system", true},
		// kindnet (pod name match)
		{"kindnet-9pqrx", "kube-system", true},
		// CSI driver
		{"csi-azuredisk-node-abc", "kube-system", true},
		// longhorn
		{"longhorn-manager-x", "longhorn-system", true},
		// node-problem-detector
		{"node-problem-detector-r7t", "kube-system", true},
		// datadog agent
		{"datadog-agent-j7k", "datadog", true},
		// newrelic agent
		{"newrelic-agent-8x9", "newrelic", true},
		// arbitrary pod in kube-system is NOT expected
		{"some-custom-pod", "kube-system", false},
		// user workloads
		{"my-api-server", "default", false},
		{"payments-service", "production", false},
	}
	for _, tc := range tests {
		t.Run(tc.podName+"/"+tc.namespace, func(t *testing.T) {
			got := isExpectedPrivileged(tc.podName, tc.namespace)
			if got != tc.want {
				t.Errorf("isExpectedPrivileged(%q, %q) = %v, want %v",
					tc.podName, tc.namespace, got, tc.want)
			}
		})
	}
}

func TestCalculateIssueCounts(t *testing.T) {
	make := func(issueType, ns, desc string) models.SecurityIssue {
		return models.SecurityIssue{Type: issueType, Namespace: ns, Description: desc}
	}

	tests := []struct {
		name              string
		issues            []models.SecurityIssue
		wantActionable    int
		wantInfra         int
		wantProdStaging   int
		wantDevelopment   int
		wantSysUnexpected int
	}{
		{
			name: "empty input",
		},
		{
			name: "expected privileged in system = infrastructure",
			issues: []models.SecurityIssue{
				make("privileged_container", "kube-system",
					"Container running in privileged mode (expected for this infrastructure component)"),
			},
			wantInfra: 1,
		},
		{
			name: "unexpected privileged in system = actionable + systemUnexpected",
			issues: []models.SecurityIssue{
				make("privileged_container", "kube-system",
					"Container running in privileged mode (review required)"),
			},
			wantActionable:    1,
			wantSysUnexpected: 1,
		},
		{
			name: "non-privileged issues in system namespaces = infrastructure",
			issues: []models.SecurityIssue{
				make("missing_resource_limits", "kube-system", ""),
				make("running_as_root", "istio-system", ""),
			},
			wantInfra: 2,
		},
		{
			name: "production issue = actionable + prodStaging",
			issues: []models.SecurityIssue{
				make("privileged_container", "payments-prod", "Container is privileged"),
			},
			wantActionable:  1,
			wantProdStaging: 1,
		},
		{
			name: "staging issue = actionable + prodStaging",
			issues: []models.SecurityIssue{
				make("running_as_root", "qa-testing", ""),
			},
			wantActionable:  1,
			wantProdStaging: 1,
		},
		{
			name: "development issue = actionable + development",
			issues: []models.SecurityIssue{
				make("running_as_root", "my-app", ""),
			},
			wantActionable:  1,
			wantDevelopment: 1,
		},
		{
			name: "mixed: infra + prod + dev",
			issues: []models.SecurityIssue{
				make("privileged_container", "kube-system",
					"Container running in privileged mode (expected for this infrastructure component)"),
				make("running_as_root", "api-prod", ""),
				make("missing_resource_limits", "my-service", ""),
			},
			wantActionable:  2,
			wantInfra:       1,
			wantProdStaging: 1,
			wantDevelopment: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotA, gotI, gotPS, gotD, gotSU := calculateIssueCounts(tc.issues)
			if gotA != tc.wantActionable {
				t.Errorf("actionable: got %d, want %d", gotA, tc.wantActionable)
			}
			if gotI != tc.wantInfra {
				t.Errorf("infrastructure: got %d, want %d", gotI, tc.wantInfra)
			}
			if gotPS != tc.wantProdStaging {
				t.Errorf("prodStaging: got %d, want %d", gotPS, tc.wantProdStaging)
			}
			if gotD != tc.wantDevelopment {
				t.Errorf("development: got %d, want %d", gotD, tc.wantDevelopment)
			}
			if gotSU != tc.wantSysUnexpected {
				t.Errorf("systemUnexpected: got %d, want %d", gotSU, tc.wantSysUnexpected)
			}
		})
	}
}
