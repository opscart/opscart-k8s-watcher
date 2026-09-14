package clusterstate

import "time"

// AcquisitionState is the trustworthiness of a cluster's most recently
// observed Kubernetes state, per docs/08 §5. The concrete transition logic
// (when acquisition moves between these states) belongs to the informer
// layer built in Phase 3; this type only defines the vocabulary and the
// question every consumer actually needs answered.
type AcquisitionState string

const (
	// AcquisitionHealthy: required resource streams have synchronized and
	// published state is current enough to analyze. Evidence may advance
	// incident lifecycle decisions.
	AcquisitionHealthy AcquisitionState = "HEALTHY"
	// AcquisitionDegraded: one or more required resource streams are
	// unhealthy or incomplete. Analysis may continue conservatively, but
	// absence must not be treated as authoritative resolution evidence.
	AcquisitionDegraded AcquisitionState = "DEGRADED"
	// AcquisitionResyncing: acquisition is rebuilding state after startup
	// or informer recovery. Previous published state may remain visible;
	// new absence must not advance incident resolution.
	AcquisitionResyncing AcquisitionState = "RESYNCING"
	// AcquisitionStale: no trustworthy acquisition progress within the
	// configured freshness boundary. Evidence must not be treated as
	// current.
	AcquisitionStale AcquisitionState = "STALE"
)

// Trustworthy reports whether evidence observed under this acquisition
// state may be used to advance absence-based incident lifecycle decisions
// (docs/08 §4). Only HEALTHY is trustworthy — DEGRADED, RESYNCING, and
// STALE must never let absence advance resolution, even though analysis
// may still run conservatively against them. Phase 2 defines this query;
// wiring it into pkg/store's resolution path is a later phase's job.
func (a AcquisitionState) Trustworthy() bool {
	return a == AcquisitionHealthy
}

// ResourceKind identifies one of the Kubernetes resource collections
// tracked by ClusterResources — the Phase 0 final resource set.
type ResourceKind string

const (
	ResourceNodes                    ResourceKind = "Nodes"
	ResourcePods                     ResourceKind = "Pods"
	ResourceNamespaces               ResourceKind = "Namespaces"
	ResourcePersistentVolumeClaims   ResourceKind = "PersistentVolumeClaims"
	ResourcePersistentVolumes        ResourceKind = "PersistentVolumes"
	ResourceDeployments              ResourceKind = "Deployments"
	ResourceStatefulSets             ResourceKind = "StatefulSets"
	ResourceReplicaSets              ResourceKind = "ReplicaSets"
	ResourceServices                 ResourceKind = "Services"
	ResourceIngresses                ResourceKind = "Ingresses"
	ResourceNetworkPolicies          ResourceKind = "NetworkPolicies"
	ResourceJobs                     ResourceKind = "Jobs"
	ResourceCronJobs                 ResourceKind = "CronJobs"
	ResourceHorizontalPodAutoscalers ResourceKind = "HorizontalPodAutoscalers"
	ResourceEndpointSlices           ResourceKind = "EndpointSlices"
	ResourcePodWarningEvents         ResourceKind = "PodWarningEvents"
)

// ResourceState is per-resource-kind acquisition metadata. Every field has
// a concrete Phase 3 use: Synced/ObservedAt let the acquisition layer
// answer "is this kind's cache initialized and how fresh is it," and
// ResourceVersion supports informer resync/continuation bookkeeping.
// Phase 2 only defines the shape — nothing yet computes these from a real
// informer, so callers populate what they know and leave the rest zero.
type ResourceState struct {
	Synced          bool
	ObservedAt      time.Time
	ResourceVersion string
}
