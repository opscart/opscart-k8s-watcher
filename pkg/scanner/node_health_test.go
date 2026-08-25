package scanner

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/kube"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func nodeWithConditions(name string, conditions ...corev1.NodeCondition) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Conditions: conditions}}
}

func condition(conditionType corev1.NodeConditionType, status corev1.ConditionStatus) corev1.NodeCondition {
	return corev1.NodeCondition{Type: conditionType, Status: status}
}

func TestDetectUnhealthyNodeConditions(t *testing.T) {
	tests := []struct {
		name          string
		conditionType corev1.NodeConditionType
		status        corev1.ConditionStatus
		want          bool
	}{
		{"Ready False", corev1.NodeReady, corev1.ConditionFalse, true},
		{"Ready Unknown", corev1.NodeReady, corev1.ConditionUnknown, true},
		{"Ready True", corev1.NodeReady, corev1.ConditionTrue, false},
		{"DiskPressure True", corev1.NodeDiskPressure, corev1.ConditionTrue, true},
		{"MemoryPressure True", corev1.NodeMemoryPressure, corev1.ConditionTrue, true},
		{"PIDPressure True", corev1.NodePIDPressure, corev1.ConditionTrue, true},
		{"NetworkUnavailable True", corev1.NodeNetworkUnavailable, corev1.ConditionTrue, true},
		{"DiskPressure False", corev1.NodeDiskPressure, corev1.ConditionFalse, false},
		{"MemoryPressure False", corev1.NodeMemoryPressure, corev1.ConditionFalse, false},
		{"PIDPressure False", corev1.NodePIDPressure, corev1.ConditionFalse, false},
		{"NetworkUnavailable False", corev1.NodeNetworkUnavailable, corev1.ConditionFalse, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectUnhealthyNodeConditions([]corev1.Node{nodeWithConditions("node-a", condition(tc.conditionType, tc.status))})
			if (len(got) == 1) != tc.want {
				t.Fatalf("got %d findings, want detected=%v: %+v", len(got), tc.want, got)
			}
		})
	}
}

func TestDetectUnhealthyNodeConditionsPreservesEvidenceAndIdentity(t *testing.T) {
	transition := metav1.NewTime(time.Date(2026, 8, 16, 12, 30, 0, 0, time.UTC))
	node := nodeWithConditions("node-a",
		corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady", Message: "kubelet stopped posting status", LastTransitionTime: transition},
		corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue, Reason: "KubeletHasDiskPressure", Message: "disk pressure reported", LastTransitionTime: transition},
	)
	got := DetectUnhealthyNodeConditions([]corev1.Node{node})
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	ready := got[1]
	if ready.NodeName != "node-a" || ready.ConditionType != "Ready" || ready.ConditionStatus != "False" ||
		ready.Reason != "KubeletNotReady" || ready.Message != "kubelet stopped posting status" || !ready.LastTransitionTime.Equal(transition.Time) {
		t.Fatalf("Ready evidence not preserved: %+v", ready)
	}
	if got[0].ConditionType != "DiskPressure" {
		t.Fatalf("conditions were not distinguishable: %+v", got)
	}
}

func TestDetectUnhealthyNodeConditionsUsesInfrastructureNodePoolIdentity(t *testing.T) {
	node := nodeWithConditions("node-a", condition(corev1.NodeReady, corev1.ConditionFalse))
	node.Labels = map[string]string{
		"agentpool":                        "aks-primary",
		"kubernetes.azure.com/agentpool":   "aks-secondary",
		"node.kubernetes.io/instance-type": "m7i.large",
		"eks.amazonaws.com/nodegroup":      "eks-workers",
	}
	got := DetectUnhealthyNodeConditions([]corev1.Node{node})
	want := kube.NodePoolName(node)
	if len(got) != 1 || got[0].NodePool != want || want != "aks-primary" {
		t.Fatalf("node pool = %q, shared Infrastructure resolver = %q", got[0].NodePool, want)
	}
}

