package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/acquisition"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestStartAcquisitionRuntimesCreatesOnePerConfiguredCluster(t *testing.T) {
	srv := newServer([]string{"cluster-a", "cluster-b"}, store.NullStore{}, 0, false)
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.startAcquisitionRuntimes(ctx)

	rtA := srv.getState("cluster-a").acquisition
	rtB := srv.getState("cluster-b").acquisition
	if rtA == nil || rtB == nil {
		t.Fatalf("expected an acquisition runtime for both clusters, got a=%v b=%v", rtA, rtB)
	}
	if rtA == rtB {
		t.Fatal("cluster-a and cluster-b share the same *acquisition.Runtime instance")
	}
}

func TestAcquisitionClusterStateIsClusterSpecific(t *testing.T) {
	srv := newServer([]string{"cluster-a", "cluster-b"}, store.NullStore{}, 0, false)
	srv.kubeClientFor = func(clusterCtx string, _ *apiCounters) (kubernetes.Interface, error) {
		if clusterCtx == "cluster-a" {
			return fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}), nil
		}
		return fake.NewSimpleClientset(
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b1"}},
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b2"}},
		), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.startAcquisitionRuntimes(ctx)

	rtA := srv.getState("cluster-a").acquisition
	rtB := srv.getState("cluster-b").acquisition
	if !rtA.WaitForSync(ctx) || !rtB.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned false, want true")
	}

	if got := len(rtA.ClusterState().Publish().Resources().Nodes); got != 1 {
		t.Fatalf("cluster-a has %d nodes, want 1", got)
	}
	if got := len(rtB.ClusterState().Publish().Resources().Nodes); got != 2 {
		t.Fatalf("cluster-b has %d nodes, want 2", got)
	}
	if got := rtA.ClusterState().Publish().ClusterID(); got != "cluster-a" {
		t.Fatalf("cluster-a snapshot ClusterID() = %q, want cluster-a", got)
	}
	if got := rtB.ClusterState().Publish().ClusterID(); got != "cluster-b" {
		t.Fatalf("cluster-b snapshot ClusterID() = %q, want cluster-b", got)
	}
}

func TestOneClusterSyncFailureDoesNotBlockAnother(t *testing.T) {
	srv := newServer([]string{"broken", "healthy"}, store.NullStore{}, 0, false)
	srv.kubeClientFor = func(clusterCtx string, _ *apiCounters) (kubernetes.Interface, error) {
		if clusterCtx == "broken" {
			return nil, fmt.Errorf("no kubeconfig context named %q", clusterCtx)
		}
		return fake.NewSimpleClientset(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.startAcquisitionRuntimes(ctx)

	if got := srv.getState("broken").acquisition; got != nil {
		t.Fatalf("expected no acquisition runtime for a cluster whose client failed, got %+v", got)
	}
	rtHealthy := srv.getState("healthy").acquisition
	if rtHealthy == nil {
		t.Fatal("expected an acquisition runtime for the healthy cluster despite the broken one")
	}
	if !rtHealthy.WaitForSync(ctx) {
		t.Fatal("healthy cluster's WaitForSync returned false, want true")
	}
}

// TestShutdownStopsAcquisitionBeforeSyncCompletes proves that canceling the
// context passed to startAcquisitionRuntimes actually reaches the
// informer's own reflector loop — even when cancellation happens before a
// cluster ever finishes its initial sync — rather than that loop hanging
// forever on a cluster that never becomes reachable. There is no
// dashboard-owned goroutine to wait on anymore: production wiring only
// creates the runtime and calls Start, so this asserts directly against
// Runtime.WaitForSync, the same call a caller with a legitimate reason to
// know the outcome would make.
func TestShutdownStopsAcquisitionBeforeSyncCompletes(t *testing.T) {
	client := fake.NewSimpleClientset()
	release := make(chan struct{})
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		<-release
		return false, nil, nil
	})
	defer close(release)

	srv := newServer([]string{"cluster-a"}, store.NullStore{}, 0, false)
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) { return client, nil }

	ctx, cancel := context.WithCancel(context.Background())
	srv.startAcquisitionRuntimes(ctx) // Pods' LIST is blocked: this cluster never finishes syncing

	cancel() // dashboard shutdown, before sync ever completed

	rt := srv.getState("cluster-a").acquisition
	result := make(chan bool, 1)
	go func() {
		result <- rt.WaitForSync(ctx)
	}()
	select {
	case synced := <-result:
		if synced {
			t.Fatal("WaitForSync reported synced after shutdown canceled its context before sync ever completed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForSync did not respect shutdown cancellation within 2s — informer loop may not be stopping")
	}
}

// ── docs/08 Phase 5: one analysis execution path per cluster ────────────────

