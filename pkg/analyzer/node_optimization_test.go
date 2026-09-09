package analyzer

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestSimulateSameShapeNodeCountFits(t *testing.T) {
	input := NodeOptimizationInput{
		CurrentNodes:            3,
		CandidateNodes:          2,
		NodeCPUCapacityMilli:    4000,
		NodeMemoryCapacityBytes: 8 * 1024,
		Pods: []NodeOptimizationPodInput{
			{Namespace: "payments", Name: "api-a", CPURequestMilli: 2000, MemoryRequestBytes: 2 * 1024},
			{Namespace: "payments", Name: "api-b", CPURequestMilli: 1500, MemoryRequestBytes: 3 * 1024},
			{Namespace: "orders", Name: "worker", CPURequestMilli: 1000, MemoryRequestBytes: 2 * 1024},
		},
	}

	got := SimulateSameShapeNodeCount(input)

	if got.Status != NodeOptimizationFit {
		t.Fatalf("status = %q, want %q; blockers=%v", got.Status, NodeOptimizationFit, got.Blockers)
	}
	if !got.PlacementFound {
		t.Fatal("PlacementFound = false, want true")
	}
	if got.PlacedPodCount != len(input.Pods) {
		t.Fatalf("PlacedPodCount = %d, want %d", got.PlacedPodCount, len(input.Pods))
	}
	if got.CPUHeadroomMilli != 3500 {
		t.Fatalf("CPUHeadroomMilli = %d, want 3500", got.CPUHeadroomMilli)
	}
}

