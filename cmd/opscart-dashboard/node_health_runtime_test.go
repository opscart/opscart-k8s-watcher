package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func unhealthyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}},
		},
	}
}

func healthyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func scheduledOwnedPod(namespace, name, node, ownerKind, ownerName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: ownerKind, Name: ownerName, Controller: boolPtr(true)},
			},
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

// TestBuildNodeHealthMatchesDirectScannerCall proves buildNodeHealth's
// snapshot-resources adaptation produces exactly what calling
// scanner.AnalyzeNodeHealth directly on the same value slices would —
// "same input produces equivalent existing Node Health findings".
func TestBuildNodeHealthMatchesDirectScannerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Nodes: []*corev1.Node{unhealthyNode("node-a")},
		Pods:  []*corev1.Pod{scheduledOwnedPod("shop", "api-7d8f9c6b5-abc12", "node-a", "ReplicaSet", "api-7d8f9c6b5")},
	}

	got := buildNodeHealth(resources)
	want := scanner.AnalyzeNodeHealth(
		[]corev1.Node{*resources.Nodes[0]},
		[]corev1.Pod{*resources.Pods[0]},
		nil,
	)

	if len(got) != len(want) || len(got) != 1 {
		t.Fatalf("buildNodeHealth = %+v, want %+v", got, want)
	}
	if got[0].NodeName != want[0].NodeName || got[0].ConditionType != want[0].ConditionType {
		t.Fatalf("buildNodeHealth = %+v, want %+v", got[0], want[0])
	}
}

func TestBuildNodeHealthHealthyNodesProduceNoFindings(t *testing.T) {
	resources := clusterstate.ClusterResources{Nodes: []*corev1.Node{healthyNode("node-a")}}

	got := buildNodeHealth(resources)

	if len(got) != 0 {
		t.Fatalf("got %+v, want no findings for an all-healthy snapshot", got)
	}
}

// TestBuildNodeHealthCorrelatesJobAndCronJobOwnership proves Job -> CronJob
// owner correlation (driven entirely by each Job's own OwnerReferences)
// survives sourcing Jobs from ClusterSnapshot instead of a live List.
func TestBuildNodeHealthCorrelatesJobAndCronJobOwnership(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "nightly-29123456",
			OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "nightly", Controller: boolPtr(true)}}},
	}
	resources := clusterstate.ClusterResources{
		Nodes: []*corev1.Node{unhealthyNode("node-a")},
		Pods:  []*corev1.Pod{scheduledOwnedPod("batch", "nightly-29123456-abc12", "node-a", "Job", job.Name)},
		Jobs:  []*batchv1.Job{job},
	}

	got := buildNodeHealth(resources)

	if len(got) != 1 || len(got[0].CorrelatedWorkloads) != 1 {
		t.Fatalf("unexpected correlation: %+v", got)
	}
	want := models.CorrelatedWorkload{Namespace: "batch", Kind: "CronJob", Name: "nightly", PodCount: 1}
	if got[0].CorrelatedWorkloads[0] != want {
		t.Fatalf("CronJob owner correlation = %+v, want %+v", got[0].CorrelatedWorkloads[0], want)
	}
}

// TestRunNodeHealthRequiresNoKubernetesClient proves this analysis path
// needs nothing beyond a ClusterSnapshot (docs/08 Phase 4D.2 scope: Node
// Health must stop acquiring Kubernetes state directly in this path).
func TestRunNodeHealthRequiresNoKubernetesClient(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Nodes: []*corev1.Node{unhealthyNode("node-a")},
		Pods:  []*corev1.Pod{scheduledOwnedPod("shop", "api-7d8f9c6b5-abc12", "node-a", "ReplicaSet", "api-7d8f9c6b5")},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNodeHealth(state, snapshot)

	if state.scan.nodeHealthGeneration != snapshot.Generation() {
		t.Fatalf("nodeHealthGeneration = %d, want %d", state.scan.nodeHealthGeneration, snapshot.Generation())
	}
	if len(state.scan.nodeHealth) != 1 || state.scan.nodeHealth[0].NodeName != "node-a" {
		t.Fatalf("nodeHealth = %+v, want one finding for node-a", state.scan.nodeHealth)
	}
}

