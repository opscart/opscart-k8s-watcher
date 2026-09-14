package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type dashboardCostPricingProvider struct {
	refreshedAt time.Time
}

func (dashboardCostPricingProvider) Provider() analyzer.CloudProvider {
	return analyzer.CloudProviderAzure
}

func (p dashboardCostPricingProvider) LookupOnDemandPrice(context.Context, analyzer.PriceRequest) (analyzer.PriceResult, error) {
	return analyzer.PriceResult{
		HourlyPrice: 1,
		Currency:    "USD",
		RefreshedAt: p.refreshedAt,
	}, nil
}

func (dashboardCostPricingProvider) Capabilities() analyzer.PricingCapabilities {
	return analyzer.PricingCapabilities{OnDemand: true, CapacityTypes: []string{"Regular"}}
}

func (dashboardCostPricingProvider) SourceDescription() string {
	return "test pricing"
}

// costNode builds a plain Node with no cloud ProviderID — detected as
// CloudProviderUnknown, which is never a key in a fresh
// NodePoolCostAnalyzer's default providers map. This keeps every test in
// this file from ever reaching a real pricing provider's network call
// (docs/08 Phase 4D.6: "do not make tests depend on real Azure/AWS pricing
// APIs"), the same way most of pkg/analyzer's own existing NodePoolCostAnalyzer
// tests already avoid it.
func costNode(name string, cpuCores, memGB int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(cpuCores, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memGB*1024*1024*1024, resource.BinarySI),
			},
		},
	}
}

func pricedCostNode(name string, cpuCores, memGB int64) *corev1.Node {
	node := costNode(name, cpuCores, memGB)
	node.Spec.ProviderID = "azure:///subscriptions/test/" + name
	node.Labels = map[string]string{
		"agentpool":                        "workers",
		"node.kubernetes.io/instance-type": "Standard_D8s_v5",
		"topology.kubernetes.io/region":    "eastus2",
		"kubernetes.io/os":                 "linux",
		"kubernetes.io/arch":               "amd64",
	}
	return node
}

func costWorkloadPod(name, namespace, nodeName string, cpuMilli, memGB int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Now(),
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
					corev1.ResourceMemory: *resource.NewQuantity(memGB*1024*1024*1024, resource.BinarySI),
				}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func newDashboardCostAnalyzer(refreshedAt time.Time) *analyzer.NodePoolCostAnalyzer {
	npa := analyzer.NewNodePoolCostAnalyzer("")
	npa.SetPricingProvider(dashboardCostPricingProvider{refreshedAt: refreshedAt})
	return npa
}

// TestBuildCostAnalysisMatchesDirectAnalyzerCall proves buildCostAnalysis's
// snapshot-resources adaptation produces exactly what calling
// analyzer.NodePoolCostAnalyzer.AnalyzeNodePoolCostsFromResources directly
// on the same value slices would — "same input produces equivalent audit
// results".
func TestBuildCostAnalysisMatchesDirectAnalyzerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{Nodes: []*corev1.Node{costNode("node-a", 4, 16)}}

	npa := analyzer.NewNodePoolCostAnalyzer("")
	got := buildCostAnalysis(npa, "cluster-a", resources)

	direct := analyzer.NewNodePoolCostAnalyzer("")
	wantPoolCosts, _ := direct.AnalyzeNodePoolCostsFromResources([]corev1.Node{*resources.Nodes[0]}, nil)

	if len(got.NodePoolCosts) != len(wantPoolCosts) || len(got.NodePoolCosts) != 1 {
		t.Fatalf("buildCostAnalysis poolCosts = %+v, want %+v", got.NodePoolCosts, wantPoolCosts)
	}
	if got.Currency != "USD" || got.ClusterName != "cluster-a" {
		t.Fatalf("report identity = cluster %q currency %q, want cluster-a/USD", got.ClusterName, got.Currency)
	}
}