// TestRunAnalysisPassBuildsAllAnalyzersFromOneSnapshot proves runAnalysisPass
// (the sole analysis entry point since Phase 5 removed the separate
// Coordinator-driven runX/publishX split) populates every analyzer's result
// — Cost, Node Optimization, Resource Analyzer, Node Health, Network,
// Security, Waste — from one snapshot, and stamps the published scan with
// that snapshot's generation.
func TestRunAnalysisPassBuildsAllAnalyzersFromOneSnapshot(t *testing.T) {
	state := &dashboardState{costAnalyzer: analyzer.NewNodePoolCostAnalyzer("")}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Nodes:      []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}},
		Pods:       []*corev1.Pod{deploymentPod("payments", "payments-api-abc12", "payments-api")},
		Namespaces: []*corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runAnalysisPass(state, snapshot, nil)

	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()
	if scan == nil {
		t.Fatal("expected runAnalysisPass to publish a *clusterScan")
	}
	if scan.generation != snapshot.Generation() {
		t.Fatalf("scan.generation = %d, want %d", scan.generation, snapshot.Generation())
	}
	if scan.report == nil {
		t.Error("report not populated")
	}
	if scan.AllWorkloads == nil {
		t.Error("AllWorkloads not populated")
	}
	// nodeHealth/netAudit/secAudit/wasteAudit/nodeOptimization can be
	// legitimately empty for this fixture (no unhealthy nodes, no policies,
	// no privileged pods, no waste, no extra nodes to consolidate) — what
	// matters is that buildClusterScan actually ran, proven by report/
	// AllWorkloads above and by cisResult below (only set once secAudit is
	// computed, never left nil by a skipped analyzer).
	if scan.cisResult == nil {
		t.Error("cisResult not populated — implies secAudit/netAudit were not both computed in this pass")
	}
}

// TestRunAnalysisPassEndToEndThroughRealCoordinator proves the actual
// production wiring: a Coordinator driving runAnalysisPass against a real
// acquisition.Runtime's ClusterState eventually publishes a *clusterScan,
// through the real coalescing window (pkg/clusterstate.coalesceWindow).
func TestRunAnalysisPassEndToEndThroughRealCoordinator(t *testing.T) {
	state := &dashboardState{costAnalyzer: analyzer.NewNodePoolCostAnalyzer("")}

	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	rt := acquisition.NewRuntime("cluster-a", client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.Start(ctx)
	state.coordinator = clusterstate.NewCoordinator(rt.ClusterState(), func(snapshot *clusterstate.ClusterSnapshot) {
		runAnalysisPass(state, snapshot, nil)
	}, 0)
	go state.coordinator.Run(ctx)

	if !rt.WaitForSync(ctx) {
		t.Fatal("runtime did not reach initial sync")
	}

	// WaitForSync's own recomputeHealth call (pkg/acquisition/runtime.go)
	// updates ClusterState's acquisition field to HEALTHY but does not
	// itself Publish a new generation — the snapshot already published
	// during initial sync may still be stamped RESYNCING. Creating one more
	// object forces a genuine informer event, which syncResource turns into
	// a fresh Publish carrying the now-HEALTHY state, giving this test a
	// deterministic trustworthy snapshot instead of racing that transition.
	if _, err := client.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create trigger node: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state.mu.RLock()
		scan := state.scan
		state.mu.RUnlock()
		if scan != nil && scan.generation > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("coordinator did not publish an analysis result within 5s of a trustworthy initial sync")
}

// TestAnalysisCoordinatorsAreClusterSpecific proves each cluster gets its
// own Coordinator instance — no shared coordinator, no cross-cluster leakage.
func TestAnalysisCoordinatorsAreClusterSpecific(t *testing.T) {
	srv := newServer([]string{"cluster-a", "cluster-b"}, store.NullStore{}, 0, false)
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.startAcquisitionRuntimes(ctx)

	coordA := srv.getState("cluster-a").coordinator
	coordB := srv.getState("cluster-b").coordinator
	if coordA == nil || coordB == nil {
		t.Fatalf("expected a coordinator for both clusters, got a=%v b=%v", coordA, coordB)
	}
	if coordA == coordB {
		t.Fatal("cluster-a and cluster-b share the same *clusterstate.Coordinator instance")
	}
}

// TestLegacyRefreshRequiresTrustworthyAcquisition proves docs/08 Phase 4E:
// refresh (scan.go) now sources its evidence entirely from the cluster's
// ClusterSnapshot rather than its own direct Kubernetes call. It must fail
// with no acquisition runtime at all, fail before that runtime's informers
// have completed their initial sync, and succeed once they have.
func TestLegacyRefreshRequiresTrustworthyAcquisition(t *testing.T) {
	t.Run("no acquisition runtime", func(t *testing.T) {
		srv := newServer([]string{"cluster-a"}, store.NullStore{}, 0, false)
		srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
			return nil, fmt.Errorf("no kubeconfig context named %q", "cluster-a")
		}
		srv.startAcquisitionRuntimes(context.Background())

		if srv.getState("cluster-a").acquisition != nil {
			t.Fatal("expected no acquisition runtime for a cluster whose client failed")
		}
		if err := srv.getState("cluster-a").refresh([]string{"cluster-a"}); err == nil {
			t.Fatal("expected refresh() to fail with no acquisition runtime")
		}
	})

	t.Run("not yet synced", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		release := make(chan struct{})
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			<-release
			return false, nil, nil
		})
		defer close(release)

		srv := newServer([]string{"cluster-a"}, store.NullStore{}, 0, false)
		srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) { return client, nil }
		srv.startAcquisitionRuntimes(context.Background()) // Pods' LIST is blocked: this cluster never finishes syncing

		if err := srv.getState("cluster-a").refresh([]string{"cluster-a"}); err == nil {
			t.Fatal("expected refresh() to fail before informers have synced")
		}
	})

	t.Run("synced", func(t *testing.T) {
		srv := newServer([]string{"cluster-a"}, store.NullStore{}, 0, false)
		srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
			return fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}), nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		srv.startAcquisitionRuntimes(ctx)

		rt := srv.getState("cluster-a").acquisition
		if !rt.WaitForSync(ctx) {
			t.Fatal("WaitForSync returned false, want true")
		}

		if err := srv.getState("cluster-a").refresh([]string{"cluster-a"}); err != nil {
			t.Fatalf("refresh() failed after successful sync: %v", err)
		}
	})
}
