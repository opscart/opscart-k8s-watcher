package main

import (
	"reflect"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSnapshotResourceCopyDereferencesIndependently(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	in := []*corev1.Node{node}

	out := snapshotResourceCopy(in)
	if len(out) != 1 || out[0].Name != "node-a" {
		t.Fatalf("snapshotResourceCopy(%v) = %v, want one Node named node-a", in, out)
	}

	out[0].Name = "mutated"
	if node.Name != "node-a" {
		t.Fatal("mutating the copied value mutated the original pointed-to Node — expected an independent value copy")
	}
}

// TestBuildNodeOptimizationHandlesEmptyResourcesWithoutPanicking proves the
// snapshot-sourced pipeline runs safely end-to-end on empty input.
// BuildNodeOptimizationRecommendations always returns at least one
// placeholder "no candidate evaluated" entry (pkg/analyzer contract, not
// something this migration changes) rather than an empty slice.
func TestBuildNodeOptimizationHandlesEmptyResourcesWithoutPanicking(t *testing.T) {
	recommendations, savings := buildNodeOptimization(clusterstate.ClusterResources{}, nil)
	if len(recommendations) != 1 {
		t.Fatalf("got %d recommendations from empty resources, want the 1 placeholder observation entry", len(recommendations))
	}
	if recommendations[0].Status != analyzer.NodeOptimizationRecommendationObservation {
		t.Fatalf("recommendations[0].Status = %s, want OBSERVATION", recommendations[0].Status)
	}
	if len(savings) != len(recommendations) {
		t.Fatalf("savings len %d does not match recommendations len %d — expected 1:1 alignment", len(savings), len(recommendations))
	}
}

// ── nodeInfos must come from generation N's live Nodes, not a legacy scan
// (docs/08 Phase 4C audit follow-up) — buildNodeInfosFromSnapshot's own
// regression coverage ─────────────────────────────────────────────────────

// liveNodeFixture builds a corev1.Node carrying the specific labels/capacity
// a test wants to prove buildNodeInfosFromSnapshot reads, without needing a
// real cluster or informer.
func liveNodeFixture(name string, cpuCores, memGB int64, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(cpuCores, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memGB*1024*1024*1024, resource.BinarySI),
			},
		},
	}
}

func TestBuildNodeInfosFromSnapshotUsesGenerationNCapacity(t *testing.T) {
	node := liveNodeFixture("node-a", 8, 32, nil)

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{node}, nil)

	if len(infos) != 1 {
		t.Fatalf("got %d NodeInfo entries, want 1", len(infos))
	}
	if infos[0].CPUCapacity != 8 || infos[0].MemGBCapacity != 32 {
		t.Fatalf("NodeInfo capacity = {CPU:%v Mem:%v}, want {CPU:8 Mem:32} from generation N's live Node.Status.Allocatable",
			infos[0].CPUCapacity, infos[0].MemGBCapacity)
	}
}

func TestBuildNodeInfosFromSnapshotUsesGenerationNPoolMetadata(t *testing.T) {
	node := liveNodeFixture("node-a", 4, 16, map[string]string{
		"node.kubernetes.io/instance-type":      "Standard_NEW",
		"topology.kubernetes.io/region":         "eastus2",
		"topology.kubernetes.io/zone":           "eastus2-1",
		"kubernetes.io/os":                      "linux",
		"kubernetes.io/arch":                    "arm64",
		"kubernetes.azure.com/scalesetpriority": "spot",
	})

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{node}, nil)

	if len(infos) != 1 {
		t.Fatalf("got %d NodeInfo entries, want 1", len(infos))
	}
	got := infos[0]
	if got.VMSize != "Standard_NEW" || got.Region != "eastus2" || got.Zone != "eastus2-1" ||
		got.OS != "linux" || got.Architecture != "arm64" || got.Priority != "spot" {
		t.Fatalf("NodeInfo pool/grouping metadata = %+v, want it sourced from generation N's live Node labels", got)
	}
}

