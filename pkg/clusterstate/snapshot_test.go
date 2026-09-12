package clusterstate

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Kubernetes objects reached through ClusterResources are read-only by
// contract, not by mechanism — exactly like objects returned from a
// client-go informer Lister (see ClusterResources' doc comment). No test in
// this file mutates a pointed-to object and expects isolation; that is not
// this package's contract and is not the target architecture. These tests
// prove what the package actually guarantees: the []*T collections are
// independent, and sharing object identity across clones/calls is
// intentional, not a leak.

func TestClusterResourcesCloneReplacingElementDoesNotAlterOriginal(t *testing.T) {
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}}
	original := ClusterResources{Pods: []*corev1.Pod{podA}}
	cloned := original.clone()

	cloned.Pods[0] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}}

	if original.Pods[0] != podA {
		t.Fatalf("replacing an element in a clone's slice altered the original's element: %+v", original.Pods[0])
	}
}

func TestClusterResourcesCloneAppendDoesNotAlterOriginal(t *testing.T) {
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}}
	original := ClusterResources{Pods: []*corev1.Pod{podA}}
	cloned := original.clone()

	cloned.Pods = append(cloned.Pods, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}})

	if len(original.Pods) != 1 {
		t.Fatalf("appending to a clone's slice grew the original: %+v", original.Pods)
	}
}

func TestClusterResourcesCloneSharesObjectIdentity(t *testing.T) {
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}}
	original := ClusterResources{Pods: []*corev1.Pod{podA}}
	cloned := original.clone()

	if cloned.Pods[0] != podA {
		t.Fatal("clone produced a different *corev1.Pod — object identity should be shared, not copied")
	}
}

func TestClusterResourcesCloneNilFieldsStayNil(t *testing.T) {
	cloned := ClusterResources{}.clone()

	if cloned.Nodes != nil || cloned.Pods != nil || cloned.PersistentVolumeClaims != nil {
		t.Fatalf("clone turned an unset (nil) field into a non-nil slice: %+v", cloned)
	}
}

func TestClusterSnapshotResourcesSliceIsIndependentPerCall(t *testing.T) {
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}}
	state := NewClusterState("cluster-a")
	state.Update(ClusterResources{Pods: []*corev1.Pod{podA}})
	snapshot := state.Publish()

	first := snapshot.Resources()
	first.Pods[0] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "replaced"}}
	first.Pods = append(first.Pods, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "extra"}})

	second := snapshot.Resources()
	if len(second.Pods) != 1 {
		t.Fatalf("one Resources() caller's append leaked into another's copy: %+v", second.Pods)
	}
	if second.Pods[0] != podA {
		t.Fatalf("one Resources() caller's element replacement leaked into another's copy: %+v", second.Pods[0])
	}
}

func TestClusterSnapshotResourcesSharesObjectIdentityAcrossCalls(t *testing.T) {
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}}
	state := NewClusterState("cluster-a")
	state.Update(ClusterResources{Pods: []*corev1.Pod{podA}})
	snapshot := state.Publish()

	first := snapshot.Resources()
	second := snapshot.Resources()

	if first.Pods[0] != second.Pods[0] || first.Pods[0] != podA {
		t.Fatal("expected every Resources() call to share the same underlying *corev1.Pod — object identity is intentional, not a leak")
	}
}

func TestClusterStateUpdateCopiesSliceOnIngest(t *testing.T) {
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}}
	pods := []*corev1.Pod{podA}

	state := NewClusterState("cluster-a")
	state.Update(ClusterResources{Pods: pods})

	// Replacing an element and appending to the caller's own slice after
	// handing it to Update must not reach ClusterState — Update took its
	// own slice copy at ingestion. (Mutating podA itself is a separate,
	// out-of-scope question: it is read-only by contract, not prevented by
	// this package — see ClusterResources' doc comment.)
	pods[0] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "swapped"}}
	pods = append(pods, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "extra"}})
	_ = pods

	got := state.Publish().Resources()
	if len(got.Pods) != 1 || got.Pods[0] != podA {
		t.Fatalf("Update did not take its own slice copy on ingest: state saw %+v", got.Pods)
	}
}