func TestSimulateSameShapeNodeCountBlocksAggregateCPU(t *testing.T) {
	input := NodeOptimizationInput{
		CurrentNodes:            7,
		CandidateNodes:          6,
		NodeCPUCapacityMilli:    1000,
		NodeMemoryCapacityBytes: 1024,
		Pods: []NodeOptimizationPodInput{
			{Name: "a", CPURequestMilli: 1000, MemoryRequestBytes: 100},
			{Name: "b", CPURequestMilli: 1000, MemoryRequestBytes: 100},
			{Name: "c", CPURequestMilli: 1000, MemoryRequestBytes: 100},
			{Name: "d", CPURequestMilli: 1000, MemoryRequestBytes: 100},
			{Name: "e", CPURequestMilli: 1000, MemoryRequestBytes: 100},
			{Name: "f", CPURequestMilli: 1000, MemoryRequestBytes: 100},
			{Name: "g", CPURequestMilli: 100, MemoryRequestBytes: 100},
		},
	}

	got := SimulateSameShapeNodeCount(input)

	if got.Status != NodeOptimizationBlockedAggregate {
		t.Fatalf("status = %q, want %q", got.Status, NodeOptimizationBlockedAggregate)
	}
	if got.PlacementFound {
		t.Fatal("PlacementFound = true, want false")
	}
	if got.CPUHeadroomMilli != -100 {
		t.Fatalf("CPUHeadroomMilli = %d, want -100", got.CPUHeadroomMilli)
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Reason != "aggregate_cpu_capacity" {
		t.Fatalf("blockers = %#v, want aggregate_cpu_capacity", got.Blockers)
	}
}

func TestSimulateSameShapeNodeCountBlocksAggregateMemory(t *testing.T) {
	input := NodeOptimizationInput{
		CurrentNodes:            3,
		CandidateNodes:          2,
		NodeCPUCapacityMilli:    4000,
		NodeMemoryCapacityBytes: 1000,
		Pods: []NodeOptimizationPodInput{
			{Name: "a", CPURequestMilli: 500, MemoryRequestBytes: 900},
			{Name: "b", CPURequestMilli: 500, MemoryRequestBytes: 900},
			{Name: "c", CPURequestMilli: 500, MemoryRequestBytes: 300},
		},
	}

	got := SimulateSameShapeNodeCount(input)

	if got.Status != NodeOptimizationBlockedAggregate {
		t.Fatalf("status = %q, want %q", got.Status, NodeOptimizationBlockedAggregate)
	}
	if got.MemoryHeadroomBytes != -100 {
		t.Fatalf("MemoryHeadroomBytes = %d, want -100", got.MemoryHeadroomBytes)
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Reason != "aggregate_memory_capacity" {
		t.Fatalf("blockers = %#v, want aggregate_memory_capacity", got.Blockers)
	}
}

func TestSimulateSameShapeNodeCountExactBoundaryFits(t *testing.T) {
	input := NodeOptimizationInput{
		CurrentNodes:            2,
		CandidateNodes:          2,
		NodeCPUCapacityMilli:    2000,
		NodeMemoryCapacityBytes: 2000,
		Pods: []NodeOptimizationPodInput{
			{Name: "a", CPURequestMilli: 2000, MemoryRequestBytes: 1000},
			{Name: "b", CPURequestMilli: 2000, MemoryRequestBytes: 1000},
			{Name: "c", CPURequestMilli: 0, MemoryRequestBytes: 1000},
			{Name: "d", CPURequestMilli: 0, MemoryRequestBytes: 1000},
		},
	}

	got := SimulateSameShapeNodeCount(input)

	if got.Status != NodeOptimizationFit {
		t.Fatalf("status = %q, want %q; blockers=%v", got.Status, NodeOptimizationFit, got.Blockers)
	}
	if got.CPUHeadroomMilli != 0 || got.MemoryHeadroomBytes != 0 {
		t.Fatalf("headroom = (%d CPU, %d memory), want (0, 0)", got.CPUHeadroomMilli, got.MemoryHeadroomBytes)
	}
}

func TestSimulateSameShapeNodeCountFragmentationReturnsPlacementNotFound(t *testing.T) {
	input := NodeOptimizationInput{
		CurrentNodes:            2,
		CandidateNodes:          2,
		NodeCPUCapacityMilli:    4000,
		NodeMemoryCapacityBytes: 4000,
		Pods: []NodeOptimizationPodInput{
			{Name: "large-a", CPURequestMilli: 3000, MemoryRequestBytes: 1000},
			{Name: "large-b", CPURequestMilli: 3000, MemoryRequestBytes: 1000},
			{Name: "small", CPURequestMilli: 2000, MemoryRequestBytes: 2000},
		},
	}

	got := SimulateSameShapeNodeCount(input)

	if got.TotalCPURequestMilli != got.CandidateCPUCapacityMilli {
		t.Fatalf("aggregate CPU does not exactly fit: request=%d capacity=%d", got.TotalCPURequestMilli, got.CandidateCPUCapacityMilli)
	}
	if got.Status != NodeOptimizationPlacementNotFound {
		t.Fatalf("status = %q, want %q", got.Status, NodeOptimizationPlacementNotFound)
	}
	if got.PlacementFound {
		t.Fatal("PlacementFound = true, want false")
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Reason != "heuristic_placement_failed" {
		t.Fatalf("blockers = %#v, want heuristic_placement_failed", got.Blockers)
	}
}

func TestSimulateSameShapeNodeCountRejectsPodLargerThanNode(t *testing.T) {
	input := NodeOptimizationInput{
		CurrentNodes:            2,
		CandidateNodes:          2,
		NodeCPUCapacityMilli:    2000,
		NodeMemoryCapacityBytes: 2000,
		Pods: []NodeOptimizationPodInput{
			{Namespace: "batch", Name: "oversized", CPURequestMilli: 2500, MemoryRequestBytes: 1000},
		},
	}

	got := SimulateSameShapeNodeCount(input)

	if got.Status != NodeOptimizationBlockedPodSize {
		t.Fatalf("status = %q, want %q", got.Status, NodeOptimizationBlockedPodSize)
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Pod != "batch/oversized" {
		t.Fatalf("blockers = %#v, want oversized pod evidence", got.Blockers)
	}
}

func TestSimulateSameShapeNodeCountIsDeterministicAcrossInputOrder(t *testing.T) {
	base := NodeOptimizationInput{
		CurrentNodes:            3,
		CandidateNodes:          2,
		NodeCPUCapacityMilli:    4000,
		NodeMemoryCapacityBytes: 4000,
		Pods: []NodeOptimizationPodInput{
			{Namespace: "a", Name: "one", CPURequestMilli: 1800, MemoryRequestBytes: 800},
			{Namespace: "b", Name: "two", CPURequestMilli: 1200, MemoryRequestBytes: 1600},
			{Namespace: "a", Name: "three", CPURequestMilli: 800, MemoryRequestBytes: 1200},
			{Namespace: "c", Name: "zero", CPURequestMilli: 0, MemoryRequestBytes: 0},
		},
	}

	reversed := base
	reversed.Pods = append([]NodeOptimizationPodInput(nil), base.Pods...)
	for i, j := 0, len(reversed.Pods)-1; i < j; i, j = i+1, j-1 {
		reversed.Pods[i], reversed.Pods[j] = reversed.Pods[j], reversed.Pods[i]
	}

	gotA := SimulateSameShapeNodeCount(base)
	gotB := SimulateSameShapeNodeCount(reversed)

	if !reflect.DeepEqual(gotA, gotB) {
		t.Fatalf("results differ by input order:\nA=%#v\nB=%#v", gotA, gotB)
	}
}

func TestSimulateSameShapeNodeCountRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input NodeOptimizationInput
	}{
		{
			name: "zero current nodes",
			input: NodeOptimizationInput{
				CurrentNodes:            0,
				CandidateNodes:          1,
				NodeCPUCapacityMilli:    1000,
				NodeMemoryCapacityBytes: 1000,
			},
		},
		{
			name: "zero candidate nodes",
			input: NodeOptimizationInput{
				CurrentNodes:            1,
				CandidateNodes:          0,
				NodeCPUCapacityMilli:    1000,
				NodeMemoryCapacityBytes: 1000,
			},
		},
		{
			name: "zero node CPU capacity",
			input: NodeOptimizationInput{
				CurrentNodes:            1,
				CandidateNodes:          1,
				NodeCPUCapacityMilli:    0,
				NodeMemoryCapacityBytes: 1000,
			},
		},
		{
			name: "zero node memory capacity",
			input: NodeOptimizationInput{
				CurrentNodes:            1,
				CandidateNodes:          1,
				NodeCPUCapacityMilli:    1000,
				NodeMemoryCapacityBytes: 0,
			},
		},
		{
			name: "negative pod request",
			input: NodeOptimizationInput{
				CurrentNodes:            1,
				CandidateNodes:          1,
				NodeCPUCapacityMilli:    1000,
				NodeMemoryCapacityBytes: 1000,
				Pods: []NodeOptimizationPodInput{
					{Name: "bad", CPURequestMilli: -1},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SimulateSameShapeNodeCount(tt.input)
			if got.Status != NodeOptimizationInvalidInput {
				t.Fatalf("status = %q, want %q", got.Status, NodeOptimizationInvalidInput)
			}
			if got.PlacementFound {
				t.Fatal("PlacementFound = true, want false")
			}
			if len(got.Blockers) == 0 {
				t.Fatal("expected explicit blocker")
			}
		})
	}
}

func TestSimulateSameShapeNodeCountAllCandidateEligibilityPreservesPlacement(t *testing.T) {
	base := NodeOptimizationInput{
		CurrentNodes:            3,
		CandidateNodes:          2,
		CandidateNodeNames:      []string{"node-b", "node-a"},
		NodeCPUCapacityMilli:    2000,
		NodeMemoryCapacityBytes: 2000,
		Pods: []NodeOptimizationPodInput{
			{Namespace: "apps", Name: "api", CPURequestMilli: 1200, MemoryRequestBytes: 800},
			{Namespace: "apps", Name: "worker", CPURequestMilli: 800, MemoryRequestBytes: 1200},
		},
	}
	explicit := base
	explicit.Pods = append([]NodeOptimizationPodInput(nil), base.Pods...)
	for i := range explicit.Pods {
		explicit.Pods[i].EligibleNodeNames = []string{"node-b", "node-a"}
	}

	unrestrictedResult := SimulateSameShapeNodeCount(base)
	explicitResult := SimulateSameShapeNodeCount(explicit)

	if !reflect.DeepEqual(unrestrictedResult, explicitResult) {
		t.Fatalf("all-node eligibility changed placement:\nunrestricted=%#v\nexplicit=%#v", unrestrictedResult, explicitResult)
	}
	if explicitResult.Status != NodeOptimizationFit {
		t.Fatalf("status = %q, want %q", explicitResult.Status, NodeOptimizationFit)
	}
}

func TestSimulateSameShapeNodeCountPlacesPodOnOnlyEligibleCandidate(t *testing.T) {
	got := SimulateSameShapeNodeCount(NodeOptimizationInput{
		CurrentNodes:            3,
		CandidateNodes:          2,
		CandidateNodeNames:      []string{"node-a", "node-b"},
		NodeCPUCapacityMilli:    2000,
		NodeMemoryCapacityBytes: 2000,
		Pods: []NodeOptimizationPodInput{
			{
				Namespace: "apps", Name: "pinned", CPURequestMilli: 1000, MemoryRequestBytes: 1000,
				EligibleNodeNames: []string{"node-b"},
			},
		},
	})

	if got.Status != NodeOptimizationFit || len(got.Placements) != 1 {
		t.Fatalf("result = %#v, want one successful placement", got)
	}
	if got.Placements[0].NodeName != "node-b" {
		t.Fatalf("placement node = %q, want node-b", got.Placements[0].NodeName)
	}
}

func TestSimulateSameShapeNodeCountPlacesOverlappingEligibleNodeSets(t *testing.T) {
	got := SimulateSameShapeNodeCount(NodeOptimizationInput{
		CurrentNodes:            4,
		CandidateNodes:          3,
		CandidateNodeNames:      []string{"node-c", "node-a", "node-b"},
		NodeCPUCapacityMilli:    1000,
		NodeMemoryCapacityBytes: 1000,
		Pods: []NodeOptimizationPodInput{
			{Name: "only-a", CPURequestMilli: 600, MemoryRequestBytes: 100, EligibleNodeNames: []string{"node-a"}},
			{Name: "a-or-b", CPURequestMilli: 400, MemoryRequestBytes: 100, EligibleNodeNames: []string{"node-b", "node-a"}},
			{Name: "b-or-c", CPURequestMilli: 600, MemoryRequestBytes: 100, EligibleNodeNames: []string{"node-c", "node-b"}},
		},
	})

	if got.Status != NodeOptimizationFit {
		t.Fatalf("status = %q, want %q; blockers=%v", got.Status, NodeOptimizationFit, got.Blockers)
	}
	allowed := map[string]map[string]bool{
		"only-a": {"node-a": true},
		"a-or-b": {"node-a": true, "node-b": true},
		"b-or-c": {"node-b": true, "node-c": true},
	}
	for _, placement := range got.Placements {
		if !allowed[placement.PodName][placement.NodeName] {
			t.Fatalf("placement %#v is outside the Pod eligible-node set", placement)
		}
	}
}

func TestSimulateSameShapeNodeCountBlocksEmptyEligibleNodeSet(t *testing.T) {
	got := SimulateSameShapeNodeCount(NodeOptimizationInput{
		CurrentNodes:            2,
		CandidateNodes:          1,
		CandidateNodeNames:      []string{"node-a"},
		NodeCPUCapacityMilli:    1000,
		NodeMemoryCapacityBytes: 1000,
		Pods: []NodeOptimizationPodInput{
			{Name: "blocked", CPURequestMilli: 100, MemoryRequestBytes: 100, EligibleNodeNames: []string{}},
		},
	})

	if got.Status != NodeOptimizationBlockedEligibility || got.PlacementFound {
		t.Fatalf("result = %#v, want hard eligible-node blocker", got)
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Reason != "no_eligible_candidate_nodes" {
		t.Fatalf("blockers = %#v, want no_eligible_candidate_nodes", got.Blockers)
	}
}

func TestSimulateSameShapeNodeCountEligibleSubsetCanCauseFragmentation(t *testing.T) {
	got := SimulateSameShapeNodeCount(NodeOptimizationInput{
		CurrentNodes:            3,
		CandidateNodes:          2,
		CandidateNodeNames:      []string{"node-a", "node-b"},
		NodeCPUCapacityMilli:    1000,
		NodeMemoryCapacityBytes: 1000,
		Pods: []NodeOptimizationPodInput{
			{Name: "a-one", CPURequestMilli: 600, MemoryRequestBytes: 100, EligibleNodeNames: []string{"node-a"}},
			{Name: "a-two", CPURequestMilli: 600, MemoryRequestBytes: 100, EligibleNodeNames: []string{"node-a"}},
		},
	})

	if got.TotalCPURequestMilli >= got.CandidateCPUCapacityMilli {
		t.Fatalf("aggregate CPU request=%d capacity=%d, want aggregate fit", got.TotalCPURequestMilli, got.CandidateCPUCapacityMilli)
	}
	if got.Status != NodeOptimizationPlacementNotFound || got.PlacementFound {
		t.Fatalf("result = %#v, want eligible-subset placement failure", got)
	}
}

func TestSimulateSameShapeNodeCountRejectsInvalidEligibleNodeEvidence(t *testing.T) {
	base := NodeOptimizationInput{
		CurrentNodes:            3,
		CandidateNodes:          2,
		CandidateNodeNames:      []string{"node-a", "node-b"},
		NodeCPUCapacityMilli:    1000,
		NodeMemoryCapacityBytes: 1000,
		Pods: []NodeOptimizationPodInput{
			{Name: "pod", CPURequestMilli: 100, MemoryRequestBytes: 100, EligibleNodeNames: []string{"node-a"}},
		},
	}

	tests := []struct {
		name   string
		mutate func(*NodeOptimizationInput)
	}{
		{"duplicate candidate identity", func(input *NodeOptimizationInput) {
			input.CandidateNodeNames = []string{"node-a", "node-a"}
		}},
		{"unknown eligible identity", func(input *NodeOptimizationInput) {
			input.Pods[0].EligibleNodeNames = []string{"node-c"}
		}},
		{"duplicate eligible identity", func(input *NodeOptimizationInput) {
			input.Pods[0].EligibleNodeNames = []string{"node-a", "node-a"}
		}},
		{"eligibility without candidate identities", func(input *NodeOptimizationInput) {
			input.CandidateNodeNames = nil
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := base
			input.CandidateNodeNames = append([]string(nil), base.CandidateNodeNames...)
			input.Pods = append([]NodeOptimizationPodInput(nil), base.Pods...)
			input.Pods[0].EligibleNodeNames = append([]string(nil), base.Pods[0].EligibleNodeNames...)
			tt.mutate(&input)

			got := SimulateSameShapeNodeCount(input)
			if got.Status != NodeOptimizationInvalidInput || got.PlacementFound {
				t.Fatalf("result = %#v, want invalid fail-closed result", got)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosAreDeterministicAcrossSnapshotOrder(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "z-0", NodePool: "z-pool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "a-0", NodePool: "a-pool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "z-1", NodePool: "z-pool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "a-1", NodePool: "a-pool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}
	pods := []corev1.Pod{
		optimizationTestPod("apps", "z-workload", "z-0", corev1.PodRunning, "1000m", "1Gi"),
		optimizationTestPod("apps", "a-workload", "a-0", corev1.PodRunning, "1000m", "1Gi"),
	}

	reversedNodes := append([]models.NodeInfo(nil), nodeInfos...)
	for i, j := 0, len(reversedNodes)-1; i < j; i, j = i+1, j-1 {
		reversedNodes[i], reversedNodes[j] = reversedNodes[j], reversedNodes[i]
	}
	reversedPods := append([]corev1.Pod(nil), pods...)
	for i, j := 0, len(reversedPods)-1; i < j; i, j = i+1, j-1 {
		reversedPods[i], reversedPods[j] = reversedPods[j], reversedPods[i]
	}

	forward := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)
	reversed := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(reversedNodes, reversedPods)
	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("snapshot order changed scenarios:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosFromSnapshots(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "user-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "user-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "user-2", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "system-0", NodePool: "systempool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pods := []corev1.Pod{
		optimizationTestPod("apps", "api-a", "user-0", corev1.PodRunning, "1500m", "2Gi"),
		optimizationTestPod("apps", "api-b", "user-1", corev1.PodRunning, "1500m", "2Gi"),
		optimizationTestPod("apps", "worker", "user-2", corev1.PodPending, "1000m", "1Gi"),
		optimizationTestPod("jobs", "complete", "user-0", corev1.PodSucceeded, "500m", "1Gi"),
		optimizationTestPod("apps", "unbound", "", corev1.PodPending, "500m", "1Gi"),
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0 because unresolved Pending demand must fail closed", len(got.Scenarios))
	}
	if got.ExcludedPodCount != 1 {
		t.Fatalf("ExcludedPodCount = %d, want 1", got.ExcludedPodCount)
	}
	if got.UnresolvedPodCount != 1 {
		t.Fatalf("UnresolvedPodCount = %d, want 1", got.UnresolvedPodCount)
	}
	if !got.EvidenceIncomplete {
		t.Fatal("EvidenceIncomplete = false, want true for unresolved Pending demand")
	}
	if got.UnresolvedNodeCount != 0 {
		t.Fatalf("UnresolvedNodeCount = %d, want 0", got.UnresolvedNodeCount)
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}
	if len(got.Warnings) == 0 || !strings.Contains(strings.Join(got.Warnings, " "), "unresolved for node optimization") {
		t.Fatalf("warnings = %#v, want unresolved Pod warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosSkipsInconsistentPoolCapacity(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-a", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-b", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 3.5, MemGBCapacity: 8},
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, nil)

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "inconsistent CPU or memory capacity") {
		t.Fatalf("warnings = %#v, want inconsistent-capacity warning", got.Warnings)
	}
}

func optimizationTestPod(namespace, name, nodeName string, phase corev1.PodPhase, cpu, memory string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpu),
							corev1.ResourceMemory: resource.MustParse(memory),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosModelsDaemonSetAsPerNodeOverhead(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-2", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pods := []corev1.Pod{
		optimizationTestDaemonSetPod("kube-system", "agent-a", "node-0", "500m", "1Gi"),
		optimizationTestDaemonSetPod("kube-system", "agent-b", "node-1", "500m", "1Gi"),
		optimizationTestDaemonSetPod("kube-system", "agent-c", "node-2", "500m", "1Gi"),
		optimizationTestPod("apps", "api-a", "node-0", corev1.PodRunning, "2500m", "2Gi"),
		optimizationTestPod("apps", "api-b", "node-1", corev1.PodRunning, "2500m", "2Gi"),
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}

	scenario := got.Scenarios[0]
	if scenario.DaemonSetCount != 1 {
		t.Fatalf("DaemonSetCount = %d, want 1", scenario.DaemonSetCount)
	}
	if scenario.DaemonSetCPUPerNodeMilli != 500 {
		t.Fatalf("DaemonSetCPUPerNodeMilli = %d, want 500", scenario.DaemonSetCPUPerNodeMilli)
	}
	if scenario.DaemonSetMemoryPerNodeBytes != 1024*1024*1024 {
		t.Fatalf("DaemonSetMemoryPerNodeBytes = %d, want 1Gi", scenario.DaemonSetMemoryPerNodeBytes)
	}
	if scenario.EligiblePodCount != 2 {
		t.Fatalf("EligiblePodCount = %d, want 2 movable pods", scenario.EligiblePodCount)
	}
	if scenario.Simulation.Status != NodeOptimizationFit {
		t.Fatalf("status = %q, want %q; blockers=%v", scenario.Simulation.Status, NodeOptimizationFit, scenario.Simulation.Blockers)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosDaemonSetOverheadCanBlockReduction(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-2", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pods := []corev1.Pod{
		optimizationTestDaemonSetPod("kube-system", "agent-a", "node-0", "1000m", "1Gi"),
		optimizationTestDaemonSetPod("kube-system", "agent-b", "node-1", "1000m", "1Gi"),
		optimizationTestDaemonSetPod("kube-system", "agent-c", "node-2", "1000m", "1Gi"),
		optimizationTestPod("apps", "api-a", "node-0", corev1.PodRunning, "3000m", "2Gi"),
		optimizationTestPod("apps", "api-b", "node-1", corev1.PodRunning, "3000m", "2Gi"),
		optimizationTestPod("apps", "api-c", "node-2", corev1.PodRunning, "1000m", "1Gi"),
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}
	if got.Scenarios[0].Simulation.Status != NodeOptimizationBlockedAggregate {
		t.Fatalf(
			"status = %q, want %q; blockers=%v",
			got.Scenarios[0].Simulation.Status,
			NodeOptimizationBlockedAggregate,
			got.Scenarios[0].Simulation.Blockers,
		)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosSkipsPartialDaemonSetScope(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-2", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pods := []corev1.Pod{
		optimizationTestDaemonSetPod("kube-system", "agent-a", "node-0", "500m", "1Gi"),
		optimizationTestDaemonSetPod("kube-system", "agent-b", "node-1", "500m", "1Gi"),
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "placement scope is not modeled yet") {
		t.Fatalf("warnings = %#v, want partial-scope warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiresDistinctDaemonSetNodeCoverage(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-2", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pods := []corev1.Pod{
		optimizationTestDaemonSetPod("kube-system", "agent-a", "node-0", "500m", "1Gi"),
		optimizationTestDaemonSetPod("kube-system", "agent-b", "node-0", "500m", "1Gi"),
		optimizationTestDaemonSetPod("kube-system", "agent-c", "node-1", "500m", "1Gi"),
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "2 of 3 distinct nodes") {
		t.Fatalf("warnings = %#v, want distinct-node coverage warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosSkipsInconsistentDaemonSetRequests(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pods := []corev1.Pod{
		optimizationTestDaemonSetPod("kube-system", "agent-a", "node-0", "500m", "1Gi"),
		optimizationTestDaemonSetPod("kube-system", "agent-b", "node-1", "750m", "1Gi"),
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "inconsistent effective CPU or memory requests") {
		t.Fatalf("warnings = %#v, want inconsistent-request warning", got.Warnings)
	}
}

func optimizationTestDaemonSetPod(namespace, name, nodeName, cpu, memory string) corev1.Pod {
	controller := true
	pod := optimizationTestPod(namespace, name, nodeName, corev1.PodRunning, cpu, memory)
	pod.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: "apps/v1",
			Kind:       "DaemonSet",
			Name:       "node-agent",
			UID:        types.UID("node-agent-uid"),
			Controller: &controller,
		},
	}
	return pod
}

func TestBuildNMinusOneNodeOptimizationScenariosAcceptsOSAndArchitectureNodeSelector(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.NodeSelector = map[string]string{
		corev1.LabelOSStable:   "linux",
		corev1.LabelArchStable: "amd64",
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pod})

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}
	if got.UnsupportedConstraintPodCount != 0 {
		t.Fatalf("UnsupportedConstraintPodCount = %d, want 0", got.UnsupportedConstraintPodCount)
	}
	if got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("status = %q, want %q", got.Scenarios[0].Simulation.Status, NodeOptimizationFit)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithEvidencePreservesCanonicalOSArchitectureSelector(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.NodeSelector = map[string]string{
		corev1.LabelOSStable:   "linux",
		corev1.LabelArchStable: "amd64",
	}

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

	if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("summary = %#v, want canonical OS/architecture selector to remain supported", got)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosSkipsMismatchedOSNodeSelector(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pod := optimizationTestPod("apps", "windows-only", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.NodeSelector = map[string]string{
		corev1.LabelOSStable: "windows",
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pod})

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.UnsupportedConstraintPodCount != 1 {
		t.Fatalf("UnsupportedConstraintPodCount = %d, want 1", got.UnsupportedConstraintPodCount)
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "does not match pool OS") {
		t.Fatalf("warnings = %#v, want OS mismatch warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosSkipsMismatchedArchitectureNodeSelector(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pod := optimizationTestPod("apps", "arm-only", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.NodeSelector = map[string]string{
		corev1.LabelArchStable: "arm64",
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pod})

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.UnsupportedConstraintPodCount != 1 || got.SkippedPoolCount != 1 {
		t.Fatalf("unsupported/skipped counts = %d/%d, want 1/1", got.UnsupportedConstraintPodCount, got.SkippedPoolCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "does not match pool architecture") {
		t.Fatalf("warnings = %#v, want architecture mismatch warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosSkipsUnsupportedNodeSelector(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pod := optimizationTestPod("apps", "zone-pinned", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.NodeSelector = map[string]string{
		"topology.kubernetes.io/zone": "centralus-1",
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pod})

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.UnsupportedConstraintPodCount != 1 {
		t.Fatalf("UnsupportedConstraintPodCount = %d, want 1", got.UnsupportedConstraintPodCount)
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "is not modeled by the same-shape simulator") {
		t.Fatalf("warnings = %#v, want unsupported nodeSelector warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosSkipsHostnameNodeSelector(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}

	pod := optimizationTestPod("apps", "host-pinned", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.NodeSelector = map[string]string{
		corev1.LabelHostname: "node-0",
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pod})

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.UnsupportedConstraintPodCount != 1 {
		t.Fatalf("UnsupportedConstraintPodCount = %d, want 1", got.UnsupportedConstraintPodCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "kubernetes.io/hostname") {
		t.Fatalf("warnings = %#v, want hostname selector warning", got.Warnings)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceJoinsAndCopiesNodeSnapshot(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{
			Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3",
			Region: "centralus", OS: "linux", Priority: "Regular",
			Provider: "azure", Architecture: "amd64",
		},
	}
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-0",
				Labels: map[string]string{
					corev1.LabelOSStable: "linux",
					"workload":           "general",
				},
			},
			Spec: corev1.NodeSpec{
				Unschedulable: true,
				Taints: []corev1.Taint{
					{
						Key:    "dedicated",
						Value:  "apps",
						Effect: corev1.TaintEffectNoSchedule,
					},
				},
			},
		},
	}

	got := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)

	if got.UnresolvedNodeCount != 0 {
		t.Fatalf("UnresolvedNodeCount = %d, want 0; warnings=%v", got.UnresolvedNodeCount, got.Warnings)
	}
	if got.UnmatchedSchedulingNodeCount != 0 {
		t.Fatalf("UnmatchedSchedulingNodeCount = %d, want 0", got.UnmatchedSchedulingNodeCount)
	}
	if len(got.Nodes) != 1 {
		t.Fatalf("node evidence count = %d, want 1", len(got.Nodes))
	}

	evidence := got.Nodes["node-0"]
	if evidence.PoolKey.PoolName != "userpool" || evidence.PoolKey.Architecture != "amd64" {
		t.Fatalf("PoolKey = %#v, want userpool/amd64", evidence.PoolKey)
	}
	if evidence.Labels["workload"] != "general" {
		t.Fatalf("labels = %#v, want workload=general", evidence.Labels)
	}
	if len(evidence.Taints) != 1 || evidence.Taints[0].Key != "dedicated" {
		t.Fatalf("taints = %#v, want dedicated taint", evidence.Taints)
	}
	if !evidence.Unschedulable {
		t.Fatal("Unschedulable = false, want copied cordon state")
	}

	nodes[0].Labels["workload"] = "mutated"
	nodes[0].Spec.Taints[0].Value = "mutated"
	if evidence.Labels["workload"] != "general" {
		t.Fatalf("evidence labels changed after source mutation: %#v", evidence.Labels)
	}
	if evidence.Taints[0].Value != "apps" {
		t.Fatalf("evidence taints changed after source mutation: %#v", evidence.Taints)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceReportsSnapshotMismatch(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{
			Name: "node-present", NodePool: "userpool", VMSize: "Standard_D4s_v3",
			Region: "centralus", OS: "linux", Priority: "Regular",
			Provider: "azure", Architecture: "amd64",
		},
		{
			Name: "node-missing", NodePool: "userpool", VMSize: "Standard_D4s_v3",
			Region: "centralus", OS: "linux", Priority: "Regular",
			Provider: "azure", Architecture: "amd64",
		},
	}
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-present"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-extra"}},
	}

	got := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)

	if len(got.Nodes) != 1 {
		t.Fatalf("node evidence count = %d, want 1", len(got.Nodes))
	}
	if got.UnresolvedNodeCount != 1 {
		t.Fatalf("UnresolvedNodeCount = %d, want 1", got.UnresolvedNodeCount)
	}
	if got.UnmatchedSchedulingNodeCount != 1 {
		t.Fatalf("UnmatchedSchedulingNodeCount = %d, want 1", got.UnmatchedSchedulingNodeCount)
	}
	if len(got.Warnings) != 2 {
		t.Fatalf("warnings = %#v, want 2 snapshot mismatch warnings", got.Warnings)
	}

	joined := strings.Join(got.Warnings, "\n")
	if !strings.Contains(joined, "node-missing") || !strings.Contains(joined, "node-extra") {
		t.Fatalf("warnings = %#v, want both missing and extra node names", got.Warnings)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceRejectsIncompletePoolIdentity(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{
			Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3",
			Region: "centralus", OS: "linux", Priority: "Regular",
			Provider: "", Architecture: "amd64",
		},
	}
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
	}

	got := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)

	if len(got.Nodes) != 0 {
		t.Fatalf("node evidence count = %d, want 0", len(got.Nodes))
	}
	if got.UnresolvedNodeCount != 1 {
		t.Fatalf("UnresolvedNodeCount = %d, want 1", got.UnresolvedNodeCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "provider") {
		t.Fatalf("warnings = %#v, want missing-provider warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceAllowsMatchingNoScheduleToleration(t *testing.T) {
	nodeInfos, nodes := optimizationTaintedPool(corev1.Taint{
		Key: "dedicated", Value: "apps", Effect: corev1.TaintEffectNoSchedule,
	})

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Tolerations = []corev1.Toleration{
		{
			Key: "dedicated", Operator: corev1.TolerationOpEqual,
			Value: "apps", Effect: corev1.TaintEffectNoSchedule,
		},
	}

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}
	if got.SchedulingBlockedPodCount != 0 {
		t.Fatalf("SchedulingBlockedPodCount = %d, want 0", got.SchedulingBlockedPodCount)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceBlocksUntoleratedNoSchedule(t *testing.T) {
	nodeInfos, nodes := optimizationTaintedPool(corev1.Taint{
		Key: "dedicated", Value: "apps", Effect: corev1.TaintEffectNoSchedule,
	})
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1", len(got.Scenarios))
	}
	if got.SchedulingBlockedPodCount != 1 {
		t.Fatalf("SchedulingBlockedPodCount = %d, want 1", got.SchedulingBlockedPodCount)
	}
	if got.Scenarios[0].Simulation.Status != NodeOptimizationBlockedEligibility {
		t.Fatalf("status = %q, want %q", got.Scenarios[0].Simulation.Status, NodeOptimizationBlockedEligibility)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceAllowsExistsToleration(t *testing.T) {
	nodeInfos, nodes := optimizationTaintedPool(corev1.Taint{
		Key: "dedicated", Value: "apps", Effect: corev1.TaintEffectNoSchedule,
	})

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Tolerations = []corev1.Toleration{
		{
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		},
	}

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceRejectsFiniteNoExecuteToleration(t *testing.T) {
	nodeInfos, nodes := optimizationTaintedPool(corev1.Taint{
		Key: "dedicated", Value: "apps", Effect: corev1.TaintEffectNoExecute,
	})

	seconds := int64(300)
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Tolerations = []corev1.Toleration{
		{
			Key: "dedicated", Operator: corev1.TolerationOpEqual,
			Value: "apps", Effect: corev1.TaintEffectNoExecute,
			TolerationSeconds: &seconds,
		},
	}

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1", len(got.Scenarios))
	}
	if got.SchedulingBlockedPodCount != 1 {
		t.Fatalf("SchedulingBlockedPodCount = %d, want 1", got.SchedulingBlockedPodCount)
	}
	if got.Scenarios[0].Simulation.Status != NodeOptimizationBlockedEligibility {
		t.Fatalf("status = %q, want %q", got.Scenarios[0].Simulation.Status, NodeOptimizationBlockedEligibility)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceUsesHeterogeneousHardTaintsAsEligibility(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-0"},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "dedicated", Value: "apps", Effect: corev1.TaintEffectNoSchedule},
			}},
		},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1", len(got.Scenarios))
	}
	if got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("status = %q, want %q", got.Scenarios[0].Simulation.Status, NodeOptimizationFit)
	}
	if got.Scenarios[0].RemovedNodeName != "node-0" {
		t.Fatalf("RemovedNodeName = %q, want node-0", got.Scenarios[0].RemovedNodeName)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceTreatsPreferNoScheduleAsCaveat(t *testing.T) {
	nodeInfos, nodes := optimizationTaintedPool(corev1.Taint{
		Key: "preferred", Value: "apps", Effect: corev1.TaintEffectPreferNoSchedule,
	})

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}
	if len(got.Scenarios[0].SchedulingCaveats) != 1 ||
		!strings.Contains(got.Scenarios[0].SchedulingCaveats[0], "PreferNoSchedule") {
		t.Fatalf("SchedulingCaveats = %#v, want PreferNoSchedule caveat", got.Scenarios[0].SchedulingCaveats)
	}
}

func optimizationTaintedPool(taint corev1.Taint) ([]models.NodeInfo, []corev1.Node) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-0"},
			Spec:       corev1.NodeSpec{Taints: []corev1.Taint{taint}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Spec:       corev1.NodeSpec{Taints: []corev1.Taint{taint}},
		},
	}
	return nodeInfos, nodes
}

func optimizationNodeInfos(count int) []models.NodeInfo {
	nodeInfos := make([]models.NodeInfo, 0, count)
	for i := 0; i < count; i++ {
		nodeInfos = append(nodeInfos, models.NodeInfo{
			Name:          fmt.Sprintf("node-%d", i),
			NodePool:      "userpool",
			VMSize:        "Standard_D4s_v3",
			Region:        "centralus",
			OS:            "linux",
			Priority:      "Regular",
			Provider:      "azure",
			Architecture:  "amd64",
			CPUCapacity:   4,
			MemGBCapacity: 8,
		})
	}
	return nodeInfos
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceAcceptsRequiredNodeAffinityMatchingAllNodes(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{"workload": "apps"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"workload": "apps"}}},
	}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Affinity = optimizationRequiredNodeAffinity(
		corev1.NodeSelectorRequirement{
			Key: "workload", Operator: corev1.NodeSelectorOpIn, Values: []string{"apps"},
		},
	)

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}
	if got.SchedulingBlockedPodCount != 0 || got.UnsupportedConstraintPodCount != 0 {
		t.Fatalf(
			"blocked=%d unsupported=%d, want 0/0",
			got.SchedulingBlockedPodCount,
			got.UnsupportedConstraintPodCount,
		)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceBlocksRequiredNodeAffinityMatchingNoNodes(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{"workload": "general"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"workload": "general"}}},
	}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Affinity = optimizationRequiredNodeAffinity(
		corev1.NodeSelectorRequirement{
			Key: "workload", Operator: corev1.NodeSelectorOpIn, Values: []string{"apps"},
		},
	)

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1", len(got.Scenarios))
	}
	if got.SchedulingBlockedPodCount != 1 {
		t.Fatalf("SchedulingBlockedPodCount = %d, want 1", got.SchedulingBlockedPodCount)
	}
	if got.Scenarios[0].Simulation.Status != NodeOptimizationBlockedEligibility {
		t.Fatalf("status = %q, want %q", got.Scenarios[0].Simulation.Status, NodeOptimizationBlockedEligibility)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidencePlacesPartialRequiredNodeAffinity(t *testing.T) {
	nodeInfos := optimizationNodeInfos(3)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{"zone": "a"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"zone": "a"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-2", Labels: map[string]string{"zone": "b"}}},
	}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Affinity = optimizationRequiredNodeAffinity(
		corev1.NodeSelectorRequirement{
			Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"},
		},
	)

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1", len(got.Scenarios))
	}
	if got.UnsupportedConstraintPodCount != 0 {
		t.Fatalf("UnsupportedConstraintPodCount = %d, want 0", got.UnsupportedConstraintPodCount)
	}
	if got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("status = %q, want %q", got.Scenarios[0].Simulation.Status, NodeOptimizationFit)
	}
	if got.Scenarios[0].RemovedNodeName != "node-0" {
		t.Fatalf("RemovedNodeName = %q, want node-0", got.Scenarios[0].RemovedNodeName)
	}
	if len(got.Scenarios[0].Simulation.Placements) != 1 ||
		got.Scenarios[0].Simulation.Placements[0].NodeName != "node-1" {
		t.Fatalf("placements = %#v, want affinity-compatible node-1", got.Scenarios[0].Simulation.Placements)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceSupportsRequiredAffinityExists(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{"gpu": "true"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"gpu": "false"}}},
	}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Affinity = optimizationRequiredNodeAffinity(
		corev1.NodeSelectorRequirement{
			Key: "gpu", Operator: corev1.NodeSelectorOpExists,
		},
	)

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceSkipsUnsupportedRequiredAffinityOperator(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{"workload": "apps"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"workload": "apps"}}},
	}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Affinity = optimizationRequiredNodeAffinity(
		corev1.NodeSelectorRequirement{
			Key: "workload", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"batch"},
		},
	)

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.UnsupportedConstraintPodCount != 1 {
		t.Fatalf("UnsupportedConstraintPodCount = %d, want 1", got.UnsupportedConstraintPodCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "operator NotIn is not modeled yet") {
		t.Fatalf("warnings = %#v, want unsupported-operator warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceSkipsRequiredAffinityMatchFields(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchFields: []corev1.NodeSelectorRequirement{
							{
								Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-0"},
							},
						},
					},
				},
			},
		},
	}

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos, []corev1.Pod{pod}, evidence,
	)

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.UnsupportedConstraintPodCount != 1 {
		t.Fatalf("UnsupportedConstraintPodCount = %d, want 1", got.UnsupportedConstraintPodCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "matchFields are not modeled yet") {
		t.Fatalf("warnings = %#v, want matchFields warning", got.Warnings)
	}
}

