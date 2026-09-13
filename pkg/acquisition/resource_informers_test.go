package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// seedAllKinds builds one minimal object of every one of the 16 Phase 0
// resource kinds, all named name, for a fake clientset to serve on initial
// LIST. Shared by every test in this package that needs a runtime whose
// caches are non-empty across all kinds.
func seedAllKinds(name string) []runtime.Object {
	return []runtime.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}},
		&autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: name},
			},
		},
		&discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Namespace: "default", Name: name},
			AddressType: discoveryv1.AddressTypeIPv4,
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "default", Name: name},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "default", Name: name},
			Type:           corev1.EventTypeWarning,
		},
	}
}

// waitFor polls cond until it returns true or timeout elapses, failing the
// test if it never does. Informer event delivery is asynchronous, so
// propagation tests need to wait rather than assume it has already
// happened by the next line.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func startAndSync(t *testing.T, rt *Runtime) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt.Start(ctx)
	if !rt.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned false, want true")
	}
	return ctx
}

func TestRuntimeSyncsAllSixteenResourceKindsFromSeedData(t *testing.T) {
	client := fake.NewSimpleClientset(seedAllKinds("seed")...)
	rt := NewRuntime("cluster-a", client)
	startAndSync(t, rt)

	resources := rt.ClusterState().Publish().Resources()
	counts := map[clusterstate.ResourceKind]int{
		clusterstate.ResourceNodes:                    len(resources.Nodes),
		clusterstate.ResourcePods:                     len(resources.Pods),
		clusterstate.ResourceNamespaces:               len(resources.Namespaces),
		clusterstate.ResourcePersistentVolumeClaims:   len(resources.PersistentVolumeClaims),
		clusterstate.ResourcePersistentVolumes:        len(resources.PersistentVolumes),
		clusterstate.ResourceDeployments:              len(resources.Deployments),
		clusterstate.ResourceStatefulSets:             len(resources.StatefulSets),
		clusterstate.ResourceReplicaSets:              len(resources.ReplicaSets),
		clusterstate.ResourceServices:                 len(resources.Services),
		clusterstate.ResourceIngresses:                len(resources.Ingresses),
		clusterstate.ResourceNetworkPolicies:          len(resources.NetworkPolicies),
		clusterstate.ResourceJobs:                     len(resources.Jobs),
		clusterstate.ResourceCronJobs:                 len(resources.CronJobs),
		clusterstate.ResourceHorizontalPodAutoscalers: len(resources.HorizontalPodAutoscalers),
		clusterstate.ResourceEndpointSlices:           len(resources.EndpointSlices),
		clusterstate.ResourcePodWarningEvents:         len(resources.PodWarningEvents),
	}
	for kind, count := range counts {
		if count != 1 {
			t.Errorf("%s: got %d objects, want 1", kind, count)
		}
	}

	// ResourceVersion is checked separately (TestResourceStateResourceVersionMatchesInformer):
	// the fake ObjectTracker never populates a LIST-level resourceVersion at
	// all, so asserting non-empty here would test a fake-tooling limitation,
	// not this package's code. Synced and ObservedAt are real behavior the
	// fake does support and are checked here.
	snapshot := rt.ClusterState().Publish()
	for kind := range counts {
		state, ok := snapshot.ResourceState(kind)
		if !ok {
			t.Errorf("%s: no ResourceState recorded", kind)
			continue
		}
		if !state.Synced {
			t.Errorf("%s: ResourceState.Synced = false, want true", kind)
		}
		if state.ObservedAt.IsZero() || time.Since(state.ObservedAt) > time.Minute {
			t.Errorf("%s: ResourceState.ObservedAt = %v, want a recent timestamp", kind, state.ObservedAt)
		}
	}
}

