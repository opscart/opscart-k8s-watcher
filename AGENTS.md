# OpsCart Agent Engineering Guide

This file defines repository-wide engineering, implementation, testing, and review practices for coding agents working on OpsCart.

Read this file before modifying code.

For architecture-specific work, also read the relevant design document under `docs/`.

When a task references a frozen architecture or implementation phase, that document is authoritative for the scope of the task.

---

## 1. Core Engineering Principles

- Preserve correctness first.
- Prefer simple, explicit, maintainable code over clever abstractions.
- Do not guess when repository behavior can be inspected.
- Do not make unrelated refactors while implementing a scoped task.
- Treat unknown, degraded, unsupported, or incomplete state explicitly.
- Preserve determinism where ordering or state transitions are observable.
- Do not silently change product semantics while changing architecture.
- Prefer evidence and measurements over assumptions.
- Do not introduce speculative infrastructure, resources, or abstractions without a real current consumer.
- Keep implementation scope aligned with the active phase or task.

---

## 2. Architecture Discipline

Before implementation:

1. Read this file.
2. Read the architecture/design document referenced by the task.
3. Inspect the relevant existing code before proposing changes.
4. Understand the current ownership boundaries and data flow.
5. Confirm the requested change belongs to the current phase.

If an architecture document is marked **FROZEN**, do not redesign it during implementation unless one of these conditions is met:

1. A correctness blocker is discovered.
2. Repository or dependency behavior disproves a documented assumption.
3. Measurements show the frozen design cannot meet its stated objective.

If an architecture change is required, stop and report:

- failed assumption
- supporting evidence
- smallest required change
- correctness impact
- timeline impact

A preferable design idea by itself is not enough reason to change frozen architecture.

---

## 3. Scope Discipline

Do not:

- perform unrelated cleanup
- rename unrelated functions or files
- reorganize packages without a task-driven reason
- add speculative features
- add generic frameworks for a narrow problem
- mix multiple architecture phases into one implementation slice
- fix unrelated bugs unless they block the requested work
- silently expand the task

If an unrelated issue is discovered, report it separately.

---

## 4. Naming

Use concise, meaningful, domain-specific names.

Names should communicate responsibility without becoming sentence-sized.

Prefer names such as:

```go
BuildSnapshot()
Publish()
Start()
Stop()
WaitForSync()
CanResolve()
MarkAbsent()
RunAnalysis()
```

Avoid names such as:

```go
BuildClusterStateSnapshotFromSharedInformerFactoryWithAcquisitionEvidence()
DetermineWhetherIncidentCanBeResolvedAfterConfiguredAbsenceDuration()
```

Guidelines:

- Do not repeat package context unnecessarily.
- Do not repeat type context unnecessarily.
- Prefer established domain terms already used in the repository.
- Avoid vague names such as `DoWork`, `HandleThing`, `ProcessData`, or `Manager` unless the domain meaning is genuinely clear.
- Avoid abbreviations that are not already conventional in the codebase.
- Keep exported names precise because they become part of the package contract.

---

## 5. Function Design

Each function should have one clear responsibility.

Prefer small, cohesive functions over large orchestration blocks.

However:

- do not create tiny helpers for trivial expressions
- do not split code purely to satisfy a line-count target
- do not hide straightforward logic behind unnecessary indirection

A function should be reviewed for decomposition when it:

- mixes multiple domain responsibilities
- performs acquisition, transformation, persistence, and publication together
- contains deeply nested control flow
- becomes difficult to name clearly
- requires extensive comments to explain what it does
- becomes difficult to test independently

Avoid 100+ line functions when natural responsibility boundaries exist.

---

## 6. File Organization

Each production file should contain one coherent responsibility.

Good file names describe the domain responsibility, for example:

```text
cluster_state.go
snapshot.go
acquisition.go
incident_resolution.go
informer_runtime.go
resource_informers.go
event_filter.go
```

Avoid generic dumping-ground files such as:

```text
helpers.go
utils.go
common.go
misc.go
stuff.go
manager.go
```

unless the repository already has a very specific established meaning for that name.

### File size guidance

Line count is a review signal, not a mechanical rule.

- Around 500–700 lines: review whether the file now contains multiple responsibilities.
- 1,000+ lines: strong signal that the file should be split if a meaningful domain boundary exists.
- 1,500–2,000+ line production files should not be created or materially expanded.

Do not split a cohesive file into many tiny files solely to reduce line count.

Prefer responsibility-based separation.

When modifying an already-large legacy file:

- avoid making it materially worse
- move new distinct responsibility into a focused file when appropriate
- do not turn the current task into a broad unrelated refactor

---

## 7. Package Placement

Put code where the responsibility naturally belongs.

Before creating a new package:

- inspect existing package boundaries
- check whether an existing package already owns the responsibility
- avoid creating a package for a single trivial type or helper
- avoid generic packages such as `utils`, `common`, or `helpers`

A new package should represent a meaningful architectural or domain boundary.

---

## 8. Test Organization

