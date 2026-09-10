package analyzer

import (
	"fmt"
	"math"
	"reflect"
	"sort"
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

func TestBuildNodeOptimizationRecommendationsSimulationPassed(t *testing.T) {
	nodeInfos := optimizationNodeInfos(3)
	pods := []corev1.Pod{
		optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "1000m", "1Gi"),
		optimizationTestPod("jobs", "worker", "node-1", corev1.PodRunning, "500m", "512Mi"),
	}

	summary := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)
	got := BuildNodeOptimizationRecommendations(summary, pods)

	if len(got) != 1 {
		t.Fatalf("recommendation count = %d, want 1", len(got))
	}
	recommendation := got[0]
	if recommendation.Status != NodeOptimizationRecommendationSimulationPassed {
		t.Fatalf("status = %q, want %q; blockers=%#v", recommendation.Status, NodeOptimizationRecommendationSimulationPassed, recommendation.Blockers)
	}
	if recommendation.Summary != "Scheduling feasibility proved under the supported model." {
		t.Fatalf("summary = %q, want exact read-only proof wording", recommendation.Summary)
	}
	if recommendation.CurrentNodeCount != 3 || recommendation.CandidateNodeCount != 2 {
		t.Fatalf("node counts = %d/%d, want 3/2", recommendation.CurrentNodeCount, recommendation.CandidateNodeCount)
	}
	if recommendation.CandidateRemovedNode != "node-0" {
		t.Fatalf("CandidateRemovedNode = %q, want deterministic node-0", recommendation.CandidateRemovedNode)
	}
	if recommendation.PodsConsidered != 2 || recommendation.PodsAssigned != 2 || len(recommendation.Assignments) != 2 {
		t.Fatalf("pod counts/assignments = %d/%d/%d, want 2/2/2", recommendation.PodsConsidered, recommendation.PodsAssigned, len(recommendation.Assignments))
	}
	wantAssignments := []NodeOptimizationRecommendationAssignment{
		{Namespace: "apps", PodName: "api", SourceNode: "node-0", DestinationNode: recommendation.Assignments[0].DestinationNode},
		{Namespace: "jobs", PodName: "worker", SourceNode: "node-1", DestinationNode: recommendation.Assignments[1].DestinationNode},
	}
	if !reflect.DeepEqual(recommendation.Assignments, wantAssignments) {
		t.Fatalf("assignments = %#v, want deterministic source-aware assignments %#v", recommendation.Assignments, wantAssignments)
	}
	for _, assignment := range recommendation.Assignments {
		if assignment.DestinationNode == "" || assignment.DestinationNode == recommendation.CandidateRemovedNode {
			t.Fatalf("assignment = %#v, want concrete retained-node destination", assignment)
		}
	}

	wantCPUCapacity := int64(8000)
	wantCPURequested := int64(1500)
	wantMemoryCapacity := int64(16 * 1024 * 1024 * 1024)
	wantMemoryRequested := int64(1536 * 1024 * 1024)
	if recommendation.RetainedCPUCapacityMilli != wantCPUCapacity || recommendation.AggregateCPURequestedMilli != wantCPURequested || recommendation.CPUHeadroomMilli != wantCPUCapacity-wantCPURequested {
		t.Fatalf("CPU capacity/request/headroom = %d/%d/%d, want %d/%d/%d", recommendation.RetainedCPUCapacityMilli, recommendation.AggregateCPURequestedMilli, recommendation.CPUHeadroomMilli, wantCPUCapacity, wantCPURequested, wantCPUCapacity-wantCPURequested)
	}
	if recommendation.RetainedMemoryCapacityBytes != wantMemoryCapacity || recommendation.AggregateMemoryRequestedBytes != wantMemoryRequested || recommendation.MemoryHeadroomBytes != wantMemoryCapacity-wantMemoryRequested {
		t.Fatalf("memory capacity/request/headroom = %d/%d/%d, want %d/%d/%d", recommendation.RetainedMemoryCapacityBytes, recommendation.AggregateMemoryRequestedBytes, recommendation.MemoryHeadroomBytes, wantMemoryCapacity, wantMemoryRequested, wantMemoryCapacity-wantMemoryRequested)
	}
	if recommendation.CPUHeadroomPercent == nil || *recommendation.CPUHeadroomPercent != 81.25 {
		t.Fatalf("CPUHeadroomPercent = %v, want 81.25", recommendation.CPUHeadroomPercent)
	}
	if recommendation.MemoryHeadroomPercent == nil || *recommendation.MemoryHeadroomPercent != 90.625 {
		t.Fatalf("MemoryHeadroomPercent = %v, want 90.625", recommendation.MemoryHeadroomPercent)
	}
	wantChecks := []NodeOptimizationVerifiedCheck{
		NodeOptimizationCheckCPUCapacity,
		NodeOptimizationCheckMemoryCapacity,
		NodeOptimizationCheckPodResourceRequests,
	}
	if !reflect.DeepEqual(recommendation.VerifiedChecks, wantChecks) {
		t.Fatalf("VerifiedChecks = %#v, want %#v", recommendation.VerifiedChecks, wantChecks)
	}
	wantNotEvaluated := []string{
		"application-level readiness",
		"cloud node provisioning or termination",
		"CSI volume attach or detach execution",
		"drain or eviction execution",
		"PodDisruptionBudget and disruption timing",
	}
	if !reflect.DeepEqual(recommendation.NotEvaluated, wantNotEvaluated) {
		t.Fatalf("NotEvaluated = %#v, want %#v", recommendation.NotEvaluated, wantNotEvaluated)
	}
}

