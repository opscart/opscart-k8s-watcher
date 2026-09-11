# Event-Driven Cluster State Architecture

**Status:** FROZEN  
**Branch:** `feature/event-driven-cluster-state`  
**Current phase:** Phase 0 — Architecture Audit and Baseline  
**Target duration:** 13–20 focused engineering days  
**Primary goal:** Replace repeated Kubernetes polling with informer-driven shared cluster state while preserving OpsCart correctness, temporal semantics, and read-only behavior.

---

## 1. Purpose

OpsCart currently performs repeated Kubernetes API reads from multiple analyzers during each scan cycle.

As the product grows, this creates several problems:

- repeated LIST calls for the same Kubernetes resources
- increasing API-server load as analyzers are added
- inconsistent analyzer views because different analyzers may observe the cluster at slightly different times
- unnecessary scan latency dominated by API acquisition
- coupling between analyzers and Kubernetes clients
- difficulty moving toward lower-latency event-driven analysis safely

The target architecture replaces repeated analyzer-owned acquisition with a shared informer-backed cluster state.

The normal operating model becomes:

```text
Kubernetes API
      ↓
SharedInformerFactory
      ↓
Informer caches
      ↓
OpsCart Cluster State
      ↓
Immutable published generation
      ↓
Coalescing coordinator
      ↓
All analyzers
      ↓
Published analysis state
      ↓
Dashboard / Store / Incidents
```

This is an event-driven architecture with deterministic reconciliation.

---

## 2. Architectural principles

The following principles are frozen for this implementation.

### 2.1 One acquisition owner

Each Kubernetes resource is acquired once by the shared acquisition layer.

Do not create separate watchers for:

- Cost
- Waste
- Security
- Node Optimization
- War Room
- Node Health
- individual dashboard pages

The acquisition layer owns Kubernetes state.

Analyzers consume snapshots.

### 2.2 Client-go owns LIST/WATCH lifecycle

OpsCart will use Kubernetes `SharedInformerFactory`.

OpsCart will not reimplement:

- watch reconnect
- resourceVersion continuation
- `410 Gone` recovery
- reflector retry behavior
- informer relisting
- watch lifecycle management

Those responsibilities remain with client-go.

OpsCart adds the product-level state above the informer cache:

- acquisition health
- generation number
- freshness
- per-resource synchronization state
- immutable published snapshots
- analysis coordination

### 2.3 Event-driven acquisition

Normal Kubernetes acquisition is:

```text
initial LIST
    ↓
continuous WATCH
    ↓
informer cache
```

There is no periodic full Kubernetes scan during healthy operation.

Relisting happens only as part of client-go informer recovery/reconciliation.

### 2.4 Snapshot-only analyzers

Target invariant:

> An analyzer receives a snapshot. It does not fetch Kubernetes state.

New analyzer code must not add direct Kubernetes acquisition.

Existing analyzers will be migrated incrementally.

### 2.5 All analyzers run for each published generation

Initial implementation deliberately avoids selective dependency routing.

```text
Kubernetes changes
      ↓
coalescing window
      ↓
publish generation N
      ↓
run all analyzers
```

Do not initially implement:

- dependency graph
- dirty-analyzer tracking
- change journal
- resource-to-analyzer routing

These may be reconsidered only after runtime measurements show that full recomputation is too expensive.

Correctness and simplicity take priority over micro-optimization.

### 2.6 Kubernetes generation is not time

These concepts are separate:

```text
Cluster generation
Analysis execution
Elapsed wall-clock time
```

A burst of Kubernetes events may produce several generations within seconds.

A generation must never be treated as equivalent to a scan interval or elapsed duration.

---

## 3. Critical temporal invariant

OpsCart incident resolution currently contains scan-count-based semantics.

Historically:

```text
resolveThreshold = 3 scans
scan interval ≈ 60 seconds
```

This silently approximates:

```text
absence for ≈ 3 minutes
```

Under event-driven generations this becomes unsafe.

For example:

```text
generation 100 at 10:00:00
generation 101 at 10:00:02
generation 102 at 10:00:04
```

Three generations are not three minutes.

### Required migration

Incident resolution must be converted to wall-clock semantics before event-driven analysis becomes authoritative.

Target meaning:

