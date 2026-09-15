package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ── docs/08 Phase 5: single publication point (publishScan) ─────────────────

// TestPublishScanRejectsOlderGeneration proves an older snapshot generation
// can never overwrite a newer already-published result.
func TestPublishScanRejectsOlderGeneration(t *testing.T) {
	state := &dashboardState{ctx: "cluster-a"}

	publishScan(state, &clusterScan{generation: 5, report: reportNamed("gen5")}, nil)
	if state.scan.generation != 5 || state.scan.report.ClusterName != "gen5" {
		t.Fatalf("expected generation 5 to publish, got generation=%d report=%+v", state.scan.generation, state.scan.report)
	}

	publishScan(state, &clusterScan{generation: 3, report: reportNamed("gen3-stale")}, nil)
	if state.scan.generation != 5 || state.scan.report.ClusterName != "gen5" {
		t.Fatal("an older generation must not overwrite a newer already-published result")
	}
}

// TestPublishScanAllowsLaterSameGenerationToWin proves the docs/08 Phase 5
// design decision: publishScan's guard is strict less-than, not
// less-or-equal, so a clock-triggered pass that deliberately reuses the
// SAME ClusterSnapshot generation as a prior event-triggered pass (because
// nothing in Kubernetes changed, only wall-clock time) is still allowed to
// win — it may differ from the earlier result purely from elapsed time
// (Waste age gates, Cost pricing TTL).
func TestPublishScanAllowsLaterSameGenerationToWin(t *testing.T) {
	state := &dashboardState{ctx: "cluster-a"}

	publishScan(state, &clusterScan{generation: 5, report: reportNamed("first-pass")}, nil)
	publishScan(state, &clusterScan{generation: 5, report: reportNamed("clock-triggered-repass")}, nil)

	if state.scan.report.ClusterName != "clock-triggered-repass" {
		t.Fatalf("a later pass for the SAME generation must win: got %q", state.scan.report.ClusterName)
	}
}

// TestPublishScanSwapsWholeScanNeverMutatesPrevious proves publishScan
// always swaps in the *clusterScan buildClusterScan just built — never
// mutates a *clusterScan a reader may already hold a pointer to (every
// existing reader captures state.scan once under RLock and reads its
// fields lock-free afterward).
func TestPublishScanSwapsWholeScanNeverMutatesPrevious(t *testing.T) {
	state := &dashboardState{ctx: "cluster-a"}
	publishScan(state, &clusterScan{generation: 1, report: reportNamed("v1")}, nil)
	previouslyRead := state.scan

	publishScan(state, &clusterScan{generation: 2, report: reportNamed("v2")}, nil)

	if state.scan == previouslyRead {
		t.Fatal("expected publishScan to swap in a new *clusterScan, not reuse the existing pointer")
	}
	if previouslyRead.report.ClusterName != "v1" {
		t.Fatal("publishScan mutated a *clusterScan a reader already held a pointer to")
	}
}

func reportNamed(name string) *models.CloudCostReport {
	return &models.CloudCostReport{ClusterName: name}
}

// ── docs/08 Phase 5: CIS derived from one coherent analysis pass ───────────

// TestBuildClusterScanCISMatchesSameCallSecurityAndNetwork proves CIS is
// structurally impossible to compute from mismatched generations: it is
// derived, inside buildClusterScan, from the very same secAudit/netAudit
// values that call also returns — never re-read from state.scan, which
// could otherwise hold values from a different, independently-timed pass.
func TestBuildClusterScanCISMatchesSameCallSecurityAndNetwork(t *testing.T) {
	state := &dashboardState{costAnalyzer: analyzer.NewNodePoolCostAnalyzer("")}

	privileged := true
	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Pods: []*corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "payments"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:            "app",
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
			}}},
		}},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	scan := buildClusterScan(state, snapshot)

	if scan.secAudit == nil || scan.netAudit == nil || scan.cisResult == nil {
		t.Fatalf("expected secAudit/netAudit/cisResult all populated from one pass, got secAudit=%v netAudit=%v cisResult=%v",
			scan.secAudit, scan.netAudit, scan.cisResult)
	}
	want := analyzer.CalculateCISScore(scan.secAudit, scan.netAudit)
	if scan.cisResult.Score != want.Score || scan.cisResult.FailedChecks != want.FailedChecks {
		t.Fatalf("cisResult = %+v, want CalculateCISScore(scan.secAudit, scan.netAudit) = %+v", scan.cisResult, want)
	}
}

