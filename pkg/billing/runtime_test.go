package billing

import (
	"context"
	"fmt"
	"strings"
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

func TestRuntimeSnapshotCarriesAttributionFieldsFromResult(t *testing.T) {
	lines := []ResourceCost{
		{ResourceID: "/subscriptions/s/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks", ResourceGroup: "rg", Cost: 10, Currency: "USD", Attributed: true},
		{ResourceID: "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/x", ResourceGroup: "rg", Cost: 90, Currency: "USD", Attributed: false},
	}
	result := Result{
		Total: 100, Currency: "USD", RowCount: 2, RetrievedAt: time.Now(),
		Lines:             lines,
		ClusterResourceID: "/subscriptions/s/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks",
		AttributedTotal:   10,
		UnattributedTotal: 90,
	}
	provider := &fakeProvider{result: result}
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())

	snap := rt.Snapshot()
	if len(snap.Lines) != 2 {
		t.Fatalf("len(Lines) = %d, want 2", len(snap.Lines))
	}
	if snap.ClusterResourceID != result.ClusterResourceID {
		t.Errorf("ClusterResourceID = %q, want %q", snap.ClusterResourceID, result.ClusterResourceID)
	}
	if snap.AttributedTotal != 10 || snap.UnattributedTotal != 90 {
		t.Errorf("AttributedTotal/UnattributedTotal = %v/%v, want 10/90", snap.AttributedTotal, snap.UnattributedTotal)
	}
	if snap.AttributedTotal+snap.UnattributedTotal != snap.Total {
		t.Errorf("AttributedTotal + UnattributedTotal = %v, want Total %v", snap.AttributedTotal+snap.UnattributedTotal, snap.Total)
	}
}

func TestRuntimeRecordSuccessClonesLinesAndDisclosures(t *testing.T) {
	lines := []ResourceCost{{ResourceID: "/r/1", ResourceGroup: "rg", Cost: 10, Currency: "USD", Attributed: true}}
	disclosures := []string{"original disclosure"}
	result := Result{Total: 10, Currency: "USD", RowCount: 1, RetrievedAt: time.Now(), Lines: lines, Disclosures: disclosures}

	provider := &fakeProvider{result: result}
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())

	// Mutate the caller's original slices after the fact — this must never
	// reach the cached Snapshot, which proves recordSuccess cloned rather
	// than aliased them.
	lines[0].Cost = 99999
	lines[0].ResourceID = "/mutated"
	disclosures[0] = "mutated disclosure"

	snap := rt.Snapshot()
	if snap.Lines[0].Cost != 10 || snap.Lines[0].ResourceID != "/r/1" {
		t.Errorf("cached Lines mutated by the original result's slice: %+v", snap.Lines[0])
	}
	if snap.Disclosures[0] != "original disclosure" {
		t.Errorf("cached Disclosures mutated by the original result's slice: %q", snap.Disclosures[0])
	}
}

func TestRuntimeSnapshotClonesLinesAndDisclosures(t *testing.T) {
	result := Result{
		Total: 10, Currency: "USD", RowCount: 1, RetrievedAt: time.Now(),
		Lines:       []ResourceCost{{ResourceID: "/r/1", ResourceGroup: "rg", Cost: 10, Currency: "USD"}},
		Disclosures: []string{"original disclosure"},
	}
	provider := &fakeProvider{result: result}
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())

	first := rt.Snapshot()
	// Mutate the caller's copy of the returned Snapshot — this must never
	// reach Runtime's cached snapshot or a subsequent Snapshot() call,
	// which proves Snapshot() clones rather than aliases them.
	first.Lines[0].Cost = 99999
	first.Lines[0].ResourceID = "/mutated"
	first.Disclosures[0] = "mutated disclosure"

	second := rt.Snapshot()
	if second.Lines[0].Cost != 10 || second.Lines[0].ResourceID != "/r/1" {
		t.Errorf("cached snapshot mutated by an earlier Snapshot() call's returned slice: %+v", second.Lines[0])
	}
	if second.Disclosures[0] != "original disclosure" {
		t.Errorf("cached snapshot mutated by an earlier Snapshot() call's returned slice: %q", second.Disclosures[0])
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

func TestRuntimeUnavailableNeverSetsLastSuccessfulRefresh(t *testing.T) {
	provider := &fakeProvider{err: fmt.Errorf("azure unreachable")}
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())

	snap := rt.Snapshot()
	if !snap.RetrievedAt.IsZero() {
		t.Errorf("RetrievedAt = %v, want zero — no refresh has ever succeeded", snap.RetrievedAt)
	}
	if snap.LastAttemptedAt.IsZero() {
		t.Error("LastAttemptedAt is zero, want the time of this failed attempt")
	}
}