func TestDetectUnhealthyNodeConditionsSortsByNodeAndCondition(t *testing.T) {
	nodes := []corev1.Node{
		nodeWithConditions("node-z", condition(corev1.NodeReady, corev1.ConditionFalse), condition(corev1.NodeDiskPressure, corev1.ConditionTrue)),
		nodeWithConditions("node-a", condition(corev1.NodePIDPressure, corev1.ConditionTrue), condition(corev1.NodeMemoryPressure, corev1.ConditionTrue)),
	}
	got := DetectUnhealthyNodeConditions(nodes)
	var identities []string
	for _, finding := range got {
		identities = append(identities, finding.NodeName+"/"+finding.ConditionType)
	}
	want := []string{"node-a/MemoryPressure", "node-a/PIDPressure", "node-z/DiskPressure", "node-z/Ready"}
	if !reflect.DeepEqual(identities, want) {
		t.Fatalf("order = %v, want %v", identities, want)
	}
}

func scheduledOwnedPod(namespace, name, node, ownerKind, ownerName string) corev1.Pod {
	controller := true
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: []metav1.OwnerReference{{Kind: ownerKind, Name: ownerName, Controller: &controller}}},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func TestCorrelateNodeWorkloadsByCurrentPlacement(t *testing.T) {
	findings := []models.NodeConditionFinding{{NodeName: "node-a", ConditionType: "Ready"}}
	pods := []corev1.Pod{
		scheduledOwnedPod("shop", "api-7d8f9c6b5-abc12", "node-a", "ReplicaSet", "api-7d8f9c6b5"),
		scheduledOwnedPod("shop", "api-7d8f9c6b5-def34", "node-a", "ReplicaSet", "api-7d8f9c6b5"),
		scheduledOwnedPod("shop", "worker-0", "node-a", "StatefulSet", "worker"),
		scheduledOwnedPod("shop", "elsewhere-7d8f9c6b5-ghi56", "node-b", "ReplicaSet", "elsewhere-7d8f9c6b5"),
		scheduledOwnedPod("shop", "unbound-7d8f9c6b5-jkl78", "", "ReplicaSet", "unbound-7d8f9c6b5"),
	}

	got := CorrelateNodeWorkloads(findings, pods, nil)
	want := []models.CorrelatedWorkload{
		{Namespace: "shop", Kind: "Deployment", Name: "api", PodCount: 2},
		{Namespace: "shop", Kind: "StatefulSet", Name: "worker", PodCount: 1},
	}
	if len(got) != 1 || len(got[0].CorrelatedWorkloads) != len(want) {
		t.Fatalf("unexpected correlation: %+v", got)
	}
	for i := range want {
		if got[0].CorrelatedWorkloads[i] != want[i] {
			t.Fatalf("workload[%d] = %+v, want %+v", i, got[0].CorrelatedWorkloads[i], want[i])
		}
	}
}

func TestCorrelateNodeWorkloadsKeepsNamespacesDistinct(t *testing.T) {
	findings := []models.NodeConditionFinding{{NodeName: "node-a", ConditionType: "MemoryPressure"}}
	pods := []corev1.Pod{
		scheduledOwnedPod("blue", "api-7d8f9c6b5-abc12", "node-a", "ReplicaSet", "api-7d8f9c6b5"),
		scheduledOwnedPod("green", "api-7d8f9c6b5-def34", "node-a", "ReplicaSet", "api-7d8f9c6b5"),
	}
	got := CorrelateNodeWorkloads(findings, pods, nil)
	if len(got[0].CorrelatedWorkloads) != 2 || got[0].CorrelatedWorkloads[0].Namespace == got[0].CorrelatedWorkloads[1].Namespace {
		t.Fatalf("namespaces were merged: %+v", got)
	}
}

