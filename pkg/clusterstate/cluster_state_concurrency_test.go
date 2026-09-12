package clusterstate

import (
	"fmt"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestClusterStateConcurrentAccess exercises Update, SetAcquisitionState,
// SetResourceState, and Publish from multiple goroutines at once, meant to
// run under go test -race. The mutex in ClusterState exists specifically
// because Phase 3's informer event handlers will call these concurrently
// with whatever goroutine publishes — this proves that contract now
// instead of deferring it until a real concurrent caller exists.
//
// The assertion is deliberately about generation bookkeeping, not resource
// content: concurrent Update/Publish calls interleave in an order this
// test does not control, so the only deterministic invariant available is
// that every Publish call hands out a distinct, sequential generation —
// exactly what the mutex is responsible for guaranteeing.
func TestClusterStateConcurrentAccess(t *testing.T) {
	const goroutinesPerKind = 4
	const iterations = 25

	state := NewClusterState("cluster-a")
	var wg sync.WaitGroup

	for g := 0; g < goroutinesPerKind; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				state.Update(ClusterResources{
					Pods: []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d-%d", id, i)}}},
				})
			}
		}(g)
	}

	for g := 0; g < goroutinesPerKind; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cycle := []AcquisitionState{AcquisitionHealthy, AcquisitionDegraded, AcquisitionResyncing, AcquisitionStale}
			for i := 0; i < iterations; i++ {
				state.SetAcquisitionState(cycle[i%len(cycle)])
				state.SetResourceState(ResourcePods, ResourceState{Synced: i%2 == 0})
			}
		}()
	}

	var mu sync.Mutex
	seenGenerations := make(map[uint64]int)

	for g := 0; g < goroutinesPerKind; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				snapshot := state.Publish()

				// Exercise the read paths concurrently too — must not race
				// with the Update/SetAcquisitionState/SetResourceState
				// goroutines above.
				resources := snapshot.Resources()
				_, _ = snapshot.ResourceState(ResourcePods)
				_ = resources
				_ = snapshot.Trustworthy()

				mu.Lock()
				seenGenerations[snapshot.Generation()]++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	wantPublishes := goroutinesPerKind * iterations
	if len(seenGenerations) != wantPublishes {
		t.Fatalf("got %d distinct generations from %d Publish calls, want %d distinct — every call must hand out a unique generation",
			len(seenGenerations), wantPublishes, wantPublishes)
	}
	for gen, count := range seenGenerations {
		if count != 1 {
			t.Fatalf("generation %d was observed %d times, want exactly once", gen, count)
		}
	}
	for gen := uint64(1); gen <= uint64(wantPublishes); gen++ {
		if _, ok := seenGenerations[gen]; !ok {
			t.Fatalf("generation %d missing — expected a contiguous 1..%d range with no gaps", gen, wantPublishes)
		}
	}

	final := state.Publish()
	if final.Generation() != uint64(wantPublishes)+1 {
		t.Fatalf("final Generation() = %d, want %d", final.Generation(), wantPublishes+1)
	}
}
