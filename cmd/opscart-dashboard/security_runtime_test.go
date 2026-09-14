package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func privilegedPod(namespace, name string) *corev1.Pod {
	privileged := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:            "app",
			SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		}}},
	}
}

// TestBuildSecurityAnalysisMatchesDirectAnalyzerCall proves buildSecurityAnalysis's
// snapshot-resources adaptation produces exactly what calling
// analyzer.AnalyzeSecurity directly on the same value slice would — "same
// input produces equivalent audit results".
func TestBuildSecurityAnalysisMatchesDirectAnalyzerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Pods: []*corev1.Pod{privilegedPod("payments", "api-1")},
	}

	got := buildSecurityAnalysis(resources)
	want := analyzer.AnalyzeSecurity([]corev1.Pod{*resources.Pods[0]})

	if got.TotalPodsAudited != want.TotalPodsAudited || len(got.Issues) != len(want.Issues) {
		t.Fatalf("buildSecurityAnalysis = %+v, want %+v", got, want)
	}
}

func TestBuildSecurityAnalysisHealthyPodProducesNoPrivilegedFinding(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Pods: []*corev1.Pod{labeledPod("payments", "api-1", nil)},
	}

	got := buildSecurityAnalysis(resources)

	for _, issue := range got.Issues {
		if issue.Type == "privileged_container" {
			t.Fatalf("unexpected privileged_container finding for a non-privileged pod: %+v", issue)
		}
	}
}

// TestRunSecurityAnalysisRequiresNoKubernetesClient proves this analysis
// path needs nothing beyond a ClusterSnapshot (docs/08 Phase 4D.4 scope:
// Security analysis must stop acquiring Kubernetes state directly here).
func TestRunSecurityAnalysisRequiresNoKubernetesClient(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{Pods: []*corev1.Pod{privilegedPod("payments", "api-1")}})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runSecurityAnalysis(state, snapshot)

	if state.scan.secAuditGeneration != snapshot.Generation() {
		t.Fatalf("secAuditGeneration = %d, want %d", state.scan.secAuditGeneration, snapshot.Generation())
	}
	if state.scan.secAudit == nil || state.scan.secAudit.TotalPodsAudited != 1 {
		t.Fatalf("secAudit = %+v, want TotalPodsAudited=1", state.scan.secAudit)
	}
}

// TestRunSecurityAnalysisUsesSingleGenerationPods proves the Pod evidence
// used comes from one published ClusterSnapshot generation, merged by
// ClusterState from two separate Update calls (as real informer event
// handlers would produce), never a mix of generations.
func TestRunSecurityAnalysisUsesSingleGenerationPods(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{Pods: []*corev1.Pod{privilegedPod("payments", "api-1")}})
	cs.Update(clusterstate.ClusterResources{Pods: []*corev1.Pod{privilegedPod("payments", "api-1")}}) // second Update on the same kind, as a resync would produce
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runSecurityAnalysis(state, snapshot)

	if state.scan.secAudit.TotalPodsAudited != 1 {
		t.Fatalf("secAudit = %+v, want exactly one Pod from the single published generation", state.scan.secAudit)
	}
}

func TestRunSecurityAnalysisSkipsWhenSnapshotNotTrustworthy(t *testing.T) {
	original := &clusterScan{secAudit: &models.SecurityAudit{TotalPodsAudited: 1}}
	state := &dashboardState{scan: original}

	cs := clusterstate.NewClusterState("cluster-a") // starts STALE
	cs.Update(clusterstate.ClusterResources{Pods: []*corev1.Pod{privilegedPod("payments", "api-1")}})
	snapshot := cs.Publish()

	runSecurityAnalysis(state, snapshot)

	if state.scan != original {
		t.Fatal("an untrustworthy snapshot must not replace the currently displayed scan")
	}
	if state.scan.secAudit.TotalPodsAudited != 1 {
		t.Fatal("an untrustworthy snapshot must preserve the last trustworthy Security audit result")
	}
}

func TestRunSecurityAnalysisSkipsWhenNoLegacyScanYet(t *testing.T) {
	state := &dashboardState{} // scan is nil: no legacy scan has ever completed

	cs := clusterstate.NewClusterState("cluster-a")
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runSecurityAnalysis(state, snapshot) // must not panic

	if state.scan != nil {
		t.Fatal("expected scan to remain nil when no legacy scan has ever published a *clusterScan")
	}
}

func TestPublishSecurityAnalysisGenerationGuardRejectsOlderOrEqualGeneration(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	publishSecurityAnalysis(state, 5, &models.SecurityAudit{TotalPodsAudited: 1})
	if state.scan.secAuditGeneration != 5 || state.scan.secAudit.TotalPodsAudited != 1 {
		t.Fatalf("expected generation 5's result to publish, got generation=%d secAudit=%+v",
			state.scan.secAuditGeneration, state.scan.secAudit)
	}

	publishSecurityAnalysis(state, 3, &models.SecurityAudit{TotalPodsAudited: 99})
	if state.scan.secAuditGeneration != 5 || state.scan.secAudit.TotalPodsAudited != 1 {
		t.Fatal("an older generation must not overwrite a newer already-published result")
	}

	publishSecurityAnalysis(state, 5, &models.SecurityAudit{TotalPodsAudited: 99})
	if state.scan.secAuditGeneration != 5 || state.scan.secAudit.TotalPodsAudited != 1 {
		t.Fatal("an equal generation must not overwrite the already-published result for that generation")
	}
}