func optimizationRequiredNodeAffinity(
	requirements ...corev1.NodeSelectorRequirement,
) *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: requirements},
				},
			},
		},
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosFailsClosedOnDuplicateNodeInfoIdentity(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodeInfos[1].Name = nodeInfos[0].Name
	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, nil)

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if !got.EvidenceIncomplete || got.DuplicateNodeCount != 1 {
		t.Fatalf("EvidenceIncomplete=%v DuplicateNodeCount=%d, want true/1", got.EvidenceIncomplete, got.DuplicateNodeCount)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosFailsClosedOnDuplicatePodIdentity(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pod, pod})

	if len(got.Scenarios) != 0 || !got.EvidenceIncomplete || got.DuplicatePodCount != 1 {
		t.Fatalf("scenarios=%d EvidenceIncomplete=%v DuplicatePodCount=%d, want 0/true/1",
			len(got.Scenarios), got.EvidenceIncomplete, got.DuplicatePodCount)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosFailsClosedOnUnresolvedPendingPod(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	pod := optimizationTestPod("apps", "pending", "", corev1.PodPending, "1000m", "1Gi")
	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pod})

	if len(got.Scenarios) != 0 || !got.EvidenceIncomplete || got.UnresolvedPodCount != 1 {
		t.Fatalf("scenarios=%d EvidenceIncomplete=%v UnresolvedPodCount=%d, want 0/true/1",
			len(got.Scenarios), got.EvidenceIncomplete, got.UnresolvedPodCount)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosCarriesBoundUnknownPhaseWarningAsCaveat(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	pod := optimizationTestPod("apps", "unknown", "node-0", corev1.PodUnknown, "1000m", "1Gi")
	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pod})

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}
	if len(got.Scenarios[0].SchedulingCaveats) == 0 ||
		!strings.Contains(strings.Join(got.Scenarios[0].SchedulingCaveats, " "), "unknown") {
		t.Fatalf("SchedulingCaveats = %#v, want unknown-phase caveat", got.Scenarios[0].SchedulingCaveats)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceFailsClosedOnDuplicateRawNodeIdentity(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	got := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)

	if !got.EvidenceIncomplete || got.DuplicateNodeCount != 1 {
		t.Fatalf("EvidenceIncomplete=%v DuplicateNodeCount=%d, want true/1", got.EvidenceIncomplete, got.DuplicateNodeCount)
	}
	if _, ok := got.Nodes["node-0"]; ok {
		t.Fatal("duplicate raw node unexpectedly retained")
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosFailsClosedOnExtraSchedulingNode(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-extra"}},
	}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, nil, evidence)

	if len(got.Scenarios) != 0 || !got.EvidenceIncomplete {
		t.Fatalf("scenarios=%d EvidenceIncomplete=%v, want 0/true", len(got.Scenarios), got.EvidenceIncomplete)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceRejectsOSArchitectureDisagreement(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{
			corev1.LabelOSStable: "windows", corev1.LabelArchStable: "amd64",
		}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{
			corev1.LabelOSStable: "linux", corev1.LabelArchStable: "arm64",
		}}},
	}
	got := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)

	if !got.EvidenceIncomplete || got.UnresolvedNodeCount != 2 {
		t.Fatalf("EvidenceIncomplete=%v UnresolvedNodeCount=%d, want true/2",
			got.EvidenceIncomplete, got.UnresolvedNodeCount)
	}
	joined := strings.Join(got.Warnings, " ")
	if !strings.Contains(joined, "OS") || !strings.Contains(joined, "architecture") {
		t.Fatalf("warnings = %#v, want OS and architecture disagreement warnings", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceEmptyRequiredAffinityTermMatchesNoNodes(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}

	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{}},
			},
		},
	}

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos,
		[]corev1.Pod{pod},
		evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1", len(got.Scenarios))
	}
	if got.SchedulingBlockedPodCount != 1 {
		t.Fatalf("SchedulingBlockedPodCount = %d, want 1", got.SchedulingBlockedPodCount)
	}
	if got.Scenarios[0].Simulation.Status != NodeOptimizationBlockedEligibility {
		t.Fatalf("status = %q, want %q", got.Scenarios[0].Simulation.Status, NodeOptimizationBlockedEligibility)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceNoExecuteUnlimitedTolerationWinsRegardlessOfOrder(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-0"},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "dedicated", Value: "apps", Effect: corev1.TaintEffectNoExecute},
			}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "dedicated", Value: "apps", Effect: corev1.TaintEffectNoExecute},
			}},
		},
	}

	finite := int64(300)
	finiteMatch := corev1.Toleration{
		Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "apps",
		Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &finite,
	}
	unlimitedMatch := corev1.Toleration{
		Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "apps",
		Effect: corev1.TaintEffectNoExecute,
	}

	cases := []struct {
		name        string
		tolerations []corev1.Toleration
	}{
		{name: "finite-first", tolerations: []corev1.Toleration{finiteMatch, unlimitedMatch}},
		{name: "unlimited-first", tolerations: []corev1.Toleration{unlimitedMatch, finiteMatch}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := optimizationTestPod("apps", "api-"+tc.name, "node-0", corev1.PodRunning, "1000m", "1Gi")
			pod.Spec.Tolerations = tc.tolerations

			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
				nodeInfos,
				[]corev1.Pod{pod},
				evidence,
			)

			if len(got.Scenarios) != 1 {
				t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
			}
			if got.SchedulingBlockedPodCount != 0 {
				t.Fatalf("SchedulingBlockedPodCount = %d, want 0", got.SchedulingBlockedPodCount)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosSameNameDifferentUIDDaemonSetsDoNotCollapse(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)

	controller := true
	oldUID := types.UID("old-daemonset-uid")
	newUID := types.UID("new-daemonset-uid")

	oldPod := optimizationTestPod("apps", "agent-old", "node-0", corev1.PodRunning, "100m", "128Mi")
	oldPod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "DaemonSet",
		Name:       "agent",
		UID:        oldUID,
		Controller: &controller,
	}}

	newPod := optimizationTestPod("apps", "agent-new", "node-1", corev1.PodRunning, "100m", "128Mi")
	newPod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "DaemonSet",
		Name:       "agent",
		UID:        newUID,
		Controller: &controller,
	}}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(
		nodeInfos,
		[]corev1.Pod{oldPod, newPod},
	)

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}

	joined := strings.Join(got.Warnings, " ")
	if !strings.Contains(joined, "observed on 1 of 2 distinct nodes") {
		t.Fatalf("warnings = %#v, want partial DaemonSet generation evidence", got.Warnings)
	}
}

