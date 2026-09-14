package main

import (
	"context"
	"log"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/acquisition"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
)

// acquisitionSyncTimeout bounds how long startup waits for one cluster's
// informer caches to complete their initial sync (docs/08 Phase 4E) before
// giving up on that cluster and moving on. It is generous relative to
// typical informer LIST latency (docs/08 Phase 0's baseline: a full scan
// against a representative cluster took ~1.5-4s) without blocking dashboard
// startup indefinitely against an unreachable or very large cluster.
const acquisitionSyncTimeout = 60 * time.Second

// startAcquisitionRuntimes starts one informer-backed acquisition runtime
// per configured cluster (docs/08 Phase 3/4A). Since Phase 4E, refresh
// (scan.go) is a direct reader of the ClusterState this populates — see
// dashboardState.acquisition's doc comment and legacy_analysis.go.
//
// ctx governs every runtime's lifetime: canceling it (dashboard shutdown)
// stops every cluster's informer factories, since each Runtime derives its
// own child context from ctx. No separate stop step is needed here.
func (srv *server) startAcquisitionRuntimes(ctx context.Context) {
	for _, clusterCtx := range srv.clusterList {
		srv.startAcquisition(ctx, clusterCtx)
	}
}

// startAcquisition creates and starts one cluster's acquisition runtime.
// It is best-effort: a cluster whose Kubernetes client cannot be
// constructed is logged and skipped, so one broken cluster's configuration
// cannot block startup for the others. Since Phase 4E that cluster's
// refresh (scan.go) can then never produce a scan (no ClusterSnapshot will
// ever exist for it) — the same fate a permanently unreachable cluster
// already had before this phase, just detected earlier.
//
// Start already owns the sync lifecycle asynchronously (pkg/acquisition):
// it marks RESYNCING, starts the informer factories, and updates
// acquisition state itself once caches sync. This function does not poll
// or wait for that outcome — ClusterState.Acquisition() remains available
// on the returned runtime for whoever needs it (future diagnostics), but
// nothing here re-derives it just to log it.
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
	startAnalysisCoordinator(ctx, state, rt)
	log.Printf("[%s] acquisition runtime started", displayName(clusterCtx))
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

// runCoordinatedAnalysis is the single per-cluster Coordinator callback
// (docs/08 §2.5: "one coalesced generation → run all migrated analyzers").
// Every Phase 4C/4D-migrated analyzer's entry point is called here, in this
// one function — not via a second Coordinator, dependency routing, or
// dirty-resource tracking. Each entry point independently gates on
// snapshot.Trustworthy() and its own generation guard (see
// runCostAnalysis/runNodeOptimization/runResourceAnalysis/runNodeHealth/
// runNetworkAnalysis/runSecurityAnalysis/runWasteAnalysis), so a skip in one
// never blocks another (except the one explicit ordering dependency below),
// and neither can overwrite a newer result the other's guard already
// protects.
//
// runCostAnalysis must run before runNodeOptimization: Node Optimization's
// savings enrichment joins against this same generation's Cost-owned pool
// identity (docs/08 Phase 4D.6), and runNodeOptimization's own gate
// (scan.costGeneration == snapshot.Generation()) requires Cost to have
// already published for this generation by the time it runs. This is one
// explicit, justified sequence — not a general analyzer dependency
// DAG/router — and every other analyzer below remains independent of
// ordering.
func runCoordinatedAnalysis(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) {
	runCostAnalysis(state, snapshot)
	runNodeOptimization(state, snapshot)
	runResourceAnalysis(state, snapshot)
	runNodeHealth(state, snapshot)
	runNetworkAnalysis(state, snapshot)
	runSecurityAnalysis(state, snapshot)
	runWasteAnalysis(state, snapshot)
}

// startAnalysisCoordinator creates and starts this cluster's Phase 4B
// coordinator over rt's ClusterState, wired to runCoordinatedAnalysis.
// Exactly one Coordinator exists per cluster, matching rt's ClusterState
// one-to-one (docs/08 §13) — adding another migrated analyzer means adding
// its call to runCoordinatedAnalysis above, not creating a second
// Coordinator here.
func startAnalysisCoordinator(ctx context.Context, state *dashboardState, rt *acquisition.Runtime) {
	coordinator := clusterstate.NewCoordinator(rt.ClusterState(), func(snapshot *clusterstate.ClusterSnapshot) {
		runCoordinatedAnalysis(state, snapshot)
	})
	state.coordinator = coordinator
	go coordinator.Run(ctx)
}
