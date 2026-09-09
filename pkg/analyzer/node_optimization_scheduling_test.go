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
			name: "inline CSI volume",
			configure: func(pod *corev1.Pod) {
				pod.Spec.Volumes = []corev1.Volume{{
					Name:         "data",
					VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "storage.example.csi"}},
				}}
			},
			wantReason: "inline CSI volume scheduling",
		},
		{
			name: "generic ephemeral volume",
			configure: func(pod *corev1.Pod) {
				pod.Spec.Volumes = []corev1.Volume{{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}},
				}}
			},
			wantReason: "generic ephemeral volume scheduling",
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

func requiredAntiAffinityTestPod(
	namespace, name, nodeName, topologyKey string,
	selector *metav1.LabelSelector,
	namespaces []string,
) corev1.Pod {
	pod := optimizationTestPod(namespace, name, nodeName, corev1.PodRunning, "500m", "256Mi")
	pod.Labels = map[string]string{"app": "web"}
	pod.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: selector,
			Namespaces:    namespaces,
			TopologyKey:   topologyKey,
		}},
	}}
	return pod
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAntiAffinityHostname(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})

	t.Run("matching replicas cannot collapse onto one hostname", func(t *testing.T) {
		pods := []corev1.Pod{
			requiredAntiAffinityTestPod("apps", "web-0", "node-0", corev1.LabelHostname, selector, nil),
			requiredAntiAffinityTestPod("apps", "web-1", "node-1", corev1.LabelHostname, selector, nil),
			requiredAntiAffinityTestPod("apps", "web-2", "node-2", corev1.LabelHostname, selector, nil),
		}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, evidence)

		if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want three replicas blocked on two hostnames", got)
		}
		simulation := got.Scenarios[0].Simulation
		if simulation.Status != NodeOptimizationPlacementNotFound || len(simulation.Blockers) != 1 || simulation.Blockers[0].Reason != "required_pod_anti_affinity" {
			t.Fatalf("simulation = %#v, want required anti-affinity blocker", simulation)
		}
	})

	t.Run("alternate hostname succeeds", func(t *testing.T) {
		pods := []corev1.Pod{
			requiredAntiAffinityTestPod("apps", "web-0", "node-0", corev1.LabelHostname, selector, nil),
			requiredAntiAffinityTestPod("apps", "web-1", "node-1", corev1.LabelHostname, selector, nil),
		}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, evidence)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want proven alternate-hostname placement", got)
		}
		placements := got.Scenarios[0].Simulation.Placements
		if len(placements) != 2 || placements[0].NodeName == placements[1].NodeName {
			t.Fatalf("placements = %#v, want distinct hostname domains", placements)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAntiAffinityZoneAndRegion(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}

	t.Run("alternate zone succeeds", func(t *testing.T) {
		info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
		info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
		info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
		nodeInfos := []models.NodeInfo{info0, info1, info2}
		evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
		pods := []corev1.Pod{
			requiredAntiAffinityTestPod("apps", "web-0", "node-0", corev1.LabelTopologyZone, selector, nil),
			requiredAntiAffinityTestPod("apps", "web-1", "node-2", corev1.LabelTopologyZone, selector, nil),
		}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, evidence)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want zone-aware fit", got)
		}
		placements := got.Scenarios[0].Simulation.Placements
		if len(placements) != 2 || placements[0].NodeName != "node-1" || placements[1].NodeName != "node-2" {
			t.Fatalf("placements = %#v, want deterministic placement across zone-a and zone-b", placements)
		}
	})

	t.Run("same region blocks", func(t *testing.T) {
		info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
		info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-b", "centralus")
		nodeInfos := []models.NodeInfo{info0, info1}
		evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1})
		pods := []corev1.Pod{
			requiredAntiAffinityTestPod("apps", "web-0", "node-0", corev1.LabelTopologyRegion, selector, nil),
			requiredAntiAffinityTestPod("apps", "web-1", "node-1", corev1.LabelTopologyRegion, selector, nil),
		}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, evidence)

		if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want same-region anti-affinity block", got)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAntiAffinitySelectorAndNamespaces(t *testing.T) {
	tests := []struct {
		name              string
		incomingNamespace string
		fixedNamespace    string
		fixedLabels       map[string]string
		selector          *metav1.LabelSelector
		namespaces        []string
		wantPlacement     bool
	}{
		{
			name: "matchExpression excludes unrelated Pod", incomingNamespace: "apps", fixedNamespace: "apps",
			fixedLabels: map[string]string{"app": "other"},
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"web"},
			}}},
			wantPlacement: true,
		},
		{
			name: "same namespace default conflicts", incomingNamespace: "apps", fixedNamespace: "apps",
			fixedLabels: map[string]string{"app": "web"}, selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
		{
			name: "explicit namespace conflicts", incomingNamespace: "apps", fixedNamespace: "shared",
			fixedLabels: map[string]string{"app": "web"}, selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			namespaces: []string{"shared"},
		},
		{
			name: "unrelated namespace does not conflict", incomingNamespace: "apps", fixedNamespace: "other",
			fixedLabels: map[string]string{"app": "web"}, selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			wantPlacement: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
			info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
			info2, node2 := topologySpreadTestNode("node-2", "fixedpool", "worker-2", "zone-a", "centralus")
			nodeInfos := []models.NodeInfo{info0, info1, info2}
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
			incoming := requiredAntiAffinityTestPod(tt.incomingNamespace, "web", "node-0", corev1.LabelTopologyZone, tt.selector, tt.namespaces)
			fixed := optimizationTestPod(tt.fixedNamespace, "fixed", "node-2", corev1.PodRunning, "500m", "256Mi")
			fixed.Labels = tt.fixedLabels

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{incoming, fixed}, evidence)

			if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound != tt.wantPlacement {
				t.Fatalf("summary = %#v, want PlacementFound=%t", got, tt.wantPlacement)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAntiAffinityChecksBothDirections(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "fixedpool", "worker-2", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})

	t.Run("incoming Pod rejects fixed resident", func(t *testing.T) {
		incoming := requiredAntiAffinityTestPod("apps", "incoming", "node-0", corev1.LabelTopologyZone,
			&metav1.LabelSelector{MatchLabels: map[string]string{"app": "fixed"}}, nil)
		fixed := optimizationTestPod("apps", "fixed", "node-2", corev1.PodRunning, "500m", "256Mi")
		fixed.Labels = map[string]string{"app": "fixed"}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{incoming, fixed}, evidence)

		if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want incoming anti-affinity conflict", got)
		}
		if !strings.Contains(got.Scenarios[0].Simulation.Blockers[0].Message, "conflicts with resident pod") {
			t.Fatalf("blocker = %#v, want incoming-side conflict", got.Scenarios[0].Simulation.Blockers)
		}
	})

	t.Run("fixed resident rejects incoming Pod", func(t *testing.T) {
		incoming := optimizationTestPod("apps", "incoming", "node-0", corev1.PodRunning, "500m", "256Mi")
		incoming.Labels = map[string]string{"role": "client"}
		fixed := requiredAntiAffinityTestPod("apps", "guard", "node-2", corev1.LabelTopologyZone, selector, nil)
		fixed.Labels = map[string]string{"app": "guard"}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{incoming, fixed}, evidence)

		if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want resident anti-affinity conflict", got)
		}
		if !strings.Contains(got.Scenarios[0].Simulation.Blockers[0].Message, "resident pod apps/guard required anti-affinity rejects") {
			t.Fatalf("blocker = %#v, want resident-side conflict", got.Scenarios[0].Simulation.Blockers)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAntiAffinityTopologyEvidenceFailsClosed(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	tests := []struct {
		name        string
		topologyKey string
		makeNodes   func() ([]models.NodeInfo, []corev1.Node)
		wantMessage string
	}{
		{
			name: "missing zone", topologyKey: corev1.LabelTopologyZone, wantMessage: "missing zone topology evidence",
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos, nodes := optimizationNodeInfos(3), []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}}, {ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}, {ObjectMeta: metav1.ObjectMeta{Name: "node-2"}}}
				return infos, nodes
			},
		},
		{
			name: "contradictory region", topologyKey: corev1.LabelTopologyRegion, wantMessage: "contradictory region topology evidence",
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := optimizationNodeInfos(3)
				nodes := make([]corev1.Node, 0, 3)
				for i := 0; i < 3; i++ {
					nodes = append(nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i), Labels: map[string]string{
						corev1.LabelHostname: fmt.Sprintf("worker-%d", i), corev1.LabelTopologyRegion: "centralus", corev1.LabelFailureDomainBetaRegion: "eastus",
					}}})
				}
				return infos, nodes
			},
		},
		{
			name: "ambiguous hostname", topologyKey: corev1.LabelHostname, wantMessage: "ambiguous across candidate nodes",
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := optimizationNodeInfos(3)
				nodes := []corev1.Node{
					{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{corev1.LabelHostname: "shared"}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{corev1.LabelHostname: "shared"}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "node-2", Labels: map[string]string{corev1.LabelHostname: "shared"}}},
				}
				return infos, nodes
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfos, nodes := tt.makeNodes()
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
			pod := requiredAntiAffinityTestPod("apps", "web", "node-0", tt.topologyKey, selector, nil)

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

			if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
				t.Fatalf("summary = %#v, want fail-closed anti-affinity topology evidence", got)
			}
			simulation := got.Scenarios[0].Simulation
			if simulation.Status != NodeOptimizationInvalidInput || len(simulation.Blockers) != 1 || simulation.Blockers[0].Reason != "pod_anti_affinity_evidence" {
				t.Fatalf("simulation = %#v, want invalid anti-affinity evidence", simulation)
			}
			if !strings.Contains(simulation.Blockers[0].Message, tt.wantMessage) {
				t.Fatalf("blocker = %#v, want %q", simulation.Blockers[0], tt.wantMessage)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRejectsUnsupportedRequiredPodAntiAffinity(t *testing.T) {
	badOperator := metav1.LabelSelectorOperator("Invalid")
	tests := []struct {
		name       string
		configure  func(*corev1.PodAffinityTerm)
		wantReason string
	}{
		{name: "unsupported topology key", configure: func(term *corev1.PodAffinityTerm) { term.TopologyKey = "rack.example/id" }, wantReason: "unsupported topology key"},
		{name: "malformed selector", configure: func(term *corev1.PodAffinityTerm) {
			term.LabelSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: badOperator}}}
		}, wantReason: "invalid labelSelector"},
		{name: "namespace selector", configure: func(term *corev1.PodAffinityTerm) { term.NamespaceSelector = &metav1.LabelSelector{} }, wantReason: "namespaceSelector"},
		{name: "match label keys", configure: func(term *corev1.PodAffinityTerm) { term.MatchLabelKeys = []string{"app"} }, wantReason: "matchLabelKeys"},
		{name: "mismatch label keys", configure: func(term *corev1.PodAffinityTerm) { term.MismatchLabelKeys = []string{"tenant"} }, wantReason: "mismatchLabelKeys"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
			info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-b", "centralus")
			nodeInfos := []models.NodeInfo{info0, info1}
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1})
			pod := requiredAntiAffinityTestPod("apps", "web", "node-0", corev1.LabelHostname,
				&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, nil)
			tt.configure(&pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0])

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

			if len(got.Scenarios) != 0 || got.UnsupportedConstraintPodCount != 1 || got.SkippedPoolCount != 1 {
				t.Fatalf("summary = %#v, want unsupported required anti-affinity to skip every scenario", got)
			}
			if !strings.Contains(strings.Join(got.Warnings, " "), tt.wantReason) {
				t.Fatalf("warnings = %#v, want %q", got.Warnings, tt.wantReason)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAntiAffinityRequiresSchedulingEvidenceGlobally(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-2", NodePool: "fixedpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}
	incoming := optimizationTestPod("apps", "client", "node-0", corev1.PodRunning, "500m", "256Mi")
	incoming.Labels = map[string]string{"role": "client"}
	resident := requiredAntiAffinityTestPod("system", "guard", "node-2", corev1.LabelTopologyZone,
		&metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}, []string{"apps"})

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{incoming, resident})

	if len(got.Scenarios) != 0 || got.SkippedPoolCount != 1 {
		t.Fatalf("summary = %#v, want all candidate pools skipped without scheduling evidence", got)
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "required inter-Pod affinity needs scheduling topology evidence") {
		t.Fatalf("warnings = %#v, want required inter-Pod affinity evidence warning", got.Warnings)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAntiAffinityIsDeterministic(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	nodes := []corev1.Node{node0, node1, node2}
	pods := []corev1.Pod{
		requiredAntiAffinityTestPod("apps", "web-0", "node-0", corev1.LabelTopologyZone, selector, nil),
		requiredAntiAffinityTestPod("apps", "web-1", "node-2", corev1.LabelTopologyZone, selector, nil),
	}

	forwardEvidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
	forward := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, forwardEvidence)
	reversedNodeInfos := []models.NodeInfo{nodeInfos[2], nodeInfos[1], nodeInfos[0]}
	reversedNodes := []corev1.Node{nodes[2], nodes[1], nodes[0]}
	reversedPods := []corev1.Pod{pods[1], pods[0]}
	reversedEvidence := BuildNodeOptimizationSchedulingEvidence(reversedNodeInfos, reversedNodes)
	reversed := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(reversedNodeInfos, reversedPods, reversedEvidence)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("required anti-affinity result changed with snapshot order:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
	if len(forward.Scenarios) != 1 || forward.Scenarios[0].RemovedNodeName != "node-0" || !forward.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want deterministic node-0 removal and fit", forward)
	}
}