func TestBuildNodeInfosFromSnapshotOmitsNodesRemovedFromLiveList(t *testing.T) {
	nodeA := liveNodeFixture("node-a", 4, 16, nil)

	// node-removed simply never appears in this generation's Nodes slice —
	// there is no legacy nodeInfos list left to reconcile against anymore.
	infos := buildNodeInfosFromSnapshot([]*corev1.Node{nodeA}, nil)

	if len(infos) != 1 || infos[0].Name != "node-a" {
		t.Fatalf("buildNodeInfosFromSnapshot = %+v, want only node-a", infos)
	}
}

func TestBuildNodeInfosFromSnapshotIncludesNewNodeWithNoLegacyReport(t *testing.T) {
	newNode := liveNodeFixture("node-new", 2, 8, nil) // just joined; no legacy scan has ever seen it

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{newNode}, nil) // report nil: no legacy scan at all

	if len(infos) != 1 || infos[0].Name != "node-new" {
		t.Fatalf("buildNodeInfosFromSnapshot = %+v, want node-new present even with no legacy NodeInfo/report", infos)
	}
}

func TestBuildNodeInfosFromSnapshotLeavesProviderAloneWithoutManualOverride(t *testing.T) {
	node := liveNodeFixture("node-a", 2, 8, nil) // no Spec.ProviderID: DetectNodeProvider -> unknown
	report := &models.CloudCostReport{ProviderDetectionMode: "detected", EffectiveProvider: "azure"}

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{node}, report)

	if infos[0].Provider != string(analyzer.CloudProviderUnknown) {
		t.Fatalf("Provider = %q, want the node's own detected provider (%q) untouched when detection mode is not manual",
			infos[0].Provider, analyzer.CloudProviderUnknown)
	}
}

func TestBuildNodeInfosFromSnapshotAppliesManualProviderOverrideFromReport(t *testing.T) {
	node := liveNodeFixture("node-a", 2, 8, nil) // no Spec.ProviderID: would auto-detect as unknown
	report := &models.CloudCostReport{ProviderDetectionMode: "manual", EffectiveProvider: "aws"}

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{node}, report)

	if infos[0].Provider != "aws" {
		t.Fatalf("Provider = %q, want the manually overridden provider %q from the legacy scan's CloudCostReport", infos[0].Provider, "aws")
	}
}

// TestBuildNodeOptimizationFeasibilityIsUnaffectedByReportPoolPricing proves
// report's pool pricing/currency (legacy-scan-age, Cost-owned enrichment)
// can differ or be entirely absent without changing which nodes/pools are
// feasible to consolidate — feasibility depends only on resources, never on
// poolCosts/currency.
func TestBuildNodeOptimizationFeasibilityIsUnaffectedByReportPoolPricing(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Nodes: []*corev1.Node{liveNodeFixture("node-a", 4, 16, map[string]string{
			"node.kubernetes.io/instance-type": "Standard_D4",
			"topology.kubernetes.io/region":    "eastus",
			"kubernetes.io/os":                 "linux",
		})},
		Pods: []*corev1.Pod{},
	}

	recsWithoutReport, savingsWithoutReport := buildNodeOptimization(resources, nil)
	recsWithReport, _ := buildNodeOptimization(resources, &models.CloudCostReport{
		Currency: "USD",
		NodePoolCosts: []models.NodePoolCost{{
			Name: "default", VMSize: "Standard_D4", Region: "eastus", OS: "linux",
			Priority: "Regular", Provider: "unknown", PricingAvailable: true, PricePerNodeMonth: 100,
		}},
	})

	if !reflect.DeepEqual(recsWithoutReport, recsWithReport) {
		t.Fatalf("report's pool pricing changed feasibility recommendations:\nwithout report: %+v\nwith report:    %+v",
			recsWithoutReport, recsWithReport)
	}
	if len(savingsWithoutReport) == 0 || savingsWithoutReport[0].Available {
		t.Fatalf("expected no priced savings with no report at all, got %+v", savingsWithoutReport)
	}
}

