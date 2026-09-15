package main

import (
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testNode(name, pool string, unschedulable bool, conditions ...corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)),
		},
		Spec: corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{
			Conditions: conditions,
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.29.4"},
		},
	}
}

func readyCondition() corev1.NodeCondition {
	return corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}
}

// infrastructureTestScanWithNodes builds a clusterScan carrying real
// corev1.Node objects (plus their NodeInfo/pod-count companions) alongside
// the existing NodePoolCost-based fixture — the Nodes tab reads only these
// authoritative fields, never NodePoolCosts.
func infrastructureTestScanWithNodes(nodes []*corev1.Node, infos []models.NodeInfo, podCounts map[string]int) *clusterScan {
	scan := infrastructureTestScan()
	scan.nodes = nodes
	scan.nodeInfos = infos
	scan.nodePodCounts = podCounts
	return scan
}

func TestInfrastructureNodesTabRendersRealNodeRows(t *testing.T) {
	nodes := []*corev1.Node{
		testNode("node-a", "userpool", false, readyCondition()),
		testNode("node-b", "userpool", false, readyCondition()),
	}
	infos := []models.NodeInfo{
		{Name: "node-a", NodePool: "userpool", Zone: "us-east-1a", CPUCapacity: 4, MemGBCapacity: 16, CPURequested: 2, MemGBRequested: 8},
		{Name: "node-b", NodePool: "userpool", Zone: "us-east-1b", CPUCapacity: 4, MemGBCapacity: 16, CPURequested: 1, MemGBRequested: 4},
	}
	podCounts := map[string]int{"node-a": 12, "node-b": 5}
	scan := infrastructureTestScanWithNodes(nodes, infos, podCounts)

	body := renderInfrastructurePage(scan, "minikube", []string{"minikube"}, "nodes")

	for _, want := range []string{
		"node-a", "node-b",
		"us-east-1a", "us-east-1b",
		"2.0 / 4.0 cores", "8.0 / 16.0 GB",
		"v1.29.4",
		`>12<`, `>5<`,
		`class="tag node-badge-good">Healthy</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Nodes tab missing %q\n%s", want, body)
		}
	}
}

func TestInfrastructureNodesTabHealthAndConditionBadges(t *testing.T) {
	notReady := testNode("node-bad", "userpool", false, corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse})
	pressured := testNode("node-warn", "userpool", false, readyCondition(), corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue})
	cordoned := testNode("node-cordoned", "userpool", true, readyCondition())
	nodes := []*corev1.Node{notReady, pressured, cordoned}

	scan := infrastructureTestScanWithNodes(nodes, nil, nil)
	body := renderInfrastructurePage(scan, "minikube", []string{"minikube"}, "nodes")

	for _, want := range []string{
		`class="tag node-badge-bad">NotReady</span>`,
		`class="tag node-badge-warn">Warning</span>`,
		`class="tag tag-pressure">DiskPressure</span>`,
		`class="tag tag-cordoned">Cordoned</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Nodes tab missing condition badge %q\n%s", want, body)
		}
	}

	if !strings.Contains(body, `>1<`) {
		t.Fatalf("Not Ready KPI did not reflect the one NotReady node\n%s", body)
	}
}

func TestInfrastructureNodesTabRequestedNeverLabeledUsage(t *testing.T) {
	nodes := []*corev1.Node{testNode("node-a", "userpool", false, readyCondition())}
	infos := []models.NodeInfo{{Name: "node-a", NodePool: "userpool", CPUCapacity: 4, MemGBCapacity: 16, CPURequested: 2, MemGBRequested: 8}}
	scan := infrastructureTestScanWithNodes(nodes, infos, map[string]int{"node-a": 3})

	body := renderInfrastructurePage(scan, "minikube", []string{"minikube"}, "nodes")
	lower := strings.ToLower(body)
	for _, forbidden := range []string{"cpu usage", "memory usage", "cpu utilization", "memory utilization", "% used"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("Nodes tab must never label requests as runtime usage/utilization; found %q:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "CPU Requested / Allocatable") || !strings.Contains(body, "Memory Requested / Allocatable") {
		t.Fatalf("Nodes tab headers must read Requested / Allocatable\n%s", body)
	}
}

func TestInfrastructurePoolsTabStillRendersAndOmitsFinancialColumns(t *testing.T) {
	scan := infrastructureTestScan()
	body := renderInfrastructurePage(scan, "minikube", []string{"minikube"}, "pools")

	for _, want := range []string{"Node Pools", "userpool", "m5.large"} {
		if !strings.Contains(body, want) {
			t.Fatalf("Pools tab missing %q\n%s", want, body)
		}
	}

	for _, forbidden := range []string{"$/Node/mo", "Pool/mo", "1yr RI Save", "3yr RI Save", "RI Potential"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("Pools tab must not duplicate Cost Intelligence financial columns; found %q\n%s", forbidden, body)
		}
	}
}

func TestInfrastructureTabNavigationLinksAndActiveState(t *testing.T) {
	scan := infrastructureTestScan()

	nodesBody := renderInfrastructurePage(scan, "minikube", []string{"minikube"}, "nodes")
	if !strings.Contains(nodesBody, `class="infra-tab active" href="/infrastructure?cluster=minikube"`) {
		t.Fatalf("Nodes tab should be active by default:\n%s", nodesBody)
	}
	if !strings.Contains(nodesBody, `href="/infrastructure?cluster=minikube&amp;tab=pools"`) {
		t.Fatalf("Pools tab link missing/incorrect on Nodes tab:\n%s", nodesBody)
	}

	poolsBody := renderInfrastructurePage(scan, "minikube", []string{"minikube"}, "pools")
	if !strings.Contains(poolsBody, `class="infra-tab active" href="/infrastructure?cluster=minikube&amp;tab=pools"`) {
		t.Fatalf("Pools tab should be marked active when selected:\n%s", poolsBody)
	}
}