func TestNMinusOneRequiredPodAntiAffinityReassignsPodFromRemovedNode(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
	evacuated := requiredAntiAffinityTestPod("apps", "evacuated-web", "node-0", corev1.LabelTopologyZone, selector, nil)
	retained := optimizationTestPod("apps", "resident-web", "node-2", corev1.PodRunning, "1000m", "512Mi")
	retained.Labels = map[string]string{"app": "web"}

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos,
		[]corev1.Pod{evacuated, retained},
		evidence,
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("summary = %#v, want one N-1 scenario", got)
	}
	scenario := got.Scenarios[0]
	if scenario.RemovedNodeName != "node-0" || !scenario.Simulation.PlacementFound {
		t.Fatalf("scenario = %#v, want proven node-0 removal", scenario)
	}
	if scenario.Simulation.TotalPodCount != 2 || scenario.Simulation.PlacedPodCount != 2 || len(scenario.Simulation.Placements) != 2 {
		t.Fatalf("simulation = %#v, want every movable Pod placed exactly once", scenario.Simulation)
	}
	if scenario.Simulation.TotalCPURequestMilli != 1500 || scenario.Simulation.TotalMemoryRequestBytes != 768*1024*1024 {
		t.Fatalf(
			"totals = %dm CPU/%d bytes, want demand from both Pods including the removed-node Pod",
			scenario.Simulation.TotalCPURequestMilli,
			scenario.Simulation.TotalMemoryRequestBytes,
		)
	}

	placementsByPod := make(map[string][]string)
	for _, placement := range scenario.Simulation.Placements {
		placementsByPod[placement.PodName] = append(placementsByPod[placement.PodName], placement.NodeName)
		if placement.NodeName == scenario.RemovedNodeName {
			t.Fatalf("placement = %#v, removed node must not remain a destination", placement)
		}
	}
	if destinations := placementsByPod["evacuated-web"]; len(destinations) != 1 {
		t.Fatalf("placements = %#v, want exactly one destination for removed-node Pod", scenario.Simulation.Placements)
	}
	if placementsByPod["evacuated-web"][0] == placementsByPod["resident-web"][0] {
		t.Fatalf("placements = %#v, want removed-node Pod's zone anti-affinity preserved after reassignment", scenario.Simulation.Placements)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosMovableResidentAntiAffinityRejectsIncomingPod(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
	guard := requiredAntiAffinityTestPod("apps", "a-guard", "node-0", corev1.LabelHostname, selector, nil)
	guard.Labels = map[string]string{"app": "guard"}
	client := optimizationTestPod("apps", "z-client", "node-1", corev1.PodRunning, "500m", "256Mi")
	client.Labels = map[string]string{"role": "client"}

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{client, guard}, evidence)

	if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want movable-resident anti-affinity fit", got)
	}
	placements := got.Scenarios[0].Simulation.Placements
	if len(placements) != 2 || placements[0].NodeName == placements[1].NodeName {
		t.Fatalf("placements = %#v, want resident anti-affinity to separate the later incoming Pod", placements)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosDaemonSetResidentAntiAffinityRejectsIncomingPod(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "systempool", "worker-2", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
	daemon := optimizationTestDaemonSetPod("system", "guard", "node-2", "100m", "64Mi")
	daemon.Labels = map[string]string{"app": "guard"}
	daemon.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}},
			Namespaces:    []string{"apps"},
			TopologyKey:   corev1.LabelTopologyZone,
		}},
	}}
	incoming := optimizationTestPod("apps", "client", "node-0", corev1.PodRunning, "500m", "256Mi")
	incoming.Labels = map[string]string{"role": "client"}

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{incoming, daemon}, evidence)

	if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want DaemonSet resident anti-affinity conflict", got)
	}
	if !strings.Contains(got.Scenarios[0].Simulation.Blockers[0].Message, "resident pod system/guard required anti-affinity rejects") {
		t.Fatalf("blockers = %#v, want DaemonSet resident-side blocker", got.Scenarios[0].Simulation.Blockers)
	}
}

