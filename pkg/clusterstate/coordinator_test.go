package clusterstate

import (
	"context"
	"sync"
	"testing"
	"time"
)

// testWindow is small enough to keep these tests fast and deterministic
// without waiting out the real coalesceWindow, and large enough that a
// burst of Publish calls issued back-to-back in a test goroutine reliably
// lands inside one window on any reasonably-loaded CI machine.
const testWindow = 30 * time.Millisecond

// recordingAnalyzer collects every snapshot Run hands it, safe for
// concurrent use by Run's goroutine and the test's assertions.
type recordingAnalyzer struct {
	mu   sync.Mutex
	seen []*ClusterSnapshot
}

func (r *recordingAnalyzer) analyze(snapshot *ClusterSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, snapshot)
}

func (r *recordingAnalyzer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *recordingAnalyzer) last() *ClusterSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return nil
	}
	return r.seen[len(r.seen)-1]
}

func waitForCount(t *testing.T, rec *recordingAnalyzer, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec.count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if got := rec.count(); got < want {
		t.Fatalf("got %d analyze calls within %s, want at least %d", got, timeout, want)
	}
}

func TestCoordinatorOneGenerationProducesOneTrigger(t *testing.T) {
	state := NewClusterState("cluster-a")
	rec := &recordingAnalyzer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newCoordinator(state, rec.analyze, testWindow)
	go c.Run(ctx)

	state.Publish()

	waitForCount(t, rec, 1, time.Second)
	time.Sleep(5 * testWindow)
	if got := rec.count(); got != 1 {
		t.Fatalf("analyze called %d times for one generation, want exactly 1", got)
	}
}

func TestCoordinatorBurstProducesOneCoalescedTrigger(t *testing.T) {
	state := NewClusterState("cluster-a")
	rec := &recordingAnalyzer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newCoordinator(state, rec.analyze, testWindow)
	go c.Run(ctx)

	for i := 0; i < 4; i++ {
		state.Publish()
	}

	waitForCount(t, rec, 1, time.Second)
	time.Sleep(5 * testWindow)
	if got := rec.count(); got != 1 {
		t.Fatalf("analyze called %d times for a 4-generation burst, want exactly 1", got)
	}
}

func TestCoordinatorTriggerRepresentsLatestGeneration(t *testing.T) {
	state := NewClusterState("cluster-a")
	rec := &recordingAnalyzer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newCoordinator(state, rec.analyze, testWindow)
	go c.Run(ctx)

	var lastPublished *ClusterSnapshot
	for i := 0; i < 4; i++ {
		lastPublished = state.Publish()
	}

	waitForCount(t, rec, 1, time.Second)
	time.Sleep(5 * testWindow)

	got := rec.last()
	if got == nil {
		t.Fatal("analyze was never called")
	}
	if got.Generation() != lastPublished.Generation() {
		t.Fatalf("analyze saw generation %d, want the latest (%d)", got.Generation(), lastPublished.Generation())
	}
}

// TestCoordinatorGenerationsDuringAnalysisAreNotLost proves the "remember
// work is pending, run once more" requirement: a generation published
// while analyze is still running for an earlier one must still produce a
// second trigger once the first finishes, for that later generation.
func TestCoordinatorGenerationsDuringAnalysisAreNotLost(t *testing.T) {
	state := NewClusterState("cluster-a")

	var mu sync.Mutex
	var seen []*ClusterSnapshot
	release := make(chan struct{})
	firstCallStarted := make(chan struct{})
	var once sync.Once

	analyze := func(snapshot *ClusterSnapshot) {
		once.Do(func() {
			close(firstCallStarted)
			<-release // hold the first call open until the test says go
		})
		mu.Lock()
		seen = append(seen, snapshot)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newCoordinator(state, analyze, testWindow)
	go c.Run(ctx)

	gen1 := state.Publish()
	<-firstCallStarted // analyze is now blocked processing gen1

	gen2 := state.Publish() // arrives while analyze is still running

	close(release) // let the first analyze call return

	waitFor := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitFor) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("got %d analyze calls, want exactly 2 (gen1, then gen2)", len(seen))
	}
	if seen[0].Generation() != gen1.Generation() {
		t.Fatalf("first analyze call saw generation %d, want %d", seen[0].Generation(), gen1.Generation())
	}
	if seen[1].Generation() != gen2.Generation() {
		t.Fatalf("second analyze call saw generation %d, want %d", seen[1].Generation(), gen2.Generation())
	}
}