func TestNodeOptimizationScenarioVerifiedChecksReflectEvaluatedConstraints(t *testing.T) {
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "100m", "128Mi")
	pod.Spec.NodeSelector = map[string]string{"workload": "general"}
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"zone-a"},
				}}}},
			},
		},
		PodAffinity: &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey:   "kubernetes.io/hostname",
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}},
		}}},
		PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey:   "topology.kubernetes.io/zone",
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
		}}},
	}
	pod.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: "topology.kubernetes.io/region", WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
	}}
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
	}}

	got := nodeOptimizationScenarioVerifiedChecks([]corev1.Pod{pod}, true, true, 1)
	want := []NodeOptimizationVerifiedCheck{
		NodeOptimizationCheckCordonState,
		NodeOptimizationCheckCPUCapacity,
		NodeOptimizationCheckDaemonSetOverhead,
		NodeOptimizationCheckTopologySpread,
		NodeOptimizationCheckMemoryCapacity,
		NodeOptimizationCheckNodeSelector,
		NodeOptimizationCheckPodResourceRequests,
		NodeOptimizationCheckPersistentVolume,
		NodeOptimizationCheckRequiredNodeAffinity,
		NodeOptimizationCheckRequiredPodAffinity,
		NodeOptimizationCheckRequiredPodAntiAffinity,
		NodeOptimizationCheckTaintsTolerations,
	}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checks = %#v, want %#v", got, want)
	}
}