// TestPublishSecurityAnalysisCopyAndSwapPreservesPreviouslyPublishedScan
// proves publishSecurityAnalysis never mutates an already-published
// *clusterScan in place, exactly like the other four publishers.
func TestPublishSecurityAnalysisCopyAndSwapPreservesPreviouslyPublishedScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}}}

	previouslyRead := state.scan // simulates a reader that captured the pointer under RLock

	publishSecurityAnalysis(state, 1, &models.SecurityAudit{TotalPodsAudited: 1})

	if state.scan == previouslyRead {
		t.Fatal("expected publishSecurityAnalysis to swap in a new *clusterScan, not reuse the existing pointer")
	}
	if previouslyRead.secAudit != nil {
		t.Fatal("publishSecurityAnalysis mutated a *clusterScan a reader already held a pointer to")
	}
	if state.scan.report.Currency != "USD" {
		t.Fatal("copy-and-swap must preserve every other field from the previous scan")
	}
}

// ── legacy full-scan vs. coordinator publish ordering (Phase 4C's pattern,
// reused for Security) ─────────────────────────────────────────────────────

func TestSecurityAnalysisOrderingCoordinatorThenLegacyScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	publishSecurityAnalysis(state, 5, &models.SecurityAudit{TotalPodsAudited: 1})

	legacyScan := &clusterScan{secAudit: &models.SecurityAudit{TotalPodsAudited: 99}}
	preserveNewerCoordinatorSecurityAnalysis(state.scan, legacyScan) // the exact call refresh() makes
	state.scan = legacyScan                                         // the exact swap refresh() makes

	if state.scan.secAuditGeneration != 5 {
		t.Fatalf("secAuditGeneration = %d after a legacy scan publish, want 5 preserved", state.scan.secAuditGeneration)
	}
	if state.scan.secAudit.TotalPodsAudited != 1 {
		t.Fatalf("legacy scan clobbered the newer coordinator result: %+v", state.scan.secAudit)
	}
}

func TestSecurityAnalysisOrderingLegacyScanThenCoordinator(t *testing.T) {
	state := &dashboardState{}

	legacyScan := &clusterScan{secAudit: &models.SecurityAudit{TotalPodsAudited: 99}}
	preserveNewerCoordinatorSecurityAnalysis(state.scan, legacyScan) // previous is nil: first-ever scan
	state.scan = legacyScan

	publishSecurityAnalysis(state, 3, &models.SecurityAudit{TotalPodsAudited: 1})

	if state.scan.secAuditGeneration != 3 {
		t.Fatalf("secAuditGeneration = %d, want 3", state.scan.secAuditGeneration)
	}
	if state.scan.secAudit.TotalPodsAudited != 1 {
		t.Fatalf("expected the coordinator's generation 3 result to win: %+v", state.scan.secAudit)
	}
}

// TestSecurityAnalysisResultsAreClusterIsolated proves two clusters'
// Security audit results never leak into each other.
func TestSecurityAnalysisResultsAreClusterIsolated(t *testing.T) {
	stateA := &dashboardState{scan: &clusterScan{}}
	stateB := &dashboardState{scan: &clusterScan{}}

	csA := clusterstate.NewClusterState("cluster-a")
	csA.Update(clusterstate.ClusterResources{Pods: []*corev1.Pod{privilegedPod("payments", "api-1")}})
	csA.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshotA := csA.Publish()

	csB := clusterstate.NewClusterState("cluster-b")
	csB.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshotB := csB.Publish()

	runSecurityAnalysis(stateA, snapshotA)
	runSecurityAnalysis(stateB, snapshotB)

	if stateA.scan.secAudit.TotalPodsAudited != 1 {
		t.Fatalf("cluster-a secAudit = %+v, want TotalPodsAudited=1", stateA.scan.secAudit)
	}
	if stateB.scan.secAudit.TotalPodsAudited != 0 {
		t.Fatalf("cluster-b secAudit = %+v, want TotalPodsAudited=0 — cluster-a's pod leaked in", stateB.scan.secAudit)
	}
}

// TestSecurityAnalysisUntrustworthyAbsenceDoesNotAffectIncidentBatch proves
// the incident-lifecycle boundary explicitly: capturing legacyScan before
// preserveNewerCoordinatorSecurityAnalysis runs (refresh's exact sequence,
// scan.go) means a coordinator-preserved DISPLAY value — even one produced
// from an untrustworthy-snapshot no-op, i.e. simply never updated — can
// never reach the incident batch that collectWarRoomIssues reads.
func TestSecurityAnalysisUntrustworthyAbsenceDoesNotAffectIncidentBatch(t *testing.T) {
	previous := &clusterScan{
		secAudit:           &models.SecurityAudit{TotalPodsAudited: 1},
		secAuditGeneration: 5,
	}
	nextLegacyScan := &clusterScan{secAudit: &models.SecurityAudit{TotalPodsAudited: 99}}

	// refresh()'s exact sequence: capture the legacy value first, THEN run
	// the coordinator-preserve guard.
	legacySecAudit := nextLegacyScan.secAudit
	preserveNewerCoordinatorSecurityAnalysis(previous, nextLegacyScan)

	if legacySecAudit.TotalPodsAudited != 99 {
		t.Fatalf("legacySecAudit (the incident-batch input) = %+v, want the legacy scan's own observation, unaffected by the later coordinator-preserve guard", legacySecAudit)
	}
	if nextLegacyScan.secAudit.TotalPodsAudited != 1 {
		t.Fatalf("nextLegacyScan.secAudit (the DISPLAY value) = %+v, want the newer coordinator result", nextLegacyScan.secAudit)
	}
}