```text
Issue observed
      ↓
Issue becomes absent in trustworthy evidence
      ↓
Absence duration begins
      ↓
Issue remains absent for configured duration
      ↓
RESOLVED may be emitted
```

The existing effective behavior should initially remain approximately three minutes unless product requirements explicitly change it.

Example:

```go
resolveAfter = 3 * time.Minute
```

Exact implementation may differ, but semantics must be wall-clock based.

---

## 4. Acquisition-health invariant

Incident resolution must depend on trustworthy acquisition state.

Frozen rule:

```text
Issue present in trustworthy snapshot
→ incident remains active

Issue absent in trustworthy snapshot
→ absence/resolution may advance

Issue absent while acquisition degraded, stale, or unknown
→ resolution must NOT advance
```

OpsCart must never resolve an incident merely because evidence disappeared while acquisition was unhealthy.

Acquisition health is therefore a first-class part of the cluster-state contract.

---

## 5. Acquisition states

The exact type may evolve during Phase 2, but the semantic states are:

```text
HEALTHY
DEGRADED
RESYNCING
STALE
```

Possible meanings:

### HEALTHY

- required informers have synchronized
- published state is current enough to analyze
- evidence may advance incident lifecycle

### DEGRADED

- one or more required resource streams are unhealthy or incomplete
- analysis may continue conservatively
- absence must not be treated as authoritative resolution evidence

### RESYNCING

- acquisition is rebuilding state after startup or informer recovery
- previous published state may remain visible
- new absence must not advance incident resolution

### STALE

- no trustworthy acquisition progress within the configured freshness boundary
- evidence must not be treated as current

Do not infer healthy acquisition solely from one boolean without defining what it means.

---

## 6. Cluster state contract

The exact structure must be designed from the Phase 0 analyzer audit.

Do not create a giant struct based only on speculation.

Conceptually, a published snapshot contains:

```go
type ClusterSnapshot struct {
    ClusterID   string
    Generation  uint64
    PublishedAt time.Time

    Acquisition AcquisitionState

    // Resource snapshots required by migrated analyzers.
}
```

Likely resources include:

```text
Nodes
Pods
Namespaces
PVCs
PVs
Deployments
StatefulSets
DaemonSets
Services
Jobs
CronJobs
ReplicaSets
Events
StorageClasses
```

The final resource set must come from actual analyzer usage.

---

## 7. Snapshot immutability

Informer-cache objects are treated as immutable.

Consumers must never mutate objects returned from informer caches or listers.

Published snapshots should avoid deep-copying every Kubernetes object on every generation.

Preferred pattern:

```text
Informer cache objects
      ↓
new immutable slice/map view
      ↓
published generation
```

The containing collection for an already-published generation must not be mutated or reused in a way that changes the snapshot.

New generation:

```text
new collection
same immutable object pointers where unchanged
```

Do not deep-copy thousands of Pods per generation unless correctness requires it.

---

## 8. Multi-resource consistency

Kubernetes does not provide one globally atomic resourceVersion across unrelated resource kinds.

Therefore OpsCart must not claim that a snapshot represents one API-server transaction.

A generation represents:

> the latest locally observed, synchronized state across the participating informer caches at publication time.

Where useful, per-resource metadata should record:

```text
synced
freshness
resourceVersion or equivalent observation metadata
```

The state must be immutable after publication even though different informers may have advanced at slightly different times.

---

## 9. Coordinator

The coordinator is intentionally simple.

Target behavior:

```text
informer events
      ↓
mark cluster state changed
      ↓
short coalescing window
      ↓
publish generation
      ↓
run all analyzers
      ↓
publish analysis generation
```

### Requirements

- coalesce bursts of events
- avoid one analyzer run per Kubernetes event
- preserve deterministic execution
- prevent overlapping conflicting analysis runs
- associate analysis results with the generation they analyzed
- support multiple clusters independently

### Non-goals

Do not add:

- dependency routing
- priority queues
- event journals
- analyzer DAGs
- partial invalidation

until measurements justify them.

---

## 10. Clock-driven behavior

Event-driven Kubernetes acquisition does not eliminate all timers.

Some OpsCart semantics depend on time passing without Kubernetes object changes.

Examples:

- incident resolution duration
- flap absorption
- finding age
- stale-resource thresholds
- evidence freshness
- pricing TTL expiration
- suppression expiration

