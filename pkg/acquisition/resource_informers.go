package acquisition

import (
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// registerResourceInformers wires all 16 resources from the Phase 0 final
// resource set to this runtime's ClusterState. Each is explicit — no
// reflection or generic resource plumbing — because 16 is a small, fixed,
// known set, and explicit code stays reviewable as informer-specific
// quirks (API group/version, the one filtered exception) inevitably show
// up.
func (r *Runtime) registerResourceInformers() {
	r.registerNodes()
	r.registerPods()
	r.registerNamespaces()
	r.registerPersistentVolumeClaims()
	r.registerPersistentVolumes()
	r.registerDeployments()
	r.registerStatefulSets()
	r.registerReplicaSets()
	r.registerServices()
	r.registerIngresses()
	r.registerNetworkPolicies()
	r.registerJobs()
	r.registerCronJobs()
	r.registerHorizontalPodAutoscalers()
	r.registerEndpointSlices()
	r.registerPodWarningEvents()
}

// trackResource registers informer as required for initial sync, wires
// sync to run on every Add/Update/Delete, and wires kind's watch-health
// signal (see health.go) so a LIST failure for this one kind is visible
// without affecting the other 15.
//
// It intentionally does not inspect the event's object: every handler
// relists its resource kind's own local cache (see syncResource) rather
// than applying the specific add/update/delete, so there is no
// cache.DeletedFinalStateUnknown tombstone handling to get wrong — the
// Lister already holds the authoritative current set.
func (r *Runtime) trackResource(kind clusterstate.ResourceKind, informer cache.SharedIndexInformer, sync func()) {
	r.synced = append(r.synced, informer.HasSynced)
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { sync() },
		UpdateFunc: func(any, any) { sync() },
		DeleteFunc: func(any) { sync() },
	})
	// Only errors if called after the informer has started; registration
	// always runs during NewRuntime, before Start, so this cannot fail in
	// practice.
	_ = informer.SetWatchErrorHandler(r.watchErrorHandler(kind))
}

// nonNil turns a nil slice into an empty, non-nil one and passes a non-nil
// slice through unchanged.
//
// A Lister's List returns nil — not an empty slice — when a resource
// kind's cache currently holds zero objects (see cache.ListAll: its named
// return starts nil and is only ever appended to). ClusterResources uses
// that same nil to mean "this kind was not part of this update" (see its
// doc comment and mergeClusterResources). Without this conversion, a
// delete that empties a kind's cache would produce a nil Lister result
// indistinguishable from "untouched," and the last object would never be
// cleared from ClusterState. Every registerX sync closure below must route
// its Lister result through this before handing it to ClusterResources.
func nonNil[T any](items []*T) []*T {
	if items == nil {
		return []*T{}
	}
	return items
}

// syncResource pushes one resource kind's current observed set into
// ClusterState: update, record fresh acquisition metadata for that kind,
// and publish a new generation. resources must have only kind's own field
// populated (see ClusterResources' nil-means-unset convention) so this
// never touches the other 15 kinds.
//
// Metadata is populated only from what client-go actually exposes per
// informer, nothing fabricated:
//   - Synced comes from informer.HasSynced().
//   - ObservedAt is wall-clock time.Now() when this runtime recorded the
//     update — client-go exposes no per-kind "last observed" timestamp of
//     its own.
//   - ResourceVersion comes from informer.LastSyncResourceVersion(), the
//     reflector's own last-synced resourceVersion for this one kind. This
//     is never combined across kinds into one global resourceVersion —
//     Kubernetes has no such thing (docs/08 §8).
func (r *Runtime) syncResource(kind clusterstate.ResourceKind, informer cache.SharedIndexInformer, resources clusterstate.ClusterResources) {
	r.state.Update(resources)
	r.state.SetResourceState(kind, clusterstate.ResourceState{
		Synced:          informer.HasSynced(),
		ObservedAt:      time.Now(),
		ResourceVersion: informer.LastSyncResourceVersion(),
	})
	// A successful sync is direct positive evidence this kind's LIST/WATCH
	// is working — see health.go.
	r.markRecovered(kind)
	r.state.Publish()
}

func (r *Runtime) registerNodes() {
	informer := r.factory.Core().V1().Nodes()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceNodes, informer.Informer(), clusterstate.ClusterResources{Nodes: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceNodes, informer.Informer(), sync)
}

