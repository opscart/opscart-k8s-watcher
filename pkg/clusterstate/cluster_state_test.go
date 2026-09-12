package clusterstate

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewClusterStateStartsStale(t *testing.T) {
	state := NewClusterState("cluster-a")
	snapshot := state.Publish()
	if snapshot.Acquisition() != AcquisitionStale {
		t.Fatalf("new ClusterState published %s, want STALE", snapshot.Acquisition())
	}
	if snapshot.Trustworthy() {
		t.Fatal("a never-updated ClusterState must not be trustworthy")
	}
}

func TestPublishGenerationMonotonic(t *testing.T) {
	state := NewClusterState("cluster-a")

	first := state.Publish()
	if first.Generation() != 1 {
		t.Fatalf("first Generation() = %d, want 1", first.Generation())
	}

	state.Update(ClusterResources{Nodes: []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}}})
	second := state.Publish()
	if second.Generation() != 2 {
		t.Fatalf("second Generation() = %d, want 2", second.Generation())
	}

	third := state.Publish()
	if third.Generation() != 3 {
		t.Fatalf("third Generation() = %d, want 3 (publishing without a new Update must still advance)", third.Generation())
	}

	if !third.PublishedAt().After(first.PublishedAt()) && third.PublishedAt() != first.PublishedAt() {
		t.Fatalf("expected non-decreasing PublishedAt across generations")
	}
}

func TestClusterStateIsolationAcrossClusters(t *testing.T) {
	a := NewClusterState("cluster-a")
	b := NewClusterState("cluster-b")

	a.Update(ClusterResources{Nodes: []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}}})
	a.SetAcquisitionState(AcquisitionHealthy)
	snapA := a.Publish()
	snapA2 := a.Publish()

	snapB := b.Publish()

	if snapA.Generation() != 1 || snapA2.Generation() != 2 {
		t.Fatalf("cluster-a generations = %d, %d, want 1, 2", snapA.Generation(), snapA2.Generation())
	}
	if snapB.Generation() != 1 {
		t.Fatalf("cluster-b generation = %d, want 1 — a's publishes must not advance b's counter", snapB.Generation())
	}
	if snapB.Acquisition() != AcquisitionStale {
		t.Fatalf("cluster-b acquisition = %s, want STALE — a's SetAcquisitionState(HEALTHY) must not leak into b", snapB.Acquisition())
	}
	if len(snapB.Resources().Nodes) != 0 {
		t.Fatalf("cluster-b has %d nodes, want 0 — a's Update must not leak into b", len(snapB.Resources().Nodes))
	}
	if snapB.ClusterID() != "cluster-b" || snapA.ClusterID() != "cluster-a" {
		t.Fatalf("cluster identity mismatch: a=%s b=%s", snapA.ClusterID(), snapB.ClusterID())
	}
}

func TestAcquisitionStatePropagatesToSnapshot(t *testing.T) {
	state := NewClusterState("cluster-a")
	state.SetAcquisitionState(AcquisitionDegraded)
	snapshot := state.Publish()

	if snapshot.Acquisition() != AcquisitionDegraded {
		t.Fatalf("Acquisition() = %s, want DEGRADED", snapshot.Acquisition())
	}
	if snapshot.Trustworthy() {
		t.Fatal("a DEGRADED snapshot must not be trustworthy")
	}
}

func TestResourceStatePropagatesToSnapshot(t *testing.T) {
	state := NewClusterState("cluster-a")
	observedAt := time.Now().Truncate(time.Second)
	state.SetResourceState(ResourcePods, ResourceState{Synced: true, ObservedAt: observedAt, ResourceVersion: "123"})
	snapshot := state.Publish()

	got, ok := snapshot.ResourceState(ResourcePods)
	if !ok {
		t.Fatal("expected ResourcePods state to be recorded")
	}
	if !got.Synced || got.ResourceVersion != "123" || !got.ObservedAt.Equal(observedAt) {
		t.Fatalf("ResourceState(Pods) = %+v, want Synced=true ResourceVersion=123 ObservedAt=%v", got, observedAt)
	}

	if _, ok := snapshot.ResourceState(ResourceNodes); ok {
		t.Fatal("expected no ResourceState recorded for Nodes")
	}
}

func TestResourceStateOnOneSnapshotDoesNotAffectAnother(t *testing.T) {
	state := NewClusterState("cluster-a")
	before := state.Publish()

	state.SetResourceState(ResourcePods, ResourceState{Synced: true})
	after := state.Publish()

	if _, ok := before.ResourceState(ResourcePods); ok {
		t.Fatal("SetResourceState after Publish must not retroactively affect an already-published snapshot")
	}
	if _, ok := after.ResourceState(ResourcePods); !ok {
		t.Fatal("expected the later snapshot to carry the resource state set before it was published")
	}
}

func TestSnapshotViewsAreDeterministic(t *testing.T) {
	state := NewClusterState("cluster-a")
	state.Update(ClusterResources{
		Pods: []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
		},
	})
	snapshot := state.Publish()

	first := snapshot.Resources()
	second := snapshot.Resources()

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two Resources() calls on the same snapshot returned different content:\n%+v\n%+v", first, second)
	}
	if first.Pods[0].Name != "pod-b" || first.Pods[1].Name != "pod-a" {
		t.Fatalf("Resources() did not preserve input order: %+v", first.Pods)
	}
}