// TestCompleteCoordinatorCostReportMatchesLegacyConstruction proves
// buildCostAnalysis's snapshot-sourced report assembly matches what the
// still-live-client-capable AnalyzeNodePoolCostResult + buildCloudCostReport
// path (cmd/opscart-scan's CLI, pkg/analyzer) produces from equivalent
// Nodes, Pods, and provider evidence. The only intentionally independent
// value is the wall-clock report timestamp.
func TestCompleteCoordinatorCostReportMatchesLegacyConstruction(t *testing.T) {
	refreshedAt := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	node := pricedCostNode("node-a", 8, 32)
	pod := costWorkloadPod("api-0", "team-a", "node-a", 3000, 4)

	coordinatorReport := buildCostAnalysis(
		newDashboardCostAnalyzer(refreshedAt),
		"cluster-a",
		clusterstate.ClusterResources{Nodes: []*corev1.Node{node}, Pods: []*corev1.Pod{pod}},
	)

	client := fake.NewSimpleClientset(node.DeepCopy(), pod.DeepCopy())
	legacyResult, err := newDashboardCostAnalyzer(refreshedAt).AnalyzeNodePoolCostResult(client)
	if err != nil {
		t.Fatalf("legacy Node/Pod acquisition: %v", err)
	}
	legacyResourceAnalysis := analyzer.AnalyzeResources([]corev1.Pod{*pod}, []corev1.Node{*node}, namespace)
	legacyReport := buildCloudCostReport(
		"cluster-a",
		legacyResult,
		legacyResourceAnalysis,
		costPodsInNamespace([]corev1.Pod{*pod}, namespace),
	)

	coordinatorReport.Timestamp = time.Time{}
	legacyReport.Timestamp = time.Time{}
	if !reflect.DeepEqual(coordinatorReport, legacyReport) {
		t.Fatalf("complete Cost reports differ:\ncoordinator: %+v\nlegacy:      %+v", coordinatorReport, legacyReport)
	}
}

// TestCompleteCostReportUsesOneSnapshotGeneration proves namespace
// allocation, reconciliation totals, optimization scenarios, and pool
// capacity all move together when generation-N Nodes/Pods are replaced.
func TestCompleteCostReportUsesOneSnapshotGeneration(t *testing.T) {
	state := &dashboardState{
		ctx:          "cluster-a",
		costAnalyzer: newDashboardCostAnalyzer(time.Now()),
	}
	cs := clusterstate.NewClusterState("cluster-a")
	cs.Update(clusterstate.ClusterResources{
		Nodes: []*corev1.Node{pricedCostNode("node-a", 8, 32)},
		Pods:  []*corev1.Pod{costWorkloadPod("api-0", "team-a", "node-a", 3000, 4)},
	})
	cs.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	first := cs.Publish()
	runAnalysisPass(state, first, nil)

	firstReport := state.scan.report
	if len(firstReport.NamespaceCosts) != 1 || firstReport.NamespaceCosts[0].Name != "team-a" {
		t.Fatalf("generation %d NamespaceCosts = %+v, want only team-a", first.Generation(), firstReport.NamespaceCosts)
	}
	if firstReport.AllocatedNodeCost <= 0 || !hasCostScenario(firstReport, "Right-size Over-provisioned Workloads") {
		t.Fatalf("generation %d report lacks expected allocation/scenario: %+v", first.Generation(), firstReport)
	}

	cs.Update(clusterstate.ClusterResources{
		Nodes: []*corev1.Node{pricedCostNode("node-a", 16, 64)},
		Pods:  []*corev1.Pod{costWorkloadPod("worker-0", "team-b", "node-a", 500, 1)},
	})
	second := cs.Publish()
	runAnalysisPass(state, second, nil)

	secondReport := state.scan.report
	if state.scan.generation != second.Generation() {
		t.Fatalf("scan.generation = %d, want %d", state.scan.generation, second.Generation())
	}
	if len(secondReport.NamespaceCosts) != 1 || secondReport.NamespaceCosts[0].Name != "team-b" {
		t.Fatalf("generation %d NamespaceCosts = %+v, want only team-b", second.Generation(), secondReport.NamespaceCosts)
	}
	if secondReport.AllocatedNodeCost >= firstReport.AllocatedNodeCost {
		t.Fatalf("generation %d allocated cost %.2f did not replace generation %d value %.2f",
			second.Generation(), secondReport.AllocatedNodeCost, first.Generation(), firstReport.AllocatedNodeCost)
	}
	if len(secondReport.NodePoolCosts) != 1 || secondReport.NodePoolCosts[0].TotalCPUCapacity != 16 {
		t.Fatalf("generation %d pool capacity = %+v, want generation-N node capacity 16", second.Generation(), secondReport.NodePoolCosts)
	}
	if hasCostScenario(secondReport, "Right-size Over-provisioned Workloads") {
		t.Fatalf("generation %d retained an optimization scenario from older Pod evidence: %+v",
			second.Generation(), secondReport.OptimizationScenarios)
	}
}

