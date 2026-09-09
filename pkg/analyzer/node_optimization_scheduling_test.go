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