Clock-driven evaluation may run against the existing shared snapshot.

It must not cause a full Kubernetes LIST.

Therefore:

```text
Kubernetes change
→ event-driven analysis

Time threshold crossed
→ clock-driven analysis

Informer recovery
→ client-go reconciliation
```

This is intentional and is not considered return to polling.

---

## 11. External data

External provider data such as cloud pricing is not Kubernetes watch state.

It remains independently refreshed based on its own TTL/cache rules.

External refresh must not be confused with Kubernetes acquisition generations.

Where analysis combines Kubernetes and external evidence, that evidence should retain independent freshness information.

---

## 12. Coding standards

These rules apply to this architecture migration.

### 12.1 Naming

Functions and types must use meaningful, domain-specific names.

Prefer:

```go
BuildSnapshot()
PublishGeneration()
RunAnalysis()
CanResolve()
RecordAbsence()
```

Avoid sentence-sized names that repeat package/type context unnecessarily.

Example to avoid:

```go
BuildClusterStateSnapshotFromSharedInformerFactoryWithAcquisitionEvidence()
```

Names must be clear without becoming excessively long.

### 12.2 Function responsibility

Each function should have one understandable responsibility.

Large orchestration functions should be decomposed around domain boundaries.

Do not split trivial logic purely to reduce line counts.

### 12.3 File responsibility

Each production file should contain one coherent responsibility.

Avoid:

- 1,500–2,000+ line production files
- unrelated features mixed into one file
- generic dumping-ground files

As guidance, consider splitting production files around 500–700 lines when natural responsibility boundaries exist.

This is guidance, not an arbitrary mechanical limit.

### 12.4 Test organization

Do not allow test files to grow indefinitely.

Tests should be split by meaningful responsibility.

Examples:

```text
cluster_state_test.go
acquisition_test.go
coordinator_test.go
incident_resolution_test.go
```

Avoid one 2,000–3,000 line test file covering the entire subsystem.

Rough guidance:

- around 700–1,000 lines should trigger a review for a meaningful split
- do not create tiny test files unnecessarily

### 12.5 No generic helper proliferation

Do not introduce vague packages/files such as:

```text
utils
helpers
common
misc
```

Shared code must have a real domain responsibility.

### 12.6 No silent fallback

Unknown or incomplete evidence remains explicit.

Never convert:

```text
unknown → zero
unknown → healthy
unknown → absent
```

without an explicit documented rule.

### 12.7 Determinism

Published state and analysis results must remain deterministic where ordering is observable.

Tests should cover ordering-sensitive behavior where appropriate.

### 12.8 Scope discipline

Each implementation phase should make only changes necessary for that phase.

Do not perform unrelated cleanup while touching acquisition/analyzer code.

---

## 13. Multi-cluster behavior

OpsCart currently supports multiple clusters.

The new architecture must preserve cluster isolation.

Each cluster should conceptually own:

```text
InformerFactory
ClusterState
Generation counter
Coordinator
Published analysis state
Acquisition health
```

A high event rate or resync in one cluster must not corrupt another cluster's state.

Cross-cluster shared mutable state should be avoided unless explicitly designed.

---

## 14. Resource ownership model

Target ownership:

```text
ClusterRuntime
  ├── Informer factory
  ├── Cluster state
  ├── Coordinator
  └── Published analysis
```

Exact naming is not frozen.

Ownership semantics are frozen:

- one acquisition owner per cluster
- analyzers do not own clients/watchers
- dashboard handlers do not acquire Kubernetes state
- store does not acquire Kubernetes state

---

## 15. Migration strategy

Do not switch every analyzer and acquisition path at once.

Migration proceeds behind the snapshot contract.

During migration, temporary coexistence between legacy and new analyzer paths is allowed only when necessary to keep the system working.

However, do not create a permanent architecture where:

```text
informers update shared state
AND
old full scan still performs all Kubernetes LISTs every minute
```

The target is removal of the old polling acquisition path.

---

# Implementation Plan

## Phase 0 — Architecture audit and baseline

**Duration:** 1–2 focused engineering days

### Goals

Understand exactly what OpsCart currently acquires and what each analyzer consumes.

Do not begin informer migration before completing this audit.

### Work

Inventory every Kubernetes API acquisition call.

Catalog:

```text
resource
package/file
function
List/Get/Watch
namespace scope
frequency
consumer/analyzer
duplicate acquisition
```