func TestNodeSelectorWarningDeterministicAcrossMapInsertionOrder(t *testing.T) {
	pool := CostPoolKey{
		OS:           "linux",
		Architecture: "amd64",
	}

	first := map[string]string{}
	first["z-custom"] = "one"
	first["a-custom"] = "two"

	second := map[string]string{}
	second["a-custom"] = "two"
	second["z-custom"] = "one"

	gotA := nodeSelectorCompatibilityReason(first, pool)
	gotB := nodeSelectorCompatibilityReason(second, pool)

	if gotA != gotB {
		t.Fatalf("warning differs by map insertion order: %q != %q", gotA, gotB)
	}
	if !strings.Contains(gotA, "a-custom") {
		t.Fatalf("warning = %q, want lexically first unsupported selector key", gotA)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWarningsDeterministicAcrossDaemonSetPodOrder(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	controller := true

	makeDaemonPod := func(name, node, uid string) corev1.Pod {
		pod := optimizationTestPod("apps", name, node, corev1.PodRunning, "100m", "128Mi")
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1",
			Kind:       "DaemonSet",
			Name:       name,
			UID:        types.UID(uid),
			Controller: &controller,
		}}
		return pod
	}

	pods := []corev1.Pod{
		makeDaemonPod("z-agent", "node-0", "uid-z"),
		makeDaemonPod("a-agent", "node-0", "uid-a"),
	}

	gotA := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)
	gotB := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pods[1], pods[0]})

	if !reflect.DeepEqual(gotA.Warnings, gotB.Warnings) {
		t.Fatalf("warnings differ by Pod order:\nA=%#v\nB=%#v", gotA.Warnings, gotB.Warnings)
	}
}

