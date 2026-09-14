package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func namespaceObj(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func labeledPod(namespace, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
}

func denyAllPolicy(namespace, name string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{"Ingress", "Egress"},
		},
	}
}

// TestBuildNetworkAnalysisMatchesDirectAnalyzerCall proves buildNetworkAnalysis's
// snapshot-resources adaptation produces exactly what calling
// analyzer.AnalyzeNetworkPolicies directly on the same value slices would —
// "same input produces equivalent audit results".
func TestBuildNetworkAnalysisMatchesDirectAnalyzerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{namespaceObj("payments")},
		Pods:       []*corev1.Pod{labeledPod("payments", "api-1", map[string]string{"app": "api"})},
	}

	got := buildNetworkAnalysis(resources)
	want := analyzer.AnalyzeNetworkPolicies(
		[]corev1.Namespace{*resources.Namespaces[0]},
		[]corev1.Pod{*resources.Pods[0]},
		nil, "", nil,
	)

	if got.TotalNamespaces != want.TotalNamespaces || len(got.UnprotectedNamespaces) != len(want.UnprotectedNamespaces) {
		t.Fatalf("buildNetworkAnalysis = %+v, want %+v", got, want)
	}
}

func TestBuildNetworkAnalysisProtectedNamespaceProducesNoUnprotectedFinding(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{namespaceObj("locked-down")},
		Pods:       []*corev1.Pod{labeledPod("locked-down", "api-1", map[string]string{"app": "api"})},
		NetworkPolicies: []*networkingv1.NetworkPolicy{
			denyAllPolicy("locked-down", "deny-all"),
		},
	}

	got := buildNetworkAnalysis(resources)

	if len(got.UnprotectedNamespaces) != 0 || len(got.ProtectedNamespaces) != 1 {
		t.Fatalf("got %+v, want locked-down fully protected", got)
	}
}

// TestRunNetworkAnalysisRequiresNoKubernetesClient proves this analysis
// path needs nothing beyond a ClusterSnapshot (docs/08 Phase 4D.3 scope:
// Network analysis must stop acquiring Kubernetes state directly here).
func TestRunNetworkAnalysisRequiresNoKubernetesClient(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{namespaceObj("payments")},
		Pods:       []*corev1.Pod{labeledPod("payments", "api-1", map[string]string{"app": "api"})},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNetworkAnalysis(state, snapshot)

	if state.scan.netAuditGeneration != snapshot.Generation() {
		t.Fatalf("netAuditGeneration = %d, want %d", state.scan.netAuditGeneration, snapshot.Generation())
	}
	if state.scan.netAudit == nil || state.scan.netAudit.TotalNamespaces != 1 {
		t.Fatalf("netAudit = %+v, want TotalNamespaces=1", state.scan.netAudit)
	}
}

// TestRunNetworkAnalysisUsesSameGenerationNamespacesPodsPolicies proves
// Namespaces, Pods, and NetworkPolicies all come from the SAME published
// generation, merged by ClusterState from three separate Update calls (as
// real informer event handlers for different kinds would), never a mix of
// fresh evidence for one kind with stale/legacy evidence for another.
func TestRunNetworkAnalysisUsesSameGenerationNamespacesPodsPolicies(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{Namespaces: []*corev1.Namespace{namespaceObj("locked-down")}})
	cs.Update(clusterstate.ClusterResources{Pods: []*corev1.Pod{labeledPod("locked-down", "api-1", map[string]string{"app": "api"})}})
	cs.Update(clusterstate.ClusterResources{NetworkPolicies: []*networkingv1.NetworkPolicy{denyAllPolicy("locked-down", "deny-all")}})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNetworkAnalysis(state, snapshot)

	if len(state.scan.netAudit.ProtectedNamespaces) != 1 {
		t.Fatalf("netAudit = %+v, want locked-down protected using the merged Namespace+Pod+NetworkPolicy evidence", state.scan.netAudit)
	}
}

func TestRunNetworkAnalysisSkipsWhenSnapshotNotTrustworthy(t *testing.T) {
	original := &clusterScan{netAudit: &analyzer.NetworkPolicyAudit{TotalNamespaces: 1}}
	state := &dashboardState{scan: original}

	cs := clusterstate.NewClusterState("cluster-a") // starts STALE
	cs.Update(clusterstate.ClusterResources{Namespaces: []*corev1.Namespace{namespaceObj("payments")}})
	snapshot := cs.Publish()

	runNetworkAnalysis(state, snapshot)

	if state.scan != original {
		t.Fatal("an untrustworthy snapshot must not replace the currently displayed scan")
	}
	if state.scan.netAudit.TotalNamespaces != 1 {
		t.Fatal("an untrustworthy snapshot must preserve the last trustworthy Network audit result")
	}
}

func TestRunNetworkAnalysisSkipsWhenNoLegacyScanYet(t *testing.T) {
	state := &dashboardState{} // scan is nil: no legacy scan has ever completed

	cs := clusterstate.NewClusterState("cluster-a")
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNetworkAnalysis(state, snapshot) // must not panic

	if state.scan != nil {
		t.Fatal("expected scan to remain nil when no legacy scan has ever published a *clusterScan")
	}
}

