package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

// TestRunResourceAnalysisRequiresNoKubernetesClient proves this analysis
// path needs nothing beyond a ClusterSnapshot — no clientset, kubeClientFor,
// or other Kubernetes acquisition reachable from runResourceAnalysis at all
// (docs/08 Phase 4D.1 scope: Resource Analyzer must stop acquiring
// Kubernetes state directly).
func TestRunResourceAnalysisRequiresNoKubernetesClient(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Nodes: []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}},
		Pods:  []*corev1.Pod{deploymentPod("payments", "payments-api-abc12", "payments-api")},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runResourceAnalysis(state, snapshot)

	if state.scan.resourceAnalysisGeneration != snapshot.Generation() {
		t.Fatalf("resourceAnalysisGeneration = %d, want %d", state.scan.resourceAnalysisGeneration, snapshot.Generation())
	}
	if len(state.scan.AllWorkloads) != 1 || state.scan.AllWorkloads[0].Name != "payments-api" {
		t.Fatalf("AllWorkloads = %+v, want one payments-api workload", state.scan.AllWorkloads)
	}
}

func TestRunResourceAnalysisSkipsWhenSnapshotNotTrustworthy(t *testing.T) {
	original := &clusterScan{AllWorkloads: []models.WorkloadRef{{Name: "existing"}}}
	state := &dashboardState{scan: original}

	cs := clusterstate.NewClusterState("cluster-a") // starts STALE
	snapshot := cs.Publish()

	runResourceAnalysis(state, snapshot)

	if state.scan != original {
		t.Fatal("an untrustworthy snapshot must not replace the currently displayed scan")
	}
}

func TestRunResourceAnalysisSkipsWhenNoLegacyScanYet(t *testing.T) {
	state := &dashboardState{} // scan is nil: no legacy scan has ever completed

	cs := clusterstate.NewClusterState("cluster-a")
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runResourceAnalysis(state, snapshot) // must not panic

	if state.scan != nil {
		t.Fatal("expected scan to remain nil when no legacy scan has ever published a *clusterScan")
	}
}

// TestRunResourceAnalysisUsesSingleGenerationForPodsAndNodes proves Pods and
// Nodes are read from the SAME published ClusterSnapshot generation, never
// combined across two different Update calls' worth of state — Pods and
// Nodes arrive via two separate Update calls here (as informer event
// handlers for different kinds naturally would), merged by ClusterState
// into one generation before Publish, then read once via snapshot.Resources().
func TestRunResourceAnalysisUsesSingleGenerationForPodsAndNodes(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Pods: []*corev1.Pod{deploymentPod("payments", "payments-api-abc12", "payments-api")},
	})
	cs.Update(clusterstate.ClusterResources{
		Nodes: []*corev1.Node{{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				},
			},
		}},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runResourceAnalysis(state, snapshot)

	if state.scan.resourceAnalysisGeneration != snapshot.Generation() {
		t.Fatalf("resourceAnalysisGeneration = %d, want %d", state.scan.resourceAnalysisGeneration, snapshot.Generation())
	}
	if len(state.scan.AllWorkloads) != 1 {
		t.Fatalf("AllWorkloads = %+v, want the single Pod merged in by the first Update", state.scan.AllWorkloads)
	}
	if len(state.scan.PodWorkloads) != 1 {
		t.Fatalf("PodWorkloads = %+v, want one entry — capacity from the second Update's Node must not erase Pods from the first", state.scan.PodWorkloads)
	}
}

func TestPublishResourceAnalysisGenerationGuardRejectsOlderOrEqualGeneration(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	workloadsV1 := []models.WorkloadRef{{Name: "v1"}}
	publishResourceAnalysis(state, 5, workloadsV1, nil)
	if state.scan.resourceAnalysisGeneration != 5 || len(state.scan.AllWorkloads) != 1 {
		t.Fatalf("expected generation 5's result to publish, got generation=%d workloads=%d",
			state.scan.resourceAnalysisGeneration, len(state.scan.AllWorkloads))
	}

	workloadsStale := []models.WorkloadRef{{Name: "stale"}}
	publishResourceAnalysis(state, 3, workloadsStale, nil)
	if state.scan.resourceAnalysisGeneration != 5 || state.scan.AllWorkloads[0].Name != "v1" {
		t.Fatal("an older generation must not overwrite a newer already-published result")
	}

	publishResourceAnalysis(state, 5, workloadsStale, nil)
	if state.scan.resourceAnalysisGeneration != 5 || state.scan.AllWorkloads[0].Name != "v1" {
		t.Fatal("an equal generation must not overwrite the already-published result for that generation")
	}
}

// TestPublishResourceAnalysisCopyAndSwapPreservesPreviouslyPublishedScan
// proves publishResourceAnalysis never mutates an already-published
// *clusterScan in place, exactly like publishNodeOptimization.
func TestPublishResourceAnalysisCopyAndSwapPreservesPreviouslyPublishedScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}}}

	previouslyRead := state.scan // simulates a reader that captured the pointer under RLock

	publishResourceAnalysis(state, 1, []models.WorkloadRef{{Name: "new"}}, nil)

	if state.scan == previouslyRead {
		t.Fatal("expected publishResourceAnalysis to swap in a new *clusterScan, not reuse the existing pointer")
	}
	if len(previouslyRead.AllWorkloads) != 0 {
		t.Fatal("publishResourceAnalysis mutated a *clusterScan a reader already held a pointer to")
	}
	if state.scan.report.Currency != "USD" {
		t.Fatal("copy-and-swap must preserve every other field from the previous scan")
	}
}

// ── legacy full-scan vs. coordinator publish ordering (Phase 4C's pattern,
// reused for Resource Analyzer) ────────────────────────────────────────────

func TestResourceAnalysisOrderingCoordinatorThenLegacyScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	publishResourceAnalysis(state, 5, []models.WorkloadRef{{Name: "coordinator-gen-5"}}, nil)

	legacyScan := &clusterScan{AllWorkloads: []models.WorkloadRef{{Name: "legacy-own-computation"}}}
	preserveNewerCoordinatorResourceAnalysis(state.scan, legacyScan) // the exact call refresh() makes
	state.scan = legacyScan                                         // the exact swap refresh() makes

	if state.scan.resourceAnalysisGeneration != 5 {
		t.Fatalf("resourceAnalysisGeneration = %d after a legacy scan publish, want 5 preserved", state.scan.resourceAnalysisGeneration)
	}
	if len(state.scan.AllWorkloads) != 1 || state.scan.AllWorkloads[0].Name != "coordinator-gen-5" {
		t.Fatalf("legacy scan clobbered the newer coordinator result: %+v", state.scan.AllWorkloads)
	}
}

func TestResourceAnalysisOrderingLegacyScanThenCoordinator(t *testing.T) {
	state := &dashboardState{}

	legacyScan := &clusterScan{AllWorkloads: []models.WorkloadRef{{Name: "legacy-own-computation"}}}
	preserveNewerCoordinatorResourceAnalysis(state.scan, legacyScan) // previous is nil: first-ever scan
	state.scan = legacyScan

	publishResourceAnalysis(state, 3, []models.WorkloadRef{{Name: "coordinator-gen-3"}}, nil)

	if state.scan.resourceAnalysisGeneration != 3 {
		t.Fatalf("resourceAnalysisGeneration = %d, want 3", state.scan.resourceAnalysisGeneration)
	}
	if len(state.scan.AllWorkloads) != 1 || state.scan.AllWorkloads[0].Name != "coordinator-gen-3" {
		t.Fatalf("expected the coordinator's generation 3 result to win: %+v", state.scan.AllWorkloads)
	}
}