// TestNodeOptimizationSavingsJoinManualProviderOverrideFromReport is the
// "manual provider override still joins savings correctly" regression: the
// override applied in buildNodeInfosFromSnapshot must flow all the way into
// the pool identity BuildNodeOptimizationSavingsProjection matches against
// report.NodePoolCosts.
func TestNodeOptimizationSavingsJoinManualProviderOverrideFromReport(t *testing.T) {
	node := liveNodeFixture("node-a", 4, 16, map[string]string{
		"node.kubernetes.io/instance-type": "Standard_D4",
		"topology.kubernetes.io/region":    "eastus",
		"kubernetes.io/os":                 "linux",
		"kubernetes.io/arch":               "amd64",
	})
	report := &models.CloudCostReport{
		Currency:              "USD",
		ProviderDetectionMode: "manual",
		EffectiveProvider:     "aws",
		NodePoolCosts: []models.NodePoolCost{{
			Name: "default", VMSize: "Standard_D4", Region: "eastus", OS: "linux",
			Priority: "Regular", Provider: "aws", PricingAvailable: true, PricePerNodeMonth: 150,
		}},
	}

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{node}, report)
	evidence := analyzer.BuildNodeOptimizationSchedulingEvidence(infos, []corev1.Node{*node})
	schedulingNode, ok := evidence.Nodes["node-a"]
	if !ok {
		t.Fatalf("node-a missing from scheduling evidence: %+v", evidence)
	}
	if schedulingNode.PoolKey.Provider != "aws" {
		t.Fatalf("PoolKey.Provider = %q, want the manually overridden provider %q to flow into pool identity",
			schedulingNode.PoolKey.Provider, "aws")
	}

	rec := analyzer.NodeOptimizationRecommendation{
		Status:               analyzer.NodeOptimizationRecommendationSimulationPassed,
		PoolKey:              schedulingNode.PoolKey,
		CandidateRemovedNode: "node-a",
		CurrentNodeCount:     1,
	}
	projection := analyzer.BuildNodeOptimizationSavingsProjection(rec, report.NodePoolCosts, report.Currency)
	if !projection.Available {
		t.Fatalf("savings projection = %+v, want Available=true — the manual provider override must still let pool identity join against report.NodePoolCosts", projection)
	}
}

// TestRunNodeOptimizationRequiresNoKubernetesClient proves this analysis path
// needs nothing beyond a ClusterSnapshot and the retained scan fields —
// there is no clientset, kubeClientFor, or other Kubernetes acquisition
// reachable from runNodeOptimization at all (docs/08 Phase 4C scope: Node
// Optimization must stop acquiring Kubernetes state directly).
func TestRunNodeOptimizationRequiresNoKubernetesClient(t *testing.T) {
	state := &dashboardState{
		scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}},
	}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNodeOptimization(state, snapshot)

	if state.scan.nodeOptimizationGeneration != snapshot.Generation() {
		t.Fatalf("nodeOptimizationGeneration = %d, want %d", state.scan.nodeOptimizationGeneration, snapshot.Generation())
	}
}

func TestRunNodeOptimizationSkipsWhenSnapshotNotTrustworthy(t *testing.T) {
	original := &clusterScan{report: &models.CloudCostReport{Currency: "USD"}}
	state := &dashboardState{scan: original}

	cs := clusterstate.NewClusterState("cluster-a") // starts STALE
	snapshot := cs.Publish()

	runNodeOptimization(state, snapshot)

	if state.scan != original {
		t.Fatal("an untrustworthy snapshot must not replace the currently displayed scan")
	}
}

func TestRunNodeOptimizationSkipsWhenNoLegacyScanYet(t *testing.T) {
	state := &dashboardState{} // scan is nil: no legacy scan has ever completed

	cs := clusterstate.NewClusterState("cluster-a")
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNodeOptimization(state, snapshot) // must not panic

	if state.scan != nil {
		t.Fatal("expected scan to remain nil when no legacy scan has published cost data")
	}
}

