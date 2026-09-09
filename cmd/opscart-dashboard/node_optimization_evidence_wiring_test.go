package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests exercise the exact same builder chain runFullScan now uses for
// Node Optimization (see server.go, "6. Node Optimization"):
//
//   BuildNodeOptimizationSchedulingEvidence
//     + BuildNodeOptimizationStorageEvidence
//     -> BuildNMinusOneNodeOptimizationScenariosWithSchedulingAndStorageEvidence
//     -> BuildNodeOptimizationRecommendations
//
// using only exported analyzer functions and plain fixtures — no fake
// clientset needed, since none of this touches Kubernetes I/O. This is the
// wiring contract; the underlying simulation/evidence semantics themselves
// are analyzer-owned and already covered by pkg/analyzer's own test suite.

func evidenceWiringNodeInfos() []models.NodeInfo {
	base := models.NodeInfo{
		NodePool: "userpool", VMSize: "Standard_D4s_v3", Region: "centralus",
		OS: "linux", Priority: "Regular", Provider: "azure", Architecture: "amd64",
		CPUCapacity: 4, MemGBCapacity: 8,
	}
	nodeA, nodeB := base, base
	nodeA.Name, nodeB.Name = "node-0", "node-1"
	return []models.NodeInfo{nodeA, nodeB}
}

func evidenceWiringPod(namespace, name, nodeName, cpu, memory string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpu),
					corev1.ResourceMemory: resource.MustParse(memory),
				}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func hasVerifiedCheck(checks []analyzer.NodeOptimizationVerifiedCheck, want analyzer.NodeOptimizationVerifiedCheck) bool {
	for _, check := range checks {
		if check == want {
			return true
		}
	}
	return false
}

// Scheduling evidence built from a live raw-Node snapshot must actually
// reach recommendation building, reflected as newly-available VerifiedChecks
// that the evidence-free path can never report.
func TestNodeOptimizationEvidenceWiring_SchedulingEvidencePassedIntoRecommendations(t *testing.T) {
	nodeInfos := evidenceWiringNodeInfos()
	rawNodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	pods := []corev1.Pod{evidenceWiringPod("apps", "api", "node-0", "500m", "512Mi")}

	schedulingEvidence := analyzer.BuildNodeOptimizationSchedulingEvidence(nodeInfos, rawNodes)
	withSummary := analyzer.BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, schedulingEvidence)
	withRecs := analyzer.BuildNodeOptimizationRecommendations(withSummary, pods)

	withoutSummary := analyzer.BuildNMinusOneNodeOptimizationScenariosFromSnapshots(nodeInfos, pods)
	withoutRecs := analyzer.BuildNodeOptimizationRecommendations(withoutSummary, pods)

	if len(withRecs) != 1 || len(withoutRecs) != 1 {
		t.Fatalf("recommendation counts = %d/%d, want 1/1", len(withRecs), len(withoutRecs))
	}
	if withRecs[0].Status != analyzer.NodeOptimizationRecommendationSimulationPassed {
		t.Fatalf("with-scheduling-evidence status = %q, want SIMULATION_PASSED; blockers=%#v", withRecs[0].Status, withRecs[0].Blockers)
	}
	if withoutRecs[0].Status != analyzer.NodeOptimizationRecommendationSimulationPassed {
		t.Fatalf("evidence-free baseline status = %q, want SIMULATION_PASSED; blockers=%#v", withoutRecs[0].Status, withoutRecs[0].Blockers)
	}

	for _, check := range []analyzer.NodeOptimizationVerifiedCheck{
		analyzer.NodeOptimizationCheckTaintsTolerations,
		analyzer.NodeOptimizationCheckCordonState,
	} {
		if !hasVerifiedCheck(withRecs[0].VerifiedChecks, check) {
			t.Errorf("with scheduling evidence: VerifiedChecks missing %q: %#v", check, withRecs[0].VerifiedChecks)
		}
		if hasVerifiedCheck(withoutRecs[0].VerifiedChecks, check) {
			t.Errorf("evidence-free baseline unexpectedly reports %q without evidence: %#v", check, withoutRecs[0].VerifiedChecks)
		}
	}
}

