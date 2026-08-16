package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func infrastructureTestScan(findings ...models.NodeConditionFinding) *clusterScan {
	return &clusterScan{
		report: &models.CloudCostReport{
			Timestamp: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), ClusterName: "minikube",
			NodePoolCosts: []models.NodePoolCost{{Name: "userpool", VMSize: "m5.large", NodeCount: 3, TotalCPUCapacity: 6, TotalMemoryCapacity: 24}},
		},
		nodeHealth: findings,
	}
}

func TestInfrastructureNodeHealthHealthyStateAndExistingContent(t *testing.T) {
	body := renderInfrastructurePage(infrastructureTestScan(), "minikube", []string{"minikube"})
	for _, want := range []string{
		"Node Health &amp; Workload Placement",
		"All nodes healthy",
		"No unhealthy Kubernetes node conditions are currently reported.",
		"Node Pools",
		"userpool",
		"m5.large",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Infrastructure page missing %q", want)
		}
	}
}

func TestInfrastructureNodeHealthRendersEvidenceAndPlacementSemantics(t *testing.T) {
	finding := models.NodeConditionFinding{
		NodeName: "worker-21", NodePool: "userpool", ConditionType: "DiskPressure", ConditionStatus: "True",
		Reason: "KubeletHasDiskPressure", Message: "disk pressure reported by kubelet",
		LastTransitionTime: time.Date(2026, 8, 16, 11, 30, 0, 0, time.UTC),
		CorrelatedWorkloads: []models.CorrelatedWorkload{
			{Namespace: "payments", Kind: "Deployment", Name: "api", PodCount: 3},
			{Namespace: "inventory", Kind: "StatefulSet", Name: "db", PodCount: 2},
		},
	}
	body := renderInfrastructurePage(infrastructureTestScan(finding), "minikube", []string{"minikube"})
	for _, want := range []string{
		"worker-21", "DiskPressure", "Kubernetes condition: True", "Reason: KubeletHasDiskPressure",
		"disk pressure reported by kubelet", "2026-08-16 11:30:00 UTC",
		"2 workload groups · 5 pods currently placed on this node",
		"Correlated by node placement — not a claim of causation.",
		"payments", "inventory", "Deployment /</span> api", "StatefulSet /</span> db",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Infrastructure node health missing %q\n%s", want, body)
		}
	}
	lower := strings.ToLower(body)
	for _, forbidden := range []string{"caused by", "impacted by", "affected workloads", "workloads impacted"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("causal wording %q appeared in rendered output", forbidden)
		}
	}
	if !strings.Contains(body, `DiskPressure</span><span class="node-condition-separator" aria-hidden="true">·</span><span class="tag node-condition-status">Kubernetes condition: True`) {
		t.Fatal("condition type and Kubernetes status are not visually separated")
	}
}

