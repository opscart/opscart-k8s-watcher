package analyzer

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// costTestNode builds a Node carrying the identity/capacity fields
// AnalyzeNodePoolCostsFromResources reads: provider ID, pool/VM/region
// labels, and Allocatable capacity.
func costTestNode(name, providerID string, labels map[string]string, cpuCores, memGB int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(cpuCores, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memGB*1024*1024*1024, resource.BinarySI),
			},
		},
	}
}

func costTestPod(nodeName string, cpuMilli, memGB int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-" + nodeName, Namespace: "default"},
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

// newTestNodePoolCostAnalyzer builds a NodePoolCostAnalyzer wired to a fixed,
// in-memory pricing provider instead of the real network-backed Azure Retail
// Prices provider NewNodePoolCostAnalyzer configures by default — tests that
// don't specifically exercise the real provider/cache must never reach the
// network (docs/08 Phase 4D.6's test requirements).
func newTestNodePoolCostAnalyzer(region string) *NodePoolCostAnalyzer {
	npa := NewNodePoolCostAnalyzer(region)
	npa.SetPricingProvider(fixedPricingProvider{provider: CloudProviderAzure, price: 1})
	return npa
}

func azureNode(name string, cpuCores, memGB int64) *corev1.Node {
	return costTestNode(name, "azure:///subscriptions/x/"+name, map[string]string{
		"node.kubernetes.io/instance-type": "Standard_D4s_v5",
		"topology.kubernetes.io/region":    "eastus2",
		"kubernetes.io/os":                 "linux",
		"agentpool":                        "userpool",
	}, cpuCores, memGB)
}

// TestAnalyzeNodePoolCostsFromResourcesRequiresNoKubernetesClient proves the
// snapshot path performs zero Kubernetes API calls — it is a plain method
// over value slices, with no clientset reachable from it at all.
func TestAnalyzeNodePoolCostsFromResourcesRequiresNoKubernetesClient(t *testing.T) {
	npa := newTestNodePoolCostAnalyzer("")
	nodes := []corev1.Node{*azureNode("node-a", 4, 16)}
	pods := []corev1.Pod{*costTestPod("node-a", 500, 2)}

	poolCosts, nodeInfos := npa.AnalyzeNodePoolCostsFromResources(nodes, pods)

	if len(nodeInfos) != 1 || nodeInfos[0].Name != "node-a" {
		t.Fatalf("nodeInfos = %+v, want one entry for node-a", nodeInfos)
	}
	if len(poolCosts) != 1 {
		t.Fatalf("poolCosts = %+v, want one pool", poolCosts)
	}
}

// TestAnalyzeNodePoolCostsFromResourcesMatchesAnalyzeNodePoolCosts runs the
// same Nodes/Pods through the live-client path (via a fake clientset) and
// the snapshot path and requires identical pool topology and request
// totals — "equivalent inputs produce equivalent results" across both.
func TestAnalyzeNodePoolCostsFromResourcesMatchesAnalyzeNodePoolCosts(t *testing.T) {
	nodes := []*corev1.Node{azureNode("node-a", 4, 16), azureNode("node-b", 4, 16)}
	pods := []*corev1.Pod{costTestPod("node-a", 500, 2), costTestPod("node-b", 250, 1)}

	client := fake.NewSimpleClientset()
	for _, n := range nodes {
		if _, err := client.CoreV1().Nodes().Create(context.Background(), n, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seeding node: %v", err)
		}
	}
	for _, p := range pods {
		if _, err := client.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seeding pod: %v", err)
		}
	}

	live := newTestNodePoolCostAnalyzer("")
	livePoolCosts, liveNodeInfos, err := live.AnalyzeNodePoolCosts(client)
	if err != nil {
		t.Fatalf("AnalyzeNodePoolCosts: %v", err)
	}

	snapshot := newTestNodePoolCostAnalyzer("")
	nodeValues := make([]corev1.Node, len(nodes))
	for i, n := range nodes {
		nodeValues[i] = *n
	}
	podValues := make([]corev1.Pod, len(pods))
	for i, p := range pods {
		podValues[i] = *p
	}
	snapshotPoolCosts, snapshotNodeInfos := snapshot.AnalyzeNodePoolCostsFromResources(nodeValues, podValues)

	if !reflect.DeepEqual(livePoolCosts, snapshotPoolCosts) {
		t.Fatalf("poolCosts diverged:\nlive:     %+v\nsnapshot: %+v", livePoolCosts, snapshotPoolCosts)
	}
	if !reflect.DeepEqual(liveNodeInfos, snapshotNodeInfos) {
		t.Fatalf("nodeInfos diverged:\nlive:     %+v\nsnapshot: %+v", liveNodeInfos, snapshotNodeInfos)
	}
}

