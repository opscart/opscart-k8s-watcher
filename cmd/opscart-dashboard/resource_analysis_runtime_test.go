package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func deploymentPod(namespace, name, deployment string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: deployment + "-7d8f9c6b5", Controller: boolPtr(true)},
			},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

// TestBuildResourceAnalysisMatchesDirectAnalyzerCall proves
// buildResourceAnalysis's snapshot-resources adaptation produces exactly
// what calling analyzer.AnalyzeResources directly on the same value slices
// would — "same input produces equivalent resource analysis".
func TestBuildResourceAnalysisMatchesDirectAnalyzerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Nodes: []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}},
		Pods:  []*corev1.Pod{deploymentPod("payments", "payments-api-abc12", "payments-api")},
	}

	got := buildResourceAnalysis(resources, "")
	want := analyzer.AnalyzeResources([]corev1.Pod{*resources.Pods[0]}, []corev1.Node{*resources.Nodes[0]}, "")

	if len(got.Workloads) != len(want.Workloads) || len(got.Workloads) != 1 {
		t.Fatalf("buildResourceAnalysis.Workloads = %+v, want %+v", got.Workloads, want.Workloads)
	}
	if len(got.PodWorkloads) != len(want.PodWorkloads) {
		t.Fatalf("buildResourceAnalysis.PodWorkloads = %+v, want %+v", got.PodWorkloads, want.PodWorkloads)
	}
}

// TestBuildResourceAnalysisFiltersByNamespace proves --namespace still
// scopes AllWorkloads/PodWorkloads when sourced from a cluster-wide
// snapshot (docs/08 Phase 4E: this is the one analyzer whose result
// --namespace genuinely scopes — see analysis.go's namespace-scoping note).
func TestBuildResourceAnalysisFiltersByNamespace(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Pods: []*corev1.Pod{
			deploymentPod("payments", "payments-api-abc12", "payments-api"),
			deploymentPod("other", "other-api-abc12", "other-api"),
		},
	}

	got := buildResourceAnalysis(resources, "payments")

	if len(got.Workloads) != 1 || got.Workloads[0].Namespace != "payments" {
		t.Fatalf("buildResourceAnalysis(namespace=payments).Workloads = %+v, want only the payments workload", got.Workloads)
	}
}