func TestRunNodeOptimizationSkipsWhenScanReportNil(t *testing.T) {
	original := &clusterScan{} // report is nil: cost data never populated
	state := &dashboardState{scan: original}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNodeOptimization(state, snapshot)

	if state.scan != original {
		t.Fatal("a scan with no cost report must not be replaced")
	}
}

func TestPublishNodeOptimizationGenerationGuardRejectsOlderOrEqualGeneration(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{}}

	recsV1 := []analyzer.NodeOptimizationRecommendation{{Summary: "v1"}}
	publishNodeOptimization(state, 5, recsV1, nil)
	if state.scan.nodeOptimizationGeneration != 5 || len(state.scan.nodeOptimization) != 1 {
		t.Fatalf("expected generation 5's result to publish, got generation=%d recs=%d",
			state.scan.nodeOptimizationGeneration, len(state.scan.nodeOptimization))
	}

	recsStale := []analyzer.NodeOptimizationRecommendation{{Summary: "stale"}}
	publishNodeOptimization(state, 3, recsStale, nil)
	if state.scan.nodeOptimizationGeneration != 5 || state.scan.nodeOptimization[0].Summary != "v1" {
		t.Fatal("an older generation must not overwrite a newer already-published result")
	}

	publishNodeOptimization(state, 5, recsStale, nil)
	if state.scan.nodeOptimizationGeneration != 5 || state.scan.nodeOptimization[0].Summary != "v1" {
		t.Fatal("an equal generation must not overwrite the already-published result for that generation")
	}
}

// TestPublishNodeOptimizationCopyAndSwapPreservesPreviouslyPublishedScan
// proves publishNodeOptimization never mutates an already-published
// *clusterScan in place — every existing reader (pages.go, node_optimization.go)
// captures state.scan once under RLock and reads its fields lock-free
// afterward, so mutating a field on that pointer after handing it out would
// race every such reader.
func TestPublishNodeOptimizationCopyAndSwapPreservesPreviouslyPublishedScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}}}

	previouslyRead := state.scan // simulates a reader that captured the pointer under RLock

	publishNodeOptimization(state, 1, []analyzer.NodeOptimizationRecommendation{{Summary: "new"}}, nil)

	if state.scan == previouslyRead {
		t.Fatal("expected publishNodeOptimization to swap in a new *clusterScan, not reuse the existing pointer")
	}
	if len(previouslyRead.nodeOptimization) != 0 {
		t.Fatal("publishNodeOptimization mutated a *clusterScan a reader already held a pointer to")
	}
	if state.scan.report.Currency != "USD" {
		t.Fatal("copy-and-swap must preserve every other field from the previous scan")
	}
}

// TestRunNodeOptimizationEndToEndOnTrustworthySnapshot exercises the full
// path — snapshot resources joined with retained cost data — via a direct
// call to runNodeOptimization (bypassing the real Coordinator's coalescing
// window, which pkg/clusterstate already tests independently).
func TestRunNodeOptimizationEndToEndOnTrustworthySnapshot(t *testing.T) {
	state := &dashboardState{
		scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}},
	}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Nodes: []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}},
		Pods:  []*corev1.Pod{},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runNodeOptimization(state, snapshot)

	if state.scan.nodeOptimizationGeneration != snapshot.Generation() {
		t.Fatalf("nodeOptimizationGeneration = %d, want %d", state.scan.nodeOptimizationGeneration, snapshot.Generation())
	}
	if len(state.scan.nodeOptimizationSavings) != len(state.scan.nodeOptimization) {
		t.Fatal("expected nodeOptimizationSavings aligned 1:1 with nodeOptimization")
	}
}

// ── Audit point 3: legacy full-scan vs. coordinator publish ordering ──────

