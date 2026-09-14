package acquisition

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"k8s.io/client-go/tools/cache"
)

// watchErrorHandler returns the client-go WatchErrorHandler registered for
// kind's informer (see trackResource).
//
// It fires only when the reflector's LIST itself fails — not on ordinary
// watch disconnects. client-go's watch loop (tools/cache/reflector.go)
// swallows routine closures (server-enforced watch timeouts, io.EOF,
// resourceVersion-expired) internally and always returns nil from
// ListAndWatch in those cases, which silently triggers a fresh relist on
// the next iteration of Reflector.Run's retry loop; none of that reaches
// this handler. WatchErrorHandler is invoked only when ListAndWatch itself
// returns a non-nil error — in practice, the initial LIST (or a
// post-disconnect relist) could not complete at all: the API server was
// unreachable, or the request was rejected. That is a genuine, specific
// "this resource kind's acquisition just failed" signal, not routine watch
// churn, so no further filtering by error type is needed.
//
// This only records the failure for ClusterState; the retry itself (with
// backoff) remains entirely client-go's responsibility via
// wait.BackoffUntil in Reflector.Run — nothing here waits, retries, or
// re-lists.
func (r *Runtime) watchErrorHandler(kind clusterstate.ResourceKind) cache.WatchErrorHandler {
	return func(_ *cache.Reflector, _ error) {
		r.markDegraded(kind)
	}
}

// markDegraded records that kind's most recent LIST attempt failed and
// recomputes the cluster's aggregate acquisition state. Unlike a resource
// update, a health transition is significant on its own — recomputeHealth
// publishes immediately when it actually changes the aggregate state,
// rather than waiting for the next resource event.
func (r *Runtime) markDegraded(kind clusterstate.ResourceKind) {
	r.healthMu.Lock()
	r.degradedKinds[kind] = struct{}{}
	r.healthMu.Unlock()
	r.recomputeHealth()
}

// markRecovered records direct, positive evidence that kind's informer
// just delivered data successfully — its LIST/WATCH is working. It runs on
// every successful sync (see syncResource), not only ones following a
// known failure: clearing an absent key is a harmless no-op, and tying
// recovery to the same single path keeps the model simple. syncResource
// always publishes the resulting generation itself regardless, so this
// call's own recomputeHealth-triggered publish (if the health transition
// happened to be the one that flips the aggregate state) is at most
// redundant here, never load-bearing — see WaitForSync for the path where
// it is.
func (r *Runtime) markRecovered(kind clusterstate.ResourceKind) {
	r.healthMu.Lock()
	delete(r.degradedKinds, kind)
	r.healthMu.Unlock()
	r.recomputeHealth()
}

// allSynced reports whether every required informer has completed its
// initial sync, without blocking. cache.WaitForCacheSync is built for a
// one-time wait; this is called on every health recomputation, so it just
// polls each already-collected cache.InformerSynced func directly.
func (r *Runtime) allSynced() bool {
	for _, synced := range r.synced {
		if !synced() {
			return false
		}
	}
	return true
}

// recomputeHealth derives the cluster's aggregate acquisition state from
// the two signals this runtime tracks — initial-sync completion and
// per-kind watch health — and is the only place that sets ClusterState's
// acquisition state after Start. Funneling both WaitForSync's
// sync-completion path and markDegraded/markRecovered's watch-health path
// through here means they can never race each other into an inconsistent
// result.
//
// Precedence: not fully synced always reads RESYNCING, even if a required
// kind is also currently marked degraded. Both states are equally
// untrustworthy for lifecycle decisions (AcquisitionState.Trustworthy), so
// this is a diagnostic choice — "still starting up" is a more useful label
// than "degraded" for a kind that has simply never synced yet — not a
// correctness one.
//
// When the resulting state actually changes ClusterState's externally
// observable trustworthiness, this publishes immediately. This is what
// closes the gap WaitForSync's success path would otherwise leave open: an
// idle, fully-synced cluster whose most recent resource-triggered publish
// happened to occur while sync was still incomplete would otherwise stay
// stamped RESYNCING forever, since nothing else would ever publish again
// on an idle cluster. SetAcquisitionState's own changed result — not a
// separate cache of "the last state we published" — is what decides this,
// so a call that leaves the state unchanged (e.g. a second markDegraded
// for an already-degraded kind) correctly produces no publish.
func (r *Runtime) recomputeHealth() {
	var changed bool
	switch {
	case !r.allSynced():
		changed = r.state.SetAcquisitionState(clusterstate.AcquisitionResyncing)
	default:
		r.healthMu.Lock()
		degraded := len(r.degradedKinds) > 0
		r.healthMu.Unlock()
		if degraded {
			changed = r.state.SetAcquisitionState(clusterstate.AcquisitionDegraded)
		} else {
			changed = r.state.SetAcquisitionState(clusterstate.AcquisitionHealthy)
		}
	}
	if changed {
		r.state.Publish()
	}
}