// TestRunNodeHealthCorrelatesFromSameGenerationPods proves unhealthy
// conditions and correlated workloads both come from the SAME published
// generation's Nodes and Pods, merged by ClusterState from two separate
// Update calls (as real informer event handlers for different kinds would),
// never a mix of a fresh Node with legacy/stale Pod evidence.
func TestRunNodeHealthCorrelatesFromSameGenerationPods(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{Nodes: []*corev1.Node{unhealthyNode("node-a")}})
	cs.Update(clusterstate.ClusterResources{
		Pods: []*corev1.Pod{scheduledOwnedPod("shop", "api-7d8f9c6b5-abc12", "node-a", "ReplicaSet", "api-7d8f9c6b5")},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNodeHealth(state, snapshot)

	if len(state.scan.nodeHealth) != 1 {
		t.Fatalf("nodeHealth = %+v, want one finding", state.scan.nodeHealth)
	}
	workloads := state.scan.nodeHealth[0].CorrelatedWorkloads
	if len(workloads) != 1 || workloads[0].Name != "api" {
		t.Fatalf("CorrelatedWorkloads = %+v, want the api Deployment merged in by the second Update", workloads)
	}
}

func TestRunNodeHealthSkipsWhenSnapshotNotTrustworthy(t *testing.T) {
	original := &clusterScan{nodeHealth: []models.NodeConditionFinding{{NodeName: "existing"}}}
	state := &dashboardState{scan: original}

	cs := clusterstate.NewClusterState("cluster-a") // starts STALE
	cs.Update(clusterstate.ClusterResources{Nodes: []*corev1.Node{unhealthyNode("node-a")}})
	snapshot := cs.Publish()

	runNodeHealth(state, snapshot)

	if state.scan != original {
		t.Fatal("an untrustworthy snapshot must not replace the currently displayed scan")
	}
	if len(state.scan.nodeHealth) != 1 || state.scan.nodeHealth[0].NodeName != "existing" {
		t.Fatal("an untrustworthy snapshot must preserve the last trustworthy Node Health result")
	}
}

func TestRunNodeHealthSkipsWhenNoLegacyScanYet(t *testing.T) {
	state := &dashboardState{} // scan is nil: no legacy scan has ever completed

	cs := clusterstate.NewClusterState("cluster-a")
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNodeHealth(state, snapshot) // must not panic

	if state.scan != nil {
		t.Fatal("expected scan to remain nil when no legacy scan has ever published a *clusterScan")
	}
}

func TestPublishNodeHealthGenerationGuardRejectsOlderOrEqualGeneration(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	findingsV1 := []models.NodeConditionFinding{{NodeName: "v1"}}
	publishNodeHealth(state, 5, findingsV1)
	if state.scan.nodeHealthGeneration != 5 || len(state.scan.nodeHealth) != 1 {
		t.Fatalf("expected generation 5's result to publish, got generation=%d findings=%d",
			state.scan.nodeHealthGeneration, len(state.scan.nodeHealth))
	}

	findingsStale := []models.NodeConditionFinding{{NodeName: "stale"}}
	publishNodeHealth(state, 3, findingsStale)
	if state.scan.nodeHealthGeneration != 5 || state.scan.nodeHealth[0].NodeName != "v1" {
		t.Fatal("an older generation must not overwrite a newer already-published result")
	}

	publishNodeHealth(state, 5, findingsStale)
	if state.scan.nodeHealthGeneration != 5 || state.scan.nodeHealth[0].NodeName != "v1" {
		t.Fatal("an equal generation must not overwrite the already-published result for that generation")
	}
}

// TestPublishNodeHealthCopyAndSwapPreservesPreviouslyPublishedScan proves
// publishNodeHealth never mutates an already-published *clusterScan in
// place, exactly like publishNodeOptimization/publishResourceAnalysis.
func TestPublishNodeHealthCopyAndSwapPreservesPreviouslyPublishedScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}}}

	previouslyRead := state.scan // simulates a reader that captured the pointer under RLock

	publishNodeHealth(state, 1, []models.NodeConditionFinding{{NodeName: "new"}})

	if state.scan == previouslyRead {
		t.Fatal("expected publishNodeHealth to swap in a new *clusterScan, not reuse the existing pointer")
	}
	if len(previouslyRead.nodeHealth) != 0 {
		t.Fatal("publishNodeHealth mutated a *clusterScan a reader already held a pointer to")
	}
	if state.scan.report.Currency != "USD" {
		t.Fatal("copy-and-swap must preserve every other field from the previous scan")
	}
}

// ── legacy full-scan vs. coordinator publish ordering (Phase 4C's pattern,
// reused for Node Health) ──────────────────────────────────────────────────

func TestNodeHealthOrderingCoordinatorThenLegacyScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	publishNodeHealth(state, 5, []models.NodeConditionFinding{{NodeName: "coordinator-gen-5"}})

	legacyScan := &clusterScan{nodeHealth: []models.NodeConditionFinding{{NodeName: "legacy-own-computation"}}}
	preserveNewerCoordinatorNodeHealth(state.scan, legacyScan) // the exact call refresh() makes
	state.scan = legacyScan                                    // the exact swap refresh() makes

	if state.scan.nodeHealthGeneration != 5 {
		t.Fatalf("nodeHealthGeneration = %d after a legacy scan publish, want 5 preserved", state.scan.nodeHealthGeneration)
	}
	if len(state.scan.nodeHealth) != 1 || state.scan.nodeHealth[0].NodeName != "coordinator-gen-5" {
		t.Fatalf("legacy scan clobbered the newer coordinator result: %+v", state.scan.nodeHealth)
	}
}