// TestResourceStateResourceVersionMatchesInformer proves the exact
// metadata claim (see syncResource's doc comment) directly: ResourceState
// .ResourceVersion is always whatever informer.LastSyncResourceVersion()
// reports for that kind, whatever that value is — including empty, which
// is what client-go's fake tooling genuinely returns (its ObjectTracker
// never populates a LIST-level resourceVersion). Nothing is fabricated to
// make this look more populated than the real informer reports.
func TestResourceStateResourceVersionMatchesInformer(t *testing.T) {
	client := fake.NewSimpleClientset(seedAllKinds("seed")...)
	rt := NewRuntime("cluster-a", client)
	startAndSync(t, rt)

	snapshot := rt.ClusterState().Publish()

	podState, ok := snapshot.ResourceState(clusterstate.ResourcePods)
	if !ok {
		t.Fatal("no ResourceState recorded for Pods")
	}
	if want := rt.factory.Core().V1().Pods().Informer().LastSyncResourceVersion(); podState.ResourceVersion != want {
		t.Fatalf("Pods ResourceState.ResourceVersion = %q, want %q (informer.LastSyncResourceVersion())", podState.ResourceVersion, want)
	}

	eventState, ok := snapshot.ResourceState(clusterstate.ResourcePodWarningEvents)
	if !ok {
		t.Fatal("no ResourceState recorded for PodWarningEvents")
	}
	if want := rt.eventFactory.Core().V1().Events().Informer().LastSyncResourceVersion(); eventState.ResourceVersion != want {
		t.Fatalf("PodWarningEvents ResourceState.ResourceVersion = %q, want %q", eventState.ResourceVersion, want)
	}
}

func TestPodAddUpdateDeletePropagateIntoClusterState(t *testing.T) {
	client := fake.NewSimpleClientset()
	rt := NewRuntime("cluster-a", client)
	ctx := startAndSync(t, rt)

	podFor := func(name string, labelValue string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, Labels: map[string]string{"v": labelValue}},
		}
	}

	if _, err := client.CoreV1().Pods("default").Create(ctx, podFor("api", "1"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return len(rt.ClusterState().Publish().Resources().Pods) == 1
	})

	if _, err := client.CoreV1().Pods("default").Update(ctx, podFor("api", "2"), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		pods := rt.ClusterState().Publish().Resources().Pods
		return len(pods) == 1 && pods[0].Labels["v"] == "2"
	})

	if err := client.CoreV1().Pods("default").Delete(ctx, "api", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return len(rt.ClusterState().Publish().Resources().Pods) == 0
	})
}

func TestGenerationAdvancesOnPublish(t *testing.T) {
	client := fake.NewSimpleClientset()
	rt := NewRuntime("cluster-a", client)
	ctx := startAndSync(t, rt)

	before := rt.ClusterState().Publish().Generation()

	if _, err := client.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	// Wait for the actual change, not just any generation bump — a
	// generation increase alone could coincidentally come from a
	// still-settling initial-sync callback for an unrelated kind rather
	// than this specific node.
	waitFor(t, 2*time.Second, func() bool {
		return len(rt.ClusterState().Publish().Resources().Nodes) == 1
	})
	if got := rt.ClusterState().Publish().Generation(); got <= before {
		t.Fatalf("Generation() = %d after publishing the new node, want > %d", got, before)
	}
}

func TestSyncResourceUpdatesOnlyItsOwnKind(t *testing.T) {
	client := fake.NewSimpleClientset(seedAllKinds("seed")...)
	rt := NewRuntime("cluster-a", client)
	ctx := startAndSync(t, rt)

	before := rt.ClusterState().Publish().Resources()

	if _, err := client.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-extra"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return len(rt.ClusterState().Publish().Resources().Nodes) == 2
	})

	after := rt.ClusterState().Publish().Resources()
	if len(after.Pods) != len(before.Pods) || len(after.Deployments) != len(before.Deployments) {
		t.Fatalf("a Nodes-only event changed other kinds: pods %d->%d, deployments %d->%d",
			len(before.Pods), len(after.Pods), len(before.Deployments), len(after.Deployments))
	}
}