func TestSimulateSameShapeNodeCountInvalidPodSelectionDeterministicAcrossInputOrder(t *testing.T) {
	input := NodeOptimizationInput{
		CurrentNodes:            3,
		CandidateNodes:          2,
		NodeCPUCapacityMilli:    4000,
		NodeMemoryCapacityBytes: 8 * 1024 * 1024 * 1024,
		Pods: []NodeOptimizationPodInput{
			{Namespace: "z", Name: "negative-z", CPURequestMilli: -1, MemoryRequestBytes: 1},
			{Namespace: "a", Name: "negative-a", CPURequestMilli: 1, MemoryRequestBytes: -1},
			{Namespace: "m", Name: "valid", CPURequestMilli: 500, MemoryRequestBytes: 1024},
		},
	}

	reversed := input
	reversed.Pods = []NodeOptimizationPodInput{input.Pods[2], input.Pods[0], input.Pods[1]}

	gotA := SimulateSameShapeNodeCount(input)
	gotB := SimulateSameShapeNodeCount(reversed)

	if !reflect.DeepEqual(gotA, gotB) {
		t.Fatalf("result differs by Pod input order:\nA=%#v\nB=%#v", gotA, gotB)
	}
	if gotA.Status != NodeOptimizationInvalidInput {
		t.Fatalf("status = %q, want %q", gotA.Status, NodeOptimizationInvalidInput)
	}
	if len(gotA.Blockers) != 1 || gotA.Blockers[0].Pod != "a/negative-a" {
		t.Fatalf("blockers = %#v, want deterministic a/negative-a blocker", gotA.Blockers)
	}
}

func TestSimulateSameShapeNodeCountOversizedPodSelectionAndTotalsDeterministicAcrossInputOrder(t *testing.T) {
	input := NodeOptimizationInput{
		CurrentNodes:            3,
		CandidateNodes:          2,
		NodeCPUCapacityMilli:    4000,
		NodeMemoryCapacityBytes: 8 * 1024 * 1024 * 1024,
		Pods: []NodeOptimizationPodInput{
			{Namespace: "z", Name: "too-big-z", CPURequestMilli: 5000, MemoryRequestBytes: 1024},
			{Namespace: "a", Name: "too-big-a", CPURequestMilli: 4500, MemoryRequestBytes: 2048},
			{Namespace: "m", Name: "valid", CPURequestMilli: 500, MemoryRequestBytes: 4096},
		},
	}

	reversed := input
	reversed.Pods = []NodeOptimizationPodInput{input.Pods[2], input.Pods[0], input.Pods[1]}

	gotA := SimulateSameShapeNodeCount(input)
	gotB := SimulateSameShapeNodeCount(reversed)

	if !reflect.DeepEqual(gotA, gotB) {
		t.Fatalf("result differs by Pod input order:\nA=%#v\nB=%#v", gotA, gotB)
	}
	if gotA.Status != NodeOptimizationBlockedPodSize {
		t.Fatalf("status = %q, want %q", gotA.Status, NodeOptimizationBlockedPodSize)
	}
	if len(gotA.Blockers) != 1 || gotA.Blockers[0].Pod != "a/too-big-a" {
		t.Fatalf("blockers = %#v, want deterministic a/too-big-a blocker", gotA.Blockers)
	}

	wantCPU := int64(10000)
	wantMem := int64(7168)
	if gotA.TotalCPURequestMilli != wantCPU || gotA.TotalMemoryRequestBytes != wantMem {
		t.Fatalf(
			"totals = %dm/%d bytes, want %dm/%d bytes",
			gotA.TotalCPURequestMilli,
			gotA.TotalMemoryRequestBytes,
			wantCPU,
			wantMem,
		)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidencePlacesCustomNodeSelectorSubset(t *testing.T) {
	nodeInfos := optimizationNodeInfos(3)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{"disk": "fast"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"disk": "slow"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-2", Labels: map[string]string{"disk": "slow"}}},
	}
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.NodeSelector = map[string]string{"disk": "fast"}

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

	if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("summary = %#v, want a proven-fit scenario", got)
	}
	if got.UnsupportedConstraintPodCount != 0 {
		t.Fatalf("UnsupportedConstraintPodCount = %d, want 0", got.UnsupportedConstraintPodCount)
	}
	if got.Scenarios[0].RemovedNodeName != "node-1" {
		t.Fatalf("RemovedNodeName = %q, want node-1 after the node-0 removal attempt is blocked", got.Scenarios[0].RemovedNodeName)
	}
	if len(got.Scenarios[0].Simulation.Placements) != 1 ||
		got.Scenarios[0].Simulation.Placements[0].NodeName != "node-0" {
		t.Fatalf("placements = %#v, want custom-selector-compatible node-0", got.Scenarios[0].Simulation.Placements)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceCombinesTaintsAndRequiredAffinity(t *testing.T) {
	nodeInfos := optimizationNodeInfos(3)
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{"zone": "a"}},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "dedicated", Value: "other", Effect: corev1.TaintEffectNoSchedule},
			}},
		},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"zone": "a"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-2", Labels: map[string]string{"zone": "b"}}},
	}
	pod := optimizationTestPod("apps", "api", "node-1", corev1.PodRunning, "1000m", "1Gi")
	pod.Spec.Affinity = optimizationRequiredNodeAffinity(corev1.NodeSelectorRequirement{
		Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"},
	})

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

	if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("summary = %#v, want a proven-fit scenario", got)
	}
	placements := got.Scenarios[0].Simulation.Placements
	if len(placements) != 1 || placements[0].NodeName != "node-1" {
		t.Fatalf("placements = %#v, want the only taint-and-affinity-compatible node", placements)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosTreatsCordonedNodeAsIneligible(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}, Spec: corev1.NodeSpec{Unschedulable: true}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	pod := optimizationTestPod("apps", "api", "node-1", corev1.PodRunning, "1000m", "1Gi")

	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

	if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("summary = %#v, want a proven-fit scenario", got)
	}
	if got.Scenarios[0].RemovedNodeName != "node-0" {
		t.Fatalf("RemovedNodeName = %q, want cordoned node-0", got.Scenarios[0].RemovedNodeName)
	}
	placements := got.Scenarios[0].Simulation.Placements
	if len(placements) != 1 || placements[0].NodeName != "node-1" {
		t.Fatalf("placements = %#v, want schedulable node-1", placements)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithEligibleSetsIsDeterministicAcrossSnapshotOrder(t *testing.T) {
	nodeInfos := optimizationNodeInfos(3)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{"zone": "a", "disk": "fast"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"zone": "a", "disk": "slow"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-2", Labels: map[string]string{"zone": "b", "disk": "fast"}}},
	}
	affinityPod := optimizationTestPod("apps", "affinity", "node-0", corev1.PodRunning, "1000m", "1Gi")
	affinityPod.Spec.Affinity = optimizationRequiredNodeAffinity(corev1.NodeSelectorRequirement{
		Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"},
	})
	selectorPod := optimizationTestPod("apps", "selector", "node-2", corev1.PodRunning, "1000m", "1Gi")
	selectorPod.Spec.NodeSelector = map[string]string{"disk": "fast"}
	pods := []corev1.Pod{affinityPod, selectorPod}

	reversedNodeInfos := append([]models.NodeInfo(nil), nodeInfos...)
	reversedNodes := append([]corev1.Node(nil), nodes...)
	reversedPods := append([]corev1.Pod(nil), pods...)
	for i, j := 0, len(reversedNodeInfos)-1; i < j; i, j = i+1, j-1 {
		reversedNodeInfos[i], reversedNodeInfos[j] = reversedNodeInfos[j], reversedNodeInfos[i]
	}
	for i, j := 0, len(reversedNodes)-1; i < j; i, j = i+1, j-1 {
		reversedNodes[i], reversedNodes[j] = reversedNodes[j], reversedNodes[i]
	}
	for i, j := 0, len(reversedPods)-1; i < j; i, j = i+1, j-1 {
		reversedPods[i], reversedPods[j] = reversedPods[j], reversedPods[i]
	}

	forwardEvidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	reversedEvidence := BuildNodeOptimizationSchedulingEvidence(reversedNodeInfos, reversedNodes)
	forward := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, forwardEvidence)
	reversed := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(reversedNodeInfos, reversedPods, reversedEvidence)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("snapshot order changed eligible-node placement:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosFailsClosedOnDeferredSchedulingConstraints(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)

	tests := []struct {
		name       string
		configure  func(*corev1.Pod)
		wantReason string
	}{
		{
			name: "required pod anti-affinity",
			configure: func(pod *corev1.Pod) {
				pod.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname"}},
				}}
			},
			wantReason: "required pod anti-affinity",
		},
		{
			name: "persistent volume topology",
			configure: func(pod *corev1.Pod) {
				pod.Spec.Volumes = []corev1.Volume{{
					Name: "data",
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "data",
					}},
				}}
			},
			wantReason: "persistent volume topology",
		},
		{
			name: "extended resource request",
			configure: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Resources.Requests[corev1.ResourceName("example.com/device")] = resource.MustParse("1")
			},
			wantReason: "requested resource example.com/device",
		},
		{
			name: "host port",
			configure: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
			},
			wantReason: "host-port scheduling conflicts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "100m", "100Mi")
			tt.configure(&pod)
			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

			if len(got.Scenarios) != 0 || got.UnsupportedConstraintPodCount != 1 || got.SkippedPoolCount != 1 {
				t.Fatalf("summary = %#v, want one fail-closed unsupported constraint", got)
			}
			if !strings.Contains(strings.Join(got.Warnings, " "), tt.wantReason) {
				t.Fatalf("warnings = %#v, want %q", got.Warnings, tt.wantReason)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRejectsInvalidNodeCapacity(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*models.NodeInfo)
		wantWarning string
	}{
		{
			name:        "NaN CPU",
			mutate:      func(info *models.NodeInfo) { info.CPUCapacity = math.NaN() },
			wantWarning: "observed CPU capacity is non-finite, negative, or outside the int64 representable range",
		},
		{
			name:        "positive infinite memory",
			mutate:      func(info *models.NodeInfo) { info.MemGBCapacity = math.Inf(1) },
			wantWarning: "observed memory capacity is non-finite, negative, or outside the int64 representable range",
		},
		{
			name:        "negative CPU",
			mutate:      func(info *models.NodeInfo) { info.CPUCapacity = -1 },
			wantWarning: "observed CPU capacity is non-finite, negative, or outside the int64 representable range",
		},
		{
			name:        "float to int overflow",
			mutate:      func(info *models.NodeInfo) { info.CPUCapacity = math.Ldexp(1, 63) / 1000 },
			wantWarning: "observed CPU capacity is non-finite, negative, or outside the int64 representable range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfos := optimizationNodeInfos(2)
			tt.mutate(&nodeInfos[0])

			got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, nil)

			if !got.EvidenceIncomplete || len(got.Scenarios) != 0 {
				t.Fatalf("summary = %#v, want incomplete evidence and no scenario", got)
			}
			if !strings.Contains(strings.Join(got.Warnings, " "), tt.wantWarning) {
				t.Fatalf("warnings = %#v, want %q", got.Warnings, tt.wantWarning)
			}
		})
	}
}

