package main

import (
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func abandonedNamespace(name string, ageDays int) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)),
		},
	}
}

// TestBuildWasteAnalysisMatchesDirectAnalyzerCall proves buildWasteAnalysis's
// snapshot-resources adaptation produces exactly what calling
// analyzer.AnalyzeWaste directly on the same value slices would — "same
// input produces equivalent audit results".
func TestBuildWasteAnalysisMatchesDirectAnalyzerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{abandonedNamespace("stale-team", 40)},
	}

	got := buildWasteAnalysis(resources)

	if len(got.AbandonedNamespaces) != 1 {
		t.Fatalf("buildWasteAnalysis = %+v, want one abandoned namespace", got.AbandonedNamespaces)
	}
}

func TestBuildWasteAnalysisHealthyNamespaceProducesNoFinding(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{abandonedNamespace("payments", 1)}, // 1 day old: below the 7-day minimum
	}

	got := buildWasteAnalysis(resources)

	if len(got.AbandonedNamespaces) != 0 {
		t.Fatalf("unexpected abandoned-namespace finding for a fresh namespace: %+v", got.AbandonedNamespaces)
	}
}

// TestRunWasteAnalysisRequiresNoKubernetesClient proves this analysis path
// needs nothing beyond a ClusterSnapshot (docs/08 Phase 4D.5 scope: Waste
// analysis must stop acquiring Kubernetes state directly here).
func TestRunWasteAnalysisRequiresNoKubernetesClient(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{Namespaces: []*corev1.Namespace{abandonedNamespace("stale-team", 40)}})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runWasteAnalysis(state, snapshot)

	if state.scan.wasteAuditGeneration != snapshot.Generation() {
		t.Fatalf("wasteAuditGeneration = %d, want %d", state.scan.wasteAuditGeneration, snapshot.Generation())
	}
	if state.scan.wasteAudit == nil || len(state.scan.wasteAudit.AbandonedNamespaces) != 1 {
		t.Fatalf("wasteAudit = %+v, want one abandoned namespace", state.scan.wasteAudit)
	}
}

// TestRunWasteAnalysisUsesSingleGenerationResources proves the resource
// evidence used comes from one published ClusterSnapshot generation, merged
// by ClusterState from two separate Update calls (as real informer event
// handlers would produce), never a mix of generations.
func TestRunWasteAnalysisUsesSingleGenerationResources(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{Namespaces: []*corev1.Namespace{abandonedNamespace("stale-team", 40)}})
	cs.Update(clusterstate.ClusterResources{Namespaces: []*corev1.Namespace{abandonedNamespace("stale-team", 40)}}) // second Update on the same kind, as a resync would produce
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runWasteAnalysis(state, snapshot)

	if len(state.scan.wasteAudit.AbandonedNamespaces) != 1 {
		t.Fatalf("wasteAudit = %+v, want exactly one finding from the single published generation", state.scan.wasteAudit)
	}
}

func TestRunWasteAnalysisSkipsWhenSnapshotNotTrustworthy(t *testing.T) {
	original := &clusterScan{wasteAudit: &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "stale-team"}}}}
	state := &dashboardState{scan: original}

	cs := clusterstate.NewClusterState("cluster-a") // starts STALE
	cs.Update(clusterstate.ClusterResources{Namespaces: []*corev1.Namespace{abandonedNamespace("stale-team", 40)}})
	snapshot := cs.Publish()

	runWasteAnalysis(state, snapshot)

	if state.scan != original {
		t.Fatal("an untrustworthy snapshot must not replace the currently displayed scan")
	}
	if len(state.scan.wasteAudit.AbandonedNamespaces) != 1 {
		t.Fatal("an untrustworthy snapshot must preserve the last trustworthy Waste audit result")
	}
}

func TestRunWasteAnalysisSkipsWhenNoLegacyScanYet(t *testing.T) {
	state := &dashboardState{} // scan is nil: no legacy scan has ever completed

	cs := clusterstate.NewClusterState("cluster-a")
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runWasteAnalysis(state, snapshot) // must not panic

	if state.scan != nil {
		t.Fatal("expected scan to remain nil when no legacy scan has ever published a *clusterScan")
	}
}

func TestPublishWasteAnalysisGenerationGuardRejectsOlderOrEqualGeneration(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	publishWasteAnalysis(state, 5, &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "a"}}})
	if state.scan.wasteAuditGeneration != 5 || len(state.scan.wasteAudit.AbandonedNamespaces) != 1 {
		t.Fatalf("expected generation 5's result to publish, got generation=%d wasteAudit=%+v",
			state.scan.wasteAuditGeneration, state.scan.wasteAudit)
	}

	publishWasteAnalysis(state, 3, &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "a"}, {Name: "b"}}})
	if state.scan.wasteAuditGeneration != 5 || len(state.scan.wasteAudit.AbandonedNamespaces) != 1 {
		t.Fatal("an older generation must not overwrite a newer already-published result")
	}

	publishWasteAnalysis(state, 5, &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "a"}, {Name: "b"}}})
	if state.scan.wasteAuditGeneration != 5 || len(state.scan.wasteAudit.AbandonedNamespaces) != 1 {
		t.Fatal("an equal generation must not overwrite the already-published result for that generation")
	}
}