func TestNodeHealthOrderingLegacyScanThenCoordinator(t *testing.T) {
	state := &dashboardState{}

	legacyScan := &clusterScan{nodeHealth: []models.NodeConditionFinding{{NodeName: "legacy-own-computation"}}}
	preserveNewerCoordinatorNodeHealth(state.scan, legacyScan) // previous is nil: first-ever scan
	state.scan = legacyScan

	publishNodeHealth(state, 3, []models.NodeConditionFinding{{NodeName: "coordinator-gen-3"}})

	if state.scan.nodeHealthGeneration != 3 {
		t.Fatalf("nodeHealthGeneration = %d, want 3", state.scan.nodeHealthGeneration)
	}
	if len(state.scan.nodeHealth) != 1 || state.scan.nodeHealth[0].NodeName != "coordinator-gen-3" {
		t.Fatalf("expected the coordinator's generation 3 result to win: %+v", state.scan.nodeHealth)
	}
}

// TestNodeHealthResultsAreClusterIsolated proves two clusters' Node Health
// results never leak into each other — each dashboardState/ClusterState
// pair is independent, matching pkg/clusterstate's own per-cluster isolation
// guarantee.
func TestNodeHealthResultsAreClusterIsolated(t *testing.T) {
	stateA := &dashboardState{scan: &clusterScan{}}
	stateB := &dashboardState{scan: &clusterScan{}}

	csA := clusterstate.NewClusterState("cluster-a")
	csA.Update(clusterstate.ClusterResources{Nodes: []*corev1.Node{unhealthyNode("node-a")}})
	csA.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshotA := csA.Publish()

	csB := clusterstate.NewClusterState("cluster-b")
	csB.Update(clusterstate.ClusterResources{Nodes: []*corev1.Node{healthyNode("node-b")}})
	csB.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshotB := csB.Publish()

	runNodeHealth(stateA, snapshotA)
	runNodeHealth(stateB, snapshotB)

	if len(stateA.scan.nodeHealth) != 1 || stateA.scan.nodeHealth[0].NodeName != "node-a" {
		t.Fatalf("cluster-a nodeHealth = %+v, want one finding for node-a", stateA.scan.nodeHealth)
	}
	if len(stateB.scan.nodeHealth) != 0 {
		t.Fatalf("cluster-b nodeHealth = %+v, want no findings — cluster-a's unhealthy node leaked in", stateB.scan.nodeHealth)
	}
}

// TestRefreshIncidentPersistenceIsolatedFromCoordinatorNodeHealthSwap proves
// the actual incident-persistence boundary docs/08 Phase 4D.2 chose (Option
// A): capturing legacyNodeHealth before preserveNewerCoordinatorNodeHealth
// runs means a coordinator-preserved DISPLAY value can never reach the
// incident batch, even though both are read from the very same *clusterScan
// object. This reproduces refresh()'s exact sequence (scan.go) rather than
// a synthetic API.
func TestRefreshIncidentPersistenceIsolatedFromCoordinatorNodeHealthSwap(t *testing.T) {
	previous := &clusterScan{
		nodeHealth:           []models.NodeConditionFinding{{NodeName: "coordinator-gen-5"}},
		nodeHealthGeneration: 5,
	}
	nextLegacyScan := &clusterScan{nodeHealth: []models.NodeConditionFinding{{NodeName: "legacy-own-computation"}}}

	// refresh()'s exact sequence: capture the legacy value first, THEN run
	// the coordinator-preserve guard.
	legacyNodeHealth := nextLegacyScan.nodeHealth
	preserveNewerCoordinatorNodeHealth(previous, nextLegacyScan)

	if len(legacyNodeHealth) != 1 || legacyNodeHealth[0].NodeName != "legacy-own-computation" {
		t.Fatalf("legacyNodeHealth (the incident-batch input) = %+v, want the legacy scan's own observation, unaffected by the later coordinator-preserve guard", legacyNodeHealth)
	}
	// Meanwhile the DISPLAY field on the same object was correctly upgraded.
	if len(nextLegacyScan.nodeHealth) != 1 || nextLegacyScan.nodeHealth[0].NodeName != "coordinator-gen-5" {
		t.Fatalf("nextLegacyScan.nodeHealth (the DISPLAY value) = %+v, want the newer coordinator result", nextLegacyScan.nodeHealth)
	}
}
