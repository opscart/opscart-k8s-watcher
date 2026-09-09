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
