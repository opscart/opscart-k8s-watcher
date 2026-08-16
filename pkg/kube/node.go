package kube

import corev1 "k8s.io/api/core/v1"

// NodePoolName returns the node-pool identity used by OpsCart's existing
// Infrastructure analysis. Label precedence is intentionally preserved.
func NodePoolName(node corev1.Node) string {
	labels := node.Labels
	if pool, ok := labels["agentpool"]; ok {
		return pool
	}
	if pool, ok := labels["kubernetes.azure.com/agentpool"]; ok {
		return pool
	}
	if _, ok := labels["node.kubernetes.io/instance-type"]; ok {
		if pool := labels["cloud.google.com/gke-nodepool"]; pool != "" {
			return pool
		}
		return labels["eks.amazonaws.com/nodegroup"]
	}
	return ""
}
