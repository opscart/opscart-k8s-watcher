package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

// blockPodList installs a reactor that blocks every "list pods" call until
// release is closed, then lets the fake's default reactor handle it
// normally. Since cache.WaitForCacheSync waits for every registered
// informer, blocking just one (Pods) is enough to hold back the whole
// runtime's sync — deterministically, regardless of goroutine scheduling,
// which is what makes the RESYNCING assertions below race-free rather than
// timing-dependent.
func blockPodList(client *fake.Clientset) (release func()) {
	ch := make(chan struct{})
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		<-ch
		return false, nil, nil
	})
	var closed bool
	return func() {
		if !closed {
			closed = true
			close(ch)
		}
	}
}

func TestNewRuntimeRegistersAllRequiredResourceKinds(t *testing.T) {
	rt := NewRuntime("cluster-a", fake.NewSimpleClientset())
	if len(rt.synced) != 16 {
		t.Fatalf("registered %d informers, want 16 (the Phase 0 final resource set)", len(rt.synced))
	}
}

func TestRuntimeStaysResyncingUntilRequiredCachesSync(t *testing.T) {
	client := fake.NewSimpleClientset()
	release := blockPodList(client)
	defer release()

	rt := NewRuntime("cluster-a", client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)

	// Pods' initial LIST is blocked, so sync cannot possibly have completed
	// yet — this assertion is deterministic, not a timing guess.
	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionResyncing {
		t.Fatalf("Acquisition() = %s immediately after Start with sync blocked, want RESYNCING", got)
	}

	release()

	if !rt.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned false, want true once the block is released")
	}
	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionHealthy {
		t.Fatalf("Acquisition() = %s after WaitForSync succeeded, want HEALTHY", got)
	}
}

func TestWaitForSyncDoesNotFakeHealthyWhenCanceledFirst(t *testing.T) {
	client := fake.NewSimpleClientset()
	release := blockPodList(client)
	defer release()

	rt := NewRuntime("cluster-a", client)
	ctx, cancel := context.WithCancel(context.Background())
	rt.Start(ctx)

	// Cancel before ever releasing the block: sync can never complete.
	cancel()

	if rt.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned true after its context was canceled before sync completed")
	}
	if got := rt.ClusterState().Publish().Acquisition(); got == clusterstate.AcquisitionHealthy {
		t.Fatal("Acquisition() reports HEALTHY despite sync never completing — must not fake healthy")
	}
}

func TestWaitForSyncIsCallableDirectlyWithoutStart(t *testing.T) {
	// WaitForSync must be a plain, safely-callable method — e.g. from a
	// test — not something only Start's internal goroutine may invoke.
	rt := NewRuntime("cluster-a", fake.NewSimpleClientset())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.Start(ctx)
	if !rt.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned false, want true")
	}
	// Calling it again after sync already completed must stay true and
	// must not panic or double-transition anything.
	if !rt.WaitForSync(ctx) {
		t.Fatal("second WaitForSync call returned false, want true (idempotent)")
	}
	if got := rt.ClusterState().Publish().Acquisition(); got != clusterstate.AcquisitionHealthy {
		t.Fatalf("Acquisition() = %s, want HEALTHY", got)
	}
}

// TestWaitForSyncPublishesWhenHealthTransitionsWithNoFurtherEvent is the
// regression test for the gap where WaitForSync's own recomputeHealth call
// updated ClusterState's acquisition field to HEALTHY without publishing a
// new snapshot. On a real cluster this can happen when the last kind to
// sync observes allSynced() as false a moment before it actually becomes
// true, leaving the last *published* snapshot stamped RESYNCING even
// though every cache is, by the time anyone checks, fully populated and
// synced. That precise interleaving is a genuine timing race, not
// something a test can reliably reproduce by scheduling — so this
// reproduces its outcome directly and deterministically: force the last
// published snapshot into that exact stuck state, then prove a bare
// WaitForSync call — with no further Kubernetes object event — is now
// enough to both republish and report the correct trustworthy state.
func TestWaitForSyncPublishesWhenHealthTransitionsWithNoFurtherEvent(t *testing.T) {
	rt := NewRuntime("cluster-a", fake.NewSimpleClientset())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.Start(ctx)
	if !cache.WaitForCacheSync(ctx.Done(), rt.synced...) {
		t.Fatal("informer caches never finished their initial sync")
	}

	// Reproduce the stuck-RESYNCING precondition: every cache is already
	// synced (confirmed above), but the last published snapshot still
	// claims otherwise.
	rt.state.SetAcquisitionState(clusterstate.AcquisitionResyncing)
	stale := rt.state.Publish()
	if stale.Trustworthy() {
		t.Fatal("test setup failed: the forced snapshot must be untrustworthy")
	}
	<-rt.state.Publications() // drain the forced publish's own signal

	if !rt.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned false, want true")
	}

	select {
	case <-rt.state.Publications():
	default:
		t.Fatal("WaitForSync's HEALTHY transition did not produce a new publication — " +
			"a coordinator waiting on Publications() would never wake up")
	}

	latest := rt.state.Latest()
	if latest.Acquisition() != clusterstate.AcquisitionHealthy || !latest.Trustworthy() {
		t.Fatalf("Latest() acquisition = %s (trustworthy=%v) after WaitForSync, want HEALTHY/trustworthy",
			latest.Acquisition(), latest.Trustworthy())
	}
}

func TestWaitForSyncRespectsShortTimeout(t *testing.T) {
	client := fake.NewSimpleClientset()
	release := blockPodList(client)
	defer release()

	rt := NewRuntime("cluster-a", client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)

	timeoutCtx, cancelTimeout := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelTimeout()
	if rt.WaitForSync(timeoutCtx) {
		t.Fatal("WaitForSync returned true before the blocked Pods list was ever released")
	}
}