func TestSimulateSameShapeNodeCountRejectsAggregateRequestOverflow(t *testing.T) {
	tests := []struct {
		name       string
		pods       []NodeOptimizationPodInput
		wantReason string
	}{
		{
			name: "CPU",
			pods: []NodeOptimizationPodInput{
				{Namespace: "apps", Name: "a", CPURequestMilli: math.MaxInt64},
				{Namespace: "apps", Name: "b", CPURequestMilli: 1},
			},
			wantReason: "aggregate_cpu_request_overflow",
		},
		{
			name: "memory",
			pods: []NodeOptimizationPodInput{
				{Namespace: "apps", Name: "a", MemoryRequestBytes: math.MaxInt64},
				{Namespace: "apps", Name: "b", MemoryRequestBytes: 1},
			},
			wantReason: "aggregate_memory_request_overflow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SimulateSameShapeNodeCount(NodeOptimizationInput{
				CurrentNodes:            2,
				CandidateNodes:          1,
				NodeCPUCapacityMilli:    math.MaxInt64,
				NodeMemoryCapacityBytes: math.MaxInt64,
				Pods:                    tt.pods,
			})

			if got.Status != NodeOptimizationInvalidInput || got.PlacementFound {
				t.Fatalf("result = %#v, want fail-closed invalid input", got)
			}
			if len(got.Blockers) != 1 || got.Blockers[0].Reason != tt.wantReason {
				t.Fatalf("blockers = %#v, want reason %q", got.Blockers, tt.wantReason)
			}
		})
	}
}

func TestSimulateSameShapeNodeCountRejectsCandidateCapacityOverflow(t *testing.T) {
	tests := []struct {
		name       string
		cpu        int64
		memory     int64
		wantReason string
	}{
		{name: "CPU", cpu: math.MaxInt64, memory: 1, wantReason: "candidate_cpu_capacity_overflow"},
		{name: "memory", cpu: 1, memory: math.MaxInt64, wantReason: "candidate_memory_capacity_overflow"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SimulateSameShapeNodeCount(NodeOptimizationInput{
				CurrentNodes:            3,
				CandidateNodes:          2,
				NodeCPUCapacityMilli:    tt.cpu,
				NodeMemoryCapacityBytes: tt.memory,
			})

			if got.Status != NodeOptimizationInvalidInput || got.PlacementFound {
				t.Fatalf("result = %#v, want fail-closed invalid input", got)
			}
			if len(got.Blockers) != 1 || got.Blockers[0].Reason != tt.wantReason {
				t.Fatalf("blockers = %#v, want reason %q", got.Blockers, tt.wantReason)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRejectsPodRequestOverflow(t *testing.T) {
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "9223372036854776", "1Gi")

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(optimizationNodeInfos(2), []corev1.Pod{pod})

	if !got.EvidenceIncomplete || len(got.Scenarios) != 0 {
		t.Fatalf("summary = %#v, want incomplete evidence and no scenario", got)
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "CPU request exceeds the int64 representable range") {
		t.Fatalf("warnings = %#v, want Pod request overflow warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRejectsEffectivePodRequestAdditionOverflow(t *testing.T) {
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "5000000000000000", "1")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: "sidecar",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("5000000000000000"),
			corev1.ResourceMemory: resource.MustParse("1"),
		}},
	})

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(optimizationNodeInfos(2), []corev1.Pod{pod})

	if !got.EvidenceIncomplete || len(got.Scenarios) != 0 {
		t.Fatalf("summary = %#v, want incomplete evidence and no scenario", got)
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "application CPU request overflows int64") {
		t.Fatalf("warnings = %#v, want effective request addition overflow warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRejectsNegativeAndOverheadRequests(t *testing.T) {
	tests := []struct {
		name        string
		pod         corev1.Pod
		wantWarning string
	}{
		{
			name:        "negative container request",
			pod:         optimizationTestPod("apps", "negative", "node-0", corev1.PodRunning, "-1m", "1Gi"),
			wantWarning: "CPU request is negative",
		},
		{
			name: "Pod overhead overflow",
			pod: func() corev1.Pod {
				pod := optimizationTestPod("apps", "overhead", "node-0", corev1.PodRunning, "9223372036854775", "1Gi")
				pod.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
				return pod
			}(),
			wantWarning: "effective CPU request overflows int64 while adding Pod overhead",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(optimizationNodeInfos(2), []corev1.Pod{tt.pod})

			if !got.EvidenceIncomplete || len(got.Scenarios) != 0 {
				t.Fatalf("summary = %#v, want incomplete evidence and no scenario", got)
			}
			if !strings.Contains(strings.Join(got.Warnings, " "), tt.wantWarning) {
				t.Fatalf("warnings = %#v, want %q", got.Warnings, tt.wantWarning)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRejectsDaemonSetOverheadOverflow(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	for i := range nodeInfos {
		nodeInfos[i].CPUCapacity = 9_000_000_000_000_000
	}
	pods := make([]corev1.Pod, 0, 4)
	controller := true
	for _, daemonSet := range []string{"agent-a", "agent-b"} {
		for node := 0; node < 2; node++ {
			pod := optimizationTestPod("system", fmt.Sprintf("%s-%d", daemonSet, node), fmt.Sprintf("node-%d", node), corev1.PodRunning, "5000000000000000", "1")
			pod.OwnerReferences = []metav1.OwnerReference{{
				Kind:       "DaemonSet",
				Name:       daemonSet,
				UID:        types.UID(daemonSet + "-uid"),
				Controller: &controller,
			}}
			pods = append(pods, pod)
		}
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)

	if !got.EvidenceIncomplete || len(got.Scenarios) != 0 || got.SkippedPoolCount != 1 {
		t.Fatalf("summary = %#v, want one fail-closed skipped pool", got)
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "DaemonSet per-node CPU overhead exceeds the int64 representable range") {
		t.Fatalf("warnings = %#v, want DaemonSet overflow warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRejectsDaemonSetOverheadExceedingCapacity(t *testing.T) {
	controller := true
	pods := make([]corev1.Pod, 0, 2)
	for node := 0; node < 2; node++ {
		pod := optimizationTestPod("system", fmt.Sprintf("agent-%d", node), fmt.Sprintf("node-%d", node), corev1.PodRunning, "5", "1Gi")
		pod.OwnerReferences = []metav1.OwnerReference{{
			Kind:       "DaemonSet",
			Name:       "agent",
			UID:        types.UID("agent-uid"),
			Controller: &controller,
		}}
		pods = append(pods, pod)
	}

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(optimizationNodeInfos(2), pods)

	if !got.EvidenceIncomplete || len(got.Scenarios) != 0 || got.SkippedPoolCount != 1 {
		t.Fatalf("summary = %#v, want one fail-closed skipped pool", got)
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "leaves no positive CPU or memory capacity") {
		t.Fatalf("warnings = %#v, want DaemonSet capacity warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosInvalidNumericDiagnosticsAreDeterministic(t *testing.T) {
	nodeInfos := optimizationNodeInfos(4)
	nodeInfos[0].CPUCapacity = math.NaN()
	nodeInfos[1].MemGBCapacity = math.Inf(1)

	cpuPod := optimizationTestPod("z", "cpu-overflow", "node-2", corev1.PodRunning, "5000000000000000", "1")
	cpuPod.Spec.Containers = append(cpuPod.Spec.Containers, corev1.Container{
		Name: "second",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("5000000000000000"),
		}},
	})
	memoryPod := optimizationTestPod("a", "memory-overflow", "node-3", corev1.PodRunning, "1m", "5000000000000000000")
	memoryPod.Spec.Containers = append(memoryPod.Spec.Containers, corev1.Container{
		Name: "second",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("5000000000000000000"),
		}},
	})
	pods := []corev1.Pod{cpuPod, memoryPod}

	forward := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)
	reversed := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(
		[]models.NodeInfo{nodeInfos[3], nodeInfos[2], nodeInfos[1], nodeInfos[0]},
		[]corev1.Pod{pods[1], pods[0]},
	)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("numeric diagnostics changed with Node/Pod order:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
	if !forward.EvidenceIncomplete || len(forward.Scenarios) != 0 {
		t.Fatalf("summary = %#v, want incomplete evidence and no scenario", forward)
	}
}

func TestInvalidNumericSimulatorInputsNeverProducePlacement(t *testing.T) {
	tests := []NodeOptimizationInput{
		{
			CurrentNodes: 2, CandidateNodes: 1,
			NodeCPUCapacityMilli: -1, NodeMemoryCapacityBytes: 1,
		},
		{
			CurrentNodes: 3, CandidateNodes: 2,
			NodeCPUCapacityMilli: math.MaxInt64, NodeMemoryCapacityBytes: 1,
		},
		{
			CurrentNodes: 2, CandidateNodes: 1,
			NodeCPUCapacityMilli: math.MaxInt64, NodeMemoryCapacityBytes: math.MaxInt64,
			Pods: []NodeOptimizationPodInput{
				{Name: "a", CPURequestMilli: math.MaxInt64},
				{Name: "b", CPURequestMilli: 1},
			},
		},
	}

	for i, input := range tests {
		if got := SimulateSameShapeNodeCount(input); got.PlacementFound {
			t.Fatalf("case %d produced a positive placement: %#v", i, got)
		}
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceExtractsStableTopology(t *testing.T) {
	nodeInfos := optimizationNodeInfos(1)
	nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{
		Name: "node-0",
		Labels: map[string]string{
			corev1.LabelHostname:       "worker-a",
			corev1.LabelTopologyZone:   "zone-a",
			corev1.LabelTopologyRegion: "centralus",
		},
	}}}

	got := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	topology := got.Nodes["node-0"].Topology

	if topology.Hostname != "worker-a" || topology.Zone != "zone-a" || topology.Region != "centralus" {
		t.Fatalf("topology = %#v, want stable hostname/zone/region", topology)
	}
	if !topology.complete() || got.TopologyEvidenceIncomplete || len(got.TopologyWarnings) != 0 {
		t.Fatalf("evidence = %#v, want complete topology", got)
	}
	if got.Nodes["node-0"].Name == topology.Hostname {
		t.Fatalf("node Name %q and topology hostname %q unexpectedly collapsed", got.Nodes["node-0"].Name, topology.Hostname)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceUsesDeprecatedTopologyFallback(t *testing.T) {
	nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{
		Name: "node-0",
		Labels: map[string]string{
			corev1.LabelHostname:                "worker-a",
			corev1.LabelFailureDomainBetaZone:   "zone-legacy",
			corev1.LabelFailureDomainBetaRegion: "centralus",
		},
	}}}

	got := BuildNodeOptimizationSchedulingEvidence(optimizationNodeInfos(1), nodes)
	topology := got.Nodes["node-0"].Topology

	if topology.Zone != "zone-legacy" || topology.Region != "centralus" || !topology.complete() {
		t.Fatalf("topology = %#v, want complete deprecated-label fallback", topology)
	}
	if len(got.TopologyWarnings) != 0 {
		t.Fatalf("TopologyWarnings = %#v, want none", got.TopologyWarnings)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceStableTopologyWinsMatchingDeprecated(t *testing.T) {
	nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{
		Name: "node-0",
		Labels: map[string]string{
			corev1.LabelHostname:                "worker-a",
			corev1.LabelTopologyZone:            "zone-a",
			corev1.LabelFailureDomainBetaZone:   "zone-a",
			corev1.LabelTopologyRegion:          "centralus",
			corev1.LabelFailureDomainBetaRegion: "centralus",
		},
	}}}

	got := BuildNodeOptimizationSchedulingEvidence(optimizationNodeInfos(1), nodes)
	topology := got.Nodes["node-0"].Topology

	if topology.Zone != "zone-a" || topology.Region != "centralus" || topology.Contradictory {
		t.Fatalf("topology = %#v, want matching stable values without contradiction", topology)
	}
	if got.TopologyEvidenceIncomplete || len(got.TopologyWarnings) != 0 {
		t.Fatalf("evidence = %#v, want complete topology", got)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceRejectsTopologyLabelConflicts(t *testing.T) {
	tests := []struct {
		name        string
		stableKey   string
		legacyKey   string
		wantValue   string
		wantWarning string
	}{
		{
			name:      "zone",
			stableKey: corev1.LabelTopologyZone, legacyKey: corev1.LabelFailureDomainBetaZone,
			wantValue: "stable-zone", wantWarning: "contradictory topology zone labels",
		},
		{
			name:      "region",
			stableKey: corev1.LabelTopologyRegion, legacyKey: corev1.LabelFailureDomainBetaRegion,
			wantValue: "centralus", wantWarning: "contradictory topology region labels",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := map[string]string{
				corev1.LabelHostname:       "worker-a",
				corev1.LabelTopologyZone:   "zone-a",
				corev1.LabelTopologyRegion: "centralus",
				tt.stableKey:               tt.wantValue,
				tt.legacyKey:               "legacy-conflict",
			}
			nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: labels}}}

			got := BuildNodeOptimizationSchedulingEvidence(optimizationNodeInfos(1), nodes)
			topology := got.Nodes["node-0"].Topology

			if !topology.Contradictory || !got.TopologyContradictory || !got.TopologyEvidenceIncomplete {
				t.Fatalf("evidence = %#v, want topology contradiction", got)
			}
			if (tt.name == "zone" && topology.Zone != tt.wantValue) || (tt.name == "region" && topology.Region != tt.wantValue) {
				t.Fatalf("topology = %#v, want stable value %q preserved", topology, tt.wantValue)
			}
			if !strings.Contains(strings.Join(got.TopologyWarnings, " "), tt.wantWarning) {
				t.Fatalf("TopologyWarnings = %#v, want %q", got.TopologyWarnings, tt.wantWarning)
			}
			requirement := nodeOptimizationTopologyRequirement{Zone: tt.name == "zone", Region: tt.name == "region"}
			if reason := topologyEvidenceRequirementReason(schedulingNodesForPool(got, got.Nodes["node-0"].PoolKey), requirement); reason == "" {
				t.Fatal("topology-dependent check accepted contradictory evidence")
			}
		})
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceReportsMissingTopologyDimensions(t *testing.T) {
	tests := []struct {
		name        string
		remove      func(map[string]string)
		wantWarning string
	}{
		{name: "hostname", remove: func(labels map[string]string) { delete(labels, corev1.LabelHostname) }, wantWarning: "missing topology hostname"},
		{name: "zone", remove: func(labels map[string]string) { delete(labels, corev1.LabelTopologyZone) }, wantWarning: "missing topology zone"},
		{name: "region", remove: func(labels map[string]string) { delete(labels, corev1.LabelTopologyRegion) }, wantWarning: "missing topology region"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := map[string]string{
				corev1.LabelHostname:       "worker-a",
				corev1.LabelTopologyZone:   "zone-a",
				corev1.LabelTopologyRegion: "centralus",
			}
			tt.remove(labels)
			nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: labels}}}

			got := BuildNodeOptimizationSchedulingEvidence(optimizationNodeInfos(1), nodes)

			if !got.TopologyEvidenceIncomplete || got.EvidenceIncomplete {
				t.Fatalf("evidence = %#v, want topology-only incompleteness", got)
			}
			if !strings.Contains(strings.Join(got.TopologyWarnings, " "), tt.wantWarning) {
				t.Fatalf("TopologyWarnings = %#v, want %q", got.TopologyWarnings, tt.wantWarning)
			}
		})
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceDetectsNodeInfoRegionMismatch(t *testing.T) {
	nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{
		Name: "node-0",
		Labels: map[string]string{
			corev1.LabelHostname:       "worker-a",
			corev1.LabelTopologyZone:   "zone-a",
			corev1.LabelTopologyRegion: "eastus",
		},
	}}}

	got := BuildNodeOptimizationSchedulingEvidence(optimizationNodeInfos(1), nodes)

	if !got.Nodes["node-0"].Topology.RegionContradictory || !got.TopologyContradictory {
		t.Fatalf("evidence = %#v, want raw Node/NodeInfo region contradiction", got)
	}
	if !strings.Contains(strings.Join(got.TopologyWarnings, " "), "disagrees with NodeInfo region") {
		t.Fatalf("TopologyWarnings = %#v, want NodeInfo mismatch warning", got.TopologyWarnings)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceTopologyIsDeterministicAcrossSnapshotOrder(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{
			corev1.LabelHostname:       "worker-a",
			corev1.LabelTopologyRegion: "centralus",
		}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{
			corev1.LabelHostname:                "worker-b",
			corev1.LabelTopologyZone:            "stable-zone",
			corev1.LabelFailureDomainBetaZone:   "legacy-zone",
			corev1.LabelTopologyRegion:          "centralus",
			corev1.LabelFailureDomainBetaRegion: "centralus",
		}}},
	}

	forward := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	reversed := BuildNodeOptimizationSchedulingEvidence(
		[]models.NodeInfo{nodeInfos[1], nodeInfos[0]},
		[]corev1.Node{nodes[1], nodes[0]},
	)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("topology evidence changed with snapshot order:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosAllowsMissingZoneAndRegion(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{corev1.LabelHostname: "worker-a"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{corev1.LabelHostname: "worker-b"}}},
	}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1", "1Gi")

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

	if !evidence.TopologyEvidenceIncomplete || evidence.EvidenceIncomplete {
		t.Fatalf("evidence = %#v, want topology-only incompleteness", evidence)
	}
	if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want non-topology placement to remain functional", got)
	}
}