// TestPublishWasteAnalysisCopyAndSwapPreservesPreviouslyPublishedScan proves
// publishWasteAnalysis never mutates an already-published *clusterScan in
// place, exactly like the other five publishers.
func TestPublishWasteAnalysisCopyAndSwapPreservesPreviouslyPublishedScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{cisResult: &analyzer.CISResult{Score: 42}}}

	previouslyRead := state.scan // simulates a reader that captured the pointer under RLock

	publishWasteAnalysis(state, 1, &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "a"}}})

	if state.scan == previouslyRead {
		t.Fatal("expected publishWasteAnalysis to swap in a new *clusterScan, not reuse the existing pointer")
	}
	if previouslyRead.wasteAudit != nil {
		t.Fatal("publishWasteAnalysis mutated a *clusterScan a reader already held a pointer to")
	}
	if state.scan.cisResult.Score != 42 {
		t.Fatal("copy-and-swap must preserve every other field from the previous scan")
	}
}

// ── legacy full-scan vs. coordinator publish ordering (Phase 4C's pattern,
// reused for Waste) ─────────────────────────────────────────────────────

func TestWasteAnalysisOrderingCoordinatorThenLegacyScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	publishWasteAnalysis(state, 5, &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "coordinator"}}})

	legacyScan := &clusterScan{wasteAudit: &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "legacy"}}}}
	preserveNewerCoordinatorWasteAnalysis(state.scan, legacyScan) // the exact call refresh() makes
	state.scan = legacyScan                                       // the exact swap refresh() makes

	if state.scan.wasteAuditGeneration != 5 {
		t.Fatalf("wasteAuditGeneration = %d after a legacy scan publish, want 5 preserved", state.scan.wasteAuditGeneration)
	}
	if state.scan.wasteAudit.AbandonedNamespaces[0].Name != "coordinator" {
		t.Fatalf("legacy scan clobbered the newer coordinator result: %+v", state.scan.wasteAudit)
	}
}

func TestWasteAnalysisOrderingLegacyScanThenCoordinator(t *testing.T) {
	state := &dashboardState{}

	legacyScan := &clusterScan{wasteAudit: &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "legacy"}}}}
	preserveNewerCoordinatorWasteAnalysis(state.scan, legacyScan) // previous is nil: first-ever scan
	state.scan = legacyScan

	publishWasteAnalysis(state, 3, &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "coordinator"}}})

	if state.scan.wasteAuditGeneration != 3 {
		t.Fatalf("wasteAuditGeneration = %d, want 3", state.scan.wasteAuditGeneration)
	}
	if state.scan.wasteAudit.AbandonedNamespaces[0].Name != "coordinator" {
		t.Fatalf("expected the coordinator's generation 3 result to win: %+v", state.scan.wasteAudit)
	}
}

// TestWasteAnalysisResultsAreClusterIsolated proves two clusters' Waste
// audit results never leak into each other.
func TestWasteAnalysisResultsAreClusterIsolated(t *testing.T) {
	stateA := &dashboardState{scan: &clusterScan{}}
	stateB := &dashboardState{scan: &clusterScan{}}

	csA := clusterstate.NewClusterState("cluster-a")
	csA.Update(clusterstate.ClusterResources{Namespaces: []*corev1.Namespace{abandonedNamespace("stale-team", 40)}})
	csA.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshotA := csA.Publish()

	csB := clusterstate.NewClusterState("cluster-b")
	csB.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshotB := csB.Publish()

	runWasteAnalysis(stateA, snapshotA)
	runWasteAnalysis(stateB, snapshotB)

	if len(stateA.scan.wasteAudit.AbandonedNamespaces) != 1 {
		t.Fatalf("cluster-a wasteAudit = %+v, want one abandoned namespace", stateA.scan.wasteAudit)
	}
	if len(stateB.scan.wasteAudit.AbandonedNamespaces) != 0 {
		t.Fatalf("cluster-b wasteAudit = %+v, want zero — cluster-a's namespace leaked in", stateB.scan.wasteAudit)
	}
}

// TestWasteAnalysisUntrustworthyAbsenceDoesNotAffectIncidentBatch proves the
// incident-lifecycle boundary explicitly: capturing legacyScan before
// preserveNewerCoordinatorWasteAnalysis runs (refresh's exact sequence,
// scan.go) means a coordinator-preserved DISPLAY value — even one produced
// from an untrustworthy-snapshot no-op, i.e. simply never updated — can
// never reach the incident batch that collectWarRoomIssues/calcIncidentScore
// read.
func TestWasteAnalysisUntrustworthyAbsenceDoesNotAffectIncidentBatch(t *testing.T) {
	previous := &clusterScan{
		wasteAudit:           &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "coordinator"}}},
		wasteAuditGeneration: 5,
	}
	nextLegacyScan := &clusterScan{wasteAudit: &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "legacy"}}}}

	// refresh()'s exact sequence: capture the legacy value first, THEN run
	// the coordinator-preserve guard.
	legacyWasteAudit := nextLegacyScan.wasteAudit
	preserveNewerCoordinatorWasteAnalysis(previous, nextLegacyScan)

	if legacyWasteAudit.AbandonedNamespaces[0].Name != "legacy" {
		t.Fatalf("legacyWasteAudit (the incident-batch input) = %+v, want the legacy scan's own observation, unaffected by the later coordinator-preserve guard", legacyWasteAudit)
	}
	if nextLegacyScan.wasteAudit.AbandonedNamespaces[0].Name != "coordinator" {
		t.Fatalf("nextLegacyScan.wasteAudit (the DISPLAY value) = %+v, want the newer coordinator result", nextLegacyScan.wasteAudit)
	}
}