func TestCorrelateNodeWorkloadsReusesOwnerNameNormalization(t *testing.T) {
	findings := []models.NodeConditionFinding{{NodeName: "node-a", ConditionType: "Ready"}}
	pod := scheduledOwnedPod("shop", "checkout-api-7d8f9c6b5-jxmtx", "node-a", "ReplicaSet", "checkout-api-7d8f9c6b5")
	got := CorrelateNodeWorkloads(findings, []corev1.Pod{pod}, nil)
	if len(got[0].CorrelatedWorkloads) != 1 || got[0].CorrelatedWorkloads[0].Name != "checkout-api" {
		t.Fatalf("existing pod normalization was not applied: %+v", got)
	}
}

func TestCorrelatedWorkloadOwnerCoverage(t *testing.T) {
	controller := true
	cronJob := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "nightly-29123456", OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "nightly", Controller: &controller}}}}
	tests := []struct {
		name string
		pod  corev1.Pod
		want models.CorrelatedWorkload
	}{
		{"Deployment", scheduledOwnedPod("apps", "api-7d8f9c6b5-abc12", "node-a", "ReplicaSet", "api-7d8f9c6b5"), models.CorrelatedWorkload{Namespace: "apps", Kind: "Deployment", Name: "api"}},
		{"StatefulSet", scheduledOwnedPod("apps", "db-0", "node-a", "StatefulSet", "db"), models.CorrelatedWorkload{Namespace: "apps", Kind: "StatefulSet", Name: "db"}},
		{"DaemonSet", scheduledOwnedPod("system", "agent-xzqwe", "node-a", "DaemonSet", "agent"), models.CorrelatedWorkload{Namespace: "system", Kind: "DaemonSet", Name: "agent"}},
		{"Job", scheduledOwnedPod("batch", "migration-abc12", "node-a", "Job", "migration"), models.CorrelatedWorkload{Namespace: "batch", Kind: "Job", Name: "migration"}},
		{"CronJob", scheduledOwnedPod("batch", "nightly-29123456-abc12", "node-a", "Job", cronJob.Name), models.CorrelatedWorkload{Namespace: "batch", Kind: "CronJob", Name: "nightly"}},
		{"bare Pod", corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "tools", Name: "debug-shell"}, Spec: corev1.PodSpec{NodeName: "node-a"}}, models.CorrelatedWorkload{Namespace: "tools", Kind: "Workload", Name: "debug-shell"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := []models.NodeConditionFinding{{NodeName: "node-a", ConditionType: "Ready"}}
			got := CorrelateNodeWorkloads(findings, []corev1.Pod{tc.pod}, []batchv1.Job{cronJob})
			if len(got) != 1 || len(got[0].CorrelatedWorkloads) != 1 {
				t.Fatalf("unexpected correlation: %+v", got)
			}
			want := tc.want
			want.PodCount = 1
			if got[0].CorrelatedWorkloads[0] != want {
				t.Fatalf("identity = %+v, want %+v", got[0].CorrelatedWorkloads[0], want)
			}
		})
	}
}

func TestSameNodeMultipleConditionsReceiveSamePlacementSnapshot(t *testing.T) {
	node := nodeWithConditions("node-a",
		condition(corev1.NodeReady, corev1.ConditionFalse),
		condition(corev1.NodeMemoryPressure, corev1.ConditionTrue),
	)
	pods := []corev1.Pod{
		scheduledOwnedPod("apps", "api-7d8f9c6b5-abc12", "node-a", "ReplicaSet", "api-7d8f9c6b5"),
		scheduledOwnedPod("apps", "api-7d8f9c6b5-def34", "node-a", "ReplicaSet", "api-7d8f9c6b5"),
		scheduledOwnedPod("apps", "db-0", "node-a", "StatefulSet", "db"),
	}
	got := CorrelateNodeWorkloads(DetectUnhealthyNodeConditions([]corev1.Node{node}), pods, nil)
	if len(got) != 2 || got[0].ConditionType == got[1].ConditionType {
		t.Fatalf("conditions not independently identifiable: %+v", got)
	}
	if !reflect.DeepEqual(got[0].CorrelatedWorkloads, got[1].CorrelatedWorkloads) {
		t.Fatalf("placement snapshots differ: %+v", got)
	}
	want := []models.CorrelatedWorkload{
		{Namespace: "apps", Kind: "Deployment", Name: "api", PodCount: 2},
		{Namespace: "apps", Kind: "StatefulSet", Name: "db", PodCount: 1},
	}
	if !reflect.DeepEqual(got[0].CorrelatedWorkloads, want) {
		t.Fatalf("placement was duplicated or miscounted: got %+v, want %+v", got[0].CorrelatedWorkloads, want)
	}
}

