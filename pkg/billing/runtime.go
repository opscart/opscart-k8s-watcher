package billing

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// defaultRefreshTimeout bounds a single refresh attempt (both resource-group
// queries plus the node-resource-group lookup), independent of how long the
// refresh interval itself is, so one slow or hanging Azure call can never
// block the next scheduled refresh indefinitely.
const defaultRefreshTimeout = 60 * time.Second

// Runtime is the process-owned, per-cluster background billing refresh
// loop. It is deliberately separate from pkg/clusterstate.Coordinator and
// every Kubernetes scan path (docs/07/AGENTS.md §16: external provider data
// keeps its own freshness/TTL semantics, independent of Kubernetes
// acquisition): nothing in this type ever runs on an HTTP request path, and
// nothing in the Kubernetes scan path ever calls FetchBilling.
//
// Each configured cluster gets its own Runtime, its own Provider, and its
// own cached Snapshot — never shared across clusters, matching the
// acquisition.Runtime per-cluster isolation convention.
type Runtime struct {
	provider Provider
	interval time.Duration
	timeout  time.Duration
	basis    CostBasis
	period   func(now time.Time) (start, end time.Time, err error)

	mu          sync.Mutex
	snapshot    Snapshot
	hasSnapshot bool

	refreshing atomic.Bool
	cancel     context.CancelFunc
	done       chan struct{}
}

// NewRuntime constructs a billing runtime for one cluster's already-valid
// configuration and provider. The returned Runtime reports StatusDisabled
// until Start's first refresh completes.
func NewRuntime(cfg ClusterConfig, provider Provider) *Runtime {
	basis := cfg.EffectiveCostBasis()
	return &Runtime{
		provider: provider,
		interval: cfg.EffectiveRefreshInterval(),
		timeout:  defaultRefreshTimeout,
		basis:    basis,
		period:   cfg.ResolvePeriod,
		snapshot: Snapshot{Status: StatusDisabled},
		done:     make(chan struct{}),
	}
}

// Start launches the background refresh loop: an immediate first refresh,
// then one every configured refresh interval, until ctx is canceled or Stop
// is called. Start does not block and performs no work on the caller's
// goroutine.
func (r *Runtime) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	go r.loop(runCtx)
}

func (r *Runtime) loop(ctx context.Context) {
	defer close(r.done)
	r.refreshOnce(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshOnce(ctx)
		}
	}
}

// Stop cancels this runtime's background loop. It has no effect on any
// other Runtime, and calling it before Start has no effect. It does not
// block — matching pkg/acquisition.Runtime.Stop's convention.
func (r *Runtime) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
}

// refreshOnce is single-flighted: a refresh already in flight (whether
// clock-triggered or the initial Start call) makes a concurrent call a
// no-op instead of a duplicate outbound Azure query.
func (r *Runtime) refreshOnce(ctx context.Context) {
	if !r.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer r.refreshing.Store(false)

	start, end, err := r.period(time.Now())
	if err != nil {
		r.recordFailure(err)
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	result, err := r.provider.FetchBilling(reqCtx, Request{PeriodStart: start, PeriodEnd: end, CostBasis: r.basis})
	if err != nil {
		r.recordFailure(err)
		return
	}
	r.recordSuccess(result)
}

func (r *Runtime) recordFailure(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hasSnapshot && r.snapshot.Status != StatusUnavailable {
		// A prior successful (or previously stale) snapshot exists — retain
		// and mark it stale rather than discarding known-good data or
		// fabricating a $0 result.
		r.snapshot.Status = StatusStale
		r.snapshot.Stale = true
		r.snapshot.UnavailableReason = err.Error()
		return
	}
	r.snapshot = Snapshot{
		Status:            StatusUnavailable,
		UnavailableReason: err.Error(),
		RetrievedAt:       time.Now(),
	}
	r.hasSnapshot = true
}

func (r *Runtime) recordSuccess(result Result) {
	status := StatusAvailable
	if result.RowCount == 0 {
		status = StatusNoData
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshot = Snapshot{
		Status:      status,
		Total:       result.Total,
		Currency:    result.Currency,
		CostBasis:   result.CostBasis,
		PeriodStart: result.PeriodStart,
		PeriodEnd:   result.PeriodEnd,
		Source:      result.Source,
		Scope:       result.Scope,
		Coverage:    result.Coverage,
		Disclosures: result.Disclosures,
		RowCount:    result.RowCount,
		RetrievedAt: result.RetrievedAt,
	}
	r.hasSnapshot = true
}

// Snapshot returns the current cached billing view. It never blocks on or
// triggers network activity — safe to call from an HTTP page-render path.
func (r *Runtime) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot
}
