// Package clusterstate implements the shared cluster-state/snapshot
// contract from docs/08-Event-Driven-Cluster-State.md §6: the boundary
// between whatever acquires Kubernetes resources (today's direct client
// calls; Phase 3's SharedInformerFactory) and the analyzers that consume
// them.
//
//	acquisition layer
//	      ↓
//	mutable internal cluster state   (ClusterState)
//	      ↓
//	published immutable ClusterSnapshot
//	      ↓
//	analyzers
//
// This package defines that boundary and its immutability guarantees. It
// does not acquire Kubernetes resources itself, does not run informers or
// watches, and does not coordinate or schedule analyzer runs — those
// remain later phases' responsibility.
package clusterstate

import (
	"sync"
	"time"
)

// ClusterState is the mutable, single-owner accumulation point for one
// cluster's most recently observed Kubernetes resources and acquisition
// health. Exactly one ClusterState exists per cluster (docs/08 §13) — there
// is no shared state between instances, and callers must not share one
// ClusterState across clusters.
//
// The acquisition layer calls Update and SetAcquisitionState as it
// observes change; Publish converts the current state into an immutable
// ClusterSnapshot. The mutex exists because Phase 3's informer event
// handlers will call Update concurrently with Publish — nothing in Phase 2
// does so yet, but the type is safe for that from the start rather than
// retrofitted later.
type ClusterState struct {
	mu sync.Mutex

	clusterID      string
	generation     uint64
	acquisition    AcquisitionState
	resources      ClusterResources
	resourceStates map[ResourceKind]ResourceState

	// latest is the most recently published snapshot, and publications
	// signals that it changed — see Latest and Publications. Together they
	// are the entire generation-notification boundary a Coordinator needs:
	// one fixed channel, one cached value, no subscription registry.
	latest       *ClusterSnapshot
	publications chan struct{}
}

// NewClusterState creates the state owner for one cluster. A freshly
// created ClusterState has never acquired anything, so it starts STALE —
// the honest answer for "no trustworthy acquisition progress yet."
func NewClusterState(clusterID string) *ClusterState {
	return &ClusterState{
		clusterID:      clusterID,
		acquisition:    AcquisitionStale,
		resourceStates: make(map[ResourceKind]ResourceState),
		publications:   make(chan struct{}, 1),
	}
}

// ClusterID identifies which cluster this state belongs to.
func (s *ClusterState) ClusterID() string { return s.clusterID }

// Update replaces the observed resources for whichever kinds resources has
// populated, leaving every other kind exactly as it was (see
// ClusterResources' nil-means-unset convention, implemented by
// mergeClusterResources). This is what lets an informer event handler for
// one resource kind — the normal Phase 3 caller — update only that kind
// without clobbering the other 15.
//
// Update takes its own copy of the []*T slices so the caller's copy of
// resources is safe to reuse or discard immediately afterward — but it
// does not copy the Kubernetes objects the pointers reference; those are
// shared with the caller's originals and must already be treated as
// read-only by the time they reach Update, matching informer-cache objects
// exactly.
func (s *ClusterState) Update(resources ClusterResources) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources = mergeClusterResources(s.resources, resources)
}

// SetAcquisitionState records the cluster's current overall acquisition
// trustworthiness. It is independent of Update: acquisition health can
// change without new resources arriving (e.g. a freshness boundary
// crossed) or resources can change without acquisition health changing.
func (s *ClusterState) SetAcquisitionState(state AcquisitionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquisition = state
}

// SetResourceState records acquisition metadata for one resource kind.
func (s *ClusterState) SetResourceState(kind ResourceKind, state ResourceState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resourceStates[kind] = state
}

// Publish increments the generation counter and returns a new immutable
// snapshot of the currently observed resources and acquisition state. The
// returned ClusterSnapshot is never mutated afterward, by this type or by
// any caller with access only to its exported methods — see
// ClusterSnapshot's doc comment.
//
// Publish itself does not copy resource data: ClusterState never mutates
// resources in place (Update always replaces it wholesale with an
// already-private clone), so handing a snapshot the same slices is safe.
// The copy consumers actually need happens lazily, in
// ClusterSnapshot.Resources, only when someone asks for the data.
//
// Publish is called synchronously from informer event-handler goroutines
// (pkg/acquisition), so it must never block on a slow or absent consumer.
// After recording the snapshot as Latest, it signals publications without
// blocking — a full channel (the coordinator hasn't caught up to the
// previous signal yet) is dropped, not queued, which is safe precisely
// because the coordinator always re-reads Latest rather than expecting the
// channel to carry every generation.
func (s *ClusterState) Publish() *ClusterSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.generation++
	states := make(map[ResourceKind]ResourceState, len(s.resourceStates))
	for kind, state := range s.resourceStates {
		states[kind] = state
	}

	snapshot := &ClusterSnapshot{
		clusterID:      s.clusterID,
		generation:     s.generation,
		publishedAt:    time.Now(),
		acquisition:    s.acquisition,
		resources:      s.resources,
		resourceStates: states,
	}
	s.latest = snapshot

	select {
	case s.publications <- struct{}{}:
	default:
	}

	return snapshot
}

// Publications returns the channel Publish signals on every time it
// produces a new generation.
//
// This is a single-consumer publication signal for the per-cluster
// Coordinator (pkg/clusterstate/coordinator.go) — not a subscription API
// or event bus. There is exactly one channel per ClusterState, meant to be
// drained by exactly one reader; it supports no registration, no fan-out,
// and no per-subscriber delivery. A signal carries no payload: it only
// ever means "call Latest," never "here is the generation," because a
// burst of Publish calls collapses into a single buffered signal (see
// Publish). Multiple concurrent readers would race over which one
// receives a given signal and are not a supported use of this channel.
func (s *ClusterState) Publications() <-chan struct{} {
	return s.publications
}

// Latest returns the most recently published snapshot, or nil if Publish
// has never been called. Unlike Publish, this never advances the
// generation counter or signals publications — it is a pure read, the
// correct way for the coordinator to catch up to the current generation
// after waking on Publications without itself acting as a second
// publisher.
func (s *ClusterState) Latest() *ClusterSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}