func TestRuntimeDistinguishesLastAttemptedFromLastSuccessfulRefresh(t *testing.T) {
	provider := &fakeProvider{result: Result{Total: 100, Currency: "USD", RowCount: 1, RetrievedAt: time.Now()}}
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())

	firstSuccess := rt.Snapshot().RetrievedAt
	if firstSuccess.IsZero() {
		t.Fatal("setup: expected RetrievedAt to be set after a successful refresh")
	}

	provider.mu.Lock()
	provider.err = fmt.Errorf("throttled")
	provider.mu.Unlock()
	time.Sleep(time.Millisecond) // ensure a distinguishable, later LastAttemptedAt
	rt.refreshOnce(context.Background())

	snap := rt.Snapshot()
	if !snap.RetrievedAt.Equal(firstSuccess) {
		t.Errorf("RetrievedAt changed to %v after a failed attempt, want it to stay at %v (last SUCCESSFUL refresh)", snap.RetrievedAt, firstSuccess)
	}
	if !snap.LastAttemptedAt.After(firstSuccess) {
		t.Errorf("LastAttemptedAt = %v, want it to advance past the first successful refresh at %v", snap.LastAttemptedAt, firstSuccess)
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

func TestRuntimeDeferredFailureSuppressesRefreshUntilCooldownElapses(t *testing.T) {
	provider := &fakeProvider{err: &DeferredError{
		Operation: "test", RetryAfter: 50 * time.Millisecond, NotBefore: time.Now().Add(50 * time.Millisecond),
		StatusCode: 429, RequestID: "req-cooldown",
	}}
	rt := newTestRuntime(provider)

	rt.refreshOnce(context.Background())
	if calls := atomic.LoadInt32(&provider.calls); calls != 1 {
		t.Fatalf("calls after the first (deferred) attempt = %d, want 1", calls)
	}
	firstAttempt := rt.Snapshot().LastAttemptedAt
	if firstAttempt.IsZero() {
		t.Fatal("expected LastAttemptedAt to be set after the deferred attempt")
	}
	// The status and request ID the query client preserved on the
	// DeferredError must reach the dashboard-facing text, not just the
	// cooldown timing.
	reason := rt.Snapshot().UnavailableReason
	if !strings.Contains(reason, "429") || !strings.Contains(reason, "req-cooldown") {
		t.Errorf("UnavailableReason = %q, want it to retain the status and request ID", reason)
	}

	// A refresh tick during the cooldown must be a complete no-op: no
	// provider call, no LastAttemptedAt change — a configured
	// refreshInterval shorter than Azure's mandated delay must not cause
	// an early request.
	rt.refreshOnce(context.Background())
	if calls := atomic.LoadInt32(&provider.calls); calls != 1 {
		t.Errorf("calls after a tick during cooldown = %d, want still 1", calls)
	}
	if got := rt.Snapshot().LastAttemptedAt; !got.Equal(firstAttempt) {
		t.Errorf("LastAttemptedAt changed to %v during a skipped (cooldown) tick, want unchanged at %v", got, firstAttempt)
	}

	time.Sleep(60 * time.Millisecond)
	provider.mu.Lock()
	provider.err = nil
	provider.result = Result{Total: 1, Currency: "USD", RowCount: 1, RetrievedAt: time.Now()}
	provider.mu.Unlock()

	rt.refreshOnce(context.Background())
	if calls := atomic.LoadInt32(&provider.calls); calls != 2 {
		t.Errorf("calls after the cooldown elapsed = %d, want 2 (a real attempt was made)", calls)
	}
	if got := rt.Snapshot().Status; got != StatusAvailable {
		t.Errorf("Status = %q, want available", got)
	}
}

func TestRuntimeSuccessClearsCooldown(t *testing.T) {
	provider := &fakeProvider{err: &DeferredError{Operation: "test", RetryAfter: time.Millisecond, NotBefore: time.Now().Add(-time.Hour)}} // already elapsed
	rt := newTestRuntime(provider)
	rt.refreshOnce(context.Background())

	rt.mu.Lock()
	notBefore := rt.notBefore
	rt.mu.Unlock()
	if notBefore.IsZero() {
		t.Fatal("setup: expected the deferred failure to set a cooldown")
	}

	provider.mu.Lock()
	provider.err = nil
	provider.result = Result{Total: 1, Currency: "USD", RowCount: 1, RetrievedAt: time.Now()}
	provider.mu.Unlock()

	rt.refreshOnce(context.Background())

	rt.mu.Lock()
	notBefore = rt.notBefore
	rt.mu.Unlock()
	if !notBefore.IsZero() {
		t.Errorf("notBefore = %v after a successful refresh, want zero (cleared)", notBefore)
	}
}

func TestRuntimeStartupJitterDelaysFirstRefresh(t *testing.T) {
	provider := &fakeProvider{result: Result{Total: 1, Currency: "USD", RowCount: 1}}
	rt := newTestRuntime(provider)
	rt.startupJitter = 200 * time.Millisecond

	// Deterministic jitter (see runtime.go's randomJitter doc comment) —
	// asserting timing against real randomness would be flaky, since a
	// genuinely random delay can legitimately land near zero.
	oldJitter := randomJitter
	randomJitter = func(time.Duration) time.Duration { return 100 * time.Millisecond }
	defer func() { randomJitter = oldJitter }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)

	time.Sleep(30 * time.Millisecond)
	if calls := atomic.LoadInt32(&provider.calls); calls != 0 {
		t.Errorf("calls = %d shortly after Start with jitter configured, want 0 (still within the jitter window)", calls)
	}

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&provider.calls) == 0 {
		select {
		case <-deadline:
			t.Fatal("jittered first refresh never happened")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestRuntimeZeroJitterRefreshesImmediately(t *testing.T) {
	provider := &fakeProvider{result: Result{Total: 1, Currency: "USD", RowCount: 1}}
	rt := newTestRuntime(provider) // startupJitter left at its zero value
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)

	deadline := time.After(time.Second)
	for atomic.LoadInt32(&provider.calls) == 0 {
		select {
		case <-deadline:
			t.Fatal("Start with zero jitter did not refresh promptly")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
