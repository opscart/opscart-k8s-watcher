package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodePoolNamePreservesInfrastructureLabelBehavior(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"AKS legacy", map[string]string{"agentpool": "system"}, "system"},
		{"AKS qualified", map[string]string{"kubernetes.azure.com/agentpool": "apps"}, "apps"},
		{"GKE", map[string]string{"node.kubernetes.io/instance-type": "e2-standard-4", "cloud.google.com/gke-nodepool": "workers"}, "workers"},
		{"EKS", map[string]string{"node.kubernetes.io/instance-type": "m7i.large", "eks.amazonaws.com/nodegroup": "workers"}, "workers"},
		{"existing precedence", map[string]string{"agentpool": "first", "kubernetes.azure.com/agentpool": "second", "node.kubernetes.io/instance-type": "m7i.large", "eks.amazonaws.com/nodegroup": "third"}, "first"},
		{"no recognized pool", map[string]string{"eks.amazonaws.com/nodegroup": "ungated"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: tc.labels}}
			if got := NodePoolName(node); got != tc.want {
				t.Fatalf("NodePoolName() = %q, want %q", got, tc.want)
			}
		})
	}
}