// A raw Node snapshot that does not cover a pool's nodes must fail closed:
// the pool's recommendation must degrade to PARTIAL, never a false
// SIMULATION_PASSED, when scheduling evidence cannot be resolved for it.
func TestNodeOptimizationEvidenceWiring_MissingSchedulingEvidenceFailsClosed(t *testing.T) {
	nodeInfos := evidenceWiringNodeInfos()
	pods := []corev1.Pod{evidenceWiringPod("apps", "api", "node-0", "500m", "512Mi")}

	// No raw Nodes at all — as if the node-health scan step had not yet run.
	incompleteEvidence := analyzer.BuildNodeOptimizationSchedulingEvidence(nodeInfos, nil)
	summary := analyzer.BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, incompleteEvidence)
	recs := analyzer.BuildNodeOptimizationRecommendations(summary, pods)

	if len(recs) != 1 {
		t.Fatalf("recommendation count = %d, want 1", len(recs))
	}
	if recs[0].Status == analyzer.NodeOptimizationRecommendationSimulationPassed {
		t.Fatalf("missing scheduling evidence produced a false SIMULATION_PASSED: %#v", recs[0])
	}
	if recs[0].Status != analyzer.NodeOptimizationRecommendationPartial {
		t.Fatalf("status = %q, want PARTIAL when scheduling evidence cannot be resolved", recs[0].Status)
	}
}

// Raw Node topology labels (zone/region), joined via scheduling evidence,
// must actually participate: a hard topology-spread pod resolves cleanly
// when the candidate nodes carry complete topology evidence, and fails
// closed — never a false SIMULATION_PASSED — when that raw Node evidence is
// missing, since the constraint cannot be proved satisfied without it.
func TestNodeOptimizationEvidenceWiring_TopologyAwareScenarioUsesRawNodeEvidence(t *testing.T) {
	nodeInfos := evidenceWiringNodeInfos()
	pod := evidenceWiringPod("apps", "api", "node-0", "500m", "512Mi")
	pod.ObjectMeta.Labels = map[string]string{"app": "api"}
	pod.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
	}}
	pods := []corev1.Pod{pod}

	withTopologyLabels := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Labels: map[string]string{
			corev1.LabelHostname: "node-0", corev1.LabelTopologyZone: "us-east-1a", corev1.LabelTopologyRegion: "us-east-1",
		}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{
			corev1.LabelHostname: "node-1", corev1.LabelTopologyZone: "us-east-1b", corev1.LabelTopologyRegion: "us-east-1",
		}}},
	}
	labeledEvidence := analyzer.BuildNodeOptimizationSchedulingEvidence(nodeInfos, withTopologyLabels)
	labeledSummary := analyzer.BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, labeledEvidence)
	labeledRecs := analyzer.BuildNodeOptimizationRecommendations(labeledSummary, pods)
	if len(labeledRecs) != 1 {
		t.Fatalf("labeled recommendation count = %d, want 1", len(labeledRecs))
	}
	if !hasVerifiedCheck(labeledRecs[0].VerifiedChecks, analyzer.NodeOptimizationCheckTopologySpread) {
		t.Errorf("VerifiedChecks missing hard_topology_spread: %#v", labeledRecs[0].VerifiedChecks)
	}
	if labeledRecs[0].Status != analyzer.NodeOptimizationRecommendationSimulationPassed {
		t.Errorf("complete zone/region topology evidence status = %q, want SIMULATION_PASSED; blockers=%#v", labeledRecs[0].Status, labeledRecs[0].Blockers)
	}

	unlabeledNodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	unlabeledEvidence := analyzer.BuildNodeOptimizationSchedulingEvidence(nodeInfos, unlabeledNodes)
	unlabeledSummary := analyzer.BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(nodeInfos, pods, unlabeledEvidence)
	unlabeledRecs := analyzer.BuildNodeOptimizationRecommendations(unlabeledSummary, pods)
	if len(unlabeledRecs) != 1 {
		t.Fatalf("unlabeled recommendation count = %d, want 1", len(unlabeledRecs))
	}
	if unlabeledRecs[0].Status == analyzer.NodeOptimizationRecommendationSimulationPassed {
		t.Errorf("missing zone/region topology evidence produced a false SIMULATION_PASSED: %#v", unlabeledRecs[0])
	}
}

