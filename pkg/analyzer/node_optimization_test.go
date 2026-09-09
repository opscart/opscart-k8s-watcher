package analyzer

import (
	"fmt"
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

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SchedulingBlockedPodCount != 1 {
		t.Fatalf("SchedulingBlockedPodCount = %d, want 1", got.SchedulingBlockedPodCount)
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "does not tolerate dedicated=apps:NoSchedule") {
		t.Fatalf("warnings = %#v, want untolerated NoSchedule warning", got.Warnings)
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

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SchedulingBlockedPodCount != 1 {
		t.Fatalf("SchedulingBlockedPodCount = %d, want 1", got.SchedulingBlockedPodCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "not durable steady-state placement evidence") {
		t.Fatalf("warnings = %#v, want finite NoExecute toleration warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceSkipsHeterogeneousHardTaints(t *testing.T) {
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

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SkippedPoolCount != 1 {
		t.Fatalf("SkippedPoolCount = %d, want 1", got.SkippedPoolCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "hard taints differ across nodes") {
		t.Fatalf("warnings = %#v, want heterogeneous hard-taint warning", got.Warnings)
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

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SchedulingBlockedPodCount != 1 {
		t.Fatalf("SchedulingBlockedPodCount = %d, want 1", got.SchedulingBlockedPodCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "matches no node in the pool") {
		t.Fatalf("warnings = %#v, want no-match affinity warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidenceSkipsPartialRequiredNodeAffinity(t *testing.T) {
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

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.UnsupportedConstraintPodCount != 1 {
		t.Fatalf("UnsupportedConstraintPodCount = %d, want 1", got.UnsupportedConstraintPodCount)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "matches 2 of 3 nodes") {
		t.Fatalf("warnings = %#v, want partial-affinity warning", got.Warnings)
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

	if len(got.Scenarios) != 0 {
		t.Fatalf("scenario count = %d, want 0", len(got.Scenarios))
	}
	if got.SchedulingBlockedPodCount != 1 {
		t.Fatalf("SchedulingBlockedPodCount = %d, want 1", got.SchedulingBlockedPodCount)
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "matches no node in the pool") {
		t.Fatalf("warnings = %#v, want no-match required-affinity warning", got.Warnings)
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
