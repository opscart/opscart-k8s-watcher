package main

import (
	"context"
	"log"

	"github.com/opscart/opscart-k8s-watcher/pkg/acquisition"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
)

// startAcquisitionRuntimes starts one informer-backed acquisition runtime
// per configured cluster (docs/08 Phase 3/4A). This is the transitional
// slice where informer acquisition runs alongside the existing runFullScan
// polling loop below — nothing yet reads from the runtimes started here;
// see dashboardState.acquisition's doc comment.
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
// cannot block startup for the others (or for this one's existing
// runFullScan path, which builds its own client independently).
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

// runCoordinatedAnalysis is the single per-cluster Coordinator callback
// (docs/08 §2.5: "one coalesced generation → run all migrated analyzers").
// Every Phase 4C/4D-migrated analyzer's entry point is called here, in this
// one function — not via a second Coordinator, dependency routing, or
// dirty-resource tracking. Each entry point independently gates on
// snapshot.Trustworthy() and its own generation guard (see
// runNodeOptimization/runResourceAnalysis/runNodeHealth), so a skip in one
// never blocks another, and neither can overwrite a newer result the
// other's guard already protects.
func runCoordinatedAnalysis(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) {
	runNodeOptimization(state, snapshot)
	runResourceAnalysis(state, snapshot)
	runNodeHealth(state, snapshot)
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