func TestInfrastructureNodeHealthCompactNamespaceSummaryAndFullDetails(t *testing.T) {
	finding := models.NodeConditionFinding{NodeName: "node-a", ConditionType: "DiskPressure", ConditionStatus: "True", CorrelatedWorkloads: []models.CorrelatedWorkload{
		{Namespace: "alpha", Kind: "Deployment", Name: "api", PodCount: 3},
		{Namespace: "alpha", Kind: "StatefulSet", Name: "db", PodCount: 1},
		{Namespace: "beta", Kind: "DaemonSet", Name: "agent", PodCount: 1},
		{Namespace: "delta", Kind: "Job", Name: "migration", PodCount: 2},
		{Namespace: "epsilon", Kind: "CronJob", Name: "backup", PodCount: 1},
		{Namespace: "eta", Kind: "Workload", Name: "debug", PodCount: 1},
		{Namespace: "gamma", Kind: "Deployment", Name: "web", PodCount: 2},
		{Namespace: "zeta", Kind: "Deployment", Name: "worker", PodCount: 1},
	}}
	body := renderInfrastructurePage(infrastructureTestScan(finding), "minikube", nil)
	for _, want := range []string{
		`class="placement-summary-list"`,
		"alpha", "2 workloads · 4 pods",
		"beta", "1 workload · 1 pod",
		"+ 2 more namespaces",
		`<details class="placement-details">`, `<summary>Show workload details</summary>`,
		"Deployment /</span> api", "StatefulSet /</span> db", "CronJob /</span> backup", "Workload /</span> debug",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("compact placement presentation missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(body), "affected") || strings.Contains(strings.ToLower(body), "impacted") || strings.Contains(strings.ToLower(body), "caused by") {
		t.Fatal("compact placement presentation introduced causal wording")
	}
}

func TestInfrastructureNodeHealthPreservesNamespaceIdentity(t *testing.T) {
	finding := models.NodeConditionFinding{NodeName: "node-a", ConditionType: "Ready", ConditionStatus: "False", CorrelatedWorkloads: []models.CorrelatedWorkload{
		{Namespace: "blue", Kind: "Deployment", Name: "api", PodCount: 1},
		{Namespace: "green", Kind: "Deployment", Name: "api", PodCount: 1},
	}}
	body := renderInfrastructurePage(infrastructureTestScan(finding), "minikube", nil)
	if !strings.Contains(body, "blue") || !strings.Contains(body, "green") || strings.Count(body, "> api</div>") != 2 {
		t.Fatalf("same-named workloads were not visibly namespace-distinct")
	}
}

func TestInfrastructureNodeHealthGroupsConditionsUnderPhysicalNode(t *testing.T) {
	workloads := []models.CorrelatedWorkload{{Namespace: "apps", Kind: "Deployment", Name: "api", PodCount: 2}}
	scan := infrastructureTestScan(
		models.NodeConditionFinding{NodeName: "node-a", NodePool: "pool-a", ConditionType: "MemoryPressure", ConditionStatus: "True", CorrelatedWorkloads: workloads},
		models.NodeConditionFinding{NodeName: "node-a", NodePool: "pool-a", ConditionType: "DiskPressure", ConditionStatus: "True", CorrelatedWorkloads: workloads},
	)
	body := renderInfrastructurePage(scan, "minikube", nil)
	if strings.Count(body, `class="node-health-card"`) != 1 || strings.Count(body, `class="node-condition"`) != 2 {
		t.Fatalf("one physical node was not rendered as one card with two conditions")
	}
	if strings.Count(body, `Deployment /</span> api`) != 1 {
		t.Fatalf("placement snapshot was duplicated across conditions")
	}
}

func TestInfrastructureNodeHealthPoolsNodesFallbackAndOrdering(t *testing.T) {
	findings := []models.NodeConditionFinding{
		{NodeName: "node-b", NodePool: "pool-b", ConditionType: "Ready", ConditionStatus: "False"},
		{NodeName: "node-a", NodePool: "pool-a", ConditionType: "MemoryPressure", ConditionStatus: "True"},
		{NodeName: "node-a", NodePool: "pool-a", ConditionType: "DiskPressure", ConditionStatus: "True"},
		{NodeName: "node-unknown", ConditionType: "PIDPressure", ConditionStatus: "True"},
	}
	reversed := append([]models.NodeConditionFinding(nil), findings...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	first := buildInfrastructureNodeHealth(findings)
	second := buildInfrastructureNodeHealth(reversed)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("node-health ordering depends on input: first=%+v second=%+v", first, second)
	}
	if len(first) != 3 || first[0].Name != defaultNodePoolDisplayName || first[1].Name != "pool-a" || first[2].Name != "pool-b" {
		t.Fatalf("pool grouping/fallback = %+v", first)
	}
	if len(first[1].Nodes) != 1 || len(first[1].Nodes[0].Conditions) != 2 || first[1].Nodes[0].Conditions[0].Type != "DiskPressure" || first[1].Nodes[0].Conditions[1].Type != "MemoryPressure" {
		t.Fatalf("node/condition ordering = %+v", first[1])
	}
	body := renderInfrastructurePage(infrastructureTestScan(findings...), "minikube", nil)
	if !strings.Contains(body, `class="node-pool-label">default</div>`) || strings.Contains(body, "Unassigned / Unknown pool") || strings.Count(body, `class="node-health-card"`) != 3 {
		t.Fatalf("unknown pool or multiple nodes did not render correctly")
	}
}
