package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/acquisition"
	"github.com/opscart/opscart-k8s-watcher/pkg/billing"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// acquisitionSyncTimeout bounds how long startup waits for one cluster's
// informer caches to complete their initial sync (docs/08 Phase 4E) before
// giving up on that cluster and moving on. It is generous relative to
// typical informer LIST latency (docs/08 Phase 0's baseline: a full scan
// against a representative cluster took ~1.5-4s) without blocking dashboard
// startup indefinitely against an unreachable or very large cluster.
const acquisitionSyncTimeout = 60 * time.Second

// startAcquisitionRuntimes constructs and starts one informer-backed
// acquisition runtime per configured cluster, and constructs (but does not
// yet run) each cluster's Coordinator (docs/08 Phase 3/4A/5).
//
// Coordinators are deliberately not started here: main.go's startup
// sequence calls this, then waitForAcquisitionSync, then runs the first
// analysis pass for clusterList[0] synchronously (runDashboard), and only
// then calls startAnalysisCoordinators. That ordering means the bootstrap
// pass can never race a Coordinator goroutine computing the exact same
// first analysis concurrently — see startAnalysisCoordinators' doc comment.
func (srv *server) startAcquisitionRuntimes(ctx context.Context) {
	for _, clusterCtx := range srv.clusterList {
		srv.startAcquisition(ctx, clusterCtx)
	}
}

// startAcquisition creates and starts one cluster's acquisition runtime and
// constructs its Coordinator (not started — see startAcquisitionRuntimes).
// It is best-effort: a cluster whose Kubernetes client cannot be
// constructed is logged and skipped, so one broken cluster's configuration
// cannot block startup for the others. That cluster's refresh (scan.go) can
// then never produce a scan (no ClusterSnapshot will ever exist for it) —
// the same fate a permanently unreachable cluster already had before Phase
// 4E, just detected earlier.
//
// Start already owns the sync lifecycle asynchronously (pkg/acquisition):
// it marks RESYNCING, starts the informer factories, and updates
// acquisition state itself once caches sync. This function does not poll
// or wait for that outcome — see waitForAcquisitionSync.
func (srv *server) startAcquisition(ctx context.Context, clusterCtx string) {
	client, err := srv.kubeClientFor(clusterCtx, nil)
	if err != nil {
		log.Printf("[%s] acquisition: kubernetes client unavailable, skipping: %v", displayName(clusterCtx), err)
		return
	}

	rt := acquisition.NewRuntime(clusterCtx, client)
	rt.Start(ctx)
	state := srv.getState(clusterCtx)
	state.acquisition = rt
	// docs/08 Phase 5: clockInterval (dashboardScanInterval) makes this
	// Coordinator also fire runAnalysisPass on a wall-clock cadence, not
	// only on ClusterState publications — required for rules that can
	// change meaning purely from elapsed time on an otherwise-unchanged
	// snapshot (Waste age gates, Cost pricing TTL; see analysis.go and
	// each *_runtime.go buildX function's doc comment).
	state.coordinator = clusterstate.NewCoordinator(rt.ClusterState(), func(snapshot *clusterstate.ClusterSnapshot) {
		runAnalysisPass(state, snapshot, srv.clusterList)
	}, dashboardScanInterval)
	log.Printf("[%s] acquisition runtime started", displayName(clusterCtx))
}

// startAnalysisCoordinators starts every configured cluster's Coordinator
// goroutine (docs/08 Phase 5) — the sole recurring analysis trigger source
// from this point on, driving every analyzer through runAnalysisPass for
// both event-coalesced and clock-triggered generations.
//
// Called by main.go only after clusterList[0]'s first *clusterScan has
// already been produced by a synchronous bootstrap call (runDashboard,
// main.go). Starting Coordinators any earlier would let a Coordinator
// goroutine's own event-triggered pass (informer initial sync is itself a
// burst of Add events) run concurrently with that bootstrap call for the
// same cluster — harmless by construction (publishScan's generation guard
// and state.mu make concurrent passes safe, never corrupt), but wasteful
// and needlessly nondeterministic. This ordering avoids it entirely rather
// than relying on that safety net.
//
// Each Coordinator's Run is tracked by srv.backgroundWG so shutdown
// (runDashboard, main.go) waits for any in-flight analysis/persistence
// pass to finish — including a clock- or event-triggered persistAnalysis
// write — before the store is closed.
func (srv *server) startAnalysisCoordinators(ctx context.Context) {
	for _, clusterCtx := range srv.clusterList {
		state := srv.getState(clusterCtx)
		if state.coordinator == nil {
			continue // this cluster's acquisition never started — see startAcquisition
		}
		srv.backgroundWG.Add(1)
		go func(coordinator *clusterstate.Coordinator) {
			defer srv.backgroundWG.Done()
			coordinator.Run(ctx)
		}(state.coordinator)
	}
}

