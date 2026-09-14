package acquisition

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// ── State-machine behavior (white-box, deterministic — no real watch/list
// choreography needed to prove this package's own logic) ──────────────────

func TestMarkDegradedThenRecoveredTransitionsHealth(t *testing.T) {
	rt := NewRuntime("cluster-a", fake.NewSimpleClientset())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)
	if !rt.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned false, want true")
	}

	rt.markDegraded(clusterstate.ResourcePods)
	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionDegraded {
		t.Fatalf("Acquisition() = %s after markDegraded, want DEGRADED", got)
	}

	rt.markRecovered(clusterstate.ResourcePods)
	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionHealthy {
		t.Fatalf("Acquisition() = %s after markRecovered, want HEALTHY", got)
	}
}

func TestAllDegradedKindsMustRecoverBeforeHealthy(t *testing.T) {
	rt := NewRuntime("cluster-a", fake.NewSimpleClientset())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)
	if !rt.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned false, want true")
	}

	rt.markDegraded(clusterstate.ResourcePods)
	rt.markDegraded(clusterstate.ResourceNodes)

	rt.markRecovered(clusterstate.ResourcePods)
	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionDegraded {
		t.Fatalf("Acquisition() = %s with Nodes still degraded, want DEGRADED", got)
	}

	rt.markRecovered(clusterstate.ResourceNodes)
	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionHealthy {
		t.Fatalf("Acquisition() = %s after both kinds recovered, want HEALTHY", got)
	}
}

func TestRecomputeHealthPrefersResyncingOverDegraded(t *testing.T) {
	client := fake.NewSimpleClientset()
	release := blockPodList(client)
	defer release()

	rt := NewRuntime("cluster-a", client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx) // Pods' initial LIST is blocked: sync can never complete yet.

	rt.markDegraded(clusterstate.ResourceNodes)

	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionResyncing {
		t.Fatalf("Acquisition() = %s with sync incomplete and a kind degraded, want RESYNCING", got)
	}
}

func TestMarkRecoveredOnNonDegradedKindIsHarmless(t *testing.T) {
	rt := NewRuntime("cluster-a", fake.NewSimpleClientset())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)
	if !rt.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned false, want true")
	}

	rt.markRecovered(clusterstate.ResourcePods) // never degraded — must not panic or change state
	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionHealthy {
		t.Fatalf("Acquisition() = %s, want HEALTHY", got)
	}
}

// ── Real wiring through client-go (SetWatchErrorHandler actually invoked
// by a genuine LIST failure, not just this package's own logic) ───────────

func TestRealListFailureInvokesWatchErrorHandler(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("boom")
	})

	rt := NewRuntime("cluster-a", client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)

	waitFor(t, 2*time.Second, func() bool {
		rt.healthMu.Lock()
		_, degraded := rt.degradedKinds[clusterstate.ResourceNetworkPolicies]
		rt.healthMu.Unlock()
		return degraded
	})

	if rt.ClusterState().Publish().Trustworthy() {
		t.Fatal("acquisition reported trustworthy while NetworkPolicies LIST is failing")
	}
}

func TestRuntimeRecoversOnceFailingListStartsSucceeding(t *testing.T) {
	client := fake.NewSimpleClientset()
	var fail atomic.Bool
	fail.Store(true)
	client.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail.Load() {
			return true, nil, fmt.Errorf("boom")
		}
		return false, nil, nil
	})

	rt := NewRuntime("cluster-a", client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)

	waitFor(t, 2*time.Second, func() bool {
		return !rt.ClusterState().Publish().Trustworthy()
	})

	fail.Store(false)

	syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
	defer syncCancel()
	if !rt.WaitForSync(syncCtx) {
		t.Fatal("WaitForSync never succeeded once the NetworkPolicies LIST started succeeding")
	}
	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionHealthy {
		t.Fatalf("Acquisition() = %s once all kinds recovered, want HEALTHY", got)
	}
}
