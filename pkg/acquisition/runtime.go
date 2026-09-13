// Package acquisition owns the informer-backed acquisition layer from
// docs/08-Event-Driven-Cluster-State.md Phase 3: one SharedInformerFactory
// pair per cluster, wired to populate a clusterstate.ClusterState.
//
//	Kubernetes API
//	      ↓
//	SharedInformerFactory      (client-go: LIST, WATCH, reconnect, relist,
//	      ↓                     resourceVersion recovery, reflector lifecycle)
//	informer caches
//	      ↓
//	clusterstate.ClusterState  (this package: registration, sync gating,
//	      ↓                     acquisition state, resource metadata)
//	ClusterSnapshot
//
// This package does not coordinate or schedule analyzer runs, does not
// debounce or coalesce publications, and does not implement any watch,
// reconnect, or relist logic of its own — all of that remains client-go's
// job, and analyzer migration remains Phase 4's.
package acquisition

import (
	"context"
	"sync"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// defaultResync is passed to both informer factories. Zero disables the
// informer's own periodic "resync" — the timer-driven redelivery of
// already-cached, unchanged objects to already-registered handlers
// (SharedIndexInformer's resyncCheckPeriod/ShouldResync machinery). That
// mechanism is a purely local, OpsCart-configured replay; it performs no
// Kubernetes API call and is entirely separate from client-go's own
// LIST/WATCH/reconnect/relist consistency behavior, which is unconditional
// and unaffected by this setting (the reflector relists on every dropped
// watch — including routine server-enforced watch timeouts, roughly every
// 5-10 minutes per resource kind — regardless of resyncPeriod). A non-zero
// value here would only add OpsCart's own synthetic Update notifications on
// top of that already-occurring baseline, producing generations with no
// underlying Kubernetes change. Nothing in this repository's client-go
// usage requires that extra replay, so it stays off unless a concrete need
// is found.
const defaultResync = 0

// Runtime is the informer-backed acquisition owner for exactly one
// cluster. Each cluster gets its own Runtime: its own factories, its own
// ClusterState, its own cancellation — nothing here is safe to share
// across clusters, and nothing in this type reaches into another Runtime.
type Runtime struct {
	clusterID string
	state     *clusterstate.ClusterState

	// factory serves the 15 resource kinds Phase 0 identified apart from
	// Events. eventFactory serves only the filtered Pod Warning Event
	// informer.
	//
	// Two factories, not one: a SharedInformerFactory's ListOptions tweak
	// (informers.NewFilteredSharedInformerFactory's tweakListOptions) is a
	// factory-wide setting applied to every informer that factory
	// constructs — the generated per-group accessors (Core().V1(), etc.)
	// have no per-informer override. Getting Events filtered without also
	// filtering the other 15 therefore requires either a second factory
	// instance (this package's choice) or hand-building the Event informer
	// from cache.NewSharedIndexInformer with a custom ListWatch. The second
	// path re-implements what the generated factory/lister code already
	// does correctly (option merging, pagination, watch bookmarks) for no
	// benefit beyond one fewer struct field, so it was rejected.
	//
	// Ownership stays one-informer-per-resource regardless: each of the 16
	// kinds has exactly one informer, in exactly one of these two
	// factories, wired to ClusterState exactly once (resource_informers.go).
	// Both factories are started and stopped together by this same Runtime
	// (Start/Stop below) — they are one lifecycle unit, not two competing
	// owners.
	factory      informers.SharedInformerFactory
	eventFactory informers.SharedInformerFactory

	synced []cache.InformerSynced

	// healthMu guards degradedKinds — see health.go for the acquisition
	// health model this supports.
	healthMu      sync.Mutex
	degradedKinds map[clusterstate.ResourceKind]struct{}

	cancel context.CancelFunc
}

// NewRuntime creates the acquisition runtime for one cluster and registers
// all 16 resource informers (see resource_informers.go). Registration only
// wires up listers and event handlers — it performs no Kubernetes API
// calls; those begin when Start runs the factories.
func NewRuntime(clusterID string, client kubernetes.Interface) *Runtime {
	r := &Runtime{
		clusterID:     clusterID,
		state:         clusterstate.NewClusterState(clusterID),
		factory:       informers.NewSharedInformerFactory(client, defaultResync),
		eventFactory:  informers.NewFilteredSharedInformerFactory(client, defaultResync, "", tweakToPodWarningEvents),
		degradedKinds: make(map[clusterstate.ResourceKind]struct{}),
	}
	r.registerResourceInformers()
	return r
}

// ClusterState returns the cluster state this runtime populates. Analyzer
// migration (Phase 4) is the intended eventual reader of the snapshots it
// publishes; nothing in this package reads it back.
func (r *Runtime) ClusterState() *clusterstate.ClusterState {
	return r.state
}

// Start begins LIST/WATCH acquisition: it marks the cluster RESYNCING,
// starts both informer factories (non-blocking — client-go runs them on
// its own goroutines), and starts a background wait for initial sync that
// marks the cluster HEALTHY once every required informer has synced.
// Canceling ctx (or calling Stop) stops both factories; client-go handles
// their shutdown.
//
// Start does not block. Call WaitForSync if the caller needs to know when
// (or whether) the initial sync actually completed.
func (r *Runtime) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	r.state.SetAcquisitionState(clusterstate.AcquisitionResyncing)

	r.factory.Start(runCtx.Done())
	r.eventFactory.Start(runCtx.Done())

	go r.WaitForSync(runCtx)
}

// WaitForSync blocks until every required informer cache has synced or ctx
// is done, whichever comes first. On success it recomputes acquisition
// state (see health.go) — HEALTHY, unless a resource kind is currently
// degraded — and returns true. On failure (ctx canceled/expired before
// sync completed) it returns false and leaves acquisition state exactly as
// it was — RESYNCING, or whatever it already was — never fabricating
// HEALTHY. It is safe to call directly (e.g. from a test) without going
// through Start's background goroutine.
func (r *Runtime) WaitForSync(ctx context.Context) bool {
	if !cache.WaitForCacheSync(ctx.Done(), r.synced...) {
		return false
	}
	r.recomputeHealth()
	return true
}

// Stop cancels this runtime's context, stopping both informer factories.
// It has no effect on any other Runtime. Calling Stop before Start has no
// effect.
func (r *Runtime) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
}
