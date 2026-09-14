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

func TestBuildNodeInfosFromSnapshotIncludesNewNodeWithNoCoordinatorCostYet(t *testing.T) {
	newNode := liveNodeFixture("node-new", 2, 8, nil) // just joined; no coordinator Cost result has ever seen it

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{newNode}, nil) // cost nil: no coordinator Cost result at all

	if len(infos) != 1 || infos[0].Name != "node-new" {
		t.Fatalf("buildNodeInfosFromSnapshot = %+v, want node-new present even with no coordinator Cost result", infos)
	}
}

func TestBuildNodeInfosFromSnapshotLeavesProviderAloneWithoutManualOverride(t *testing.T) {
	node := liveNodeFixture("node-a", 2, 8, nil) // no Spec.ProviderID: DetectNodeProvider -> unknown
	cost := &models.CloudCostReport{ProviderDetectionMode: "detected", EffectiveProvider: "azure"}

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{node}, cost)

	if infos[0].Provider != string(analyzer.CloudProviderUnknown) {
		t.Fatalf("Provider = %q, want the node's own detected provider (%q) untouched when detection mode is not manual",
			infos[0].Provider, analyzer.CloudProviderUnknown)
	}
}

func TestBuildNodeInfosFromSnapshotAppliesManualProviderOverrideFromCost(t *testing.T) {
	node := liveNodeFixture("node-a", 2, 8, nil) // no Spec.ProviderID: would auto-detect as unknown
	cost := &models.CloudCostReport{ProviderDetectionMode: "manual", EffectiveProvider: "aws"}

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{node}, cost)

	if infos[0].Provider != "aws" {
		t.Fatalf("Provider = %q, want the manually overridden provider %q from the coordinator's Cost result", infos[0].Provider, "aws")
	}
}

// TestBuildNodeOptimizationFeasibilityIsUnaffectedByCostPoolPricing proves
// cost's pool pricing/currency (Cost-owned enrichment) can differ or be
// entirely absent without changing which nodes/pools are feasible to
// consolidate — feasibility depends only on resources, never on
// poolCosts/currency.
func TestBuildNodeOptimizationFeasibilityIsUnaffectedByCostPoolPricing(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Nodes: []*corev1.Node{liveNodeFixture("node-a", 4, 16, map[string]string{
			"node.kubernetes.io/instance-type": "Standard_D4",
			"topology.kubernetes.io/region":    "eastus",
			"kubernetes.io/os":                 "linux",
		})},
		Pods: []*corev1.Pod{},
	}

	recsWithoutCost, savingsWithoutCost := buildNodeOptimization(resources, nil)
	recsWithCost, _ := buildNodeOptimization(resources, &models.CloudCostReport{
		Currency: "USD",
		NodePoolCosts: []models.NodePoolCost{{
			Name: "default", VMSize: "Standard_D4", Region: "eastus", OS: "linux",
			Priority: "Regular", Provider: "unknown", PricingAvailable: true, PricePerNodeMonth: 100,
		}},
	})

	if !reflect.DeepEqual(recsWithoutCost, recsWithCost) {
		t.Fatalf("cost's pool pricing changed feasibility recommendations:\nwithout cost: %+v\nwith cost:    %+v",
			recsWithoutCost, recsWithCost)
	}
	if len(savingsWithoutCost) == 0 || savingsWithoutCost[0].Available {
		t.Fatalf("expected no priced savings with no cost result at all, got %+v", savingsWithoutCost)
	}
}

// TestNodeOptimizationSavingsJoinManualProviderOverrideFromCost is the
// "manual provider override still joins savings correctly" regression: the
// override applied in buildNodeInfosFromSnapshot must flow all the way into
// the pool identity BuildNodeOptimizationSavingsProjection matches against
// report.NodePoolCosts.
func TestNodeOptimizationSavingsJoinManualProviderOverrideFromCost(t *testing.T) {
	node := liveNodeFixture("node-a", 4, 16, map[string]string{
		"node.kubernetes.io/instance-type": "Standard_D4",
		"topology.kubernetes.io/region":    "eastus",
		"kubernetes.io/os":                 "linux",
		"kubernetes.io/arch":               "amd64",
	})
	cost := &models.CloudCostReport{
		Currency:              "USD",
		ProviderDetectionMode: "manual",
		EffectiveProvider:     "aws",
		NodePoolCosts: []models.NodePoolCost{{
			Name: "default", VMSize: "Standard_D4", Region: "eastus", OS: "linux",
			Priority: "Regular", Provider: "aws", PricingAvailable: true, PricePerNodeMonth: 150,
		}},
	}

	infos := buildNodeInfosFromSnapshot([]*corev1.Node{node}, cost)
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
	projection := analyzer.BuildNodeOptimizationSavingsProjection(rec, cost.NodePoolCosts, cost.Currency)
	if !projection.Available {
		t.Fatalf("savings projection = %+v, want Available=true — the manual provider override must still let pool identity join against report.NodePoolCosts", projection)
	}
}

// TestNodeOptimizationEndToEndViaRunAnalysisPass exercises the full path —
// snapshot resources joined with this same pass's Cost report, via
// runAnalysisPass (the one analysis execution path since docs/08 Phase 5,
// replacing the former runNodeOptimization/publishNodeOptimization split).
func TestNodeOptimizationEndToEndViaRunAnalysisPass(t *testing.T) {
	state := &dashboardState{costAnalyzer: analyzer.NewNodePoolCostAnalyzer("")}

	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Nodes: []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}},
		Pods:  []*corev1.Pod{},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	snapshot := cs.Publish()

	runAnalysisPass(state, snapshot, nil)

	if state.scan.generation != snapshot.Generation() {
		t.Fatalf("scan.generation = %d, want %d", state.scan.generation, snapshot.Generation())
	}
	if len(state.scan.nodeOptimizationSavings) != len(state.scan.nodeOptimization) {
		t.Fatal("expected nodeOptimizationSavings aligned 1:1 with nodeOptimization")
	}
}