func TestTopologyEvidenceRequirementRefusesIncompleteScope(t *testing.T) {
	nodes := []NodeOptimizationSchedulingNode{
		{Name: "node-b", Topology: NodeOptimizationTopology{Hostname: "worker-b", Region: "centralus"}},
		{Name: "node-a", Topology: NodeOptimizationTopology{Hostname: "worker-a", Region: "centralus"}},
	}

	reason := topologyEvidenceRequirementReason(nodes, nodeOptimizationTopologyRequirement{Zone: true})

	if reason != "candidate node node-a is missing zone topology evidence" {
		t.Fatalf("reason = %q, want deterministic missing-zone blocker", reason)
	}
}

func TestBuildNodeOptimizationSchedulingEvidenceDuplicateHostnameIsDeterministicAndScoped(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{
			corev1.LabelHostname: "shared-worker", corev1.LabelTopologyZone: "zone-a", corev1.LabelTopologyRegion: "centralus",
		}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{
			corev1.LabelHostname: "shared-worker", corev1.LabelTopologyZone: "zone-b", corev1.LabelTopologyRegion: "centralus",
		}}},
	}

	forward := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	reversed := BuildNodeOptimizationSchedulingEvidence(
		[]models.NodeInfo{nodeInfos[1], nodeInfos[0]},
		[]corev1.Node{nodes[1], nodes[0]},
	)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("duplicate-hostname evidence changed with snapshot order:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
	if forward.DuplicateTopologyHostnameCount != 1 || !forward.TopologyEvidenceIncomplete || forward.EvidenceIncomplete {
		t.Fatalf("evidence = %#v, want topology-only duplicate-hostname ambiguity", forward)
	}
	nodeList := schedulingNodesForPool(forward, forward.Nodes["node-0"].PoolKey)
	if reason := topologyEvidenceRequirementReason(nodeList, nodeOptimizationTopologyRequirement{Hostname: true}); !strings.Contains(reason, "ambiguous") {
		t.Fatalf("reason = %q, want ambiguous-hostname blocker", reason)
	}
	if reason := topologyEvidenceRequirementReason(nodeList, nodeOptimizationTopologyRequirement{Zone: true}); reason != "" {
		t.Fatalf("zone-only requirement was blocked by hostname ambiguity: %q", reason)
	}
}

func topologySpreadTestNode(name, pool, hostname, zone, region string) (models.NodeInfo, corev1.Node) {
	labels := make(map[string]string)
	if hostname != "" {
		labels[corev1.LabelHostname] = hostname
	}
	if zone != "" {
		labels[corev1.LabelTopologyZone] = zone
	}
	if region != "" {
		labels[corev1.LabelTopologyRegion] = region
	}
	return models.NodeInfo{
		Name: name, NodePool: pool, VMSize: "Standard_D4s_v3", Region: region,
		OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64",
		CPUCapacity: 4, MemGBCapacity: 8,
	}, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func topologySpreadTestPod(namespace, name, nodeName, topologyKey string, maxSkew int32) corev1.Pod {
	pod := optimizationTestPod(namespace, name, nodeName, corev1.PodRunning, "500m", "256Mi")
	pod.Labels = map[string]string{"app": "demo"}
	pod.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew:           maxSkew,
		TopologyKey:       topologyKey,
		WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo"}},
	}}
	return pod
}

