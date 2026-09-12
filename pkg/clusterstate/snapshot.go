package clusterstate

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// ClusterResources is one batch of observed Kubernetes resources for a
// cluster — the Phase 0 final resource set (docs/08 §6), and nothing more:
// do not add a field here without a current analyzer that needs it.
//
// Every field is a slice of pointers, matching what a client-go
// SharedInformerFactory lister returns (Phase 3) and what this package
// requires of every object regardless of source today:
//
//	Kubernetes objects are read-only by contract, never by mechanism.
//
// Nothing in this package prevents a caller from mutating a *corev1.Pod it
// obtained from ClusterResources — exactly like nothing in client-go
// prevents mutating an object returned from an informer Lister. The
// contract is the same one client-go already documents for cache objects:
// treat every object reached through ClusterResources as read-only, full
// stop. What this package DOES guarantee is collection independence: the
// []*T slices themselves — their length, order, and set of pointers — are
// never shared in a way a caller could corrupt by appending to or
// reassigning an element of a slice it was handed. See cloneSlice.
//
// A nil field means that resource kind was not part of the update that
// produced it, not that the cluster has zero objects of that kind — the
// same convention client-go's typed List responses already establish
// (Items is always non-nil, even when empty).
type ClusterResources struct {
	Nodes                    []*corev1.Node
	Pods                     []*corev1.Pod
	Namespaces               []*corev1.Namespace
	PersistentVolumeClaims   []*corev1.PersistentVolumeClaim
	PersistentVolumes        []*corev1.PersistentVolume
	Deployments              []*appsv1.Deployment
	StatefulSets             []*appsv1.StatefulSet
	ReplicaSets              []*appsv1.ReplicaSet
	Services                 []*corev1.Service
	Ingresses                []*networkingv1.Ingress
	NetworkPolicies          []*networkingv1.NetworkPolicy
	Jobs                     []*batchv1.Job
	CronJobs                 []*batchv1.CronJob
	HorizontalPodAutoscalers []*autoscalingv2.HorizontalPodAutoscaler
	EndpointSlices           []*discoveryv1.EndpointSlice
	// PodWarningEvents is pre-filtered to Warning-type Events involving a
	// Pod (docs/08 Phase 0 findings: "filtered informer over Pod-involved
	// Warning events") — not a general cluster-wide Event cache.
	PodWarningEvents []*corev1.Event
}

// cloneSlice returns an independent copy of the slice in — its own backing
// array, own length, own capacity — while copying each element as-is. For
// the pointer element types ClusterResources uses, "copying the element"
// means copying the pointer value, not the object it points to: the clone
// and the original end up with distinct slices that happen to point at the
// same shared, read-only-by-contract objects. That sharing is intentional
// (see ClusterResources' doc comment) and is exactly what makes this cheap
// enough for per-generation publication — this function never deep-copies
// a Kubernetes object, and must not be changed to do so.
func cloneSlice[T any](in []T) []T {
	if in == nil {
		return nil
	}
	return append([]T(nil), in...)
}

func (r ClusterResources) clone() ClusterResources {
	return ClusterResources{
		Nodes:                    cloneSlice(r.Nodes),
		Pods:                     cloneSlice(r.Pods),
		Namespaces:               cloneSlice(r.Namespaces),
		PersistentVolumeClaims:   cloneSlice(r.PersistentVolumeClaims),
		PersistentVolumes:        cloneSlice(r.PersistentVolumes),
		Deployments:              cloneSlice(r.Deployments),
		StatefulSets:             cloneSlice(r.StatefulSets),
		ReplicaSets:              cloneSlice(r.ReplicaSets),
		Services:                 cloneSlice(r.Services),
		Ingresses:                cloneSlice(r.Ingresses),
		NetworkPolicies:          cloneSlice(r.NetworkPolicies),
		Jobs:                     cloneSlice(r.Jobs),
		CronJobs:                 cloneSlice(r.CronJobs),
		HorizontalPodAutoscalers: cloneSlice(r.HorizontalPodAutoscalers),
		EndpointSlices:           cloneSlice(r.EndpointSlices),
		PodWarningEvents:         cloneSlice(r.PodWarningEvents),
	}
}

// ClusterSnapshot is the immutable, published view of one cluster's
// observed Kubernetes state at a point in time — the boundary analyzers
// will eventually consume instead of a Kubernetes client (docs/08 §2.4).
//
// "Immutable" describes the collections, not the Kubernetes objects inside
// them. Every field is unexported and reachable only through
// value-returning methods, so a consumer holding a *ClusterSnapshot has no
// way to reach a mutable pointer into its internal state, its map, or its
// slice headers — the only producer is ClusterState.Publish, and the only
// fields it hands out are independent collection copies. The
// *corev1.Pod/*corev1.Node/etc. objects those collections point to are not
// copied and are not protected from mutation by the type system — they are
// read-only by the same contract client-go already places on informer
// cache objects (see ClusterResources' doc comment). This is why snapshots
// stay cheap enough for per-generation publication (docs/08 §7): the cost
// this package pays is O(objects) pointer copies per read, never
// O(object size) data copies.
type ClusterSnapshot struct {
	clusterID      string
	generation     uint64
	publishedAt    time.Time
	acquisition    AcquisitionState
	resources      ClusterResources
	resourceStates map[ResourceKind]ResourceState
}

// ClusterID identifies which cluster this snapshot belongs to.
func (s *ClusterSnapshot) ClusterID() string { return s.clusterID }

// Generation is monotonically increasing per cluster, starting at 1 for the
// first published snapshot. It identifies "the latest locally observed
// state across the participating resources at publication time" (docs/08
// §8) — it is never a claim of one atomic Kubernetes transaction, and it is
// not a proxy for elapsed time (docs/08 §2.6).
func (s *ClusterSnapshot) Generation() uint64 { return s.generation }

// PublishedAt is when this snapshot was produced, not when any individual
// resource within it was last observed by Kubernetes.
func (s *ClusterSnapshot) PublishedAt() time.Time { return s.publishedAt }

// Acquisition is the trustworthiness of this snapshot's evidence.
func (s *ClusterSnapshot) Acquisition() AcquisitionState { return s.acquisition }

// Trustworthy reports whether this snapshot's evidence may be used to
// advance absence-based incident lifecycle decisions. See
// AcquisitionState.Trustworthy.
func (s *ClusterSnapshot) Trustworthy() bool { return s.acquisition.Trustworthy() }

// Resources returns an independent copy of the resource collections
// observed as of this snapshot's generation: fresh slices the caller may
// append to, sort, or reassign elements of without affecting this
// snapshot, any other snapshot, or the live ClusterState that published
// it. The Kubernetes objects those slices point to are shared, not copied
// — callers must treat every *corev1.Pod/*corev1.Node/etc. reached through
// the result as read-only. See ClusterResources' doc comment.
func (s *ClusterSnapshot) Resources() ClusterResources {
	return s.resources.clone()
}

// ResourceState returns the acquisition metadata recorded for kind as of
// this snapshot's generation, and whether any was recorded at all.
func (s *ClusterSnapshot) ResourceState(kind ResourceKind) (ResourceState, bool) {
	state, ok := s.resourceStates[kind]
	return state, ok
}