// TestBuildClusterScanNamespaceCountIsAuthoritativeNotCostDerived is the
// regression test for the minikube "0 Namespaces" bug: buildClusterScan
// must set namespaceCount from the snapshot's real Kubernetes Namespace
// inventory (resources.Namespaces), independent of whether Cost
// Intelligence produced any NamespaceCosts allocation entries — a
// namespace can exist with zero priced/allocated workloads and still be a
// real namespace. Mirrors the exact reported condition: 4 real namespaces,
// empty NamespaceCosts.
func TestBuildClusterScanNamespaceCountIsAuthoritativeNotCostDerived(t *testing.T) {
	state := &dashboardState{costAnalyzer: analyzer.NewNodePoolCostAnalyzer("")}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{
			{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "kube-public"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "kube-node-lease"}},
		},
		// Deliberately no priced Nodes/Pods: Cost Intelligence produces no
		// NamespaceCosts allocation entries for this generation, exactly
		// the minikube condition that exposed the bug.
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	scan := buildClusterScan(state, snapshot)

	if len(scan.report.NamespaceCosts) != 0 {
		t.Fatalf("test fixture invalid: expected zero NamespaceCosts, got %d", len(scan.report.NamespaceCosts))
	}
	if scan.namespaceCount != 4 {
		t.Fatalf("namespaceCount = %d, want 4 (authoritative Kubernetes namespace inventory, independent of empty NamespaceCosts)", scan.namespaceCount)
	}
}

// ── docs/08 Phase 5: bounded, trigger-agnostic persistence cadence ─────────

type snapshotWriteSpy struct {
	store.NullStore
	writeSnapshotCalls int
}

func (s *snapshotWriteSpy) WriteSnapshot(cluster, scanID string, snap store.SnapshotData) error {
	s.writeSnapshotCalls++
	return nil
}

func scanForPersist() *clusterScan {
	return &clusterScan{report: &models.CloudCostReport{}}
}

// TestPersistAnalysisThrottlesWithinInterval proves persistAnalysis skips
// writing when called again before persistenceInterval has elapsed since
// its last write — the mechanism that keeps an event-coalesced pass
// (potentially every ~2s during a burst) from becoming ~2s-cadence
// scan_history/incident writes (docs/08 Phase 5).
func TestPersistAnalysisThrottlesWithinInterval(t *testing.T) {
	spy := &snapshotWriteSpy{}
	state := &dashboardState{ctx: "cluster-a", db: spy}

	persistAnalysis(state, scanForPersist(), time.Now())
	persistAnalysis(state, scanForPersist(), time.Now())

	if spy.writeSnapshotCalls != 1 {
		t.Fatalf("writeSnapshotCalls = %d, want exactly 1 — the second call arrived within persistenceInterval and must be throttled", spy.writeSnapshotCalls)
	}
}

// TestPersistAnalysisWritesAgainAfterInterval proves the throttle is a
// bound on frequency, not a one-time latch: once persistenceInterval has
// genuinely elapsed (simulated here via lastPersistedAt, since this test
// must not sleep 60s), a subsequent pass persists again.
func TestPersistAnalysisWritesAgainAfterInterval(t *testing.T) {
	spy := &snapshotWriteSpy{}
	state := &dashboardState{ctx: "cluster-a", db: spy}

	persistAnalysis(state, scanForPersist(), time.Now())

	state.mu.Lock()
	state.lastPersistedAt = time.Now().Add(-persistenceInterval - time.Second)
	state.mu.Unlock()

	persistAnalysis(state, scanForPersist(), time.Now())

	if spy.writeSnapshotCalls != 2 {
		t.Fatalf("writeSnapshotCalls = %d, want 2 — a pass after persistenceInterval has elapsed must persist again", spy.writeSnapshotCalls)
	}
}

