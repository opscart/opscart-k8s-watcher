package scanner

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// NodeSnapshot lets other scan-pipeline consumers (Node Optimization
// scheduling evidence) reuse the raw Node list FindNodeHealthConditions
// already fetched, instead of listing Nodes again.
func TestScannerNodeSnapshotReusesHealthCheckFetch(t *testing.T) {
	nodeA := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"topology.kubernetes.io/zone": "us-east-1a"}}}
	nodeB := nodeWithConditions("node-b", condition(corev1.NodeDiskPressure, corev1.ConditionTrue))
	client := fake.NewSimpleClientset(&nodeA, &nodeB)
	s := NewScannerWithClientset(client, "cluster-a")

	if before := s.NodeSnapshot(); before != nil {
		t.Fatalf("NodeSnapshot() before any scan = %+v, want nil", before)
	}

	if _, err := s.FindNodeHealthConditions(); err != nil {
		t.Fatalf("FindNodeHealthConditions: %v", err)
	}

	snapshot := s.NodeSnapshot()
	if len(snapshot) != 2 {
		t.Fatalf("NodeSnapshot() len = %d, want 2", len(snapshot))
	}
	names := map[string]bool{}
	for _, node := range snapshot {
		names[node.Name] = true
	}
	if !names["node-a"] || !names["node-b"] {
		t.Fatalf("NodeSnapshot() missing expected Nodes: %+v", snapshot)
	}
	for _, node := range snapshot {
		if node.Name == "node-a" && node.Labels["topology.kubernetes.io/zone"] != "us-east-1a" {
			t.Fatalf("NodeSnapshot() lost topology label evidence: %+v", node)
		}
	}

	// Exactly one Node LIST must have happened — the snapshot is a read of
	// already-fetched data, not a second fetch.
	count := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "nodes" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("Node LIST count = %d, want 1 (NodeSnapshot must not trigger another list)", count)
	}
}

// PVCSnapshot's mutation-isolation counterpart for Nodes: the accessor
// returns a defensive copy so a caller can't corrupt the Scanner's retained
// snapshot.
func TestScannerNodeSnapshotIsDefensiveCopy(t *testing.T) {
	nodeA := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	client := fake.NewSimpleClientset(&nodeA)
	s := NewScannerWithClientset(client, "cluster-a")
	if _, err := s.FindNodeHealthConditions(); err != nil {
		t.Fatalf("FindNodeHealthConditions: %v", err)
	}

	snapshot := s.NodeSnapshot()
	snapshot[0].Name = "mutated"

	again := s.NodeSnapshot()
	if again[0].Name != "node-a" {
		t.Fatalf("NodeSnapshot() was mutated by caller: %+v", again)
	}
}