func TestRequiredPodAntiAffinitySupportsStandardMatchExpressions(t *testing.T) {
	tests := []struct {
		name     string
		operator metav1.LabelSelectorOperator
		values   []string
		labels   map[string]string
	}{
		{name: "In", operator: metav1.LabelSelectorOpIn, values: []string{"web"}, labels: map[string]string{"app": "web"}},
		{name: "NotIn", operator: metav1.LabelSelectorOpNotIn, values: []string{"other"}, labels: map[string]string{"app": "web"}},
		{name: "Exists", operator: metav1.LabelSelectorOpExists, labels: map[string]string{"app": "web"}},
		{name: "DoesNotExist", operator: metav1.LabelSelectorOpDoesNotExist, labels: map[string]string{"other": "value"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := requiredAntiAffinityTestPod("apps", "web", "node-0", corev1.LabelHostname,
				&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "app", Operator: tt.operator, Values: tt.values,
				}}}, nil)
			terms, reason := buildNodeOptimizationRequiredAntiAffinityTerms(pod)
			if reason != "" || len(terms) != 1 {
				t.Fatalf("terms = %#v, reason = %q, want one supported term", terms, reason)
			}
			target := nodeOptimizationTopologyPod{Namespace: "apps", Name: "target", Labels: tt.labels}
			if !requiredPodAffinityTermMatchesPod(terms[0], target) {
				t.Fatalf("term did not match target labels %#v", tt.labels)
			}
		})
	}
}

