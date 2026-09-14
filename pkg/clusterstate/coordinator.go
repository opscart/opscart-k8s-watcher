package clusterstate

import (
	"context"
	"time"
)

// coalesceWindow is how long the coordinator waits, from the first
// generation-change notification of a burst, before triggering analysis
// for whatever is latest at that point. It is a plain constant — easy to
// find, easy to change — not configuration: docs/08 Phase 4B asks for the
// smallest explicit mechanism, and nothing today needs this tunable at
// runtime.
const coalesceWindow = 2 * time.Second

// AnalysisFunc is called at most once per coalescing window, with the
// latest available snapshot. No analyzer is wired to this yet (Phase 4B);
// a test/dummy AnalysisFunc is enough to prove the coalescing behavior.
type AnalysisFunc func(*ClusterSnapshot)

// Coordinator converts a burst of ClusterState generation changes into one
// analysis trigger per coalescing window, always for the latest available
// generation — never one trigger per generation, and never a generation
// count treated as elapsed time. Exactly one Coordinator exists per
// cluster, matching its ClusterState (docs/08 §13): nothing here is safe
// to share across clusters.
//
// clockInterval (docs/08 Phase 5) optionally adds a second trigger source:
// a periodic wall-clock tick that also calls analyze against the latest
// snapshot even when no new ClusterState publication has occurred. This is
// required for rules that can change meaning purely from elapsed time on an
// otherwise-unchanged snapshot (Waste age gates, Cost pricing TTL) — see
// docs/08 Phase 5's clock-driven behavior audit. A clock tick never causes
// a new ClusterState generation and never touches Kubernetes; it only
// re-invokes analyze with whatever ClusterState.Latest() already holds.
// Zero disables it, preserving the event-only behavior every pre-Phase-5
// caller relies on.
type Coordinator struct {
	state         *ClusterState
	analyze       AnalysisFunc
	window        time.Duration
	clockInterval time.Duration
}

// NewCoordinator creates the coordinator for one cluster's ClusterState.
// clockInterval is the wall-clock re-analysis cadence (0 disables it —
// see Coordinator's doc comment).
func NewCoordinator(state *ClusterState, analyze AnalysisFunc, clockInterval time.Duration) *Coordinator {
	return newCoordinator(state, analyze, coalesceWindow, clockInterval)
}

// newCoordinator is NewCoordinator with an explicit window, so tests can
// use a millisecond-scale window instead of waiting out coalesceWindow —
// the smallest seam that keeps timing tests fast without a clock
// abstraction.
func newCoordinator(state *ClusterState, analyze AnalysisFunc, window, clockInterval time.Duration) *Coordinator {
	return &Coordinator{state: state, analyze: analyze, window: window, clockInterval: clockInterval}
}

// Run processes generation notifications until ctx is done, then returns.
// Call it as go coordinator.Run(ctx); it blocks for its entire lifetime.
//
// Algorithm: wait for the first notification, then absorb any further
// notifications for up to window without extending that deadline (a fixed
// window from the first event, not a reset-per-event debounce — this
// bounds worst-case trigger latency to window even under a sustained
// stream), then call analyze once against ClusterState.Latest. A clock
// tick (docs/08 Phase 5) is a second, independent trigger into the exact
// same call: it needs no coalescing window of its own — it is already one
// deliberate, rate-limited signal, not a burst to absorb — so it falls
// straight through to runOnce.
//
// Because this loop is single-threaded, a second analyze call can never
// start before the previous one returns — overlap is impossible by
// construction, not by locking, regardless of which of the two trigger
// sources caused it. A notification that arrives while analyze is running
// is not lost: Publications' channel already holds it (or will, since a
// full channel only ever holds one pending signal), so the next
// iteration's wait returns immediately and coalesces into one more run,
// exactly the "remember work is pending, run once more" behavior — with no
// queue, counter, or extra goroutine needed to implement it. A clock tick
// that arrives while an event notification is being coalesced or analyzed
// is not queued either (time.Ticker holds at most one pending tick): at
// worst it is observed one iteration late, on the very next return to this
// select — bounded by window plus one analyze call, never starved
// indefinitely under a continuous event stream.
//
// Coordinator is the sole intended reader of ClusterState.Publications for
// this cluster — see that method's doc comment. Nothing here registers or
// fans out to additional consumers.
//
// Cancellation is checked between notification-wait, coalescing-drain, and
// after analyze returns — not injected into analyze itself. This package
// has no way to safely force-interrupt arbitrary consumer code, so a
// currently-running analyze call is allowed to finish; Run then stops on
// its next check rather than leaking.
func (c *Coordinator) Run(ctx context.Context) {
	var clockTicks <-chan time.Time
	if c.clockInterval > 0 {
		ticker := time.NewTicker(c.clockInterval)
		defer ticker.Stop()
		clockTicks = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.state.Publications():
			if !c.waitForWindow(ctx) {
				return
			}
		case <-clockTicks:
		}

		c.runOnce()
	}
}

// waitForWindow absorbs notifications for up to c.window from the moment
// it's called, returning true once the window elapses or false if ctx was
// canceled first.
func (c *Coordinator) waitForWindow(ctx context.Context) bool {
	timer := time.NewTimer(c.window)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-c.state.Publications():
			// Absorbed into the current window; deliberately not reset.
		case <-timer.C:
			return true
		}
	}
}

// runOnce calls analyze with the latest snapshot, if one has ever been
// published. ClusterState.Latest is a pure read — this never publishes a
// new generation itself.
func (c *Coordinator) runOnce() {
	snapshot := c.state.Latest()
	if snapshot == nil {
		return
	}
	c.analyze(snapshot)
}
