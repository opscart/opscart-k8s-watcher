package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestMultiClusterRuntimesDoNotShareState(t *testing.T) {
	clientA := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	clientB := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b1"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b2"}})

	rtA := NewRuntime("cluster-a", clientA)
	rtB := NewRuntime("cluster-b", clientB)
	startAndSync(t, rtA)
	startAndSync(t, rtB)

	snapA := rtA.ClusterState().Publish()
	snapB := rtB.ClusterState().Publish()

	if snapA.ClusterID() != "cluster-a" || snapB.ClusterID() != "cluster-b" {
		t.Fatalf("cluster identity mismatch: a=%s b=%s", snapA.ClusterID(), snapB.ClusterID())
	}
	if len(snapA.Resources().Nodes) != 1 {
		t.Fatalf("cluster-a has %d nodes, want 1", len(snapA.Resources().Nodes))
	}
	if len(snapB.Resources().Nodes) != 2 {
		t.Fatalf("cluster-b has %d nodes, want 2", len(snapB.Resources().Nodes))
	}

	// Add a node only to cluster-a; cluster-b must never see it.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	if _, err := clientA.CoreV1().Nodes().Create(ctxA, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a2"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create node in cluster-a: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return len(rtA.ClusterState().Publish().Resources().Nodes) == 2
	})
	if got := len(rtB.ClusterState().Publish().Resources().Nodes); got != 2 {
		t.Fatalf("cluster-a's new node leaked into cluster-b: cluster-b now has %d nodes, want 2", got)
	}
}

func TestStoppingOneRuntimeDoesNotAffectAnother(t *testing.T) {
	clientA := fake.NewSimpleClientset()
	clientB := fake.NewSimpleClientset()

	rtA := NewRuntime("cluster-a", clientA)
	rtB := NewRuntime("cluster-b", clientB)
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	rtA.Start(ctxA)
	if !rtA.WaitForSync(ctxA) {
		t.Fatal("cluster-a WaitForSync returned false")
	}
	ctxB := startAndSync(t, rtB)

	rtA.Stop()

	// cluster-b must keep working after cluster-a stops: new events still
	// propagate and health stays HEALTHY.
	if _, err := clientB.CoreV1().Pods("default").Create(ctxB, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod in cluster-b: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return len(rtB.ClusterState().Publish().Resources().Pods) == 1
	})
	if got := rtB.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionHealthy {
		t.Fatalf("cluster-b Acquisition() = %s after cluster-a stopped, want HEALTHY", got)
	}
}

func TestStopBeforeStartIsHarmless(t *testing.T) {
	rt := NewRuntime("cluster-a", fake.NewSimpleClientset())
	rt.Stop() // must not panic
}
