package analyzer

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
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