func TestBuildNMinusOneNodeOptimizationScenariosHardTopologySpreadExactFit(t *testing.T) {
	t.Run("hostname", func(t *testing.T) {
		info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
		info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
		info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-a", "centralus")
		nodeInfos := []models.NodeInfo{info0, info1, info2}
		evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
		pods := []corev1.Pod{
			topologySpreadTestPod("apps", "api-0", "node-0", corev1.LabelHostname, 1),
			topologySpreadTestPod("apps", "api-1", "node-1", corev1.LabelHostname, 1),
		}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, evidence)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want hostname-spread fit", got)
		}
		placements := got.Scenarios[0].Simulation.Placements
		if len(placements) != 2 || placements[0].NodeName == placements[1].NodeName {
			t.Fatalf("placements = %#v, want distinct hostname domains", placements)
		}
	})

	t.Run("zone", func(t *testing.T) {
		info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
		info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
		info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
		nodeInfos := []models.NodeInfo{info0, info1, info2}
		evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
		pods := []corev1.Pod{
			topologySpreadTestPod("apps", "api-0", "node-0", corev1.LabelTopologyZone, 1),
			topologySpreadTestPod("apps", "api-1", "node-1", corev1.LabelTopologyZone, 1),
		}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, evidence)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want zone-spread fit", got)
		}
		placements := got.Scenarios[0].Simulation.Placements
		if len(placements) != 2 || placements[0].NodeName == placements[1].NodeName {
			t.Fatalf("placements = %#v, want placement across zone-a and zone-b", placements)
		}
	})

	t.Run("region", func(t *testing.T) {
		info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
		info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
		info2, node2 := topologySpreadTestNode("node-2", "otherpool", "worker-2", "zone-b", "eastus")
		nodeInfos := []models.NodeInfo{info0, info1, info2}
		evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
		fixed := optimizationTestPod("apps", "east-existing", "node-2", corev1.PodRunning, "500m", "256Mi")
		fixed.Labels = map[string]string{"app": "demo"}
		pods := []corev1.Pod{
			topologySpreadTestPod("apps", "api-0", "node-0", corev1.LabelTopologyRegion, 1),
			topologySpreadTestPod("apps", "api-1", "node-1", corev1.LabelTopologyRegion, 1),
			fixed,
		}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, evidence)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want region-spread fit", got)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosHardTopologySpreadBlocksSkewViolation(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "other-a", "worker-2", "zone-a", "centralus")
	info3, node3 := topologySpreadTestNode("node-3", "other-b", "worker-3", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2, info3}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2, node3})
	fixed := optimizationTestPod("apps", "existing", "node-2", corev1.PodRunning, "500m", "256Mi")
	fixed.Labels = map[string]string{"app": "demo"}
	pod := topologySpreadTestPod("apps", "api", "node-0", corev1.LabelTopologyZone, 1)

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod, fixed}, evidence)

	if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want topology-blocked scenario", got)
	}
	simulation := got.Scenarios[0].Simulation
	if simulation.Status != NodeOptimizationPlacementNotFound || len(simulation.Blockers) != 1 || simulation.Blockers[0].Reason != "topology_spread_constraint" {
		t.Fatalf("simulation = %#v, want topology spread blocker", simulation)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosHardTopologySpreadUsesAlternateCandidate(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	info3, node3 := topologySpreadTestNode("node-3", "otherpool", "worker-3", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2, info3}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2, node3})
	fixed := optimizationTestPod("apps", "existing", "node-3", corev1.PodRunning, "500m", "256Mi")
	fixed.Labels = map[string]string{"app": "demo"}
	pod := topologySpreadTestPod("apps", "api", "node-0", corev1.LabelTopologyZone, 1)

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod, fixed}, evidence)

	if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want proven alternate-candidate fit", got)
	}
	placements := got.Scenarios[0].Simulation.Placements
	if len(placements) != 1 || placements[0].NodeName != "node-2" {
		t.Fatalf("placements = %#v, want zone-b node-2", placements)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosTopologySpreadSelectorAndNamespaceScoping(t *testing.T) {
	tests := []struct {
		name           string
		fixedNamespace string
		fixedLabels    map[string]string
		selector       *metav1.LabelSelector
	}{
		{
			name:           "selector excludes unrelated Pod",
			fixedNamespace: "apps",
			fixedLabels:    map[string]string{"app": "other"},
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"demo"},
			}}},
		},
		{
			name:           "namespace excludes matching Pod",
			fixedNamespace: "other",
			fixedLabels:    map[string]string{"app": "demo"},
			selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
			info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
			info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
			info3, node3 := topologySpreadTestNode("node-3", "otherpool", "worker-3", "zone-a", "centralus")
			nodeInfos := []models.NodeInfo{info0, info1, info2, info3}
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2, node3})
			fixed := optimizationTestPod(tt.fixedNamespace, "existing", "node-3", corev1.PodRunning, "500m", "256Mi")
			fixed.Labels = tt.fixedLabels
			pod := topologySpreadTestPod("apps", "api", "node-0", corev1.LabelTopologyZone, 1)
			pod.Spec.TopologySpreadConstraints[0].LabelSelector = tt.selector

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod, fixed}, evidence)

			if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
				t.Fatalf("summary = %#v, want proven fit", got)
			}
			placements := got.Scenarios[0].Simulation.Placements
			if len(placements) != 1 || placements[0].NodeName != "node-1" {
				t.Fatalf("placements = %#v, want unrelated fixed Pod excluded and deterministic node-1", placements)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosTopologySpreadRequiredEvidenceFailsClosed(t *testing.T) {
	tests := []struct {
		name        string
		topologyKey string
		makeNodes   func() ([]models.NodeInfo, []corev1.Node)
		wantMessage string
	}{
		{
			name:        "missing zone",
			topologyKey: corev1.LabelTopologyZone,
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := make([]models.NodeInfo, 0, 3)
				nodes := make([]corev1.Node, 0, 3)
				for i := 0; i < 3; i++ {
					info, node := topologySpreadTestNode(fmt.Sprintf("node-%d", i), "userpool", fmt.Sprintf("worker-%d", i), "", "centralus")
					infos, nodes = append(infos, info), append(nodes, node)
				}
				return infos, nodes
			},
			wantMessage: "missing zone topology evidence",
		},
		{
			name:        "contradictory region",
			topologyKey: corev1.LabelTopologyRegion,
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := make([]models.NodeInfo, 0, 3)
				nodes := make([]corev1.Node, 0, 3)
				for i := 0; i < 3; i++ {
					info, node := topologySpreadTestNode(fmt.Sprintf("node-%d", i), "userpool", fmt.Sprintf("worker-%d", i), "zone-a", "centralus")
					node.Labels[corev1.LabelFailureDomainBetaRegion] = "eastus"
					infos, nodes = append(infos, info), append(nodes, node)
				}
				return infos, nodes
			},
			wantMessage: "contradictory region topology evidence",
		},
		{
			name:        "ambiguous hostname",
			topologyKey: corev1.LabelHostname,
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := make([]models.NodeInfo, 0, 3)
				nodes := make([]corev1.Node, 0, 3)
				for i := 0; i < 3; i++ {
					info, node := topologySpreadTestNode(fmt.Sprintf("node-%d", i), "userpool", "shared-worker", "zone-a", "centralus")
					infos, nodes = append(infos, info), append(nodes, node)
				}
				return infos, nodes
			},
			wantMessage: "ambiguous across candidate nodes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfos, nodes := tt.makeNodes()
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
			pod := topologySpreadTestPod("apps", "api", "node-0", tt.topologyKey, 1)

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

			if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
				t.Fatalf("summary = %#v, want fail-closed topology scenario", got)
			}
			simulation := got.Scenarios[0].Simulation
			if simulation.Status != NodeOptimizationInvalidInput || len(simulation.Blockers) != 1 || simulation.Blockers[0].Reason != "topology_spread_evidence" {
				t.Fatalf("simulation = %#v, want invalid topology evidence blocker", simulation)
			}
			if !strings.Contains(simulation.Blockers[0].Message, tt.wantMessage) {
				t.Fatalf("blocker = %#v, want %q", simulation.Blockers[0], tt.wantMessage)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosScheduleAnywayDoesNotBlock(t *testing.T) {
	nodeInfos := optimizationNodeInfos(2)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "500m", "256Mi")
	pod.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew: 0, TopologyKey: "unsupported.example/key", WhenUnsatisfiable: corev1.ScheduleAnyway,
		MatchLabelKeys: []string{"app"},
	}}

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

	if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want ScheduleAnyway ignored by hard placement", got)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRejectsUnsupportedHardTopologySpread(t *testing.T) {
	affinityIgnore := corev1.NodeInclusionPolicyIgnore
	taintsHonor := corev1.NodeInclusionPolicyHonor
	tests := []struct {
		name       string
		constraint corev1.TopologySpreadConstraint
		wantReason string
	}{
		{
			name:       "unsupported topology key",
			constraint: corev1.TopologySpreadConstraint{MaxSkew: 1, TopologyKey: "rack.example/id", WhenUnsatisfiable: corev1.DoNotSchedule},
			wantReason: "unsupported topologyKey",
		},
		{
			name: "unsupported selector operator",
			constraint: corev1.TopologySpreadConstraint{
				MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "app", Operator: metav1.LabelSelectorOperator("Invalid"), Values: []string{"demo"},
				}}},
			},
			wantReason: "invalid labelSelector",
		},
		{
			name:       "zero maxSkew",
			constraint: corev1.TopologySpreadConstraint{MaxSkew: 0, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule},
			wantReason: "nonpositive maxSkew",
		},
		{
			name:       "negative maxSkew",
			constraint: corev1.TopologySpreadConstraint{MaxSkew: -1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule},
			wantReason: "nonpositive maxSkew",
		},
		{
			name:       "matchLabelKeys",
			constraint: corev1.TopologySpreadConstraint{MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule, MatchLabelKeys: []string{"app"}},
			wantReason: "matchLabelKeys",
		},
		{
			name:       "node affinity Ignore",
			constraint: corev1.TopologySpreadConstraint{MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule, NodeAffinityPolicy: &affinityIgnore},
			wantReason: "nodeAffinityPolicy",
		},
		{
			name:       "node taints Honor",
			constraint: corev1.TopologySpreadConstraint{MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule, NodeTaintsPolicy: &taintsHonor},
			wantReason: "nodeTaintsPolicy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
			info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-b", "centralus")
			nodeInfos := []models.NodeInfo{info0, info1}
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1})
			pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "500m", "256Mi")
			pod.Labels = map[string]string{"app": "demo"}
			pod.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{tt.constraint}

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

			if len(got.Scenarios) != 0 || got.UnsupportedConstraintPodCount != 1 || got.SkippedPoolCount != 1 {
				t.Fatalf("summary = %#v, want unsupported hard topology constraint", got)
			}
			if !strings.Contains(strings.Join(got.Warnings, " "), tt.wantReason) {
				t.Fatalf("warnings = %#v, want %q", got.Warnings, tt.wantReason)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosHardTopologySpreadMinDomains(t *testing.T) {
	minDomains := int32(3)
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
	pods := []corev1.Pod{
		topologySpreadTestPod("apps", "api-0", "node-0", corev1.LabelTopologyZone, 1),
		topologySpreadTestPod("apps", "api-1", "node-1", corev1.LabelTopologyZone, 1),
		topologySpreadTestPod("apps", "api-2", "node-2", corev1.LabelTopologyZone, 1),
	}
	for i := range pods {
		pods[i].Spec.TopologySpreadConstraints[0].MinDomains = &minDomains
	}

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, evidence)

	if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want MinDomains-enforced placement failure", got)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosHardTopologySpreadIsDeterministic(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	nodes := []corev1.Node{node0, node1, node2}
	pods := []corev1.Pod{
		topologySpreadTestPod("apps", "api-0", "node-0", corev1.LabelTopologyZone, 1),
		topologySpreadTestPod("apps", "api-1", "node-1", corev1.LabelTopologyZone, 1),
	}

	forwardEvidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	forward := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, forwardEvidence)
	reversedNodeInfos := []models.NodeInfo{nodeInfos[2], nodeInfos[1], nodeInfos[0]}
	reversedNodes := []corev1.Node{nodes[2], nodes[1], nodes[0]}
	reversedPods := []corev1.Pod{pods[1], pods[0]}
	reversedEvidence := BuildNodeOptimizationSchedulingEvidence(reversedNodeInfos, reversedNodes)
	reversed := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(reversedNodeInfos, reversedPods, reversedEvidence)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("topology spread result changed with snapshot order:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
	if len(forward.Scenarios) != 1 || forward.Scenarios[0].RemovedNodeName != "node-0" || !forward.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want deterministic node-0 removal and fit", forward)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosNoTopologySpreadPreservesPlacement(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1})
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "500m", "256Mi")

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

	if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound || got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("summary = %#v, want unchanged non-topology fit", got)
	}
}