func TestBuildNodeOptimizationRecommendationsClassifiesSupportedBlockers(t *testing.T) {
	tests := []struct {
		name     string
		status   NodeOptimizationStatus
		raw      string
		wantCode string
	}{
		{name: "insufficient CPU", status: NodeOptimizationBlockedAggregate, raw: "aggregate_cpu_capacity", wantCode: "insufficient_cpu"},
		{name: "required anti-affinity", status: NodeOptimizationPlacementNotFound, raw: "required_pod_anti_affinity", wantCode: "required_pod_anti_affinity_conflict"},
		{name: "topology spread", status: NodeOptimizationPlacementNotFound, raw: "topology_spread_constraint", wantCode: "topology_spread_conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := NodeOptimizationSnapshotSummary{Scenarios: []NodeOptimizationScenario{{
				RemovedNodeName:    "node-0",
				CandidateNodeNames: []string{"node-1", "node-2"},
				EligiblePodCount:   1,
				Simulation: NodeOptimizationResult{
					Status: tt.status, CurrentNodes: 3, CandidateNodes: 2, TotalPodCount: 1,
					CandidateCPUCapacityMilli: 8000, CandidateMemoryCapacityBytes: 16 * 1024,
					TotalCPURequestMilli: 9000, TotalMemoryRequestBytes: 1024,
					Blockers: []NodeOptimizationBlocker{{Reason: tt.raw, Pod: "apps/api", Message: "modeled constraint was not satisfied"}},
				},
			}}}
			got := BuildNodeOptimizationRecommendations(summary, nil)
			if len(got) != 1 || got[0].Status != NodeOptimizationRecommendationBlocked {
				t.Fatalf("recommendations = %#v, want one BLOCKED result", got)
			}
			if len(got[0].Blockers) != 1 || got[0].Blockers[0].Code != tt.wantCode || got[0].Blockers[0].Pod != "apps/api" || got[0].Blockers[0].Node != "node-0" {
				t.Fatalf("blockers = %#v, want code %q with Pod/node identity", got[0].Blockers, tt.wantCode)
			}
		})
	}
}

func TestBuildNodeOptimizationRecommendationsClassifiesIncompleteEvidenceAsPartial(t *testing.T) {
	tests := []struct {
		name     string
		warning  string
		wantCode string
	}{
		{name: "unsupported scheduling semantics", warning: "pool userpool skipped because a hard scheduling constraint is not modeled", wantCode: "unsupported_hard_scheduling_constraint"},
		{name: "unsupported CSI storage", warning: "pod apps/stateful: CSI storage topology cannot be proven from PVC and PV evidence", wantCode: "pvc_storage_mobility_unproven"},
		{name: "duplicate evidence", warning: "duplicate Pod identity apps/api makes scheduling evidence incomplete", wantCode: "incomplete_scheduling_evidence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := NodeOptimizationSnapshotSummary{
				EvidenceIncomplete:            true,
				SkippedPoolCount:              1,
				UnsupportedConstraintPodCount: 1,
				Warnings:                      []string{tt.warning},
			}
			got := BuildNodeOptimizationRecommendations(summary, nil)
			if len(got) != 1 || got[0].Status != NodeOptimizationRecommendationPartial {
				t.Fatalf("recommendations = %#v, want one PARTIAL result", got)
			}
			if got[0].Evidence.Complete || !got[0].Evidence.EvidenceIncomplete || got[0].Evidence.SkippedPoolCount != 1 {
				t.Fatalf("evidence = %#v, want explicit incomplete/skipped evidence", got[0].Evidence)
			}
			if len(got[0].Blockers) != 1 || got[0].Blockers[0].Code != tt.wantCode {
				t.Fatalf("blockers = %#v, want evidence reason %q", got[0].Blockers, tt.wantCode)
			}
			if len(got[0].NotEvaluated) == 0 {
				t.Fatal("NotEvaluated is empty, want centralized execution concerns")
			}
		})
	}
}