func TestNodeConditionIncidentsIdentityAndMutableEvidence(t *testing.T) {
	transition := time.Date(2026, 8, 16, 12, 30, 0, 0, time.UTC)
	base := models.NodeConditionFinding{
		NodeName: "worker-21", NodePool: "system", ConditionType: "DiskPressure",
		ConditionStatus: "True", Reason: "DiskPressure", Message: "disk pressure reported",
		LastTransitionTime: transition,
		CorrelatedWorkloads: []models.CorrelatedWorkload{
			{Namespace: "shop", Kind: "Deployment", Name: "checkout-api", PodCount: 3},
			{Namespace: "shop", Kind: "StatefulSet", Name: "catalog", PodCount: 2},
		},
	}
	changed := base
	changed.NodePool = "replacement-pool"
	changed.Reason = "UpdatedReason"
	changed.Message = "updated Kubernetes evidence"
	changed.CorrelatedWorkloads = []models.CorrelatedWorkload{
		{Namespace: "shop", Kind: "Deployment", Name: "checkout-api", PodCount: 2},
		{Namespace: "shop", Kind: "StatefulSet", Name: "catalog", PodCount: 2},
		{Namespace: "monitoring", Kind: "DaemonSet", Name: "exporter", PodCount: 1},
	}

	first := NodeConditionIncidents([]models.NodeConditionFinding{base})[0]
	second := NodeConditionIncidents([]models.NodeConditionFinding{changed})[0]
	if first.Fingerprint != "cluster/Node/worker-21/DiskPressure" || second.Fingerprint != first.Fingerprint {
		t.Fatalf("fingerprints changed with evidence: first=%q second=%q", first.Fingerprint, second.Fingerprint)
	}
	if first.Severity != "high" || first.Namespace != "" || first.Resource != "worker-21" || first.IssueType != "DiskPressure" {
		t.Fatalf("unexpected incident mapping (DiskPressure must use models.NodeConditionSeverity, not a hardcoded value): %+v", first)
	}
	if first.DetailsJSON == second.DetailsJSON {
		t.Fatal("mutable evidence did not change details_json")
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(second.DetailsJSON), &details); err != nil {
		t.Fatalf("details_json: %v", err)
	}
	if details["correlation_semantics"] != "correlated_by_node_placement" || details["node_pool"] != "replacement-pool" || details["last_transition_time"] != transition.Format(time.RFC3339) {
		t.Fatalf("evidence not preserved: %#v", details)
	}
	workloads, ok := details["correlated_workloads"].([]any)
	if !ok || len(workloads) != 3 {
		t.Fatalf("correlated workloads not preserved: %#v", details["correlated_workloads"])
	}
}

func TestNodeConditionIncidentConditionAndNodeIsolation(t *testing.T) {
	findings := []models.NodeConditionFinding{
		{NodeName: "worker-21", ConditionType: "DiskPressure", ConditionStatus: "True"},
		{NodeName: "worker-21", ConditionType: "MemoryPressure", ConditionStatus: "True"},
		{NodeName: "worker-22", ConditionType: "DiskPressure", ConditionStatus: "True"},
	}
	incidents := NodeConditionIncidents(findings)
	want := []string{
		"cluster/Node/worker-21/DiskPressure",
		"cluster/Node/worker-21/MemoryPressure",
		"cluster/Node/worker-22/DiskPressure",
	}
	for i := range want {
		if incidents[i].Fingerprint != want[i] {
			t.Fatalf("fingerprint[%d] = %q, want %q", i, incidents[i].Fingerprint, want[i])
		}
	}
}

