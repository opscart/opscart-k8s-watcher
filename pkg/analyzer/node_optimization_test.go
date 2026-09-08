package analyzer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			name: "zero candidate nodes",
			input: NodeOptimizationInput{
				CurrentNodes:            1,
				CandidateNodes:          0,
				NodeCPUCapacityMilli:    1000,
				NodeMemoryCapacityBytes: 1000,
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

	if len(got.Scenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1; warnings=%v", len(got.Scenarios), got.Warnings)
	}
	if got.ExcludedPodCount != 1 {
		t.Fatalf("ExcludedPodCount = %d, want 1", got.ExcludedPodCount)
	}
	if got.UnresolvedPodCount != 1 {
		t.Fatalf("UnresolvedPodCount = %d, want 1", got.UnresolvedPodCount)
	}
	if got.UnresolvedNodeCount != 0 {
		t.Fatalf("UnresolvedNodeCount = %d, want 0", got.UnresolvedNodeCount)
	}
	if got.SkippedPoolCount != 0 {
		t.Fatalf("SkippedPoolCount = %d, want 0", got.SkippedPoolCount)
	}

	scenario := got.Scenarios[0]
	if scenario.PoolKey.PoolName != "userpool" {
		t.Fatalf("pool = %q, want userpool", scenario.PoolKey.PoolName)
	}
	if scenario.Simulation.CurrentNodes != 3 || scenario.Simulation.CandidateNodes != 2 {
		t.Fatalf("node counts = %d -> %d, want 3 -> 2", scenario.Simulation.CurrentNodes, scenario.Simulation.CandidateNodes)
	}
	if scenario.EligiblePodCount != 3 {
		t.Fatalf("EligiblePodCount = %d, want 3", scenario.EligiblePodCount)
	}
	if scenario.Simulation.TotalCPURequestMilli != 4000 {
		t.Fatalf("TotalCPURequestMilli = %d, want 4000", scenario.Simulation.TotalCPURequestMilli)
	}
	if scenario.Simulation.Status != NodeOptimizationFit {
		t.Fatalf("status = %q, want %q; blockers=%v", scenario.Simulation.Status, NodeOptimizationFit, scenario.Simulation.Blockers)
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