func requiredAffinityTestPod(
	namespace, name, nodeName, topologyKey string,
	selector *metav1.LabelSelector,
	namespaces []string,
) corev1.Pod {
	pod := optimizationTestPod(namespace, name, nodeName, corev1.PodRunning, "500m", "256Mi")
	pod.Labels = map[string]string{"app": "client"}
	pod.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: selector,
			Namespaces:    namespaces,
			TopologyKey:   topologyKey,
		}},
	}}
	return pod
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAffinityHostname(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})

	t.Run("fixed DaemonSet Pod satisfies hostname term", func(t *testing.T) {
		client := requiredAffinityTestPod("apps", "client", "node-0", corev1.LabelHostname, selector, nil)
		daemons := []corev1.Pod{
			optimizationTestDaemonSetPod("apps", "agent-0", "node-0", "100m", "64Mi"),
			optimizationTestDaemonSetPod("apps", "agent-1", "node-1", "100m", "64Mi"),
			optimizationTestDaemonSetPod("apps", "agent-2", "node-2", "100m", "64Mi"),
		}
		daemons[1].Labels = map[string]string{"app": "database"}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
			nodeInfos,
			append([]corev1.Pod{client}, daemons...),
			evidence,
		)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want hostname affinity satisfied by fixed DaemonSet Pod", got)
		}
		placements := got.Scenarios[0].Simulation.Placements
		if len(placements) != 1 || placements[0].NodeName != "node-1" {
			t.Fatalf("placements = %#v, want client placed with matching fixed Pod on node-1", placements)
		}
	})

	t.Run("no matching Pod blocks", func(t *testing.T) {
		client := requiredAffinityTestPod("apps", "client", "node-0", corev1.LabelHostname, selector, nil)

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{client}, evidence)

		if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want missing hostname-affinity match to block", got)
		}
		if got.Scenarios[0].Simulation.Blockers[0].Reason != "required_pod_affinity" {
			t.Fatalf("blockers = %#v, want required Pod affinity blocker", got.Scenarios[0].Simulation.Blockers)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAffinityZoneAndRegion(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}
	tests := []struct {
		name        string
		topologyKey string
		region      string
		zone        string
	}{
		{name: "zone", topologyKey: corev1.LabelTopologyZone, zone: "zone-a", region: "centralus"},
		{name: "region", topologyKey: corev1.LabelTopologyRegion, zone: "zone-b", region: "centralus"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
			info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
			info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
			info3, node3 := topologySpreadTestNode("node-3", "databasepool", "worker-3", tt.zone, tt.region)
			nodeInfos := []models.NodeInfo{info0, info1, info2, info3}
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2, node3})
			client := requiredAffinityTestPod("apps", "client", "node-0", tt.topologyKey, selector, nil)
			database := optimizationTestPod("apps", "database", "node-3", corev1.PodRunning, "500m", "256Mi")
			database.Labels = map[string]string{"app": "database"}

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{client, database}, evidence)

			if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
				t.Fatalf("summary = %#v, want %s affinity satisfied", got, tt.name)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAffinityUsesPlacementState(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})

	t.Run("previously placed movable Pod satisfies later Pod", func(t *testing.T) {
		database := optimizationTestPod("apps", "database", "node-2", corev1.PodRunning, "1000m", "512Mi")
		database.Labels = map[string]string{"app": "database"}
		client := requiredAffinityTestPod("apps", "client", "node-0", corev1.LabelTopologyZone, selector, nil)

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{client, database}, evidence)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want movable database to satisfy later client", got)
		}
		placements := got.Scenarios[0].Simulation.Placements
		if len(placements) != 2 || placements[0].NodeName != placements[1].NodeName {
			t.Fatalf("placements = %#v, want client co-located in movable database's assigned zone", placements)
		}
	})

	t.Run("old location never satisfies before movable Pod is placed", func(t *testing.T) {
		database := optimizationTestPod("apps", "database", "node-0", corev1.PodRunning, "500m", "256Mi")
		database.Labels = map[string]string{"app": "database"}
		client := requiredAffinityTestPod("apps", "client", "node-1", corev1.LabelTopologyZone, selector, nil)
		client.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1000m")

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos[:2], []corev1.Pod{client, database}, BuildNodeOptimizationSchedulingEvidence(nodeInfos[:2], []corev1.Node{node0, node1}))

		if len(got.Scenarios) != 1 || got.Scenarios[0].RemovedNodeName != "node-0" || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want removed-node database excluded from old occupancy", got)
		}
		if !strings.Contains(got.Scenarios[0].Simulation.Blockers[0].Message, "no resident Pod matching") {
			t.Fatalf("blockers = %#v, want no-resident affinity blocker", got.Scenarios[0].Simulation.Blockers)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAffinitySelectorAndNamespaces(t *testing.T) {
	tests := []struct {
		name              string
		clientNamespace   string
		residentNamespace string
		residentLabels    map[string]string
		selector          *metav1.LabelSelector
		namespaces        []string
		wantPlacement     bool
	}{
		{
			name: "selector excludes unrelated Pod", clientNamespace: "apps", residentNamespace: "apps",
			residentLabels: map[string]string{"app": "other"},
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"database"},
			}}},
		},
		{
			name: "same namespace default", clientNamespace: "apps", residentNamespace: "apps",
			residentLabels: map[string]string{"app": "database"}, selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}},
			wantPlacement: true,
		},
		{
			name: "explicit namespace", clientNamespace: "apps", residentNamespace: "shared",
			residentLabels: map[string]string{"app": "database"}, selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}},
			namespaces: []string{"shared", "shared"}, wantPlacement: true,
		},
		{
			name: "unrelated namespace does not satisfy", clientNamespace: "apps", residentNamespace: "other",
			residentLabels: map[string]string{"app": "database"}, selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
			info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
			info2, node2 := topologySpreadTestNode("node-2", "databasepool", "worker-2", "zone-a", "centralus")
			nodeInfos := []models.NodeInfo{info0, info1, info2}
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
			client := requiredAffinityTestPod(tt.clientNamespace, "client", "node-0", corev1.LabelTopologyZone, tt.selector, tt.namespaces)
			resident := optimizationTestPod(tt.residentNamespace, "database", "node-2", corev1.PodRunning, "500m", "256Mi")
			resident.Labels = tt.residentLabels

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{client, resident}, evidence)

			if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound != tt.wantPlacement {
				t.Fatalf("summary = %#v, want PlacementFound=%t", got, tt.wantPlacement)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAffinityMultipleTerms(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "databasepool", "worker-2", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
	newClient := func() corev1.Pod {
		client := optimizationTestPod("apps", "client", "node-0", corev1.PodRunning, "500m", "256Mi")
		client.Labels = map[string]string{"app": "client"}
		client.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}, TopologyKey: corev1.LabelTopologyZone},
				{LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "primary"}}, TopologyKey: corev1.LabelTopologyRegion},
			},
		}}
		return client
	}

	t.Run("all terms satisfied", func(t *testing.T) {
		resident := optimizationTestPod("apps", "database", "node-2", corev1.PodRunning, "500m", "256Mi")
		resident.Labels = map[string]string{"app": "database", "tier": "primary"}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{newClient(), resident}, evidence)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want both required terms satisfied", got)
		}
	})

	t.Run("one unsatisfied term blocks", func(t *testing.T) {
		resident := optimizationTestPod("apps", "database", "node-2", corev1.PodRunning, "500m", "256Mi")
		resident.Labels = map[string]string{"app": "database", "tier": "replica"}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{newClient(), resident}, evidence)

		if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want incomplete required term set blocked", got)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAffinityFirstPodException(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "otherpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})

	t.Run("self-matching first Pod is allowed", func(t *testing.T) {
		pod := requiredAffinityTestPod("apps", "web", "node-0", corev1.LabelTopologyZone, selector, nil)
		pod.Labels = map[string]string{"app": "web"}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want Kubernetes first-Pod self-affinity exception", got)
		}
	})

	t.Run("non-self-matching Pod is blocked", func(t *testing.T) {
		pod := requiredAffinityTestPod("apps", "client", "node-0", corev1.LabelTopologyZone, selector, nil)

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

		if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want non-self-matching affinity blocked", got)
		}
	})

	t.Run("matching resident elsewhere disables exception", func(t *testing.T) {
		pod := requiredAffinityTestPod("apps", "web", "node-0", corev1.LabelTopologyZone, selector, nil)
		pod.Labels = map[string]string{"app": "web"}
		resident := optimizationTestPod("apps", "existing-web", "node-2", corev1.PodRunning, "500m", "256Mi")
		resident.Labels = map[string]string{"app": "web"}

		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod, resident}, evidence)

		if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want existing global match to disable first-Pod exception", got)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAffinityTopologyEvidenceFailsClosed(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}
	tests := []struct {
		name        string
		topologyKey string
		makeNodes   func() ([]models.NodeInfo, []corev1.Node)
		wantMessage string
	}{
		{
			name: "missing zone", topologyKey: corev1.LabelTopologyZone, wantMessage: "missing zone topology evidence",
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := optimizationNodeInfos(3)
				nodes := []corev1.Node{
					{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{corev1.LabelHostname: "worker-0"}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{corev1.LabelHostname: "worker-1"}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "node-2", Labels: map[string]string{corev1.LabelHostname: "worker-2"}}},
				}
				return infos, nodes
			},
		},
		{
			name: "contradictory region", topologyKey: corev1.LabelTopologyRegion, wantMessage: "contradictory region topology evidence",
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := optimizationNodeInfos(3)
				nodes := make([]corev1.Node, 0, 3)
				for i := 0; i < 3; i++ {
					nodes = append(nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i), Labels: map[string]string{
						corev1.LabelHostname: fmt.Sprintf("worker-%d", i), corev1.LabelTopologyRegion: "centralus", corev1.LabelFailureDomainBetaRegion: "eastus",
					}}})
				}
				return infos, nodes
			},
		},
		{
			name: "ambiguous hostname", topologyKey: corev1.LabelHostname, wantMessage: "ambiguous across candidate nodes",
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := optimizationNodeInfos(3)
				nodes := []corev1.Node{
					{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{corev1.LabelHostname: "shared"}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{corev1.LabelHostname: "shared"}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "node-2", Labels: map[string]string{corev1.LabelHostname: "shared"}}},
				}
				return infos, nodes
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfos, nodes := tt.makeNodes()
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes)
			pod := requiredAffinityTestPod("apps", "client", "node-0", tt.topologyKey, selector, nil)

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

			if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
				t.Fatalf("summary = %#v, want fail-closed affinity topology evidence", got)
			}
			simulation := got.Scenarios[0].Simulation
			if simulation.Status != NodeOptimizationInvalidInput || len(simulation.Blockers) != 1 || simulation.Blockers[0].Reason != "pod_affinity_evidence" {
				t.Fatalf("simulation = %#v, want invalid affinity evidence", simulation)
			}
			if !strings.Contains(simulation.Blockers[0].Message, tt.wantMessage) {
				t.Fatalf("blocker = %#v, want %q", simulation.Blockers[0], tt.wantMessage)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRejectsUnsupportedRequiredPodAffinity(t *testing.T) {
	badOperator := metav1.LabelSelectorOperator("Invalid")
	tests := []struct {
		name       string
		configure  func(*corev1.PodAffinityTerm)
		wantReason string
	}{
		{name: "unsupported topology key", configure: func(term *corev1.PodAffinityTerm) { term.TopologyKey = "rack.example/id" }, wantReason: "unsupported topology key"},
		{name: "malformed selector", configure: func(term *corev1.PodAffinityTerm) {
			term.LabelSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: badOperator}}}
		}, wantReason: "invalid labelSelector"},
		{name: "namespace selector", configure: func(term *corev1.PodAffinityTerm) { term.NamespaceSelector = &metav1.LabelSelector{} }, wantReason: "namespaceSelector"},
		{name: "match label keys", configure: func(term *corev1.PodAffinityTerm) { term.MatchLabelKeys = []string{"app"} }, wantReason: "matchLabelKeys"},
		{name: "mismatch label keys", configure: func(term *corev1.PodAffinityTerm) { term.MismatchLabelKeys = []string{"tenant"} }, wantReason: "mismatchLabelKeys"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
			info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-b", "centralus")
			nodeInfos := []models.NodeInfo{info0, info1}
			evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1})
			pod := requiredAffinityTestPod("apps", "client", "node-0", corev1.LabelHostname,
				&metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}, nil)
			tt.configure(&pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0])

			got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

			if len(got.Scenarios) != 0 || got.UnsupportedConstraintPodCount != 1 || got.SkippedPoolCount != 1 {
				t.Fatalf("summary = %#v, want unsupported required affinity to skip every scenario", got)
			}
			if !strings.Contains(strings.Join(got.Warnings, " "), tt.wantReason) {
				t.Fatalf("warnings = %#v, want %q", got.Warnings, tt.wantReason)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAffinityRequiresSchedulingEvidenceGlobally(t *testing.T) {
	nodeInfos := []models.NodeInfo{
		{Name: "node-0", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
		{Name: "node-1", NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus", OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64", CPUCapacity: 4, MemGBCapacity: 8},
	}
	pod := requiredAffinityTestPod("apps", "client", "node-0", corev1.LabelHostname,
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}, nil)

	got := BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, []corev1.Pod{pod})

	if len(got.Scenarios) != 0 || got.SkippedPoolCount != 1 {
		t.Fatalf("summary = %#v, want missing scheduling evidence to block required affinity globally", got)
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "required inter-Pod affinity needs scheduling topology evidence") {
		t.Fatalf("warnings = %#v, want explicit missing inter-Pod affinity evidence warning", got.Warnings)
	}
}