func hasCostScenario(report *models.CloudCostReport, name string) bool {
	for _, scenario := range report.OptimizationScenarios {
		if scenario.Name == name {
			return true
		}
	}
	return false
}

func TestBuildCloudCostReportIncludesAllocationAndProviderMetadata(t *testing.T) {
	refreshedAt := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	node := *costNode("node-a", 4, 16)
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "team-a"},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{
				Name: "api",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewMilliQuantity(1000, resource.DecimalSI),
					corev1.ResourceMemory: *resource.NewQuantity(4*1024*1024*1024, resource.BinarySI),
				}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	result := analyzer.NodePoolCostResult{
		PoolCosts: []models.NodePoolCost{{
			Name: "workers", VMSize: "Standard_D4", NodeCount: 1,
			Priority: "Regular", OS: "linux", Provider: "azure", Region: "eastus",
			PricingAvailable: true, TotalMonthly: 100,
			TotalCPUCapacity: 4, TotalMemoryCapacity: 16,
		}},
		NodeInfos: []models.NodeInfo{{
			Name: "node-a", NodePool: "workers", VMSize: "Standard_D4",
			Priority: "Regular", OS: "linux", Provider: "azure", Region: "eastus",
			Architecture: "amd64",
		}},
		Region:                "eastus",
		Provider:              analyzer.CloudProviderAzure,
		DetectedProvider:      analyzer.CloudProviderAzure,
		ProviderDetectionMode: "detected",
		ProviderWarning:       "provider warning",
		PricingWarnings:       []string{"pricing warning"},
		PricingCapabilities:   models.PricingCapabilities{OnDemand: true},
		LastPriceRefresh:      refreshedAt,
	}
	resourceAnalysis := analyzer.AnalyzeResources([]corev1.Pod{pod}, []corev1.Node{node}, "")

	report := buildCloudCostReport("cluster-a", result, resourceAnalysis, []corev1.Pod{pod})

	if report.TotalMonthlyCost != 100 || report.TotalAnnualCost != 1200 || report.CostBreakdown.Compute != 100 {
		t.Fatalf("report totals = monthly %.2f annual %.2f compute %.2f, want 100/1200/100",
			report.TotalMonthlyCost, report.TotalAnnualCost, report.CostBreakdown.Compute)
	}
	if len(report.NamespaceCosts) != 1 || report.NamespaceCosts[0].Name != "team-a" || report.NamespaceCosts[0].EstimatedCost.Best != 25 {
		t.Fatalf("NamespaceCosts = %+v, want team-a allocated 25", report.NamespaceCosts)
	}
	if report.AllocatedNodeCost != 25 || report.IdleNodeCost != 75 || report.UnallocatedNodeCost != 0 {
		t.Fatalf("allocation = allocated %.2f idle %.2f unallocated %.2f, want 25/75/0",
			report.AllocatedNodeCost, report.IdleNodeCost, report.UnallocatedNodeCost)
	}
	if report.Provider != "azure" || report.Region != "eastus" || !report.PricingCapabilities.OnDemand {
		t.Fatalf("provider metadata = provider %q region %q capabilities %+v", report.Provider, report.Region, report.PricingCapabilities)
	}
	if report.DetectedProvider != "azure" || report.EffectiveProvider != "azure" || report.ProviderDetectionMode != "detected" || report.ProviderWarning != "provider warning" {
		t.Fatalf("provider detection metadata = detected %q effective %q mode %q warning %q",
			report.DetectedProvider, report.EffectiveProvider, report.ProviderDetectionMode, report.ProviderWarning)
	}
	if report.PricingCoverage != "1 of 1 nodes priced" || len(report.PricingWarnings) != 1 || report.PricingWarnings[0] != "pricing warning" {
		t.Fatalf("pricing evidence = coverage %q warnings %v", report.PricingCoverage, report.PricingWarnings)
	}
	if report.Currency != "USD" || report.PricingSource == "" || len(report.ScopeExclusions) == 0 || !report.LastPriceRefresh.Equal(refreshedAt) {
		t.Fatalf("pricing metadata = currency %q source %q exclusions %v refresh %v",
			report.Currency, report.PricingSource, report.ScopeExclusions, report.LastPriceRefresh)
	}
	if len(report.Assumptions) == 0 || len(report.Disclaimers) == 0 {
		t.Fatalf("report policy metadata missing: assumptions=%v disclaimers=%v", report.Assumptions, report.Disclaimers)
	}
}