// TestAnalyzeNodePoolCostsFromResourcesProviderDetection proves provider
// detection from supplied Nodes matches the live path's DetectClusterProvider
// call — same evidence, same result.
func TestAnalyzeNodePoolCostsFromResourcesProviderDetection(t *testing.T) {
	npa := NewNodePoolCostAnalyzer("")
	nodes := []corev1.Node{*costTestNode("node-a", "aws:///us-east-1a/i-1", nil, 4, 16)}

	npa.AnalyzeNodePoolCostsFromResources(nodes, nil)

	if got := npa.DetectedProvider(); got != CloudProviderAWS {
		t.Fatalf("DetectedProvider() = %q, want aws", got)
	}
	if got := npa.Provider(); got != CloudProviderAWS {
		t.Fatalf("Provider() = %q, want aws (no override configured)", got)
	}
	if got := npa.ProviderDetectionMode(); got != "detected" {
		t.Fatalf("ProviderDetectionMode() = %q, want detected", got)
	}
}

// TestAnalyzeNodePoolCostsFromResourcesManualOverride proves a manual
// override still applies from the snapshot path exactly as it does live:
// effective provider flips, detected provider is preserved as its own
// value, and ProviderWarning fires when they disagree.
func TestAnalyzeNodePoolCostsFromResourcesManualOverride(t *testing.T) {
	npa := NewNodePoolCostAnalyzer("")
	npa.SetCloudProviderOverride(CloudProviderAWS)
	nodes := []corev1.Node{*azureNode("node-a", 4, 16)} // detected as Azure

	_, nodeInfos := npa.AnalyzeNodePoolCostsFromResources(nodes, nil)

	if got := npa.DetectedProvider(); got != CloudProviderAzure {
		t.Fatalf("DetectedProvider() = %q, want azure (unaffected by override)", got)
	}
	if got := npa.Provider(); got != CloudProviderAWS {
		t.Fatalf("Provider() = %q, want aws (override applied)", got)
	}
	if got := npa.ProviderDetectionMode(); got != "manual" {
		t.Fatalf("ProviderDetectionMode() = %q, want manual", got)
	}
	if npa.ProviderWarning() == "" {
		t.Fatal("expected a ProviderWarning: override (aws) disagrees with detection (azure)")
	}
	if nodeInfos[0].Provider != string(CloudProviderAWS) {
		t.Fatalf("nodeInfos[0].Provider = %q, want the override applied to NodeInfo too", nodeInfos[0].Provider)
	}
}

func TestAnalyzeNodePoolCostResultCapturesOnePassMetadata(t *testing.T) {
	npa := NewNodePoolCostAnalyzer("")
	npa.SetCloudProviderOverride(CloudProviderAWS)

	result := npa.AnalyzeNodePoolCostResultFromResources(
		[]corev1.Node{*azureNode("node-a", 4, 16)},
		nil,
	)

	if result.Provider != CloudProviderAWS || result.DetectedProvider != CloudProviderAzure {
		t.Fatalf("provider result = effective %q detected %q, want aws/azure", result.Provider, result.DetectedProvider)
	}
	if result.ProviderDetectionMode != "manual" || result.ProviderWarning == "" {
		t.Fatalf("override metadata = mode %q warning %q, want manual with warning",
			result.ProviderDetectionMode, result.ProviderWarning)
	}
	if result.Region != "eastus2" || len(result.NodeInfos) != 1 || result.NodeInfos[0].Provider != "aws" {
		t.Fatalf("result identity = region %q nodeInfos %+v, want eastus2 with overridden aws node",
			result.Region, result.NodeInfos)
	}
}

// TestAnalyzeNodePoolCostsFromResourcesRegionHandling proves region
// discovery from the first labeled node, and that it is sticky across
// calls — matching the live path's identical "discover once" behavior.
func TestAnalyzeNodePoolCostsFromResourcesRegionHandling(t *testing.T) {
	npa := newTestNodePoolCostAnalyzer("") // no configured region: must be discovered
	nodes := []corev1.Node{*azureNode("node-a", 4, 16)}

	npa.AnalyzeNodePoolCostsFromResources(nodes, nil)
	if got := npa.Region(); got != "eastus2" {
		t.Fatalf("Region() = %q, want eastus2 discovered from node-a", got)
	}

	// A later pass with a different region must not override the sticky
	// first-discovered value.
	otherRegionNode := costTestNode("node-b", "azure:///x", map[string]string{
		"topology.kubernetes.io/region": "westus",
	}, 2, 8)
	npa.AnalyzeNodePoolCostsFromResources([]corev1.Node{*otherRegionNode}, nil)
	if got := npa.Region(); got != "eastus2" {
		t.Fatalf("Region() = %q after a second pass, want eastus2 still sticky", got)
	}
}