Tests should be organized by behavior and responsibility.

Prefer focused files such as:

```text
incident_resolution_test.go
cluster_state_test.go
acquisition_test.go
coordinator_test.go
event_filter_test.go
```

Avoid one giant test file covering an entire subsystem.

### Test file size guidance

- Around 700–1,000 lines: review for a meaningful split.
- 2,000+ line test files should not continue growing when distinct behaviors can be separated cleanly.

Do not create tiny test files unnecessarily.

### Test quality

Tests should verify behavior and architectural invariants, not incidental implementation details.

Prefer deterministic tests.

Avoid long real-time sleeps.

For time-dependent logic:

- use a narrow time-control seam
- use persisted timestamps directly in tests when appropriate
- do not introduce a broad clock framework unless the design genuinely requires one

For concurrent code:

- add race coverage
- test deterministic invariants rather than thread scheduling
- avoid flaky timing-sensitive tests

---

## 9. Error and Evidence Handling

Unknown or incomplete evidence must remain explicit.

Never silently convert:

```text
unknown → zero
unknown → healthy
unknown → absent
unknown → resolved
```

Do not fabricate values to keep a pipeline moving.

Prefer explicit degraded, unavailable, partial, or unsupported state where applicable.

Do not suppress errors without a documented reason.

If fallback behavior exists:

- make it explicit
- preserve evidence that fallback occurred
- do not present fallback output as exact if it is not exact

---

## 10. Kubernetes Engineering Rules

### 10.1 Shared acquisition

Prefer shared Kubernetes acquisition over repeated analyzer-owned reads.

Do not add analyzer-specific watchers or duplicate LIST paths when a shared acquisition layer exists.

### 10.2 Informer ownership

When using client-go informers:

- let client-go manage LIST/WATCH lifecycle
- do not reimplement reconnect logic
- do not reimplement resourceVersion recovery
- do not reimplement `410 Gone` handling
- do not build custom relist loops unless a documented requirement proves client-go is insufficient

### 10.3 Informer object mutability

Objects returned from informer caches and listers are read-only by contract.

Do not mutate informer-owned Kubernetes objects.

Snapshots may share object pointers intentionally.

Protect collection structure separately from object identity.

Do not deep-copy entire Kubernetes object graphs merely to claim immutability unless correctness requires it.

### 10.4 Resource versions

Do not invent a global Kubernetes resourceVersion across unrelated resource kinds.

Do not fabricate resourceVersion metadata if the API or informer does not expose it reliably.

### 10.5 Acquisition health

Acquisition health must be explicit when architecture requires it.

Do not infer healthy state merely because a function returned no error.

Do not advance lifecycle decisions from degraded, stale, resyncing, or otherwise untrustworthy evidence when the architecture forbids it.

### 10.6 Read-only product behavior

Do not introduce mutating Kubernetes operations unless the task explicitly requires a write-capable feature.

---

## 11. Event-Driven Architecture Rules

For the event-driven cluster-state migration:

- one acquisition owner per cluster
- analyzers consume snapshots rather than Kubernetes clients
- one shared state boundary per cluster
- cluster generations are not elapsed time
- analyzer execution count is not elapsed time
- wall-clock temporal semantics must remain wall-clock based
- do not add dependency routing before measurements justify it
- do not add change journals before measurements justify them
- do not add analyzer DAGs before measurements justify them
- use coalescing rather than one analysis pass per Kubernetes event
- preserve multi-cluster isolation

When the active architecture document defines stricter rules, follow that document.

---

## 12. Incident and Temporal Semantics

Do not use scan count, generation count, or analyzer execution count as a substitute for elapsed time unless explicitly required by product semantics.

When a lifecycle rule is time-based:

- persist the correct temporal anchor when restart survival matters
- distinguish last positive observation from first absence
- do not allow rapid event bursts to accelerate time-based transitions
- preserve flap/debounce semantics independently of generation frequency

Do not silently change incident resolution behavior as a side effect of architecture work.

---

## 13. Persistence and Schema Changes

Before changing persistence:

- inspect the existing schema versioning strategy
- inspect existing migration helpers
- preserve compatibility with existing databases
- use the established migration pattern
- do not invent a second migration mechanism

For schema changes:

- prefer nullable/additive changes where appropriate
- preserve existing data
- do not overload unrelated columns to avoid a migration
- add migration coverage when existing generic migration tests do not already prove compatibility

Do not drop legacy columns during unrelated work unless there is a clear need.

---

## 14. Concurrency

Concurrent code must have explicit ownership and synchronization.

Avoid shared mutable global state.

Prefer per-cluster ownership for cluster-specific runtime state.

When adding synchronization:

- keep locking scope clear
- avoid holding locks across slow external calls
- avoid nested lock ordering where possible
- document non-obvious synchronization assumptions

Run race tests for affected concurrent packages where practical.

---

## 15. Performance Discipline

Do not optimize unmeasured problems unless the architecture explicitly requires the optimization.

Prefer:

1. baseline
2. change
3. after-measurement
4. only then further optimization

Do not add complexity such as dependency routing, caching layers, or selective invalidation without evidence that simpler behavior is insufficient.

Avoid performance regressions created by defensive deep-copying or repeated acquisition.

---

## 16. External API and Cache Behavior

External provider data is separate from Kubernetes acquisition.

Examples include cloud pricing APIs.

Keep independent freshness and TTL semantics.

Do not tie external API refresh frequency directly to Kubernetes event frequency unless explicitly designed.

Caches intended to persist across analysis generations must have a lifetime long enough to do so.

---

## 17. Comments and Documentation

Comments should explain:

- why something exists
- non-obvious invariants
- correctness constraints
- architectural ownership
- unusual edge cases

Do not write comments that merely restate the code.

When behavior changes, update stale comments in the same scope.

Do not knowingly leave comments describing removed behavior.

Architecture documents should remain concise and authoritative.

Do not create duplicate design documents for the same architecture.

---

## 18. Generic Abstractions

Avoid abstraction for its own sake.

Do not introduce generic frameworks for:

- informer registration
- reflection-driven Kubernetes resource plumbing
- generic event buses
- generic immutable collections
- generic repositories
- generic service managers

unless the existing codebase already uses such a pattern and it clearly reduces complexity.

For a fixed, known resource set, explicit code is often preferable to reflection-heavy plumbing.

---

## 19. Dependency Changes

Do not add dependencies without a clear need.

Before adding a dependency:

- check whether Go standard library or existing dependencies already solve the problem
- consider long-term maintenance impact
- avoid introducing large frameworks for small tasks

If a new dependency is necessary, explain why.

---

## 20. Security and Sensitive Data

Do not:

- log secrets
- log credentials
- expose tokens
- include internal corporate identifiers in committed documentation unless explicitly required
- broadly cache sensitive Kubernetes resources without a real consumer

Use generic environment labels in documentation and tests unless exact names are required for correctness.

Do not add Secrets or ConfigMaps to shared caches speculatively.

---

## 21. Multi-Cluster Rules

Cluster-specific runtime state must remain isolated.

Each cluster should independently own relevant runtime components such as:

- acquisition runtime
- cluster state
- generation counter
- health state
- coordinator
- analysis publication

A failure, resync, stop, or high event rate in one cluster must not corrupt another cluster's state.

Avoid shared mutable cross-cluster state unless explicitly designed.

---

## 22. Review Before Implementation

For non-trivial changes, inspect first.

Before editing, identify:

- relevant packages
- current ownership
- current file sizes
- existing tests
- current data flow
- existing helper patterns
- architecture constraints
- persistence implications
- concurrency implications
- API-call implications

For architecture-phase work, provide a concise implementation design before coding when requested.

---

## 23. Validation

Run focused tests first.

For Go changes, the normal validation set is:

```bash
git diff --check
go test ./...
go vet ./...
go build ./...
```

Use repeated focused tests where temporal or deterministic behavior matters:

```bash
go test <affected-package> -count=20
```

Use race tests for concurrent code:

```bash
go test -race <affected-package> -count=1
```

For important concurrency changes, additional repeated race runs may be appropriate.

Do not claim validation passed unless the command was actually run successfully.

If a command cannot be run, report that explicitly.

---

## 24. Final Implementation Report

At the end of a coding task, report:

- design implemented
- exact files changed
- purpose of each file
- important function/type names introduced
- tests added or changed
- schema/migration impact, if any
- concurrency impact, if any
- validation results
- `git diff --stat`
- line counts for every modified/new Go file
- remaining concerns
- anything intentionally deferred

Do not hide unresolved issues.

---

## 25. Line Count Reporting

For modified or newly-created Go files, report line counts before completion.

Example:

```bash
wc -l \
  pkg/example/example.go \
  pkg/example/example_test.go
```

Line count is not a success metric by itself.

Use it to detect responsibility creep.

---

## 26. Git and Commit Policy

Do not commit unless explicitly asked.

Do not push unless explicitly asked.

Do not force-push unless explicitly asked.

Do not create or switch branches unless the task explicitly requires it.

Before handing work back:

```bash
git status --short
git diff --check
git diff --stat
```

When requested, show the relevant diff for review.

---

## 27. Agent Behavior

Do not ask the user questions that can be answered by inspecting the repository.

If the repository provides the answer:

- inspect it
- report what you found
- proceed within scope

Ask only when a decision genuinely requires user intent or business context not available in code/docs.

Do not guess around missing evidence.

When uncertain:

- state the uncertainty
- inspect further
- present the smallest number of meaningful options

---

## 28. Definition of Good Work

A good change should be:

- correct
- scoped
- readable
- testable
- deterministic where practical
- appropriately placed
- consistent with architecture
- free from unnecessary abstraction
- free from unrelated cleanup
- validated
- reviewable
- explainable

The goal is not merely to make the tests pass.

The goal is to leave the codebase easier to reason about than before.