func TestBuildNodeOptimizationRecommendationsRejectsIncompleteAssignments(t *testing.T) {
	pods := []corev1.Pod{
		optimizationTestPod("apps", "api-a", "node-0", corev1.PodRunning, "100m", "128Mi"),
		optimizationTestPod("apps", "api-b", "node-1", corev1.PodRunning, "100m", "128Mi"),
	}
	summary := NodeOptimizationSnapshotSummary{Scenarios: []NodeOptimizationScenario{{
		RemovedNodeName:    "node-0",
		CandidateNodeNames: []string{"node-1", "node-2"},
		EligiblePodCount:   2,
		movablePodKeys:     []string{namespacedKey("apps", "api-a"), namespacedKey("apps", "api-b")},
		Simulation: NodeOptimizationResult{
			Status: NodeOptimizationFit, PlacementFound: true,
			CurrentNodes: 3, CandidateNodes: 2,
			TotalPodCount: 2, PlacedPodCount: 1,
			CandidateCPUCapacityMilli: 8000, CandidateMemoryCapacityBytes: 16 * 1024,
			TotalCPURequestMilli: 200, TotalMemoryRequestBytes: 256,
			CPUHeadroomMilli: 7800, MemoryHeadroomBytes: 16*1024 - 256,
			Placements: []NodeOptimizationPlacement{{Namespace: "apps", PodName: "api-a", NodeName: "node-1"}},
		},
	}}}

	got := BuildNodeOptimizationRecommendations(summary, pods)
	if len(got) != 1 || got[0].Status != NodeOptimizationRecommendationPartial {
		t.Fatalf("recommendations = %#v, want one PARTIAL result", got)
	}
	if len(got[0].Assignments) != 0 || got[0].PodsAssigned != 0 {
		t.Fatalf("assignments = %#v assigned=%d, want no partial assignment evidence", got[0].Assignments, got[0].PodsAssigned)
	}
	if len(got[0].Blockers) != 1 || got[0].Blockers[0].Code != "assignment_evidence_incomplete" {
		t.Fatalf("blockers = %#v, want assignment_evidence_incomplete", got[0].Blockers)
	}
}

func TestBuildNodeOptimizationRecommendationsNeverPassesWithIncompleteScenarioEvidence(t *testing.T) {
	nodeInfos := optimizationNodeInfos(3)
	pods := []corev1.Pod{
		optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "500m", "256Mi"),
	}
	summary := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)
	if len(summary.Scenarios) != 1 || !summary.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("precondition summary = %#v, want one positive simulation", summary)
	}
	summary.EvidenceIncomplete = true
	summary.UnresolvedNodeCount = 1
	summary.Warnings = []string{"scheduling evidence is incomplete for node-z"}
	summary.PoolEvidence = []NodeOptimizationPoolEvidence{{
		PoolKey:             summary.Scenarios[0].PoolKey,
		EvidenceIncomplete:  true,
		UnresolvedNodeCount: 1,
		Warnings:            []string{"scheduling evidence is incomplete for node-z"},
	}}

	got := BuildNodeOptimizationRecommendations(summary, pods)
	if len(got) != 1 || got[0].Status != NodeOptimizationRecommendationPartial {
		t.Fatalf("recommendations = %#v, want one PARTIAL result", got)
	}
	if len(got[0].Assignments) != 0 || got[0].PodsAssigned != 0 {
		t.Fatalf("assignments = %#v assigned=%d, want no positive proof with incomplete evidence", got[0].Assignments, got[0].PodsAssigned)
	}
}

