package analyzer

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
)

// derefAll converts a ClusterSnapshot resource collection ([]*T, matching
// what a Phase 3 informer lister returns) into the []T value slice Node
// Optimization's entry points expect. Their signatures predate the
// snapshot contract; this one-time, one-way copy at the analyzer boundary
// is the expected adaptation, not evidence that ClusterSnapshot itself
// deep-copies anything — clusterstate.ClusterResources never does.
func derefAll[T any](in []*T) []T {
	out := make([]T, len(in))
	for i, p := range in {
		out[i] = *p
	}
	return out
}

// TestNodeOptimizationConsumesClusterSnapshot is the Phase 2 exit-criterion
// proof (docs/08 Phase 2: "At least one analyzer can consume the new
// snapshot contract without performing Kubernetes acquisition"). It runs
// Node Optimization's real, unmodified entry points using only data
// obtained from a clusterstate.ClusterSnapshot — no Kubernetes client,
// informer, or acquisition call appears anywhere in this test.
func TestNodeOptimizationConsumesClusterSnapshot(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "500m", "256Mi")
	claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", nil)

	state := clusterstate.NewClusterState("cluster-a")
	state.Update(clusterstate.ClusterResources{
		Nodes:                  []*corev1.Node{&node0, &node1},
		Pods:                   []*corev1.Pod{&pod},
		PersistentVolumeClaims: []*corev1.PersistentVolumeClaim{&claim},
		PersistentVolumes:      []*corev1.PersistentVolume{&volume},
	})
	state.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := state.Publish()

	if !snapshot.Trustworthy() {
		t.Fatal("expected a HEALTHY snapshot to be trustworthy")
	}

	// From here on, every input is snapshot-derived (via derefAll — see its
	// doc comment for why). This is the analyzer's real pipeline (see
	// cmd/opscart-dashboard/node_optimization_runtime.go's
	// buildNodeOptimization), unchanged — only its data source has moved.
	resources := snapshot.Resources()
	nodes := derefAll(resources.Nodes)
	pods := derefAll(resources.Pods)
	claims := derefAll(resources.PersistentVolumeClaims)
	volumes := derefAll(resources.PersistentVolumes)

	schedulingEvidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	if schedulingEvidence.EvidenceIncomplete {
		t.Fatalf("scheduling evidence incomplete from snapshot-sourced Nodes: %+v", schedulingEvidence)
	}

	storageEvidence := BuildNodeOptimizationStorageEvidence(claims, volumes)
	if storageEvidence.EvidenceIncomplete {
		t.Fatalf("storage evidence incomplete from snapshot-sourced PVCs/PVs: %+v", storageEvidence)
	}

	summary := BuildNMinusOneNodeOptimizationScenariosWithSchedulingAndStorageEvidence(
		nodeInfos, pods, schedulingEvidence, storageEvidence,
	)
	if summary.EvidenceIncomplete || len(summary.Scenarios) != 1 {
		t.Fatalf("expected exactly one complete N-1 scenario from a 2-node pool, got %d (incomplete=%v): %+v",
			len(summary.Scenarios), summary.EvidenceIncomplete, summary)
	}
	if !summary.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("expected placement to succeed: %+v", summary.Scenarios[0].Simulation)
	}

	recommendations := BuildNodeOptimizationRecommendations(summary, pods)
	if len(recommendations) != 1 {
		t.Fatalf("expected exactly one recommendation, got %d", len(recommendations))
	}
	if recommendations[0].PoolKey != schedulingEvidence.Nodes["node-0"].PoolKey {
		t.Fatalf("recommendation pool key %+v does not match snapshot-derived scheduling evidence pool key %+v",
			recommendations[0].PoolKey, schedulingEvidence.Nodes["node-0"].PoolKey)
	}
}