func (r *Runtime) registerPods() {
	informer := r.factory.Core().V1().Pods()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourcePods, informer.Informer(), clusterstate.ClusterResources{Pods: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourcePods, informer.Informer(), sync)
}

func (r *Runtime) registerNamespaces() {
	informer := r.factory.Core().V1().Namespaces()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceNamespaces, informer.Informer(), clusterstate.ClusterResources{Namespaces: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceNamespaces, informer.Informer(), sync)
}

func (r *Runtime) registerPersistentVolumeClaims() {
	informer := r.factory.Core().V1().PersistentVolumeClaims()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourcePersistentVolumeClaims, informer.Informer(), clusterstate.ClusterResources{PersistentVolumeClaims: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourcePersistentVolumeClaims, informer.Informer(), sync)
}

func (r *Runtime) registerPersistentVolumes() {
	informer := r.factory.Core().V1().PersistentVolumes()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourcePersistentVolumes, informer.Informer(), clusterstate.ClusterResources{PersistentVolumes: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourcePersistentVolumes, informer.Informer(), sync)
}

func (r *Runtime) registerDeployments() {
	informer := r.factory.Apps().V1().Deployments()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceDeployments, informer.Informer(), clusterstate.ClusterResources{Deployments: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceDeployments, informer.Informer(), sync)
}

func (r *Runtime) registerStatefulSets() {
	informer := r.factory.Apps().V1().StatefulSets()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceStatefulSets, informer.Informer(), clusterstate.ClusterResources{StatefulSets: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceStatefulSets, informer.Informer(), sync)
}

func (r *Runtime) registerReplicaSets() {
	informer := r.factory.Apps().V1().ReplicaSets()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceReplicaSets, informer.Informer(), clusterstate.ClusterResources{ReplicaSets: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceReplicaSets, informer.Informer(), sync)
}

func (r *Runtime) registerServices() {
	informer := r.factory.Core().V1().Services()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceServices, informer.Informer(), clusterstate.ClusterResources{Services: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceServices, informer.Informer(), sync)
}

func (r *Runtime) registerIngresses() {
	informer := r.factory.Networking().V1().Ingresses()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceIngresses, informer.Informer(), clusterstate.ClusterResources{Ingresses: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceIngresses, informer.Informer(), sync)
}

func (r *Runtime) registerNetworkPolicies() {
	informer := r.factory.Networking().V1().NetworkPolicies()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceNetworkPolicies, informer.Informer(), clusterstate.ClusterResources{NetworkPolicies: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceNetworkPolicies, informer.Informer(), sync)
}

func (r *Runtime) registerJobs() {
	informer := r.factory.Batch().V1().Jobs()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceJobs, informer.Informer(), clusterstate.ClusterResources{Jobs: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceJobs, informer.Informer(), sync)
}

func (r *Runtime) registerCronJobs() {
	informer := r.factory.Batch().V1().CronJobs()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceCronJobs, informer.Informer(), clusterstate.ClusterResources{CronJobs: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceCronJobs, informer.Informer(), sync)
}

func (r *Runtime) registerHorizontalPodAutoscalers() {
	informer := r.factory.Autoscaling().V2().HorizontalPodAutoscalers()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceHorizontalPodAutoscalers, informer.Informer(), clusterstate.ClusterResources{HorizontalPodAutoscalers: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceHorizontalPodAutoscalers, informer.Informer(), sync)
}

func (r *Runtime) registerEndpointSlices() {
	informer := r.factory.Discovery().V1().EndpointSlices()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourceEndpointSlices, informer.Informer(), clusterstate.ClusterResources{EndpointSlices: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourceEndpointSlices, informer.Informer(), sync)
}

// registerPodWarningEvents is the one filtered exception: it registers
// against eventFactory (see Runtime's doc comment), whose ListOptions are
// already tweaked to podWarningEventFieldSelector, so this Lister's cache
// only ever contains Warning events involving a Pod.
func (r *Runtime) registerPodWarningEvents() {
	informer := r.eventFactory.Core().V1().Events()
	sync := func() {
		items, _ := informer.Lister().List(labels.Everything())
		r.syncResource(clusterstate.ResourcePodWarningEvents, informer.Informer(), clusterstate.ClusterResources{PodWarningEvents: nonNil(items)})
	}
	r.trackResource(clusterstate.ResourcePodWarningEvents, informer.Informer(), sync)
}