func TestExistingWorkloadFingerprintRegression(t *testing.T) {
	if got := store.Fingerprint("prod", "Workload", store.OwnerNameFromPod("checkout-api-7d8f9c6b5-jxmtx"), "CrashLoopBackOff"); got != "prod/Workload/checkout-api/crash_loop" {
		t.Fatalf("existing workload fingerprint changed: %q", got)
	}
}

func TestFindNodeHealthConditionsCollectionFailureIsAnError(t *testing.T) {
	t.Run("node list failure", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		client.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("nodes unavailable")
		})
		_, err := NewScannerWithClientset(client, "cluster-a").FindNodeHealthConditions()
		if err == nil {
			t.Fatal("node collection failure was reported as a healthy observation")
		}
	})

	t.Run("pod list failure", func(t *testing.T) {
		node := nodeWithConditions("worker-21", condition(corev1.NodeDiskPressure, corev1.ConditionTrue))
		client := fake.NewSimpleClientset(&node)
		client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("pods unavailable")
		})
		_, err := NewScannerWithClientset(client, "cluster-a").FindNodeHealthConditions()
		if err == nil {
			t.Fatal("pod correlation failure was reported as a healthy observation")
		}
	})
}

func TestNodeHealthProductionFlowPersistsStableIdentityAcrossPlacementChurn(t *testing.T) {
	transition := metav1.NewTime(time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC))
	node := nodeWithConditions("worker-a", corev1.NodeCondition{
		Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue,
		Reason: "InitialReason", Message: "initial message", LastTransitionTime: transition,
	})
	node.Labels = map[string]string{"agentpool": "pool-a"}
	podA := scheduledOwnedPod("apps", "api-7d8f9c6b5-abc12", "worker-a", "ReplicaSet", "api-7d8f9c6b5")
	podB := scheduledOwnedPod("apps", "worker-0", "worker-a", "StatefulSet", "worker")
	unbound := scheduledOwnedPod("apps", "pending-7d8f9c6b5-abc12", "", "ReplicaSet", "pending-7d8f9c6b5")
	client := fake.NewSimpleClientset(&node, &podA, &podB, &unbound)
	scanner := NewScannerWithClientset(client, "cluster-a")

	firstFindings, err := scanner.FindNodeHealthConditions()
	if err != nil {
		t.Fatalf("FindNodeHealthConditions(first): %v", err)
	}
	if len(firstFindings) != 1 {
		t.Fatalf("first findings = %+v", firstFindings)
	}
	first := NodeConditionIncidents(firstFindings)[0]
	if first.Fingerprint != "cluster/Node/worker-a/DiskPressure" {
		t.Fatalf("production fingerprint = %q", first.Fingerprint)
	}
	if strings.Contains(first.DetailsJSON, "pending") {
		t.Fatalf("unbound pod entered persisted placement evidence: %s", first.DetailsJSON)
	}
	for _, want := range []string{`"name":"api"`, `"name":"worker"`, `"node_pool":"pool-a"`} {
		if !strings.Contains(first.DetailsJSON, want) {
			t.Errorf("first evidence missing %s: %s", want, first.DetailsJSON)
		}
	}

	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "node-flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.UpsertIncidents("cluster-a", "scan-1", []store.IncidentData{first}); err != nil {
		t.Fatal(err)
	}

	currentNode, _ := client.CoreV1().Nodes().Get(scanner.ctx, "worker-a", metav1.GetOptions{})
	currentNode.Labels["agentpool"] = "pool-b"
	currentNode.Status.Conditions[0].Reason = "UpdatedReason"
	currentNode.Status.Conditions[0].Message = "updated message"
	currentNode.Status.Conditions[0].LastTransitionTime = metav1.NewTime(transition.Add(time.Hour))
	if _, err := client.CoreV1().Nodes().Update(scanner.ctx, currentNode, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := client.CoreV1().Pods("apps").Delete(scanner.ctx, podB.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	podC := scheduledOwnedPod("apps", "exporter-xzqwe", "worker-a", "DaemonSet", "exporter")
	if _, err := client.CoreV1().Pods("apps").Create(scanner.ctx, &podC, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	secondFindings, err := scanner.FindNodeHealthConditions()
	if err != nil {
		t.Fatalf("FindNodeHealthConditions(second): %v", err)
	}
	second := NodeConditionIncidents(secondFindings)[0]
	if second.Fingerprint != first.Fingerprint {
		t.Fatalf("placement/evidence churn changed identity: %q -> %q", first.Fingerprint, second.Fingerprint)
	}
	for _, want := range []string{`"name":"api"`, `"name":"exporter"`, `"node_pool":"pool-b"`, `"reason":"UpdatedReason"`} {
		if !strings.Contains(second.DetailsJSON, want) {
			t.Errorf("updated evidence missing %s: %s", want, second.DetailsJSON)
		}
	}
	for _, absent := range []string{`"name":"worker"`, "pending"} {
		if strings.Contains(second.DetailsJSON, absent) {
			t.Errorf("stale/unbound placement %s remained: %s", absent, second.DetailsJSON)
		}
	}
	if err := db.UpsertIncidents("cluster-a", "scan-2", []store.IncidentData{second}); err != nil {
		t.Fatal(err)
	}
	record, err := db.GetIncidentHistory("cluster-a", first.Fingerprint)
	if err != nil || record == nil || record.DetailsJSON != second.DetailsJSON {
		t.Fatalf("current evidence not updated: record=%+v err=%v", record, err)
	}
	items, total, err := db.QueryIncidents(store.IncidentFilter{Cluster: "cluster-a"})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("evidence churn created duplicate incident: total=%d items=%+v err=%v", total, items, err)
	}
	events, err := db.GetIncidentTimeline("cluster-a", first.Fingerprint)
	if err != nil || len(events) != 1 || events[0].EventType != "DETECTED" {
		t.Fatalf("evidence churn emitted lifecycle events: %+v err=%v", events, err)
	}
}

// TestNodeConditionIncidentsSeverityMatchesDisplayClassifier guards the
// severity-consistency bug: a node condition shown as critical in the War
// Room (via models.NodeConditionSeverity) must be persisted with that same
// severity, not a hardcoded value. Previously every node incident was
// persisted as "low" regardless of condition type, so a Ready=False node —
// critical everywhere it was displayed — silently showed as low severity
// in incident listings, historical counts, and any scoring that reads the
// stored value.
func TestNodeConditionIncidentsSeverityMatchesDisplayClassifier(t *testing.T) {
	cases := []struct {
		conditionType string
		wantSeverity  string
	}{
		{"Ready", "critical"},
		{"DiskPressure", "high"},
		{"MemoryPressure", "high"},
		{"PIDPressure", "high"},
		{"NetworkUnavailable", "high"},
		// A condition type the classifier doesn't recognize must not fail
		// persistence outright; it falls back rather than matching any of
		// the known severities.
		{"SomeFutureConditionType", "low"},
	}
	for _, c := range cases {
		finding := models.NodeConditionFinding{
			NodeName: "worker-1", ConditionType: c.conditionType,
			ConditionStatus: "True", Reason: c.conditionType,
		}
		got := NodeConditionIncidents([]models.NodeConditionFinding{finding})[0]
		if got.Severity != c.wantSeverity {
			t.Errorf("NodeConditionIncidents(%s).Severity = %q, want %q (must match models.NodeConditionSeverity used for display)",
				c.conditionType, got.Severity, c.wantSeverity)
		}
	}
}