// waitForAcquisitionSync blocks, once per configured cluster, until that
// cluster's acquisition runtime reports its informers synced or
// acquisitionSyncTimeout elapses, whichever comes first. This is what lets
// refresh (scan.go) source its evidence from ClusterState.Latest() instead
// of a direct Kubernetes call (docs/08 Phase 4E): without this wait, the
// very first scan for any cluster would race informer sync and either
// observe an empty/untrustworthy snapshot, or — for clusterList[0], whose
// initial scan main.go treats as fatal — fail dashboard startup outright on
// a cluster that just needed a little longer to sync.
//
// A cluster whose client could not be constructed (state.acquisition == nil
// — see startAcquisition) or whose sync does not complete within the
// timeout is logged and left to keep syncing in the background: refresh
// will keep returning an error for that one cluster until its snapshot
// becomes trustworthy, the same best-effort tolerance startAcquisition
// itself already documents for the rest of the cluster list.
func (srv *server) waitForAcquisitionSync(ctx context.Context, clusterList []string) {
	for _, clusterCtx := range clusterList {
		state := srv.getState(clusterCtx)
		if state.acquisition == nil {
			continue
		}
		syncCtx, cancel := context.WithTimeout(ctx, acquisitionSyncTimeout)
		synced := state.acquisition.WaitForSync(syncCtx)
		cancel()
		if !synced {
			log.Printf("[%s] acquisition: initial sync did not complete within %s, continuing in background", displayName(clusterCtx), acquisitionSyncTimeout)
		}
	}
}

// runAnalysisPass is the one analysis execution path per cluster (docs/08
// Phase 5): called by each cluster's Coordinator (event-coalesced or
// clock-triggered) and by refresh's on-demand/bootstrap synchronous call
// (scan.go) — always the same function, always gated on Trustworthy(),
// always publishing atomically. It never touches Kubernetes: all evidence
// comes from snapshot, already synchronized by pkg/acquisition.
//
// clusterList is needed only for renderHTML's sidebar — the Coordinator's
// callback captures srv.clusterList once at construction time
// (startAcquisition above); refresh passes its own parameter straight
// through. Both end up calling this same function, never two independent
// analysis paths.
func runAnalysisPass(state *dashboardState, snapshot *clusterstate.ClusterSnapshot, clusterList []string) {
	if !snapshot.Trustworthy() {
		// DEGRADED/RESYNCING/STALE: state.scan keeps showing whatever was
		// last published. This is what keeps an untrustworthy snapshot's
		// absence of a finding from ever being treated as evidence a
		// condition cleared (docs/08 §4) — this function simply does not
		// run, so it cannot feed a false transition into anything.
		return
	}

	start := time.Now()
	scan := buildClusterScan(state, snapshot)
	publishScan(state, scan, clusterList)
	persistAnalysis(state, scan, start)

	state.mu.Lock()
	state.observation = scanObservation{CompletedAt: time.Now(), Duration: time.Since(start)}
	state.mu.Unlock()
}

// publishScan renders and atomically swaps state.scan/state.htmlPage —
// docs/08 Phase 5's single publication point, replacing the seven
// per-analyzer publishX copy-and-swaps (and their preserveNewerCoordinatorX
// counterparts) Phase 4C/4D/4E built up while two independent analysis
// paths coexisted.
//
// scan.generation < state.scan.generation is the only case skipped —
// strictly less, not less-or-equal: Coordinator's single-threaded loop
// already makes an older snapshot generation being analyzed after a newer
// one structurally impossible, but a clock-triggered pass deliberately
// reuses the SAME generation as a prior event-triggered pass (docs/08
// Phase 5 — a clock tick never advances ClusterSnapshot's generation), and
// that later, same-generation result must still be allowed to win: it may
// differ from the earlier one purely from elapsed time (Waste age gates,
// Cost pricing TTL). The single-threaded Coordinator loop guarantees a
// second call for an unchanged generation is always chronologically later
// than the first, so "equal generation, called again later, wins" is
// exactly as safe as "greater generation always wins" — see
// pkg/clusterstate/coordinator.go's Run.
func publishScan(state *dashboardState, scan *clusterScan, clusterList []string) {
	var billingSnapshot billing.Snapshot
	if state.billingRuntime != nil {
		billingSnapshot = state.billingRuntime.Snapshot()
	}
	page := renderHTML(scan, state.ctx, clusterList, billingSnapshot, state.billingRuntime != nil)

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.scan != nil && scan.generation < state.scan.generation {
		return
	}
	state.scan = scan
	state.htmlPage = page
}

