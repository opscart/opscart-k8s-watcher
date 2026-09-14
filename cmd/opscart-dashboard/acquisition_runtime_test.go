package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/acquisition"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
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

// ── Phase 4D.1/4D.2/4D.3/4D.4: one Coordinator drives every migrated
// analyzer ─────────────────────────────────────────────────────────────────

// TestRunCoordinatedAnalysisRunsBothAnalyzers proves runCoordinatedAnalysis
// invokes Node Optimization, Resource Analyzer, Node Health, Network, and
// Security analysis from the same snapshot — "one coalesced generation ->
// all migrated analyzers run" (docs/08 §2.5) — without needing a real
// Coordinator or informer wiring to prove it.
func TestRunCoordinatedAnalysisRunsBothAnalyzers(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Nodes:      []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}},
		Pods:       []*corev1.Pod{deploymentPod("payments", "payments-api-abc12", "payments-api")},
		Namespaces: []*corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runCoordinatedAnalysis(state, snapshot)

	if state.scan.nodeOptimizationGeneration != snapshot.Generation() {
		t.Fatalf("nodeOptimizationGeneration = %d, want %d", state.scan.nodeOptimizationGeneration, snapshot.Generation())
	}
	if state.scan.resourceAnalysisGeneration != snapshot.Generation() {
		t.Fatalf("resourceAnalysisGeneration = %d, want %d", state.scan.resourceAnalysisGeneration, snapshot.Generation())
	}
	if state.scan.nodeHealthGeneration != snapshot.Generation() {
		t.Fatalf("nodeHealthGeneration = %d, want %d", state.scan.nodeHealthGeneration, snapshot.Generation())
	}
	if state.scan.netAuditGeneration != snapshot.Generation() {
		t.Fatalf("netAuditGeneration = %d, want %d", state.scan.netAuditGeneration, snapshot.Generation())
	}
	if state.scan.secAuditGeneration != snapshot.Generation() {
		t.Fatalf("secAuditGeneration = %d, want %d", state.scan.secAuditGeneration, snapshot.Generation())
	}
}

// TestStartAnalysisCoordinatorDrivesBothAnalyzersEndToEnd proves the actual
// production wiring: a Coordinator created by startAnalysisCoordinator
// against a real acquisition.Runtime's ClusterState eventually publishes
// Node Optimization, Resource Analyzer, Node Health, Network, and Security
// results, through the real coalescing window
// (pkg/clusterstate.coalesceWindow) — "latest generation wins after
// coalescing" for every migrated analyzer at once, not a test seam.
func TestStartAnalysisCoordinatorDrivesBothAnalyzersEndToEnd(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}}}

	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	rt := acquisition.NewRuntime("cluster-a", client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.Start(ctx)
	startAnalysisCoordinator(ctx, state, rt)

	if state.coordinator == nil {
		t.Fatal("expected startAnalysisCoordinator to set state.coordinator")
	}
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
		nodeOptGen := state.scan.nodeOptimizationGeneration
		resourceGen := state.scan.resourceAnalysisGeneration
		nodeHealthGen := state.scan.nodeHealthGeneration
		netAuditGen := state.scan.netAuditGeneration
		secAuditGen := state.scan.secAuditGeneration
		state.mu.RUnlock()
		if nodeOptGen > 0 && resourceGen > 0 && nodeHealthGen > 0 && netAuditGen > 0 && secAuditGen > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("coordinator did not publish Node Optimization, Resource Analyzer, and Node Health results within 5s of a trustworthy initial sync")
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

// TestLegacyRefreshUnaffectedByAcquisitionStartup proves the existing scan
// path's behavior is unchanged by also starting that cluster's acquisition
// runtime — the Phase 4A coexistence requirement.
func TestLegacyRefreshUnaffectedByAcquisitionStartup(t *testing.T) {
	srv := newServer([]string{bogusClusterCtx}, store.NullStore{}, 0, false)
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.startAcquisitionRuntimes(ctx)

	if srv.getState(bogusClusterCtx).acquisition == nil {
		t.Fatal("expected an acquisition runtime even for the cluster refresh() will fail against")
	}

	// runFullScan builds its own client via kubeClientWithCounters, not
	// srv.kubeClientFor — bogusClusterCtx fails deterministically without
	// touching the network, exactly as it did before acquisition existed.
	if err := srv.getState(bogusClusterCtx).refresh([]string{bogusClusterCtx}); err == nil {
		t.Fatal("expected refresh() to fail for a nonexistent kubeconfig context, as before Phase 4A")
	}
}
