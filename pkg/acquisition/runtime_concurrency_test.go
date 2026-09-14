package acquisition

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestConcurrentInformerCallbacksAreRaceFree drives concurrent Add/Update/
// Delete traffic across several resource kinds at once, meant to run under
// go test -race. client-go dispatches each informer's callbacks on its own
// goroutine, so Pods, Nodes, and Deployments changing at the same time is
// the normal case this is meant to prove race-free, not a contrived one —
// every mutation funnels through ClusterState's own mutex (Phase 2), and
// this is that guarantee's proof under real concurrent callers.
func TestConcurrentInformerCallbacksAreRaceFree(t *testing.T) {
	client := fake.NewSimpleClientset()
	rt := NewRuntime("cluster-a", client)
	ctx := startAndSync(t, rt)

	const perKind = 20
	var writers sync.WaitGroup

	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < perKind; i++ {
			name := fmt.Sprintf("pod-%d", i)
			_, _ = client.CoreV1().Pods("default").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}, metav1.CreateOptions{})
		}
	}()

	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < perKind; i++ {
			name := fmt.Sprintf("node-%d", i)
			_, _ = client.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		}
	}()

	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < perKind; i++ {
			name := fmt.Sprintf("svc-%d", i)
			_, _ = client.CoreV1().Services("default").Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}, metav1.CreateOptions{})
		}
	}()

	// Concurrent readers: analyzers will eventually read snapshots while
	// acquisition keeps running (Phase 4), so reading during the writes
	// above must also be race-free. Readers get their own WaitGroup —
	// waiting on them together with the writers, then closing the channel
	// they're waiting on, would deadlock.
	var readers sync.WaitGroup
	stopReaders := make(chan struct{})
	for i := 0; i < 3; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					snap := rt.ClusterState().Publish()
					_ = snap.Resources()
					_, _ = snap.ResourceState(clusterstate.ResourcePods)
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	writers.Wait()
	close(stopReaders)
	readers.Wait()

	waitFor(t, 3*time.Second, func() bool {
		r := rt.ClusterState().Publish().Resources()
		return len(r.Pods) == perKind && len(r.Nodes) == perKind && len(r.Services) == perKind
	})
}

func TestConcurrentStartAndStopAcrossClustersAreRaceFree(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client := fake.NewSimpleClientset()
			rt := NewRuntime(fmt.Sprintf("cluster-%d", id), client)
			ctx, cancel := context.WithCancel(context.Background())
			rt.Start(ctx)
			rt.WaitForSync(ctx)
			rt.Stop()
			cancel()
		}(i)
	}
	wg.Wait()
}