// persistenceInterval bounds how often persistAnalysis actually writes to
// the store, independent of how often runAnalysisPass itself runs (docs/08
// Phase 5): an event-coalesced pass can fire every ~2s during a burst, and
// that must never turn into ~2s-cadence scan_history/incident writes. It
// reuses dashboardScanInterval, preserving the same persistence cadence
// users saw before Phase 5 (every pre-Phase-5 refresh() call persisted, and
// refresh() only ran about every dashboardScanInterval in steady state).
const persistenceInterval = dashboardScanInterval

// persistAnalysis writes incidents/history for scan if at least
// persistenceInterval has elapsed since the last write for this cluster —
// regardless of what triggered this pass (event, clock, or an on-demand
// refresh() call: this is deliberately one throttle keyed on elapsed time,
// not "only the clock trigger may persist," so a user-initiated refresh
// still counts toward the same bound instead of needing its own special
// case). This is the sole incident writer (docs/08 Phase 1/4D.2's
// incident-lifecycle boundary): ResolveMissing is cluster-wide and must
// never be called independently per analyzer.
func persistAnalysis(state *dashboardState, scan *clusterScan, start time.Time) {
	if state.db == nil {
		return
	}

	state.mu.Lock()
	if time.Since(state.lastPersistedAt) < persistenceInterval {
		state.mu.Unlock()
		return
	}
	state.lastPersistedAt = time.Now()
	state.mu.Unlock()

	scanID := newScanID()

	incScore, _, _ := calcIncidentScore(scan)
	issues := collectWarRoomIssues(scan, 0)

	critical, warnings := tallySnapshotCounts(issues, scan.nodeHealth)
	var incidents []store.IncidentData
	for _, is := range issues {
		details, _ := json.Marshal(map[string]any{
			"resource_age_days": is.ResourceAgeDays,
			"message":           is.Message,
		})
		incidents = append(incidents, store.IncidentData{
			Fingerprint:  store.WorkloadFingerprintForPod(is.Namespace, is.Resource, is.Type),
			Namespace:    is.Namespace,
			Resource:     is.Resource,
			IssueType:    is.Type,
			Severity:     is.Severity,
			DetailsJSON:  string(details),
			RestartCount: is.RestartCount,
		})
	}

	snap := store.SnapshotData{
		ScannedAt:     time.Now(),
		IncidentScore: incScore,
		CriticalCount: critical,
		WarningCount:  warnings,
		SecurityScore: scan.securityScore(),
		WasteCount:    scan.wasteTotal(),
		MonthlyCost:   scan.report.TotalMonthlyCost,
		PodCount:      scan.monthlyPodCount(),
	}

	if err := state.db.WriteSnapshot(state.ctx, scanID, snap); err != nil {
		log.Printf("[%s] store snapshot: %v", displayName(state.ctx), err)
	}
	incidents = completeIncidentBatch(incidents, scan.nodeHealth)
	resolved, persistErr := persistCompleteIncidentBatch(state.db, state.ctx, scanID, incidents)
	if persistErr != nil {
		log.Printf("[%s] store incidents: %v", displayName(state.ctx), persistErr)
	} else if resolved > 0 {
		log.Printf("[%s] %d incident(s) resolved", displayName(state.ctx), resolved)
	}
	if cutoff, ok := store.RetentionCutoff(state.retentionDays, time.Now()); ok {
		if pruned, err := state.db.PruneOlderThan(state.ctx, cutoff); err != nil {
			log.Printf("[%s] store prune: %v", displayName(state.ctx), err)
		} else if pruned > 0 {
			log.Printf("[%s] retention: pruned %d incident(s) older than %d day(s)", displayName(state.ctx), pruned, state.retentionDays)
		}
	}
	_ = state.db.WriteScanHistory(state.ctx, scanID, store.ScanMeta{
		DurationMS: time.Since(start).Milliseconds(),
		Success:    true,
		Version:    Version,
	})
	log.Printf("[%s] scan complete: %d namespaces, $%s/month, %d critical issues", displayName(state.ctx), len(scan.report.NamespaceCosts), formatMoney(scan.report.TotalMonthlyCost), countCriticalIssues(scan))
}
