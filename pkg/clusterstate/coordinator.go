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
type Coordinator struct {
	state   *ClusterState
	analyze AnalysisFunc
	window  time.Duration
}

// NewCoordinator creates the coordinator for one cluster's ClusterState.
func NewCoordinator(state *ClusterState, analyze AnalysisFunc) *Coordinator {
	return newCoordinator(state, analyze, coalesceWindow)
}

// newCoordinator is NewCoordinator with an explicit window, so tests can
// use a millisecond-scale window instead of waiting out coalesceWindow —
// the smallest seam that keeps timing tests fast without a clock
// abstraction.
func newCoordinator(state *ClusterState, analyze AnalysisFunc, window time.Duration) *Coordinator {
	return &Coordinator{state: state, analyze: analyze, window: window}
}

// Run processes generation notifications until ctx is done, then returns.
// Call it as go coordinator.Run(ctx); it blocks for its entire lifetime.
//
// Algorithm: wait for the first notification, then absorb any further
// notifications for up to window without extending that deadline (a fixed
// window from the first event, not a reset-per-event debounce — this
// bounds worst-case trigger latency to window even under a sustained
// stream), then call analyze once against ClusterState.Latest.
//
// Because this loop is single-threaded, a second analyze call can never
// start before the previous one returns — overlap is impossible by
// construction, not by locking. A notification that arrives while analyze
// is running is not lost: Publications' channel already holds it (or
// will, since a full channel only ever holds one pending signal), so the
// next iteration's wait returns immediately and coalesces into one more
// run, exactly the "remember work is pending, run once more" behavior —
// with no queue, counter, or extra goroutine needed to implement it.
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.state.Publications():
		}

		if !c.waitForWindow(ctx) {
			return
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