func TestRequiredPodAffinitySupportsStandardMatchExpressions(t *testing.T) {
	tests := []struct {
		name     string
		operator metav1.LabelSelectorOperator
		values   []string
		labels   map[string]string
	}{
		{name: "In", operator: metav1.LabelSelectorOpIn, values: []string{"database"}, labels: map[string]string{"app": "database"}},
		{name: "NotIn", operator: metav1.LabelSelectorOpNotIn, values: []string{"other"}, labels: map[string]string{"app": "database"}},
		{name: "Exists", operator: metav1.LabelSelectorOpExists, labels: map[string]string{"app": "database"}},
		{name: "DoesNotExist", operator: metav1.LabelSelectorOpDoesNotExist, labels: map[string]string{"other": "value"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := requiredAffinityTestPod("apps", "client", "node-0", corev1.LabelHostname,
				&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "app", Operator: tt.operator, Values: tt.values,
				}}}, nil)
			terms, reason := buildNodeOptimizationRequiredAffinityTerms(pod)
			if reason != "" || len(terms) != 1 {
				t.Fatalf("terms = %#v, reason = %q, want one supported term", terms, reason)
			}
			target := nodeOptimizationTopologyPod{Namespace: "apps", Name: "target", Labels: tt.labels}
			if !requiredPodAffinityTermMatchesPod(terms[0], target) {
				t.Fatalf("term did not match target labels %#v", tt.labels)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosCombinesRequiredPodAffinityAndAntiAffinity(t *testing.T) {
	newSnapshot := func(guardZone string) ([]models.NodeInfo, []corev1.Node, []corev1.Pod) {
		info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
		info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
		info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
		info3, node3 := topologySpreadTestNode("node-3", "databasepool", "worker-3", "zone-a", "centralus")
		info4, node4 := topologySpreadTestNode("node-4", "guardpool", "worker-4", guardZone, "centralus")
		client := requiredAffinityTestPod("apps", "client", "node-0", corev1.LabelTopologyZone,
			&metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}, nil)
		client.Labels = map[string]string{"role": "client"}
		client.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "guard"}},
				TopologyKey:   corev1.LabelTopologyZone,
			}},
		}
		database := optimizationTestPod("apps", "database", "node-3", corev1.PodRunning, "500m", "256Mi")
		database.Labels = map[string]string{"app": "database"}
		guard := optimizationTestPod("apps", "guard", "node-4", corev1.PodRunning, "500m", "256Mi")
		guard.Labels = map[string]string{"app": "guard"}
		return []models.NodeInfo{info0, info1, info2, info3, info4}, []corev1.Node{node0, node1, node2, node3, node4}, []corev1.Pod{client, database, guard}
	}

	t.Run("candidate allowed only when both constraints pass", func(t *testing.T) {
		nodeInfos, nodes, pods := newSnapshot("zone-b")
		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes))

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want combined affinity and anti-affinity fit", got)
		}
		placements := got.Scenarios[0].Simulation.Placements
		if len(placements) != 1 || placements[0].NodeName != "node-1" {
			t.Fatalf("placements = %#v, want node-1 where affinity and anti-affinity both pass", placements)
		}
	})

	t.Run("anti-affinity rejects every affinity-compatible candidate", func(t *testing.T) {
		nodeInfos, nodes, pods := newSnapshot("zone-a")
		got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes))

		if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want combined constraints to block", got)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosRequiredPodAffinityIsDeterministic(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	info3, node3 := topologySpreadTestNode("node-3", "databasepool", "worker-3", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2, info3}
	nodes := []corev1.Node{node0, node1, node2, node3}
	client := requiredAffinityTestPod("apps", "client", "node-0", corev1.LabelTopologyZone,
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}, nil)
	database := optimizationTestPod("apps", "database", "node-3", corev1.PodRunning, "500m", "256Mi")
	database.Labels = map[string]string{"app": "database"}
	pods := []corev1.Pod{client, database}

	forward := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes))
	reversedNodeInfos := []models.NodeInfo{nodeInfos[3], nodeInfos[2], nodeInfos[1], nodeInfos[0]}
	reversedNodes := []corev1.Node{nodes[3], nodes[2], nodes[1], nodes[0]}
	reversedPods := []corev1.Pod{pods[1], pods[0]}
	reversed := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(reversedNodeInfos, reversedPods, BuildNodeOptimizationSchedulingEvidence(reversedNodeInfos, reversedNodes))

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("required affinity result changed with snapshot order:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
	if len(forward.Scenarios) != 1 || forward.Scenarios[0].RemovedNodeName != "node-0" || !forward.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want deterministic node-0 removal and fit", forward)
	}
}

func TestNMinusOneRequiredPodAffinityReassignsPodFromRemovedNode(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	info3, node3 := topologySpreadTestNode("node-3", "databasepool", "worker-3", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2, info3}
	nodes := []corev1.Node{node0, node1, node2, node3}
	evacuated := requiredAffinityTestPod("apps", "evacuated-client", "node-0", corev1.LabelTopologyZone,
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "database"}}, nil)
	database := optimizationTestPod("apps", "database", "node-3", corev1.PodRunning, "500m", "256Mi")
	database.Labels = map[string]string{"app": "database"}

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
		nodeInfos,
		[]corev1.Pod{evacuated, database},
		BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes),
	)

	if len(got.Scenarios) != 1 {
		t.Fatalf("summary = %#v, want one userpool scenario", got)
	}
	scenario := got.Scenarios[0]
	if scenario.RemovedNodeName != "node-0" || !scenario.Simulation.PlacementFound {
		t.Fatalf("scenario = %#v, want successful deterministic node-0 removal", scenario)
	}
	if scenario.Simulation.TotalCPURequestMilli != 500 || scenario.Simulation.TotalMemoryRequestBytes != 256*1024*1024 {
		t.Fatalf("simulation totals = CPU %dm memory %d, want removed-node Pod demand retained", scenario.Simulation.TotalCPURequestMilli, scenario.Simulation.TotalMemoryRequestBytes)
	}
	if len(scenario.Simulation.Placements) != 1 {
		t.Fatalf("placements = %#v, want exactly one destination for removed-node Pod", scenario.Simulation.Placements)
	}
	placement := scenario.Simulation.Placements[0]
	if placement.PodName != "evacuated-client" || placement.NodeName != "node-1" || placement.NodeName == scenario.RemovedNodeName {
		t.Fatalf("placement = %#v, want removed-node Pod reassigned into its required affinity zone", placement)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithoutPodAffinityPreservesPlacement(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1})
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "500m", "256Mi")

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

	if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound || got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("summary = %#v, want unchanged placement without Pod affinity", got)
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosWithoutPodAntiAffinityPreservesPlacement(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1}
	evidence := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1})
	pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "500m", "256Mi")

	got := BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, []corev1.Pod{pod}, evidence)

	if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound || got.Scenarios[0].Simulation.Status != NodeOptimizationFit {
		t.Fatalf("summary = %#v, want unchanged placement without Pod anti-affinity", got)
	}
}

func pvcBackedOptimizationTestPod(namespace, name, nodeName, claimName string) corev1.Pod {
	pod := optimizationTestPod(namespace, name, nodeName, corev1.PodRunning, "500m", "256Mi")
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: claimName,
		}},
	}}
	return pod
}

func boundOptimizationTestVolume(
	namespace, claimName, volumeName string,
	nodeAffinity *corev1.VolumeNodeAffinity,
) (corev1.PersistentVolumeClaim, corev1.PersistentVolume) {
	uid := "uid-" + namespace + "-" + claimName
	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: claimName, UID: types.UID(uid)},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:  volumeName,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	volume := corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{
				Server: "storage.internal", Path: "/" + volumeName,
			}},
			AccessModes:  []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			ClaimRef:     &corev1.ObjectReference{Namespace: namespace, Name: claimName, UID: types.UID(uid)},
			NodeAffinity: nodeAffinity,
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	return claim, volume
}

func optimizationTestVolumeNodeAffinity(
	key string,
	operator corev1.NodeSelectorOperator,
	values ...string,
) *corev1.VolumeNodeAffinity {
	return &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: key, Operator: operator, Values: values}},
	}}}}
}

func buildStorageAwareOptimizationScenarios(
	nodeInfos []models.NodeInfo,
	nodes []corev1.Node,
	pods []corev1.Pod,
	claims []corev1.PersistentVolumeClaim,
	volumes []corev1.PersistentVolume,
) NodeOptimizationSnapshotSummary {
	return BuildNMinusOneNodeOptimizationScenariosWithSchedulingAndStorageEvidence(
		nodeInfos,
		pods,
		BuildNodeOptimizationSchedulingEvidence(nodeInfos, nodes),
		BuildNodeOptimizationStorageEvidence(claims, volumes),
	)
}

func TestBuildNMinusOneNodeOptimizationScenariosStorageLegacyAndUnconstrained(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1}
	nodes := []corev1.Node{node0, node1}

	t.Run("Pod without PVC preserves legacy behavior", func(t *testing.T) {
		pod := optimizationTestPod("apps", "api", "node-0", corev1.PodRunning, "500m", "256Mi")
		got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{pod}, nil, nil)

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want unchanged non-storage placement", got)
		}
	})

	t.Run("Bound PVC with unconstrained PV preserves candidate eligibility", func(t *testing.T) {
		pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
		claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", nil)
		got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{pod}, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume})

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound {
			t.Fatalf("summary = %#v, want portable bound PV placement", got)
		}
		if placements := got.Scenarios[0].Simulation.Placements; len(placements) != 1 || placements[0].NodeName != "node-1" {
			t.Fatalf("placements = %#v, want reassignment to retained node-1", placements)
		}
	})
}

