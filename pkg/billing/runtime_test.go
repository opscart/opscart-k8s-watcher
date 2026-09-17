package billing

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProvider is a synthetic Provider fixture: every test in this file
// controls exactly what FetchBilling returns, so Runtime tests never touch
// a real network.
type fakeProvider struct {
	mu          sync.Mutex
	calls       int32
	inflight    int32
	maxInFlight int32
	result      Result
	err         error
	// block, when non-nil, is closed by the test to let a held call
	// through — used to prove single-flighting.
	block <-chan struct{}
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) FetchBilling(ctx context.Context, req Request) (Result, error) {
	atomic.AddInt32(&f.calls, 1)
	current := atomic.AddInt32(&f.inflight, 1)
	defer atomic.AddInt32(&f.inflight, -1)
	for {
		max := atomic.LoadInt32(&f.maxInFlight)
		if current <= max {
			break
		}
		if atomic.CompareAndSwapInt32(&f.maxInFlight, max, current) {
			break
		}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, f.err
}

func testPeriod(now time.Time) (time.Time, time.Time, error) {
	return now.Add(-time.Hour), now, nil
}

func newTestRuntime(provider Provider) *Runtime {
	return &Runtime{
		provider: provider,
		interval: time.Hour, // real ticker never fires within these tests
		timeout:  5 * time.Second,
		basis:    CostBasisActualCost,
		period:   testPeriod,
		snapshot: Snapshot{Status: StatusDisabled},
		done:     make(chan struct{}),
	}
}

func TestRuntimeSnapshotDisabledBeforeAnyRefresh(t *testing.T) {
	rt := newTestRuntime(&fakeProvider{})
	snap := rt.Snapshot()
	if snap.Status != StatusDisabled {
		t.Errorf("Status = %q, want disabled before any refresh", snap.Status)
	}
}

func TestRuntimeRefreshSuccessIsAvailable(t *testing.T) {
	provider := &fakeProvider{result: Result{Total: 42, Currency: "USD", RowCount: 3, RetrievedAt: time.Now()}}
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())

	snap := rt.Snapshot()
	if snap.Status != StatusAvailable {
		t.Fatalf("Status = %q, want available", snap.Status)
	}
	if snap.Total != 42 {
		t.Errorf("Total = %v, want 42", snap.Total)
	}
}

func TestRuntimeZeroRowsIsNoData(t *testing.T) {
	provider := &fakeProvider{result: Result{Total: 0, RowCount: 0, RetrievedAt: time.Now()}}
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())

	if got := rt.Snapshot().Status; got != StatusNoData {
		t.Errorf("Status = %q, want no_data", got)
	}
}

func TestRuntimeFailureWithNoPriorSnapshotIsUnavailable(t *testing.T) {
	provider := &fakeProvider{err: fmt.Errorf("azure unreachable")}
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())

	snap := rt.Snapshot()
	if snap.Status != StatusUnavailable {
		t.Fatalf("Status = %q, want unavailable", snap.Status)
	}
	if snap.UnavailableReason == "" {
		t.Error("expected a non-empty UnavailableReason")
	}
	if snap.Total != 0 {
		t.Error("an error must never be reported as a zero-cost result")
	}
}

func TestRuntimeRetainsLastSnapshotAndMarksStaleOnFailure(t *testing.T) {
	provider := &fakeProvider{result: Result{Total: 100, Currency: "USD", RowCount: 1, RetrievedAt: time.Now()}}
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())
	if rt.Snapshot().Status != StatusAvailable {
		t.Fatalf("setup: expected first refresh to succeed")
	}

	provider.mu.Lock()
	provider.err = fmt.Errorf("throttled")
	provider.mu.Unlock()
	rt.refreshOnce(context.Background())

	snap := rt.Snapshot()
	if snap.Status != StatusStale {
		t.Fatalf("Status = %q, want stale", snap.Status)
	}
	if !snap.Stale {
		t.Error("Stale = false, want true")
	}
	if snap.Total != 100 {
		t.Errorf("Total = %v, want retained prior value 100", snap.Total)
	}
	if snap.UnavailableReason == "" {
		t.Error("expected UnavailableReason to record why this snapshot is stale")
	}
}

func TestRuntimePreventsDuplicateConcurrentRefresh(t *testing.T) {
	block := make(chan struct{})
	provider := &fakeProvider{result: Result{Total: 1, Currency: "USD", RowCount: 1}, block: block}
	rt := newTestRuntime(provider)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.refreshOnce(context.Background())
		}()
	}
	// Give every goroutine a chance to hit the CompareAndSwap gate before
	// releasing the one that got through.
	time.Sleep(20 * time.Millisecond)
	close(block)
	wg.Wait()

	if calls := atomic.LoadInt32(&provider.calls); calls != 1 {
		t.Errorf("provider.calls = %d, want exactly 1 (single-flighted)", calls)
	}
	if max := atomic.LoadInt32(&provider.maxInFlight); max != 1 {
		t.Errorf("max concurrent FetchBilling calls = %d, want 1", max)
	}
}

func TestRuntimeClusterIsolation(t *testing.T) {
	providerA := &fakeProvider{result: Result{Total: 10, Currency: "USD", RowCount: 1}}
	providerB := &fakeProvider{err: fmt.Errorf("cluster B unreachable")}
	rtA := newTestRuntime(providerA)
	rtB := newTestRuntime(providerB)

	rtA.refreshOnce(context.Background())
	rtB.refreshOnce(context.Background())

	if got := rtA.Snapshot().Status; got != StatusAvailable {
		t.Errorf("cluster A Status = %q, want available", got)
	}
	if got := rtB.Snapshot().Status; got != StatusUnavailable {
		t.Errorf("cluster B Status = %q, want unavailable", got)
	}
	if rtA.Snapshot().Total != 10 {
		t.Errorf("cluster A Total = %v, cluster B's failure must not affect it", rtA.Snapshot().Total)
	}
}

func TestRuntimeStartAndStopLifecycle(t *testing.T) {
	provider := &fakeProvider{result: Result{Total: 1, Currency: "USD", RowCount: 1}}
	rt := newTestRuntime(provider)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.Start(ctx)
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&provider.calls) == 0 {
		select {
		case <-deadline:
			t.Fatal("Start did not perform its immediate first refresh in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := rt.Snapshot().Status; got != StatusAvailable {
		t.Errorf("Status after Start = %q, want available", got)
	}
	rt.Stop()
	<-rt.done
}
