package main

import (
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
)

// This file is docs/08 Phase 4D.5: Waste analysis migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline — the
// sixth analyzer (after Node Optimization Phase 4C, Resource Analyzer
// Phase 4D.1, Node Health Phase 4D.2, Network Phase 4D.3, Security
// Phase 4D.4) driven by the per-cluster Coordinator. See
// acquisition_runtime.go's runCoordinatedAnalysis for where all six are
// invoked from the same coalesced generation.
//
// This migration is DISPLAY-only, exactly like Node Health/Network/Security.
// Incident persistence (UpsertIncidents/ResolveMissing) remains entirely
// owned by the legacy scan cycle (refresh, scan.go) — see
// preserveNewerCoordinatorWasteAnalysis's doc comment there for exactly why
// (wasteAudit.StalePods/AbandonedNamespaces feed collectWarRoomIssues, and
// wasteAudit.OrphanedPVCs feeds calcIncidentScore, part of that same
// incident batch), and how the two coexist without becoming two
// independent incident writers.
//
// The legacy Waste call inside runFullScan (analyzer.NewWasteAuditor +
// AuditWaste, server.go step 3) is deliberately left in place, not
// disabled: WasteAuditor.PVCSnapshot() is still the sole PVC-acquisition
// path for Node Optimization's legacy duplicate storage evidence
// (server.go's step 6), which does not migrate in this slice. Duplicate
// execution for the DISPLAY value is tolerated the same way
// Phase 4C/4D.1-4D.4 established: whichever publishes last wins the
// display, guarded by publishWasteAnalysis's generation check below.
//
// Time semantics: most Waste rules are clock-driven age gates (see docs/08
// Phase 4D.5's audit), meaning an age threshold can be crossed by elapsed
// wall-clock time alone, with no new Kubernetes event to trigger a fresh
// coordinator generation. This does not regress correctness here: the
// legacy 60-second scan loop above already re-evaluates every age-gated
// rule on its own wall-clock cadence, independent of the coordinator, and
// continues to do so unchanged. If that loop is ever removed (Phase 5),
// age-gated Waste findings will need an explicit clock-driven
// re-evaluation trigger — not solved here, and not to be solved by adding
// a periodic Kubernetes read.
func buildWasteAnalysis(resources clusterstate.ClusterResources) *analyzer.WasteAudit {
	input := analyzer.WasteSnapshot{
		Namespaces:               snapshotResourceCopy(resources.Namespaces),
		Pods:                     snapshotResourceCopy(resources.Pods),
		PersistentVolumeClaims:   snapshotResourceCopy(resources.PersistentVolumeClaims),
		Jobs:                     snapshotResourceCopy(resources.Jobs),
		CronJobs:                 snapshotResourceCopy(resources.CronJobs),
		Deployments:              snapshotResourceCopy(resources.Deployments),
		StatefulSets:             snapshotResourceCopy(resources.StatefulSets),
		ReplicaSets:              snapshotResourceCopy(resources.ReplicaSets),
		Services:                 snapshotResourceCopy(resources.Services),
		Ingresses:                snapshotResourceCopy(resources.Ingresses),
		HorizontalPodAutoscalers: snapshotResourceCopy(resources.HorizontalPodAutoscalers),
		EndpointSlices:           snapshotResourceCopy(resources.EndpointSlices),
		PodWarningEvents:         snapshotResourceCopy(resources.PodWarningEvents),
	}
	return analyzer.AnalyzeWaste(input, dashboardWasteMinAgeDays, time.Now())
}

// runWasteAnalysis is the Coordinator-facing analysis step for one
// cluster: it gates on acquisition trustworthiness, then computes and
// publishes a new Waste audit for display. It never touches incident
// persistence — see this file's header comment.
//
// A DEGRADED/RESYNCING/STALE snapshot is not analyzed: state.scan keeps
// showing whatever result (coordinator- or legacy-produced) was last
// published. This is also what keeps an untrustworthy snapshot from ever
// being treated as evidence a waste finding was resolved — the function
// simply does not run, so it cannot feed a false transition into anything,
// incident-related or not.
func runWasteAnalysis(state *dashboardState, snapshot *clusterstate.ClusterSnapshot) {
	if !snapshot.Trustworthy() {
		return
	}

	state.mu.RLock()
	hasScan := state.scan != nil
	state.mu.RUnlock()
	if !hasScan {
		// No legacy scan has ever published a *clusterScan yet — there is
		// nothing to copy-and-swap this result onto.
		return
	}

	audit := buildWasteAnalysis(snapshot.Resources())
	publishWasteAnalysis(state, snapshot.Generation(), audit)
}

// publishWasteAnalysis replaces state.scan.wasteAudit via copy-and-swap of
// the whole *clusterScan pointer — the exact mechanism the other five
// publishers use, for the same reasons: every existing reader treats an
// already-published *clusterScan as immutable, and the generation check is
// defense-in-depth documenting an invariant Coordinator's single-threaded
// loop already guarantees structurally.
func publishWasteAnalysis(state *dashboardState, generation uint64, audit *analyzer.WasteAudit) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.scan == nil || generation <= state.scan.wasteAuditGeneration {
		return
	}

	updated := *state.scan
	updated.wasteAudit = audit
	updated.wasteAuditGeneration = generation
	state.scan = &updated
}