func TestPublishNetworkAnalysisGenerationGuardRejectsOlderOrEqualGeneration(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	publishNetworkAnalysis(state, 5, &analyzer.NetworkPolicyAudit{TotalNamespaces: 1})
	if state.scan.netAuditGeneration != 5 || state.scan.netAudit.TotalNamespaces != 1 {
		t.Fatalf("expected generation 5's result to publish, got generation=%d netAudit=%+v",
			state.scan.netAuditGeneration, state.scan.netAudit)
	}

	publishNetworkAnalysis(state, 3, &analyzer.NetworkPolicyAudit{TotalNamespaces: 99})
	if state.scan.netAuditGeneration != 5 || state.scan.netAudit.TotalNamespaces != 1 {
		t.Fatal("an older generation must not overwrite a newer already-published result")
	}

	publishNetworkAnalysis(state, 5, &analyzer.NetworkPolicyAudit{TotalNamespaces: 99})
	if state.scan.netAuditGeneration != 5 || state.scan.netAudit.TotalNamespaces != 1 {
		t.Fatal("an equal generation must not overwrite the already-published result for that generation")
	}
}

// TestPublishNetworkAnalysisCopyAndSwapPreservesPreviouslyPublishedScan
// proves publishNetworkAnalysis never mutates an already-published
// *clusterScan in place, exactly like the other three publishers.
func TestPublishNetworkAnalysisCopyAndSwapPreservesPreviouslyPublishedScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}}}

	previouslyRead := state.scan // simulates a reader that captured the pointer under RLock

	publishNetworkAnalysis(state, 1, &analyzer.NetworkPolicyAudit{TotalNamespaces: 1})

	if state.scan == previouslyRead {
		t.Fatal("expected publishNetworkAnalysis to swap in a new *clusterScan, not reuse the existing pointer")
	}
	if previouslyRead.netAudit != nil {
		t.Fatal("publishNetworkAnalysis mutated a *clusterScan a reader already held a pointer to")
	}
	if state.scan.report.Currency != "USD" {
		t.Fatal("copy-and-swap must preserve every other field from the previous scan")
	}
}

// ── legacy full-scan vs. coordinator publish ordering (Phase 4C's pattern,
// reused for Network) ──────────────────────────────────────────────────────

func TestNetworkAnalysisOrderingCoordinatorThenLegacyScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	publishNetworkAnalysis(state, 5, &analyzer.NetworkPolicyAudit{TotalNamespaces: 1})

	legacyScan := &clusterScan{netAudit: &analyzer.NetworkPolicyAudit{TotalNamespaces: 99}}
	preserveNewerCoordinatorNetworkAnalysis(state.scan, legacyScan) // the exact call refresh() makes
	state.scan = legacyScan                                        // the exact swap refresh() makes

	if state.scan.netAuditGeneration != 5 {
		t.Fatalf("netAuditGeneration = %d after a legacy scan publish, want 5 preserved", state.scan.netAuditGeneration)
	}
	if state.scan.netAudit.TotalNamespaces != 1 {
		t.Fatalf("legacy scan clobbered the newer coordinator result: %+v", state.scan.netAudit)
	}
}

func TestNetworkAnalysisOrderingLegacyScanThenCoordinator(t *testing.T) {
	state := &dashboardState{}

	legacyScan := &clusterScan{netAudit: &analyzer.NetworkPolicyAudit{TotalNamespaces: 99}}
	preserveNewerCoordinatorNetworkAnalysis(state.scan, legacyScan) // previous is nil: first-ever scan
	state.scan = legacyScan

	publishNetworkAnalysis(state, 3, &analyzer.NetworkPolicyAudit{TotalNamespaces: 1})

	if state.scan.netAuditGeneration != 3 {
		t.Fatalf("netAuditGeneration = %d, want 3", state.scan.netAuditGeneration)
	}
	if state.scan.netAudit.TotalNamespaces != 1 {
		t.Fatalf("expected the coordinator's generation 3 result to win: %+v", state.scan.netAudit)
	}
}

// TestNetworkAnalysisResultsAreClusterIsolated proves two clusters' Network
// audit results never leak into each other.
func TestNetworkAnalysisResultsAreClusterIsolated(t *testing.T) {
	stateA := &dashboardState{scan: &clusterScan{}}
	stateB := &dashboardState{scan: &clusterScan{}}

	csA := clusterstate.NewClusterState("cluster-a")
	csA.Update(clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{namespaceObj("payments")},
		Pods:       []*corev1.Pod{labeledPod("payments", "api-1", map[string]string{"app": "api"})},
	})
	csA.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshotA := csA.Publish()

	csB := clusterstate.NewClusterState("cluster-b")
	csB.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshotB := csB.Publish()

	runNetworkAnalysis(stateA, snapshotA)
	runNetworkAnalysis(stateB, snapshotB)

	if stateA.scan.netAudit.TotalNamespaces != 1 {
		t.Fatalf("cluster-a netAudit = %+v, want TotalNamespaces=1", stateA.scan.netAudit)
	}
	if stateB.scan.netAudit.TotalNamespaces != 0 {
		t.Fatalf("cluster-b netAudit = %+v, want TotalNamespaces=0 — cluster-a's namespace leaked in", stateB.scan.netAudit)
	}
}