func TestPodStorageEligibleNodeNamesSupportsPVNodeAffinity(t *testing.T) {
	poolKey := CostPoolKey{Provider: "azure", PoolName: "userpool", InstanceType: "Standard_D4s_v3", CapacityType: "Regular", Region: "centralus", OS: "linux", Architecture: "amd64"}
	nodes := []NodeOptimizationSchedulingNode{
		{Name: "node-0", PoolKey: poolKey, Labels: map[string]string{corev1.LabelHostname: "worker-0", corev1.LabelTopologyZone: "zone-a", corev1.LabelTopologyRegion: "centralus", "storage.example/tier": "slow"}, Topology: NodeOptimizationTopology{Hostname: "worker-0", Zone: "zone-a", Region: "centralus"}},
		{Name: "node-1", PoolKey: poolKey, Labels: map[string]string{corev1.LabelHostname: "worker-1", corev1.LabelTopologyZone: "zone-a", corev1.LabelTopologyRegion: "centralus", "storage.example/tier": "fast"}, Topology: NodeOptimizationTopology{Hostname: "worker-1", Zone: "zone-a", Region: "centralus"}},
		{Name: "node-2", PoolKey: poolKey, Labels: map[string]string{corev1.LabelHostname: "worker-2", corev1.LabelTopologyZone: "zone-b", corev1.LabelTopologyRegion: "eastus", "storage.example/tier": "fast"}, Topology: NodeOptimizationTopology{Hostname: "worker-2", Zone: "zone-b", Region: "eastus"}},
	}
	tests := []struct {
		name      string
		key       string
		value     string
		wantNodes []string
	}{
		{name: "hostname", key: corev1.LabelHostname, value: "worker-1", wantNodes: []string{"node-1"}},
		{name: "zone", key: corev1.LabelTopologyZone, value: "zone-a", wantNodes: []string{"node-0", "node-1"}},
		{name: "region", key: corev1.LabelTopologyRegion, value: "centralus", wantNodes: []string{"node-0", "node-1"}},
		{name: "arbitrary label", key: "storage.example/tier", value: "fast", wantNodes: []string{"node-1", "node-2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
			pod.Spec.NodeName = tt.wantNodes[0]
			claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(tt.key, corev1.NodeSelectorOpIn, tt.value))
			evidence := BuildNodeOptimizationStorageEvidence([]corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume})

			got, hasStorage, reason := podStorageEligibleNodeNames(pod, nodes, &evidence, nil)

			if reason != "" || !hasStorage || !reflect.DeepEqual(got, tt.wantNodes) {
				t.Fatalf("eligible = %#v, hasStorage=%t, reason=%q, want %#v", got, hasStorage, reason, tt.wantNodes)
			}
		})
	}
}

func TestPodStorageEligibleNodeNamesIntersectsMultiplePVCs(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	node0.Labels["storage.example/tier"] = "slow"
	node1.Labels["storage.example/tier"] = "fast"
	node2.Labels["storage.example/tier"] = "fast"
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	scheduling := BuildNodeOptimizationSchedulingEvidence(nodeInfos, []corev1.Node{node0, node1, node2})
	nodes := schedulingNodesForPool(scheduling, costPoolKeyFromNodeInfo(info0))
	pod := pvcBackedOptimizationTestPod("apps", "api", "node-1", "zone-data")
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "fast-data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: "fast-data",
		}},
	})
	zoneClaim, zoneVolume := boundOptimizationTestVolume("apps", "zone-data", "pv-zone", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"))
	fastClaim, fastVolume := boundOptimizationTestVolume("apps", "fast-data", "pv-fast", optimizationTestVolumeNodeAffinity("storage.example/tier", corev1.NodeSelectorOpIn, "fast"))

	t.Run("common candidate remains eligible", func(t *testing.T) {
		evidence := BuildNodeOptimizationStorageEvidence(
			[]corev1.PersistentVolumeClaim{zoneClaim, fastClaim},
			[]corev1.PersistentVolume{zoneVolume, fastVolume},
		)
		got, hasStorage, reason := podStorageEligibleNodeNames(pod, nodes, &evidence, nil)
		if reason != "" || !hasStorage || !reflect.DeepEqual(got, []string{"node-1"}) {
			t.Fatalf("eligible = %#v, hasStorage=%t, reason=%q, want node-1", got, hasStorage, reason)
		}
	})

	t.Run("conflicting PV constraints produce empty eligibility", func(t *testing.T) {
		fastVolume.Spec.NodeAffinity = optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-b")
		evidence := BuildNodeOptimizationStorageEvidence(
			[]corev1.PersistentVolumeClaim{zoneClaim, fastClaim},
			[]corev1.PersistentVolume{zoneVolume, fastVolume},
		)
		got, hasStorage, reason := podStorageEligibleNodeNames(pod, nodes, &evidence, nil)
		if reason != "" || !hasStorage || got == nil || len(got) != 0 {
			t.Fatalf("eligible = %#v, hasStorage=%t, reason=%q, want explicit empty set", got, hasStorage, reason)
		}
	})
}

func TestBuildNMinusOneNodeOptimizationScenariosStorageEvidenceFailsClosed(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1}
	nodes := []corev1.Node{node0, node1}

	tests := []struct {
		name        string
		build       func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume)
		wantWarning string
	}{
		{
			name: "missing PVC",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				return pvcBackedOptimizationTestPod("apps", "api", "node-0", "missing"), nil, nil
			},
			wantWarning: "missing PersistentVolumeClaim apps/missing",
		},
		{
			name: "unbound PVC",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, _ := boundOptimizationTestVolume("apps", "data", "pv-data", nil)
				claim.Status.Phase = corev1.ClaimPending
				claim.Spec.VolumeName = ""
				return pod, []corev1.PersistentVolumeClaim{claim}, nil
			},
			wantWarning: "not Bound; late binding is not modeled",
		},
		{
			name: "Bound PVC without volumeName",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, _ := boundOptimizationTestVolume("apps", "data", "pv-data", nil)
				claim.Spec.VolumeName = ""
				return pod, []corev1.PersistentVolumeClaim{claim}, nil
			},
			wantWarning: "has no spec.volumeName",
		},
		{
			name: "missing PV",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, _ := boundOptimizationTestVolume("apps", "data", "pv-missing", nil)
				return pod, []corev1.PersistentVolumeClaim{claim}, nil
			},
			wantWarning: "missing PersistentVolume \"pv-missing\"",
		},
		{
			name: "inconsistent PVC PV binding",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", nil)
				volume.Spec.ClaimRef.Name = "other"
				return pod, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume}
			},
			wantWarning: "does not match PersistentVolumeClaim apps/data",
		},
		{
			name: "malformed PV node affinity",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn))
				return pod, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume}
			},
			wantWarning: "invalid or unsupported",
		},
		{
			name: "unsupported PV affinity operator",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOperator("Invalid"), "zone-a"))
				return pod, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume}
			},
			wantWarning: "unsupported node selector operator",
		},
		{
			name: "PV affinity matchFields",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				affinity := &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-0"}}},
				}}}}
				claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", affinity)
				return pod, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume}
			},
			wantWarning: "uses matchFields, which is not modeled",
		},
		{
			name: "ReadWriteOncePod",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", nil)
				claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
				volume.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
				return pod, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume}
			},
			wantWarning: "ReadWriteOncePod",
		},
		{
			name: "CSI PersistentVolume",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"))
				volume.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "storage.example.csi", VolumeHandle: "volume-1"}}
				return pod, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume}
			},
			wantWarning: "CSI attachment limits and driver-specific scheduling constraints are not modeled",
		},
		{
			name: "unsupported attachable PersistentVolume source",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"))
				volume.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{AzureDisk: &corev1.AzureDiskVolumeSource{DataDiskURI: "disk://example"}}
				return pod, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume}
			},
			wantWarning: "attachment and driver-specific scheduling constraints are not modeled",
		},
		{
			name: "local PV without node affinity",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", nil)
				volume.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/data"}}
				return pod, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume}
			},
			wantWarning: "cannot be treated as portable",
		},
		{
			name: "observed node contradicts PV node affinity",
			build: func() (corev1.Pod, []corev1.PersistentVolumeClaim, []corev1.PersistentVolume) {
				pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
				claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelHostname, corev1.NodeSelectorOpIn, "worker-1"))
				return pod, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume}
			},
			wantWarning: "observed on node node-0, which does not satisfy its PersistentVolume node-affinity intersection",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, claims, volumes := tt.build()
			got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{pod}, claims, volumes)

			for _, scenario := range got.Scenarios {
				if scenario.Simulation.PlacementFound {
					t.Fatalf("summary = %#v, unsupported/incomplete storage produced positive placement", got)
				}
			}
			if !strings.Contains(strings.Join(got.Warnings, " "), tt.wantWarning) {
				t.Fatalf("warnings = %#v, want %q", got.Warnings, tt.wantWarning)
			}
		})
	}
}