// TestCoordinatorSlowConsumerNeverOverlaps proves overlap is structurally
// impossible: even with continuous publishing while a slow analyze call is
// in flight, at most one call runs at a time.
func TestCoordinatorSlowConsumerNeverOverlaps(t *testing.T) {
	state := NewClusterState("cluster-a")

	var running int32
	var mu sync.Mutex
	var maxConcurrent int32

	analyze := func(*ClusterSnapshot) {
		mu.Lock()
		running++
		if running > maxConcurrent {
			maxConcurrent = running
		}
		mu.Unlock()

		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		running--
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newCoordinator(state, analyze, 5*time.Millisecond)
	go c.Run(ctx)

	stop := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(stop) {
		state.Publish()
		time.Sleep(time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond) // let any final in-flight call finish

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent > 1 {
		t.Fatalf("observed %d overlapping analyze calls, want at most 1", maxConcurrent)
	}
}

// TestCoordinatorRepeatedBurstsRemainBounded proves memory/goroutines stay
// bounded across many bursts: many more Publish calls than analyze calls,
// and no growth pattern that would indicate a queue building up.
func TestCoordinatorRepeatedBurstsRemainBounded(t *testing.T) {
	state := NewClusterState("cluster-a")
	rec := &recordingAnalyzer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newCoordinator(state, rec.analyze, testWindow)
	go c.Run(ctx)

	const bursts = 5
	const perBurst = 20
	for b := 0; b < bursts; b++ {
		for i := 0; i < perBurst; i++ {
			state.Publish()
		}
		time.Sleep(3 * testWindow)
	}

	got := rec.count()
	if got == 0 {
		t.Fatal("expected at least one analyze call across 5 bursts")
	}
	if got > bursts {
		t.Fatalf("analyze called %d times for %d well-separated bursts, want at most %d (one per burst)", got, bursts, bursts)
	}
}

func TestCoordinatorCancellationStopsRun(t *testing.T) {
	state := NewClusterState("cluster-a")
	rec := &recordingAnalyzer{}
	ctx, cancel := context.WithCancel(context.Background())

	c := newCoordinator(state, rec.analyze, testWindow)
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of cancellation")
	}
}

func TestCoordinatorCancellationDuringCoalesceWindowStopsPromptly(t *testing.T) {
	state := NewClusterState("cluster-a")
	rec := &recordingAnalyzer{}
	ctx, cancel := context.WithCancel(context.Background())

	c := newCoordinator(state, rec.analyze, time.Hour) // window intentionally long
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	state.Publish() // enters the (very long) coalescing window
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop within 1s of cancellation while waiting out the coalescing window")
	}
}

func TestCoordinatorsAreIsolatedAcrossClusters(t *testing.T) {
	stateA := NewClusterState("cluster-a")
	stateB := NewClusterState("cluster-b")
	recA := &recordingAnalyzer{}
	recB := &recordingAnalyzer{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cA := newCoordinator(stateA, recA.analyze, testWindow)
	cB := newCoordinator(stateB, recB.analyze, testWindow)
	go cA.Run(ctx)
	go cB.Run(ctx)

	stateA.Publish()

	waitForCount(t, recA, 1, time.Second)
	time.Sleep(5 * testWindow)

	if got := recA.count(); got != 1 {
		t.Fatalf("cluster-a analyze called %d times, want 1", got)
	}
	if got := recB.count(); got != 0 {
		t.Fatalf("cluster-a's generation triggered cluster-b's analyzer: %d calls, want 0", got)
	}
}

func TestSlowClusterDoesNotBlockAnother(t *testing.T) {
	stateSlow := NewClusterState("cluster-slow")
	stateFast := NewClusterState("cluster-fast")

	blockSlow := make(chan struct{})
	slowStarted := make(chan struct{})
	var once sync.Once
	slowAnalyze := func(*ClusterSnapshot) {
		once.Do(func() { close(slowStarted) })
		<-blockSlow
	}
	fast := &recordingAnalyzer{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cSlow := newCoordinator(stateSlow, slowAnalyze, testWindow)
	cFast := newCoordinator(stateFast, fast.analyze, testWindow)
	go cSlow.Run(ctx)
	go cFast.Run(ctx)

	stateSlow.Publish()
	<-slowStarted // cluster-slow's coordinator is now stuck in analyze

	stateFast.Publish()
	waitForCount(t, fast, 1, time.Second)

	close(blockSlow)
}

func TestCoordinatorStoppingOneDoesNotStopAnother(t *testing.T) {
	stateA := NewClusterState("cluster-a")
	stateB := NewClusterState("cluster-b")
	recA := &recordingAnalyzer{}
	recB := &recordingAnalyzer{}

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	cA := newCoordinator(stateA, recA.analyze, testWindow)
	cB := newCoordinator(stateB, recB.analyze, testWindow)
	doneA := make(chan struct{})
	go func() { cA.Run(ctxA); close(doneA) }()
	go cB.Run(ctxB)

	cancelA()
	select {
	case <-doneA:
	case <-time.After(time.Second):
		t.Fatal("cluster-a's coordinator did not stop")
	}

	stateB.Publish()
	waitForCount(t, recB, 1, time.Second)
}