Audit analyzer field usage by resource.

For each analyzer record:

```text
Analyzer
Required resources
Fields consumed
Time-dependent behavior
External dependencies
Current Kubernetes client ownership
```

Measure current runtime baseline.

At minimum:

- full scan duration
- Kubernetes API action count per scan
- LIST count by resource
- duplicate LIST count
- process memory / heap baseline
- relevant object counts
- analyzer execution time if measurable without invasive changes

Use representative environments where practical:

```text
minikube
real/large cluster evidence if safely available
```

### Deliverables

- acquisition inventory
- analyzer/resource matrix
- current performance baseline
- final proposed `ClusterSnapshot` resource contents
- no production architecture migration yet

### Exit criteria

Phase 0 completes only when we can answer:

1. Which Kubernetes resources are currently fetched?
2. Which are fetched multiple times?
3. Which analyzers need each resource?
4. What is the current API-call baseline?
5. What is the current scan/runtime baseline?
6. What resources belong in the shared state?

---

## Phase 1 — Time-based incident resolution

**Duration:** 1–2 focused engineering days

### Goal

Remove scan-count-based incident resolution semantics before event-driven generations are introduced.

### Work

Replace scan-count resolution threshold with elapsed-time semantics.

Preserve the current effective behavior initially.

Expected meaning:

```text
continuous trustworthy absence for approximately 3 minutes
→ may resolve
```

Integrate acquisition-trust semantics where possible without requiring the full informer architecture.

### Required tests

- many generations/scans within less than resolve duration do not resolve
- absence crossing the duration resolves
- issue reappearing resets/updates absence behavior correctly
- flap absorption remains time-based
- degraded/untrusted evidence does not advance resolution
- restart/store persistence behavior remains correct

### Exit criteria

No incident lifecycle behavior depends on generation count as a proxy for elapsed time.

---

## Phase 2 — Cluster state contract

**Duration:** 2–3 focused engineering days

### Goal

Create the shared state and immutable generation contract without changing acquisition mechanism yet.

### Work

Define:

- cluster state ownership
- snapshot type
- generation metadata
- acquisition-health type
- per-resource metadata where required
- immutable publication semantics

Use current acquisition to populate snapshots temporarily if necessary.

Do not add informer acquisition in the same initial slice unless the contract is already independently validated.

### Required tests

- snapshot immutability
- generation monotonicity
- cluster isolation
- no mutation through consumers
- deterministic snapshot views
- health metadata propagation

### Exit criteria

At least one analyzer can consume the new snapshot contract without performing Kubernetes acquisition.

---

## Phase 3 — Shared informer acquisition

**Duration:** 3–4 focused engineering days

### Goal

Replace repeated Kubernetes acquisition with one informer-owned state source per cluster.

### Work

Create and own one `SharedInformerFactory` per cluster.

Add informers only for resources identified in Phase 0.

Wait for initial sync before declaring acquisition healthy.

Populate shared cluster state from informer caches.

Publish generations when observed state changes.

Do not implement custom reconnect/relist logic.

### Required tests

- initial synchronization
- add/update/delete propagation
- generation advancement
- reconnect/resync behavior as observable through client-go test mechanisms
- acquisition-health transitions
- multi-cluster isolation
- race tests

### Exit criteria

Shared state receives Kubernetes changes through informers and can publish trustworthy immutable generations.

---

## Phase 4 — Coordinator and analyzer migration

**Duration:** 4–6 focused engineering days

### Goal

Make shared state the authoritative input for analyzers.

### Work

Introduce the coalescing coordinator.

Migrate analyzers incrementally to snapshot-only input.

Initial rule:

```text
one published generation
→ one coalesced full analyzer pass
```

Remove direct Kubernetes reads from migrated analyzers.

Do not add dependency routing.

### Suggested migration order

Use Phase 0 audit to finalize order.

Prefer lower-risk/read-heavy analyzers first.

Potential progression:

```text
Node Health
Cost
Waste & Drift
Security
Node Optimization
War Room / incident-producing analysis
```

This order is not frozen until Phase 0 confirms dependencies.

### Required tests

- analyzers produce equivalent results from snapshot data
- no direct Kubernetes call from migrated analyzer
- one event burst produces one coalesced analysis pass
- no overlapping stale publication
- generation identity preserved into analysis
- race detector clean