func TestPreserveNewerCoordinatorNodeOptimizationHandlesNilPrevious(t *testing.T) {
	next := &clusterScan{nodeOptimization: []analyzer.NodeOptimizationRecommendation{{Summary: "legacy"}}}

	preserveNewerCoordinatorNodeOptimization(nil, next) // first-ever scan for this cluster

	if len(next.nodeOptimization) != 1 || next.nodeOptimization[0].Summary != "legacy" {
		t.Fatal("a nil previous scan must leave next's own computation untouched")
	}
}

func TestPreserveNewerCoordinatorNodeOptimizationLeavesLegacyResultWhenNoCoordinatorYet(t *testing.T) {
	previous := &clusterScan{nodeOptimization: []analyzer.NodeOptimizationRecommendation{{Summary: "old-legacy"}}} // generation 0: never coordinator-published
	next := &clusterScan{nodeOptimization: []analyzer.NodeOptimizationRecommendation{{Summary: "new-legacy"}}}

	preserveNewerCoordinatorNodeOptimization(previous, next)

	if len(next.nodeOptimization) != 1 || next.nodeOptimization[0].Summary != "new-legacy" {
		t.Fatal("with no coordinator publish yet, next's own freshly-computed legacy result must stand")
	}
}

// TestNodeOptimizationOrderingCoordinatorThenLegacyScan is the audit-point-3
// regression: a coordinator-published result for generation N must survive
// a legacy full scan that completes and publishes afterward — reproducing
// refresh()'s actual *clusterScan swap (scan.go), not just the guard
// function in isolation.
func TestNodeOptimizationOrderingCoordinatorThenLegacyScan(t *testing.T) {
	state := &dashboardState{scan: &clusterScan{report: &models.CloudCostReport{Currency: "USD"}}}

	coordinatorRecs := []analyzer.NodeOptimizationRecommendation{{Summary: "coordinator-gen-5"}}
	publishNodeOptimization(state, 5, coordinatorRecs, nil)

	legacyScan := &clusterScan{
		report:           &models.CloudCostReport{Currency: "USD"},
		nodeOptimization: []analyzer.NodeOptimizationRecommendation{{Summary: "legacy-own-computation"}},
	}
	preserveNewerCoordinatorNodeOptimization(state.scan, legacyScan) // the exact call refresh() makes
	state.scan = legacyScan                                         // the exact swap refresh() makes

	if state.scan.nodeOptimizationGeneration != 5 {
		t.Fatalf("nodeOptimizationGeneration = %d after a legacy scan publish, want 5 preserved", state.scan.nodeOptimizationGeneration)
	}
	if len(state.scan.nodeOptimization) != 1 || state.scan.nodeOptimization[0].Summary != "coordinator-gen-5" {
		t.Fatalf("legacy scan clobbered the newer coordinator result: %+v", state.scan.nodeOptimization)
	}
}

// TestNodeOptimizationOrderingLegacyScanThenCoordinator is the inverse
// ordering: a legacy scan publishing first must not block a subsequent
// coordinator generation from winning.
func TestNodeOptimizationOrderingLegacyScanThenCoordinator(t *testing.T) {
	state := &dashboardState{}

	legacyScan := &clusterScan{
		report:           &models.CloudCostReport{Currency: "USD"},
		nodeOptimization: []analyzer.NodeOptimizationRecommendation{{Summary: "legacy-own-computation"}},
	}
	preserveNewerCoordinatorNodeOptimization(state.scan, legacyScan) // previous is nil: first-ever scan
	state.scan = legacyScan

	coordinatorRecs := []analyzer.NodeOptimizationRecommendation{{Summary: "coordinator-gen-3"}}
	publishNodeOptimization(state, 3, coordinatorRecs, nil)

	if state.scan.nodeOptimizationGeneration != 3 {
		t.Fatalf("nodeOptimizationGeneration = %d, want 3", state.scan.nodeOptimizationGeneration)
	}
	if len(state.scan.nodeOptimization) != 1 || state.scan.nodeOptimization[0].Summary != "coordinator-gen-3" {
		t.Fatalf("expected the coordinator's generation 3 result to win: %+v", state.scan.nodeOptimization)
	}
}