// TestAnalyzeNodePoolCostsFromResourcesPricingFailureStaysUnavailable proves
// a provider lookup failure produces an unavailable/warning pool, never a
// guessed price — no fallback catalog, no nearest-SKU substitution.
func TestAnalyzeNodePoolCostsFromResourcesPricingFailureStaysUnavailable(t *testing.T) {
	npa := NewNodePoolCostAnalyzer("")
	npa.SetPricingProvider(fixedPricingProvider{provider: CloudProviderAzure, err: context.DeadlineExceeded})
	nodes := []corev1.Node{*azureNode("node-a", 4, 16)}

	poolCosts, _ := npa.AnalyzeNodePoolCostsFromResources(nodes, nil)

	if len(poolCosts) != 1 {
		t.Fatalf("poolCosts = %+v, want 1 pool", poolCosts)
	}
	if poolCosts[0].PricingAvailable || poolCosts[0].TotalMonthly != 0 {
		t.Fatalf("pool = %+v, want unavailable pricing with zero cost, never a guessed value", poolCosts[0])
	}
	if poolCosts[0].PricingWarning == "" {
		t.Fatal("expected a non-empty PricingWarning explaining the pricing failure")
	}
	if !warningsMention(npa.PricingWarnings(), poolCosts[0].Name) {
		t.Fatalf("PricingWarnings() = %v, want it to mention pool %q", npa.PricingWarnings(), poolCosts[0].Name)
	}
}

