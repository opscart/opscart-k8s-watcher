package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func renderNodeHealth(findings ...models.NodeConditionFinding) string {
	var output bytes.Buffer
	printNodeHealth(&output, findings)
	return output.String()
}

func TestEmergencyNodeHealthHealthyStateAndWorkloadRegression(t *testing.T) {
	if got := renderNodeHealth(); got != "" {
		t.Fatalf("healthy Node state rendered a section: %q", got)
	}
	workload := []enrichedIssue{{EmergencyIssue: models.EmergencyIssue{
		Severity: "critical", Resource: "pod", Namespace: "payments", Name: "api-pod",
		Reason: "CrashLoopBackOff", Message: "Container app is crash looping",
	}}}
	var existing, withNodes bytes.Buffer
	printEmergencyIssuesEnrichedWithNextSteps(&existing, workload, "prod", false)
	printEmergencyTriage(&withNodes, workload, nil, "prod", false)
	if withNodes.String() != existing.String() {
		t.Fatalf("workload-only emergency output changed:\ngot:\n%s\nwant:\n%s", withNodes.String(), existing.String())
	}
}

func TestEmergencyNodeHealthDiskPressureCompactOutput(t *testing.T) {
	output := renderNodeHealth(models.NodeConditionFinding{
		NodeName: "opscart-m02", ConditionType: "DiskPressure", ConditionStatus: "True",
		Reason: "KubeletHasDiskPressure",
		CorrelatedWorkloads: []models.CorrelatedWorkload{
			{Namespace: "payments", Kind: "Deployment", Name: "api", PodCount: 3},
			{Namespace: "inventory", Kind: "StatefulSet", Name: "db", PodCount: 2},
		},
	})
	for _, want := range []string{
		"NODE HEALTH", "HIGH  DiskPressure on opscart-m02", "Reason: KubeletHasDiskPressure",
		"Placement: 2 workloads · 5 pods colocated",
		"Correlated by node placement — not a claim of causation.",
		"kubectl describe node opscart-m02",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("Node output missing %q:\n%s", want, output)
		}
	}
	for _, workloadName := range []string{"payments", "inventory", "Deployment", "StatefulSet", "api", "db"} {
		if strings.Contains(output, workloadName) {
			t.Errorf("compact Node output dumped workload detail %q:\n%s", workloadName, output)
		}
	}
}

func TestEmergencyNodeConditionSeverityPolicy(t *testing.T) {
	findings := []models.NodeConditionFinding{
		{NodeName: "ready-false", ConditionType: "Ready", ConditionStatus: "False"},
		{NodeName: "ready-unknown", ConditionType: "Ready", ConditionStatus: "Unknown"},
		{NodeName: "disk", ConditionType: "DiskPressure", ConditionStatus: "True"},
		{NodeName: "memory", ConditionType: "MemoryPressure", ConditionStatus: "True"},
		{NodeName: "pid", ConditionType: "PIDPressure", ConditionStatus: "True"},
		{NodeName: "network", ConditionType: "NetworkUnavailable", ConditionStatus: "True"},
	}
	output := renderNodeHealth(findings...)
	for _, want := range []string{
		"CRITICAL  Ready=False on ready-false", "CRITICAL  Ready=Unknown on ready-unknown",
		"HIGH  DiskPressure on disk", "HIGH  MemoryPressure on memory",
		"HIGH  PIDPressure on pid", "HIGH  NetworkUnavailable on network",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("severity policy missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "LOW") {
		t.Errorf("CLI used the persisted placeholder severity:\n%s", output)
	}
}

func TestEmergencyNodeWorkloadCountDoesNotPromoteSeverityAndPluralizes(t *testing.T) {
	one := models.NodeConditionFinding{
		NodeName: "node-one", ConditionType: "DiskPressure", ConditionStatus: "True",
		CorrelatedWorkloads: []models.CorrelatedWorkload{{Name: "api", PodCount: 1}},
	}
	many := models.NodeConditionFinding{NodeName: "node-many", ConditionType: "DiskPressure", ConditionStatus: "True"}
	for i := 0; i < 20; i++ {
		many.CorrelatedWorkloads = append(many.CorrelatedWorkloads, models.CorrelatedWorkload{Name: "workload", PodCount: 1})
	}
	output := renderNodeHealth(one, many)
	for _, want := range []string{
		"HIGH  DiskPressure on node-one", "Placement: 1 workload · 1 pod colocated",
		"HIGH  DiskPressure on node-many", "Placement: 20 workloads · 20 pods colocated",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("count/severity output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "CRITICAL  DiskPressure") {
		t.Errorf("workload count promoted DiskPressure severity:\n%s", output)
	}
}

func TestEmergencyNodeHealthMultipleConditionsNodesAndDeterministicOrder(t *testing.T) {
	findings := []models.NodeConditionFinding{
		{NodeName: "node-b", ConditionType: "DiskPressure", ConditionStatus: "True"},
		{NodeName: "node-a", ConditionType: "MemoryPressure", ConditionStatus: "True"},
		{NodeName: "node-a", ConditionType: "DiskPressure", ConditionStatus: "True"},
		{NodeName: "node-z", ConditionType: "Ready", ConditionStatus: "False"},
	}
	first := renderNodeHealth(findings...)
	reversed := []models.NodeConditionFinding{findings[3], findings[2], findings[1], findings[0]}
	second := renderNodeHealth(reversed...)
	if first != second {
		t.Fatalf("Node CLI ordering depends on input order:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	assertCLIOrder(t, first,
		"Ready=False on node-z",
		"DiskPressure on node-a",
		"MemoryPressure on node-a",
		"DiskPressure on node-b",
	)
	if strings.Count(first, " on node-a") != 2 {
		t.Fatalf("same-node conditions were merged:\n%s", first)
	}
}

func TestEmergencyNodeHealthLanguageAndGuidanceSafety(t *testing.T) {
	output := strings.ToLower(renderNodeHealth(models.NodeConditionFinding{
		NodeName: "worker-21", ConditionType: "NetworkUnavailable", ConditionStatus: "True",
		Reason: "RouteNotCreated", Message: "intentionally not rendered",
	}))
	for _, forbidden := range []string{"affected", "impacted", "caused by", "kubectl drain", "kubectl cordon", "kubectl delete", "systemctl", "restart"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("Node CLI contains forbidden causal/mutating text %q:\n%s", forbidden, output)
		}
	}
	if !strings.Contains(output, "kubectl describe node worker-21") {
		t.Errorf("Node CLI missing read-only guidance:\n%s", output)
	}
	if strings.Contains(output, "intentionally not rendered") {
		t.Errorf("Node CLI rendered verbose condition message:\n%s", output)
	}
}

func assertCLIOrder(t *testing.T, output string, values ...string) {
	t.Helper()
	position := -1
	for _, value := range values {
		next := strings.Index(output[position+1:], value)
		if next < 0 {
			t.Fatalf("output missing %q:\n%s", value, output)
		}
		position += next + 1
	}
}