### Exit criteria

All targeted analyzers operate from shared snapshots.

---

## Phase 5 — Remove polling and prove the architecture

**Duration:** 2–3 focused engineering days

### Goal

Remove the legacy full Kubernetes polling path and prove the new architecture improves acquisition behavior without correctness regression.

### Work

Disable/remove recurring full-scan Kubernetes acquisition.

Retain intentional clock-driven analysis only.

Run:

- regression tests
- race tests
- soak tests
- watch disruption/resync tests
- multi-cluster tests

Measure against Phase 0 baseline:

```text
Before vs After
```

Compare:

- Kubernetes LIST calls
- API actions per minute
- scan/analysis latency
- process memory
- CPU
- analyzer runtime
- freshness latency
- incident lifecycle correctness

### Success target

Healthy steady-state Kubernetes acquisition should no longer perform repeated full LIST polling.

API load should primarily consist of watch traffic and client-go recovery behavior.

### Exit criteria

- old polling acquisition removed
- event-driven acquisition authoritative
- baseline comparison documented
- incident semantics validated
- no known stale-analysis correctness bug
- race detector clean

---

# Deferred Work

The following are explicitly outside this architecture release unless measurements or correctness force reconsideration:

- dependency router
- analyzer DAG
- dirty-resource tracking
- change journal
- selective analyzer execution
- custom watch reconnect implementation
- custom resource-version recovery
- broad scheduler enhancements
- Node Optimization execution actions
- PDB/drain automation
- unrelated dashboard redesign
- unrelated analyzer cleanup

---

# Measurement Gate for Future Dependency Routing

After Phase 5, measure full-analysis recomputation cost.

Only consider selective dependency routing if one or more are demonstrated:

- analysis CPU materially impacts OpsCart
- large clusters cannot meet acceptable freshness
- high event rate creates sustained analysis backlog
- memory or latency measurements show full recomputation is not viable

If full analysis remains cheap, keep the simpler architecture.

---

# Architecture Change Control

This document is frozen for the duration of the migration.

The architecture may change only when one of these occurs:

1. implementation discovers a correctness blocker
2. actual client-go behavior contradicts a documented assumption
3. measurement demonstrates that a frozen design cannot meet the objective

A preferable design idea alone is not sufficient reason to change the architecture.

Any architecture change must document:

```text
Failed assumption
Evidence
Smallest required change
Correctness impact
Timeline impact
```

Do not redesign opportunistically during implementation.

---

# Timeline

Expected focused engineering effort:

| Phase | Work | Estimate |
|---|---|---:|
| 0 | Audit + baseline | 1–2 days |
| 1 | Incident temporal semantics | 1–2 days |
| 2 | Cluster state contract | 2–3 days |
| 3 | Shared informer acquisition | 3–4 days |
| 4 | Coordinator + analyzer migration | 4–6 days |
| 5 | Remove polling + validation | 2–3 days |
| **Total** | | **13–20 days** |

Allow up to one additional calendar week for unexpected informer/concurrency issues.

The target should remain approximately three focused engineering weeks, with a fourth week only as contingency.

---

# Progress

| Phase | Status |
|---|---|
| Phase 0 — Audit + baseline | NOT STARTED |
| Phase 1 — Time-based incident resolution | NOT STARTED |
| Phase 2 — Cluster state contract | NOT STARTED |
| Phase 3 — Shared informer acquisition | NOT STARTED |
| Phase 4 — Coordinator + analyzer migration | NOT STARTED |
| Phase 5 — Remove polling + validation | NOT STARTED |

Update only this progress table as phases advance.

Do not continuously rewrite the architecture sections during implementation.

---

# Definition of Done

The migration is complete when:

- Kubernetes resources are acquired through shared informer ownership
- analyzers no longer independently poll Kubernetes
- healthy steady state does not perform recurring full LIST scans
- published snapshots are immutable and generation-addressable
- acquisition health is first-class
- incident resolution is wall-clock based
- degraded acquisition cannot falsely resolve incidents
- all analyzers operate correctly against shared snapshots
- coalesced full recomputation is measured and acceptable
- multi-cluster behavior remains isolated
- race tests pass
- before/after API-load and runtime measurements are documented
- no dependency router is added unless measurements justify it