// TestRunAnalysisPassSkipsEverythingWhenSnapshotNotTrustworthy proves a
// DEGRADED/RESYNCING/STALE snapshot never reaches buildClusterScan,
// publishScan, or persistAnalysis at all — an untrustworthy snapshot's
// absence of a finding must never be treated as evidence a condition
// cleared (docs/08 §4), and must never advance incident resolution.
func TestRunAnalysisPassSkipsEverythingWhenSnapshotNotTrustworthy(t *testing.T) {
	spy := &snapshotWriteSpy{}
	state := &dashboardState{ctx: "cluster-a", db: spy}

	cs := clusterstate.NewClusterState("cluster-a") // starts STALE
	snapshot := cs.Publish()

	runAnalysisPass(state, snapshot, nil)

	if state.scan != nil {
		t.Fatal("an untrustworthy snapshot must not publish any *clusterScan")
	}
	if spy.writeSnapshotCalls != 0 {
		t.Fatal("an untrustworthy snapshot must never reach persistAnalysis")
	}
}

// TestStaticClusterResolvesIncidentWithNoKubernetesEvent proves the exact
// scenario docs/08 Phase 5 exists to solve: an issue that becomes absent on
// an otherwise-unchanging cluster (no new ClusterSnapshot generation
// between passes) still resolves once resolveAfter elapses, driven purely
// by repeated persistAnalysis calls — simulating the Coordinator's clock
// trigger firing with nothing new in Kubernetes — never by a Kubernetes
// event. lastPersistedAt is reset directly between passes only to bypass
// the persistenceInterval throttle within this fast test; resolveAfter
// elapsing is still exercised for real via backdateAbsentSince below,
// exactly as pkg/store's own Phase 1 tests do.
func TestStaticClusterResolvesIncidentWithNoKubernetesEvent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "clock-resolve.db")
	db, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	state := &dashboardState{ctx: "cluster-a", db: db}
	finding := models.NodeConditionFinding{NodeName: "worker-1", ConditionType: "DiskPressure", ConditionStatus: "True"}
	fingerprint := store.Fingerprint("cluster", "Node", "worker-1", "DiskPressure")

	resetThrottle := func() {
		state.mu.Lock()
		state.lastPersistedAt = time.Time{}
		state.mu.Unlock()
	}

	// Clock tick 1: issue present.
	present := &clusterScan{report: &models.CloudCostReport{}, nodeHealth: []models.NodeConditionFinding{finding}}
	persistAnalysis(state, present, time.Now())
	if rec, _ := db.GetIncidentHistory("cluster-a", fingerprint); rec == nil || rec.Status != "active" {
		t.Fatalf("expected the incident active after its first present pass, got %+v", rec)
	}

	// Clock tick 2 (no Kubernetes event in between): issue absent — starts
	// the absence clock.
	resetThrottle()
	absent := &clusterScan{report: &models.CloudCostReport{}}
	persistAnalysis(state, absent, time.Now())
	if rec, _ := db.GetIncidentHistory("cluster-a", fingerprint); rec == nil || rec.Status != "active" {
		t.Fatalf("expected the incident still active immediately after becoming absent, got %+v", rec)
	}

	// Fast-forward past resolveAfter without sleeping or any Kubernetes
	// event — simulates however long it takes for the next clock tick to
	// fire.
	backdateAbsentSince(t, dbPath, "cluster-a", fingerprint, 5*time.Minute)

	// Clock tick 3 (still no Kubernetes event): issue still absent.
	resetThrottle()
	persistAnalysis(state, absent, time.Now())
	if rec, _ := db.GetIncidentHistory("cluster-a", fingerprint); rec == nil || rec.Status != "resolved" {
		t.Fatalf("expected the incident resolved once resolveAfter elapsed with no Kubernetes event, got %+v", rec)
	}
}