func TestBuildNodeOptimizationRecommendationsRejectsWrongOrDuplicateAssignmentIdentity(t *testing.T) {
	pods := []corev1.Pod{
		optimizationTestPod("apps", "api-a", "node-0", corev1.PodRunning, "100m", "128Mi"),
		optimizationTestPod("apps", "api-b", "node-1", corev1.PodRunning, "100m", "128Mi"),
		optimizationTestPod("apps", "unrelated", "node-2", corev1.PodSucceeded, "100m", "128Mi"),
	}
	tests := []struct {
		name       string
		placements []NodeOptimizationPlacement
	}{
		{
			name: "unrelated Pod substitutes for movable Pod",
			placements: []NodeOptimizationPlacement{
				{Namespace: "apps", PodName: "api-a", NodeName: "node-1"},
				{Namespace: "apps", PodName: "unrelated", NodeName: "node-2"},
			},
		},
		{
			name: "duplicate Pod destinations",
			placements: []NodeOptimizationPlacement{
				{Namespace: "apps", PodName: "api-a", NodeName: "node-1"},
				{Namespace: "apps", PodName: "api-a", NodeName: "node-2"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := NodeOptimizationSnapshotSummary{Scenarios: []NodeOptimizationScenario{{
				RemovedNodeName:    "node-0",
				CandidateNodeNames: []string{"node-1", "node-2"},
				EligiblePodCount:   2,
				movablePodKeys:     []string{namespacedKey("apps", "api-a"), namespacedKey("apps", "api-b")},
				Simulation: NodeOptimizationResult{
					Status: NodeOptimizationFit, PlacementFound: true,
					CurrentNodes: 3, CandidateNodes: 2,
					TotalPodCount: 2, PlacedPodCount: 2,
					CandidateCPUCapacityMilli: 8000, CandidateMemoryCapacityBytes: 16 * 1024,
					TotalCPURequestMilli: 200, TotalMemoryRequestBytes: 256,
					CPUHeadroomMilli: 7800, MemoryHeadroomBytes: 16*1024 - 256,
					Placements: tt.placements,
				},
			}}}
			got := BuildNodeOptimizationRecommendations(summary, pods)
			if len(got) != 1 || got[0].Status != NodeOptimizationRecommendationPartial {
				t.Fatalf("recommendations = %#v, want one PARTIAL result", got)
			}
			if len(got[0].Assignments) != 0 || got[0].PodsAssigned != 0 {
				t.Fatalf("assignments = %#v assigned=%d, want no partial assignment evidence", got[0].Assignments, got[0].PodsAssigned)
			}
		})
	}
}

func TestBuildNodeOptimizationRecommendationsDeterministicAcrossSnapshotOrder(t *testing.T) {
	nodeInfos := optimizationNodeInfos(3)
	pods := []corev1.Pod{
		optimizationTestPod("zeta", "worker", "node-2", corev1.PodRunning, "500m", "512Mi"),
		optimizationTestPod("alpha", "api", "node-0", corev1.PodRunning, "1000m", "1Gi"),
	}
	forward := BuildNodeOptimizationRecommendations(
		BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods),
		pods,
	)
	reversedNodes := []models.NodeInfo{nodeInfos[2], nodeInfos[1], nodeInfos[0]}
	reversedPods := []corev1.Pod{pods[1], pods[0]}
	reversed := BuildNodeOptimizationRecommendations(
		BuildNMinusOneNodeOptimizationScenariosFromSnapshots(reversedNodes, reversedPods),
		reversedPods,
	)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("recommendations differ by snapshot order:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
}

func TestBuildNodeOptimizationRecommendationsLegacyObservationIsReadOnly(t *testing.T) {
	summary := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(optimizationNodeInfos(1), nil)
	got := BuildNodeOptimizationRecommendations(summary, nil)
	if len(got) != 1 || got[0].Status != NodeOptimizationRecommendationObservation {
		t.Fatalf("recommendations = %#v, want one OBSERVATION", got)
	}
	if len(got[0].NotEvaluated) == 0 {
		t.Fatal("NotEvaluated is empty, want execution concerns on observation")
	}
	language := strings.ToLower(fmt.Sprintf("%#v", got[0]))
	for _, prohibited := range []string{"actionable", "safe to remove", "ready to delete"} {
		if strings.Contains(language, prohibited) {
			t.Fatalf("recommendation contains prohibited execution wording %q: %s", prohibited, language)
		}
	}
}

func TestNodeOptimizationRecommendationContractContainsNoMonetaryFields(t *testing.T) {
	typeOf := reflect.TypeOf(NodeOptimizationRecommendation{})
	for i := 0; i < typeOf.NumField(); i++ {
		name := strings.ToLower(typeOf.Field(i).Name)
		for _, monetary := range []string{"cost", "dollar", "price", "saving"} {
			if strings.Contains(name, monetary) {
				t.Fatalf("recommendation field %q introduces monetary/provider-pricing output", typeOf.Field(i).Name)
			}
		}
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