func TestSharedReadWriteOncePVCFailsClosed(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	nodes := []corev1.Node{node0, node1, node2}
	pod0 := pvcBackedOptimizationTestPod("apps", "api-0", "node-0", "data")
	pod1 := pvcBackedOptimizationTestPod("apps", "api-1", "node-1", "data")
	claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", nil)
	claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	volume.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}

	got := buildStorageAwareOptimizationScenarios(
		nodeInfos,
		nodes,
		[]corev1.Pod{pod0, pod1},
		[]corev1.PersistentVolumeClaim{claim},
		[]corev1.PersistentVolume{volume},
	)

	for _, scenario := range got.Scenarios {
		if scenario.Simulation.PlacementFound {
			t.Fatalf("summary = %#v, shared ReadWriteOnce PVC produced positive placement", got)
		}
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "shared multi-attach placement is not modeled") {
		t.Fatalf("warnings = %#v, want explicit shared ReadWriteOnce blocker", got.Warnings)
	}
}

func TestBuildNodeOptimizationStorageEvidenceRejectsDuplicateIdentitiesDeterministically(t *testing.T) {
	claimA, volumeA := boundOptimizationTestVolume("apps", "data", "pv-data", nil)
	claimB := *claimA.DeepCopy()
	volumeB := *volumeA.DeepCopy()

	forward := BuildNodeOptimizationStorageEvidence(
		[]corev1.PersistentVolumeClaim{claimA, claimB},
		[]corev1.PersistentVolume{volumeA, volumeB},
	)
	reversed := BuildNodeOptimizationStorageEvidence(
		[]corev1.PersistentVolumeClaim{claimB, claimA},
		[]corev1.PersistentVolume{volumeB, volumeA},
	)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("storage evidence changed with snapshot order:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
	if !forward.EvidenceIncomplete || forward.DuplicatePersistentVolumeClaimCount != 1 || forward.DuplicatePersistentVolumeCount != 1 {
		t.Fatalf("evidence = %#v, want deterministic duplicate PVC and PV rejection", forward)
	}

	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-b", "centralus")
	pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
	summary := BuildNMinusOneNodeOptimizationScenariosWithSchedulingAndStorageEvidence(
		[]models.NodeInfo{info0, info1},
		[]corev1.Pod{pod},
		BuildNodeOptimizationSchedulingEvidence([]models.NodeInfo{info0, info1}, []corev1.Node{node0, node1}),
		forward,
	)
	if len(summary.Scenarios) != 0 || !summary.EvidenceIncomplete {
		t.Fatalf("summary = %#v, want duplicate referenced storage snapshot to fail closed", summary)
	}
}

func TestPersistentVolumeTopologyEvidenceFailsClosed(t *testing.T) {
	selector := optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a")
	tests := []struct {
		name        string
		makeNodes   func() ([]models.NodeInfo, []corev1.Node)
		wantWarning string
	}{
		{
			name: "missing zone",
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := optimizationNodeInfos(2)
				return infos, []corev1.Node{
					{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{corev1.LabelHostname: "worker-0"}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{corev1.LabelHostname: "worker-1"}}},
				}
			},
			wantWarning: "missing zone topology evidence",
		},
		{
			name: "contradictory zone",
			makeNodes: func() ([]models.NodeInfo, []corev1.Node) {
				infos := optimizationNodeInfos(2)
				nodes := []corev1.Node{
					{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{corev1.LabelHostname: "worker-0", corev1.LabelTopologyZone: "zone-a", corev1.LabelFailureDomainBetaZone: "zone-b"}}},
					{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{corev1.LabelHostname: "worker-1", corev1.LabelTopologyZone: "zone-a", corev1.LabelFailureDomainBetaZone: "zone-b"}}},
				}
				return infos, nodes
			},
			wantWarning: "contradictory zone topology evidence",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfos, nodes := tt.makeNodes()
			pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
			claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", selector)

			got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{pod}, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume})

			for _, scenario := range got.Scenarios {
				if scenario.Simulation.PlacementFound {
					t.Fatalf("summary = %#v, incomplete PV topology evidence produced positive placement", got)
				}
			}
			if !strings.Contains(strings.Join(got.Warnings, " "), tt.wantWarning) {
				t.Fatalf("warnings = %#v, want %q", got.Warnings, tt.wantWarning)
			}
		})
	}
}

func TestBuildNMinusOneNodeOptimizationScenariosLocalPVsBlockTheirNodeRemoval(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1}
	nodes := []corev1.Node{node0, node1}
	pod0 := pvcBackedOptimizationTestPod("apps", "stateful-0", "node-0", "data-0")
	pod1 := pvcBackedOptimizationTestPod("apps", "stateful-1", "node-1", "data-1")
	claim0, volume0 := boundOptimizationTestVolume("apps", "data-0", "pv-data-0", optimizationTestVolumeNodeAffinity(corev1.LabelHostname, corev1.NodeSelectorOpIn, "worker-0"))
	claim1, volume1 := boundOptimizationTestVolume("apps", "data-1", "pv-data-1", optimizationTestVolumeNodeAffinity(corev1.LabelHostname, corev1.NodeSelectorOpIn, "worker-1"))
	volume0.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/data/0"}}
	volume1.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/data/1"}}

	got := buildStorageAwareOptimizationScenarios(
		nodeInfos,
		nodes,
		[]corev1.Pod{pod0, pod1},
		[]corev1.PersistentVolumeClaim{claim0, claim1},
		[]corev1.PersistentVolume{volume0, volume1},
	)

	if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want every node removal blocked by its local PV", got)
	}
	if got.Scenarios[0].Simulation.Status != NodeOptimizationBlockedEligibility {
		t.Fatalf("simulation = %#v, want explicit empty storage eligibility after removal", got.Scenarios[0].Simulation)
	}
}

func TestNMinusOnePVCBackedPodFromRemovedNodeGetsStorageValidDestination(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	nodes := []corev1.Node{node0, node1, node2}
	pod := pvcBackedOptimizationTestPod("apps", "stateful", "node-0", "data")
	claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"))

	got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{pod}, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume})

	if len(got.Scenarios) != 1 || got.Scenarios[0].RemovedNodeName != "node-0" || !got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want portable PVC-backed Pod evacuated from node-0", got)
	}
	simulation := got.Scenarios[0].Simulation
	if simulation.TotalCPURequestMilli != 500 || simulation.TotalMemoryRequestBytes != 256*1024*1024 {
		t.Fatalf("totals = CPU %dm memory %d, want PVC-backed Pod demand retained", simulation.TotalCPURequestMilli, simulation.TotalMemoryRequestBytes)
	}
	if len(simulation.Placements) != 1 || simulation.Placements[0].PodName != "stateful" || simulation.Placements[0].NodeName != "node-1" {
		t.Fatalf("placements = %#v, want exactly one zone-a destination on node-1", simulation.Placements)
	}
}

func TestPersistentVolumeNodeAffinitySupportsStandardOperators(t *testing.T) {
	node := NodeOptimizationSchedulingNode{
		Name: "node-0",
		Labels: map[string]string{
			"storage.example/tier":       "fast",
			"storage.example/generation": "3",
		},
	}
	tests := []struct {
		name       string
		expression corev1.NodeSelectorRequirement
	}{
		{name: "In", expression: corev1.NodeSelectorRequirement{Key: "storage.example/tier", Operator: corev1.NodeSelectorOpIn, Values: []string{"fast"}}},
		{name: "NotIn", expression: corev1.NodeSelectorRequirement{Key: "storage.example/tier", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"slow"}}},
		{name: "Exists", expression: corev1.NodeSelectorRequirement{Key: "storage.example/tier", Operator: corev1.NodeSelectorOpExists}},
		{name: "DoesNotExist", expression: corev1.NodeSelectorRequirement{Key: "storage.example/absent", Operator: corev1.NodeSelectorOpDoesNotExist}},
		{name: "Gt", expression: corev1.NodeSelectorRequirement{Key: "storage.example/generation", Operator: corev1.NodeSelectorOpGt, Values: []string{"2"}}},
		{name: "Lt", expression: corev1.NodeSelectorRequirement{Key: "storage.example/generation", Operator: corev1.NodeSelectorOpLt, Values: []string{"4"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			terms := []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{tt.expression}}}
			if reason := persistentVolumeNodeAffinityUnsupportedReason("pv-data", terms); reason != "" {
				t.Fatalf("reason = %q, want supported operator", reason)
			}
			if !nodeMatchesPersistentVolumeNodeAffinity(node.Labels, terms) {
				t.Fatalf("expression %#v did not match node labels %#v", tt.expression, node.Labels)
			}
		})
	}
}

