package main

import (
	"context"
	"fmt"
	"testing"
	"time"

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