func warningsMention(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// TestAnalyzeNodePoolCostsFromResourcesResetsWarningsPerPass proves warnings
// is this pass's own result, not an accumulating log across many analysis
// passes on the same persistent analyzer (docs/08 Phase 4D.6: npa is now
// reused across scan cycles/generations instead of discarded each time).
func TestAnalyzeNodePoolCostsFromResourcesResetsWarningsPerPass(t *testing.T) {
	npa := NewNodePoolCostAnalyzer("")
	npa.SetPricingProvider(fixedPricingProvider{provider: CloudProviderAzure, err: context.DeadlineExceeded})
	nodes := []corev1.Node{*azureNode("node-a", 4, 16)}

	npa.AnalyzeNodePoolCostsFromResources(nodes, nil)
	if len(npa.PricingWarnings()) != 1 {
		t.Fatalf("after pass 1: PricingWarnings() = %v, want exactly 1", npa.PricingWarnings())
	}

	npa.AnalyzeNodePoolCostsFromResources(nodes, nil)
	npa.AnalyzeNodePoolCostsFromResources(nodes, nil)
	if len(npa.PricingWarnings()) != 1 {
		t.Fatalf("after pass 3: PricingWarnings() = %v, want still exactly 1 (reset each pass, not accumulated)", npa.PricingWarnings())
	}
}

// ── Azure provider cache lifetime (docs/08 Phase 0/4D.6 fix) ──────────────

// TestAzureProviderSurvivesMultipleAnalysisPasses proves a persistent
// NodePoolCostAnalyzer's Azure provider cache survives across repeated
// AnalyzeNodePoolCostsFromResources calls — one HTTP price lookup, reused by
// every later pass within TTL — the actual fix for the confirmed defect
// where a fresh analyzer (and therefore a fresh, empty-cache provider) was
// constructed every legacy scan cycle.
func TestAzureProviderSurvivesMultipleAnalysisPasses(t *testing.T) {
	calls := 0
	body := `{"Items":[{"armSkuName":"Standard_D4s_v5","armRegionName":"eastus2","serviceName":"Virtual Machines","productName":"Virtual Machines Dsv5 Series","skuName":"D4s v5","meterName":"D4s v5","type":"Consumption","unitOfMeasure":"1 Hour","currencyCode":"USD","retailPrice":0.5}]}`
	client := azureRetailClient(t, http.StatusOK, body, &calls, "Standard_D4s_v5", "eastus2")
	provider := newAzurePricingProvider(client, azureRetailPricesURL, 24*time.Hour)

	npa := NewNodePoolCostAnalyzer("")
	npa.SetPricingProvider(provider)
	nodes := []corev1.Node{*azureNode("node-a", 4, 16)}

	for i := 0; i < 3; i++ {
		poolCosts, _ := npa.AnalyzeNodePoolCostsFromResources(nodes, nil)
		if !poolCosts[0].PricingAvailable {
			t.Fatalf("pass %d: pricing unavailable: %+v", i, poolCosts[0])
		}
	}
	if calls != 1 {
		t.Fatalf("Azure Retail Prices API calls = %d across 3 analysis passes, want 1 — the cache should serve every pass after the first", calls)
	}
}

// TestAzureProviderCacheExpiryTriggersRefetch proves an expired cache entry
// is refetched, not served stale forever — TTL expiry still works once the
// provider is long-lived. The entry is expired by direct manipulation
// (package-internal, deterministic) rather than a real sleep.
func TestAzureProviderCacheExpiryTriggersRefetch(t *testing.T) {
	calls := 0
	body := `{"Items":[{"armSkuName":"Standard_D4s_v5","armRegionName":"eastus2","serviceName":"Virtual Machines","productName":"Virtual Machines Dsv5 Series","skuName":"D4s v5","meterName":"D4s v5","type":"Consumption","unitOfMeasure":"1 Hour","currencyCode":"USD","retailPrice":0.5}]}`
	client := azureRetailClient(t, http.StatusOK, body, &calls, "Standard_D4s_v5", "eastus2")
	provider := newAzurePricingProvider(client, azureRetailPricesURL, time.Hour)

	npa := NewNodePoolCostAnalyzer("")
	npa.SetPricingProvider(provider)
	nodes := []corev1.Node{*azureNode("node-a", 4, 16)}

	npa.AnalyzeNodePoolCostsFromResources(nodes, nil)
	if calls != 1 {
		t.Fatalf("calls after pass 1 = %d, want 1", calls)
	}

	// Force every cached entry to look expired, without a real sleep.
	provider.mu.Lock()
	for key, entry := range provider.cache {
		entry.expiresAt = time.Now().Add(-time.Second)
		provider.cache[key] = entry
	}
	provider.mu.Unlock()

	npa.AnalyzeNodePoolCostsFromResources(nodes, nil)
	if calls != 2 {
		t.Fatalf("calls after cache expiry = %d, want 2 — an expired entry must be refetched", calls)
	}
}

// TestNodePoolCostAnalyzerClusterIsolation proves two clusters' Cost
// runtimes never share pricing/provider state — one persistent
// NodePoolCostAnalyzer per cluster (docs/08 Phase 4D.6), never a global.
func TestNodePoolCostAnalyzerClusterIsolation(t *testing.T) {
	clusterA := NewNodePoolCostAnalyzer("")
	clusterA.SetCloudProviderOverride(CloudProviderAWS)
	clusterB := NewNodePoolCostAnalyzer("")

	clusterA.AnalyzeNodePoolCostsFromResources([]corev1.Node{*azureNode("node-a", 4, 16)}, nil)
	clusterB.AnalyzeNodePoolCostsFromResources([]corev1.Node{*costTestNode("node-b", "aws:///x", nil, 2, 8)}, nil)

	if clusterA.Provider() != CloudProviderAWS {
		t.Fatalf("cluster A Provider() = %q, want aws (its own override)", clusterA.Provider())
	}
	if clusterB.Provider() == CloudProviderAWS && clusterB.ProviderDetectionMode() == "manual" {
		t.Fatal("cluster B inherited cluster A's manual override — pricing/provider state leaked across clusters")
	}
	if clusterB.ProviderDetectionMode() != "detected" {
		t.Fatalf("cluster B ProviderDetectionMode() = %q, want detected (no override of its own)", clusterB.ProviderDetectionMode())
	}
}

// TestNodePoolCostAnalyzerConcurrentAnalysisIsRaceFree exercises the
// concurrency hazard docs/08 Phase 4D.6 introduced: a persistent, per-
// cluster NodePoolCostAnalyzer is now called from both the legacy scan loop
// and the Coordinator callback, on independent goroutines. Run under
// -race.
func TestNodePoolCostAnalyzerConcurrentAnalysisIsRaceFree(t *testing.T) {
	npa := newTestNodePoolCostAnalyzer("")
	nodes := []corev1.Node{*azureNode("node-a", 4, 16)}
	pods := []corev1.Pod{*costTestPod("node-a", 500, 2)}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			npa.AnalyzeNodePoolCostsFromResources(nodes, pods)
		}()
		go func() {
			defer wg.Done()
			_ = npa.Provider()
			_ = npa.ProviderDetectionMode()
			_ = npa.PricingWarnings()
			_ = npa.LastPriceRefresh()
			_ = npa.PricingCapabilities()
		}()
	}
	wg.Wait()
}
