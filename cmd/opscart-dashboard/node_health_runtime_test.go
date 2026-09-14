package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func unhealthyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}},
		},
	}
}

func healthyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func scheduledOwnedPod(namespace, name, node, ownerKind, ownerName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: ownerKind, Name: ownerName, Controller: boolPtr(true)},
			},
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

// TestBuildNodeHealthMatchesDirectScannerCall proves buildNodeHealth's
// snapshot-resources adaptation produces exactly what calling
// scanner.AnalyzeNodeHealth directly on the same value slices would —
// "same input produces equivalent existing Node Health findings".
func TestBuildNodeHealthMatchesDirectScannerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Nodes: []*corev1.Node{unhealthyNode("node-a")},
		Pods:  []*corev1.Pod{scheduledOwnedPod("shop", "api-7d8f9c6b5-abc12", "node-a", "ReplicaSet", "api-7d8f9c6b5")},
	}

	got := buildNodeHealth(resources)
	want := scanner.AnalyzeNodeHealth(
		[]corev1.Node{*resources.Nodes[0]},
		[]corev1.Pod{*resources.Pods[0]},
		nil,
	)

	if len(got) != len(want) || len(got) != 1 {
		t.Fatalf("buildNodeHealth = %+v, want %+v", got, want)
	}
	if got[0].NodeName != want[0].NodeName || got[0].ConditionType != want[0].ConditionType {
		t.Fatalf("buildNodeHealth = %+v, want %+v", got[0], want[0])
	}
}

func TestBuildNodeHealthHealthyNodesProduceNoFindings(t *testing.T) {
	resources := clusterstate.ClusterResources{Nodes: []*corev1.Node{healthyNode("node-a")}}

	got := buildNodeHealth(resources)

	if len(got) != 0 {
		t.Fatalf("got %+v, want no findings for an all-healthy snapshot", got)
	}
}

// TestBuildNodeHealthCorrelatesJobAndCronJobOwnership proves Job -> CronJob
// owner correlation (driven entirely by each Job's own OwnerReferences)
// survives sourcing Jobs from ClusterSnapshot instead of a live List.
func TestBuildNodeHealthCorrelatesJobAndCronJobOwnership(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "nightly-29123456",
			OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "nightly", Controller: boolPtr(true)}}},
	}
	resources := clusterstate.ClusterResources{
		Nodes: []*corev1.Node{unhealthyNode("node-a")},
		Pods:  []*corev1.Pod{scheduledOwnedPod("batch", "nightly-29123456-abc12", "node-a", "Job", job.Name)},
		Jobs:  []*batchv1.Job{job},
	}

	got := buildNodeHealth(resources)

	if len(got) != 1 || len(got[0].CorrelatedWorkloads) != 1 {
		t.Fatalf("unexpected correlation: %+v", got)
	}
	want := models.CorrelatedWorkload{Namespace: "batch", Kind: "CronJob", Name: "nightly", PodCount: 1}
	if got[0].CorrelatedWorkloads[0] != want {
		t.Fatalf("CronJob owner correlation = %+v, want %+v", got[0].CorrelatedWorkloads[0], want)
	}
}
