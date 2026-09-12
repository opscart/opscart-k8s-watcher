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
}

// NewClusterState creates the state owner for one cluster. A freshly
// created ClusterState has never acquired anything, so it starts STALE —
// the honest answer for "no trustworthy acquisition progress yet."
func NewClusterState(clusterID string) *ClusterState {
	return &ClusterState{
		clusterID:      clusterID,
		acquisition:    AcquisitionStale,
		resourceStates: make(map[ResourceKind]ResourceState),
	}
}

// ClusterID identifies which cluster this state belongs to.
func (s *ClusterState) ClusterID() string { return s.clusterID }

// Update replaces the observed resources for whichever kinds resources has
// populated (see ClusterResources' nil-means-unset convention). It takes
// its own copy of the []*T slices so the caller's copy of resources is
// safe to reuse or discard immediately afterward — but it does not copy
// the Kubernetes objects the pointers reference; those are shared with the
// caller's originals and must already be treated as read-only by the time
// they reach Update, matching Phase 3's informer-cache objects exactly.
// Phase 2 only supports full-batch replacement, matching how every current
// acquisition call site works today (a one-shot List per scan); per-kind
// incremental updates are a Phase 3 concern once informers exist to drive
// them, deliberately not invented here ahead of that need.
func (s *ClusterState) Update(resources ClusterResources) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources = resources.clone()
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
func (s *ClusterState) Publish() *ClusterSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.generation++
	states := make(map[ResourceKind]ResourceState, len(s.resourceStates))
	for kind, state := range s.resourceStates {
		states[kind] = state
	}

	return &ClusterSnapshot{
		clusterID:      s.clusterID,
		generation:     s.generation,
		publishedAt:    time.Now(),
		acquisition:    s.acquisition,
		resources:      s.resources,
		resourceStates: states,
	}
}