func TestStorageEligibilityIntersectsNodeAffinityAndTaints(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	node0.Labels["storage.example/tier"] = "slow"
	node1.Labels["storage.example/tier"] = "fast"
	node2.Labels["storage.example/tier"] = "fast"
	node1.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "storage", Effect: corev1.TaintEffectNoSchedule}}
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	nodes := []corev1.Node{node0, node1, node2}

	t.Run("required node affinity intersection", func(t *testing.T) {
		pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
		pod.Spec.Affinity = optimizationRequiredNodeAffinity(corev1.NodeSelectorRequirement{
			Key: "storage.example/tier", Operator: corev1.NodeSelectorOpIn, Values: []string{"fast"},
		})
		pod.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Value: "storage", Effect: corev1.TaintEffectNoSchedule}}
		claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"))

		got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{pod}, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume})

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound || got.Scenarios[0].Simulation.Placements[0].NodeName != "node-1" {
			t.Fatalf("summary = %#v, want node-affinity/storage intersection on node-1", got)
		}
	})

	t.Run("taint toleration intersection", func(t *testing.T) {
		pod := pvcBackedOptimizationTestPod("apps", "api", "node-1", "data")
		pod.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Value: "storage", Effect: corev1.TaintEffectNoSchedule}}
		claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelHostname, corev1.NodeSelectorOpIn, "worker-1"))

		got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{pod}, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume})

		if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound || got.Scenarios[0].Simulation.Placements[0].NodeName != "node-1" {
			t.Fatalf("summary = %#v, want tolerated storage destination node-1", got)
		}
	})
}

func TestStorageEligibilityCombinesWithPodAntiAffinity(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	info3, node3 := topologySpreadTestNode("node-3", "guardpool", "worker-3", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2, info3}
	nodes := []corev1.Node{node0, node1, node2, node3}
	pod := pvcBackedOptimizationTestPod("apps", "api", "node-0", "data")
	pod.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "guard"}},
			TopologyKey:   corev1.LabelTopologyZone,
		}},
	}}
	guard := optimizationTestPod("apps", "guard", "node-3", corev1.PodRunning, "500m", "256Mi")
	guard.Labels = map[string]string{"app": "guard"}
	claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"))

	got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{pod, guard}, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume})

	if len(got.Scenarios) != 1 || got.Scenarios[0].Simulation.PlacementFound {
		t.Fatalf("summary = %#v, want anti-affinity to reject every storage-eligible zone-a node", got)
	}
}

func TestStorageEligibilityCombinesWithHardTopologySpread(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	info3, node3 := topologySpreadTestNode("node-3", "fixedpool", "worker-3", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2, info3}
	nodes := []corev1.Node{node0, node1, node2, node3}
	pod := pvcBackedOptimizationTestPod("apps", "web", "node-0", "data")
	pod.Labels = map[string]string{"app": "web"}
	pod.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       corev1.LabelTopologyZone,
		WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
	}}
	fixed := optimizationTestPod("apps", "fixed-web", "node-3", corev1.PodRunning, "500m", "256Mi")
	fixed.Labels = map[string]string{"app": "web"}
	claim, volume := boundOptimizationTestVolume("apps", "data", "pv-data", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"))

	got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{pod, fixed}, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume})

	if len(got.Scenarios) != 1 || !got.Scenarios[0].Simulation.PlacementFound || got.Scenarios[0].RemovedNodeName != "node-2" {
		t.Fatalf("summary = %#v, want node-0/node-1 removals rejected and deterministic node-2 removal", got)
	}
	if placement := got.Scenarios[0].Simulation.Placements[0]; placement.NodeName != "node-0" {
		t.Fatalf("placement = %#v, want retained zone-a storage destination", placement)
	}
}

func TestStorageAwareNodeOptimizationIsDeterministicAcrossSnapshotOrder(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	info2, node2 := topologySpreadTestNode("node-2", "userpool", "worker-2", "zone-b", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1, info2}
	nodes := []corev1.Node{node0, node1, node2}
	podA := pvcBackedOptimizationTestPod("apps", "stateful-a", "node-0", "data-a")
	podB := pvcBackedOptimizationTestPod("apps", "stateful-b", "node-2", "data-b")
	claimA, volumeA := boundOptimizationTestVolume("apps", "data-a", "pv-data-a", optimizationTestVolumeNodeAffinity(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"))
	claimB, volumeB := boundOptimizationTestVolume("apps", "data-b", "pv-data-b", nil)
	pods := []corev1.Pod{podA, podB}
	claims := []corev1.PersistentVolumeClaim{claimA, claimB}
	volumes := []corev1.PersistentVolume{volumeA, volumeB}

	forward := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, pods, claims, volumes)
	reversed := buildStorageAwareOptimizationScenarios(
		[]models.NodeInfo{nodeInfos[2], nodeInfos[1], nodeInfos[0]},
		[]corev1.Node{nodes[2], nodes[1], nodes[0]},
		[]corev1.Pod{pods[1], pods[0]},
		[]corev1.PersistentVolumeClaim{claims[1], claims[0]},
		[]corev1.PersistentVolume{volumes[1], volumes[0]},
	)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("storage-aware result changed with snapshot order:\nforward=%#v\nreversed=%#v", forward, reversed)
	}
	if len(forward.Scenarios) != 1 || !forward.Scenarios[0].Simulation.PlacementFound || forward.Scenarios[0].RemovedNodeName != "node-0" {
		t.Fatalf("summary = %#v, want deterministic node-0 removal", forward)
	}
	placements := forward.Scenarios[0].Simulation.Placements
	if len(placements) != 2 {
		t.Fatalf("placements = %#v, want every movable PVC-backed Pod assigned exactly once", placements)
	}
	seen := make(map[string]string, len(placements))
	for _, placement := range placements {
		if _, duplicate := seen[placement.PodName]; duplicate {
			t.Fatalf("placements = %#v, duplicate assignment for %s", placements, placement.PodName)
		}
		seen[placement.PodName] = placement.NodeName
	}
	if seen["stateful-a"] != "node-1" || seen["stateful-b"] == "" || seen["stateful-b"] == "node-0" {
		t.Fatalf("placements = %#v, want one storage-valid retained destination per Pod", placements)
	}
}

func TestPVCBackedDaemonSetFailsClosed(t *testing.T) {
	info0, node0 := topologySpreadTestNode("node-0", "userpool", "worker-0", "zone-a", "centralus")
	info1, node1 := topologySpreadTestNode("node-1", "userpool", "worker-1", "zone-a", "centralus")
	nodeInfos := []models.NodeInfo{info0, info1}
	nodes := []corev1.Node{node0, node1}
	daemon0 := optimizationTestDaemonSetPod("system", "agent-0", "node-0", "100m", "64Mi")
	daemon1 := optimizationTestDaemonSetPod("system", "agent-1", "node-1", "100m", "64Mi")
	for _, daemon := range []*corev1.Pod{&daemon0, &daemon1} {
		daemon.Spec.Volumes = []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: "data",
			}},
		}}
	}
	claim, volume := boundOptimizationTestVolume("system", "data", "pv-data", nil)

	got := buildStorageAwareOptimizationScenarios(nodeInfos, nodes, []corev1.Pod{daemon0, daemon1}, []corev1.PersistentVolumeClaim{claim}, []corev1.PersistentVolume{volume})

	if len(got.Scenarios) != 0 || !strings.Contains(strings.Join(got.Warnings, " "), "PVC-backed DaemonSet storage placement is not modeled") {
		t.Fatalf("summary = %#v, want PVC-backed DaemonSet fail closed", got)
	}
}