// Storage evidence built from live PVC/PV snapshots must reach recommendation
// building: a movable Pod's PVC reference should surface the PVC/PV storage
// topology check, and remain compatible with reaching SIMULATION_PASSED when
// the referenced volume is portable (no restrictive node affinity).
func TestNodeOptimizationEvidenceWiring_StorageEvidencePassedIntoRecommendations(t *testing.T) {
	nodeInfos := evidenceWiringNodeInfos()
	rawNodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	pod := evidenceWiringPod("apps", "api", "node-0", "500m", "512Mi")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
	}}
	pods := []corev1.Pod{pod}

	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "data", UID: "pvc-uid-1"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-data"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-data"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef:               &corev1.ObjectReference{Namespace: "apps", Name: "data", UID: "pvc-uid-1"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs.example.com", Path: "/export"}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}

	schedulingEvidence := analyzer.BuildNodeOptimizationSchedulingEvidence(nodeInfos, rawNodes)
	storageEvidence := analyzer.BuildNodeOptimizationStorageEvidence(
		[]corev1.PersistentVolumeClaim{pvc},
		[]corev1.PersistentVolume{pv},
	)

	summary := analyzer.BuildNMinusOneNodeOptimizationScenariosWithSchedulingAndStorageEvidence(
		nodeInfos, pods, schedulingEvidence, storageEvidence,
	)
	recs := analyzer.BuildNodeOptimizationRecommendations(summary, pods)

	if len(recs) != 1 {
		t.Fatalf("recommendation count = %d, want 1", len(recs))
	}
	if !hasVerifiedCheck(recs[0].VerifiedChecks, analyzer.NodeOptimizationCheckPersistentVolume) {
		t.Fatalf("VerifiedChecks missing pvc_pv_storage_topology with connected storage evidence: %#v", recs[0].VerifiedChecks)
	}
	if recs[0].Status != analyzer.NodeOptimizationRecommendationSimulationPassed {
		t.Fatalf("status = %q, want SIMULATION_PASSED for a portable PVC/PV; blockers=%#v", recs[0].Status, recs[0].Blockers)
	}
}

// This is the same full evidence-building chain runFullScan now uses,
// exercised end-to-end with both scheduling and storage evidence supplied
// together — the exact shape the dashboard consumes via clusterScan.nodeOptimization.
func TestNodeOptimizationEvidenceWiring_FullChainMatchesScanPipelineShape(t *testing.T) {
	nodeInfos := evidenceWiringNodeInfos()
	rawNodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	pods := []corev1.Pod{evidenceWiringPod("apps", "api", "node-0", "500m", "512Mi")}

	schedulingEvidence := analyzer.BuildNodeOptimizationSchedulingEvidence(nodeInfos, rawNodes)
	storageEvidence := analyzer.BuildNodeOptimizationStorageEvidence(nil, nil)
	summary := analyzer.BuildNMinusOneNodeOptimizationScenariosWithSchedulingAndStorageEvidence(
		nodeInfos, pods, schedulingEvidence, storageEvidence,
	)
	recs := analyzer.BuildNodeOptimizationRecommendations(summary, pods)

	page := renderNodeOptimizationPage(&clusterScan{
		report:           &models.CloudCostReport{ClusterName: "wiring-test"},
		nodeOptimization: recs,
	}, "wiring-test", []string{"wiring-test"})

	if page == "" {
		t.Fatal("renderNodeOptimizationPage produced empty output for the real evidence-building chain's output")
	}
}
