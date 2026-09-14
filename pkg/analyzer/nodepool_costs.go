package analyzer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/kube"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodePoolCostAnalyzer detects AKS/K8s node pools and calculates real costs.
//
// docs/08 Phase 4D.6: one NodePoolCostAnalyzer is now constructed per
// cluster and reused for the lifetime of the dashboard process (see
// cmd/opscart-dashboard's dashboardState.costAnalyzer), rather than being
// rebuilt on every scan — that reuse is what lets providers' own pricing
// caches (e.g. azurePricingProvider's 24h TTL) actually survive across
// analysis passes. Because of that, this type is no longer only touched by
// one goroutine at a time: the legacy scan-timer loop and the Coordinator
// callback can both call into the same instance for the same cluster. mu
// protects its mutable runtime bookkeeping. ctx, providers, and
// providerOverride are configured before the analyzer is shared — see the
// configuration methods' doc comment. The pricing providers themselves
// (azurePricingProvider, AWSPricingProvider) already guard their own caches
// with their own mutex. AnalyzeNodePoolCostResultFromResources holds mu
// through provider lookups, including HTTP/API calls on cache misses. This
// intentionally serializes Cost analysis per cluster during the migration so
// one result cannot mix pool costs with another pass's mutable metadata.
type NodePoolCostAnalyzer struct {
	ctx               context.Context
	region            string
	providers         map[CloudProvider]PricingProvider
	providerOverride  CloudProvider
	mu                sync.Mutex
	detectedProvider  CloudProvider
	effectiveProvider CloudProvider
	warnings          []string
	lastPriceRefresh  time.Time
}

// NodePoolCostResult is one internally consistent pricing pass. Metadata is
// captured under the same analyzer lock as PoolCosts and NodeInfos so callers
// cannot combine one generation's Kubernetes-derived costs with another
// concurrent pass's provider state.
type NodePoolCostResult struct {
	PoolCosts             []models.NodePoolCost
	NodeInfos             []models.NodeInfo
	Region                string
	Provider              CloudProvider
	DetectedProvider      CloudProvider
	ProviderDetectionMode string
	ProviderWarning       string
	PricingWarnings       []string
	PricingCapabilities   PricingCapabilities
	LastPriceRefresh      time.Time
}

// NewNodePoolCostAnalyzer creates a new node pool cost analyzer. It takes no
// Kubernetes client: acquisition is the caller's concern (see
// AnalyzeNodePoolCosts for the live-client wrapper and
// AnalyzeNodePoolCostsFromResources for the snapshot-driven path), while
// this type owns only cluster-scoped pricing/provider runtime state.
func NewNodePoolCostAnalyzer(region string) *NodePoolCostAnalyzer {
	return &NodePoolCostAnalyzer{
		ctx:       context.Background(),
		region:    region,
		providers: map[CloudProvider]PricingProvider{CloudProviderAzure: NewAzurePricingProvider()},
	}
}

// SetPricingProvider and SetCloudProviderOverride are configuration steps,
// intended to be called once immediately after construction — before this
// analyzer is shared across goroutines — so they deliberately do not take
// mu (see the type's doc comment).
func (npa *NodePoolCostAnalyzer) SetPricingProvider(provider PricingProvider) {
	if provider != nil {
		npa.providers[provider.Provider()] = provider
	}
}

func (npa *NodePoolCostAnalyzer) SetCloudProviderOverride(provider CloudProvider) {
	if provider == CloudProviderAzure || provider == CloudProviderAWS {
		npa.providerOverride = provider
	}
}

func (npa *NodePoolCostAnalyzer) Provider() CloudProvider {
	npa.mu.Lock()
	defer npa.mu.Unlock()
	return npa.effectiveProvider
}
func (npa *NodePoolCostAnalyzer) DetectedProvider() CloudProvider {
	npa.mu.Lock()
	defer npa.mu.Unlock()
	return npa.detectedProvider
}
func (npa *NodePoolCostAnalyzer) ProviderDetectionMode() string {
	npa.mu.Lock()
	defer npa.mu.Unlock()
	return npa.providerDetectionModeLocked()
}
func (npa *NodePoolCostAnalyzer) ProviderWarning() string {
	npa.mu.Lock()
	defer npa.mu.Unlock()
	return npa.providerWarningLocked()
}
func (npa *NodePoolCostAnalyzer) Region() string {
	npa.mu.Lock()
	defer npa.mu.Unlock()
	return npa.region
}
func (npa *NodePoolCostAnalyzer) PricingWarnings() []string {
	npa.mu.Lock()
	defer npa.mu.Unlock()
	return append([]string(nil), npa.warnings...)
}
func (npa *NodePoolCostAnalyzer) LastPriceRefresh() time.Time {
	npa.mu.Lock()
	defer npa.mu.Unlock()
	return npa.lastPriceRefresh
}

// PricingCapabilities returns the capabilities advertised by the effective
// provider implementation. Unknown and mixed providers remain unsupported
// rather than having capabilities inferred from their names.
func (npa *NodePoolCostAnalyzer) PricingCapabilities() PricingCapabilities {
	npa.mu.Lock()
	defer npa.mu.Unlock()
	return npa.pricingCapabilitiesLocked()
}

// AnalyzeNodePoolCosts is the live-client wrapper: it acquires Nodes and
// Pods itself (one unconditional LIST each) and delegates every other step
// to AnalyzeNodePoolCostsFromResources, so the live and snapshot-driven
// paths run exactly one pool-grouping/pricing algorithm. clientset is a
// per-call parameter rather than a stored field because it is scan-scoped
// (each legacy scan cycle builds a fresh client for its own API-call
// counters — see cmd/opscart-dashboard's kubeClientWithCounters), unlike
// this analyzer's own pricing/provider runtime state, which is cluster-
// scoped and persists across calls.
func (npa *NodePoolCostAnalyzer) AnalyzeNodePoolCosts(clientset kubernetes.Interface) ([]models.NodePoolCost, []models.NodeInfo, error) {
	result, err := npa.AnalyzeNodePoolCostResult(clientset)
	if err != nil {
		return nil, nil, err
	}
	return result.PoolCosts, result.NodeInfos, nil
}

// AnalyzeNodePoolCostResult is AnalyzeNodePoolCosts with the provider metadata
// from the same pricing pass included in its result.
func (npa *NodePoolCostAnalyzer) AnalyzeNodePoolCostResult(clientset kubernetes.Interface) (NodePoolCostResult, error) {
	nodeList, err := clientset.CoreV1().Nodes().List(npa.ctx, metav1.ListOptions{})
	if err != nil {
		return NodePoolCostResult{}, fmt.Errorf("listing nodes: %w", err)
	}

	// Get all pods to calculate per-node resource requests
	podList, err := clientset.CoreV1().Pods("").List(npa.ctx, metav1.ListOptions{})
	if err != nil {
		return NodePoolCostResult{}, fmt.Errorf("listing pods: %w", err)
	}

	return npa.AnalyzeNodePoolCostResultFromResources(nodeList.Items, podList.Items), nil
}

// AnalyzeNodePoolCostsFromResources is AnalyzeNodePoolCosts' Kubernetes-free
// counterpart (docs/08 Phase 4D.6): the same node-pool discovery and
// pricing algorithm, from already-observed Nodes/Pods instead of the
// analyzer's own LIST calls. It performs no Kubernetes API calls; pricing
// lookups still go through npa's persistent, cluster-scoped provider state
// (see this type's doc comment), so a provider's own cache/TTL continues to
// apply across repeated calls exactly as it does for the live-client path.
//
// There is no error return: unlike AuditWaste/AnalyzeNodePoolCosts, there is
// no LIST that can fail here — a per-pool pricing miss is captured as that
// pool's own PricingWarning, not a function-level error, matching the
// existing "unavailable, never guessed" evidence model.
func (npa *NodePoolCostAnalyzer) AnalyzeNodePoolCostsFromResources(nodes []corev1.Node, pods []corev1.Pod) ([]models.NodePoolCost, []models.NodeInfo) {
	result := npa.AnalyzeNodePoolCostResultFromResources(nodes, pods)
	return result.PoolCosts, result.NodeInfos
}

// AnalyzeNodePoolCostResultFromResources returns one atomic result containing
// both snapshot-derived costs and the provider metadata produced by that pass.
func (npa *NodePoolCostAnalyzer) AnalyzeNodePoolCostResultFromResources(nodes []corev1.Node, pods []corev1.Pod) NodePoolCostResult {
	npa.mu.Lock()
	defer npa.mu.Unlock()

	npa.detectedProvider = DetectClusterProvider(nodes)
	npa.effectiveProvider = npa.detectedProvider
	if npa.providerOverride != "" {
		npa.effectiveProvider = npa.providerOverride
	}
	// warnings is this pass's own pricing-warning list, not an
	// accumulating log — must be reset here now that npa (and therefore
	// this slice) persists across many analysis passes instead of being
	// discarded with a freshly-constructed analyzer each time.
	npa.warnings = nil

	// Map node name → total requests on that node
	nodeRequests := make(map[string]models.ResourceCapacity)
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			continue
		}
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				req := nodeRequests[nodeName]
				req.CPU += float64(cpuReq.MilliValue()) / 1000.0
				nodeRequests[nodeName] = req
			}
			if memReq, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				req := nodeRequests[nodeName]
				req.Memory += float64(memReq.Value()) / (1024 * 1024 * 1024)
				nodeRequests[nodeName] = req
			}
		}
	}

	// Discover region from first node if not set. This is sticky by design
	// (like every other field here): once discovered, it is never
	// overwritten by a later pass with a different first node.
	if npa.region == "" {
		for _, node := range nodes {
			if r := npa.extractRegion(node); r != "" {
				npa.region = r
				break
			}
		}
	}

	// Group nodes by pool
	poolMap := make(map[string]*nodePoolBuilder)
	var nodeInfos []models.NodeInfo

	for _, node := range nodes {
		info := npa.extractNodeInfo(node)
		if npa.providerOverride != "" {
			info.Provider = string(npa.providerOverride)
		}

		// Add resource requests
		if req, ok := nodeRequests[node.Name]; ok {
			info.CPURequested = req.CPU
			info.MemGBRequested = req.Memory
		}

		nodeInfos = append(nodeInfos, info)

		// Group by pool
		poolName := info.NodePool
		if poolName == "" {
			poolName = "default"
		}

		poolKey := info.Provider + "\x00" + poolName + "\x00" + info.VMSize + "\x00" + info.Priority + "\x00" + info.Region + "\x00" + info.OS
		if _, exists := poolMap[poolKey]; !exists {
			poolMap[poolKey] = &nodePoolBuilder{
				name:     poolName,
				vmSize:   info.VMSize,
				os:       info.OS,
				priority: info.Priority,
				region:   info.Region,
				provider: CloudProvider(info.Provider),
			}
		}
		poolMap[poolKey].nodes = append(poolMap[poolKey].nodes, info)
	}

	// Build NodePoolCost structs
	var poolCosts []models.NodePoolCost
	for _, builder := range poolMap {
		poolCost := builder.build(npa)
		if poolCost.PricingWarning != "" {
			npa.warnings = append(npa.warnings, fmt.Sprintf("%s: %s", poolCost.Name, poolCost.PricingWarning))
		}
		poolCosts = append(poolCosts, poolCost)
	}

	return NodePoolCostResult{
		PoolCosts:             poolCosts,
		NodeInfos:             nodeInfos,
		Region:                npa.region,
		Provider:              npa.effectiveProvider,
		DetectedProvider:      npa.detectedProvider,
		ProviderDetectionMode: npa.providerDetectionModeLocked(),
		ProviderWarning:       npa.providerWarningLocked(),
		PricingWarnings:       append([]string(nil), npa.warnings...),
		PricingCapabilities:   npa.pricingCapabilitiesLocked(),
		LastPriceRefresh:      npa.lastPriceRefresh,
	}
}

func (npa *NodePoolCostAnalyzer) providerDetectionModeLocked() string {
	if npa.providerOverride != "" {
		return "manual"
	}
	return "detected"
}

func (npa *NodePoolCostAnalyzer) providerWarningLocked() string {
	if npa.providerOverride != "" && npa.providerOverride != npa.detectedProvider {
		return fmt.Sprintf("%s pricing is enabled by manual provider override; the cluster provider was not detected as %s.",
			providerDisplayName(npa.providerOverride), providerDisplayName(npa.providerOverride))
	}
	return ""
}

func (npa *NodePoolCostAnalyzer) pricingCapabilitiesLocked() PricingCapabilities {
	provider, ok := npa.providers[npa.effectiveProvider]
	if !ok {
		return PricingCapabilities{}
	}
	capabilities := provider.Capabilities()
	capabilities.CapacityTypes = append([]string(nil), capabilities.CapacityTypes...)
	return capabilities
}

func providerDisplayName(provider CloudProvider) string {
	switch provider {
	case CloudProviderAzure:
		return "Azure"
	case CloudProviderAWS:
		return "AWS"
	default:
		return "Unknown"
	}
}

// TotalClusterCost sums all node pool monthly costs
func TotalClusterCostFromPools(pools []models.NodePoolCost) float64 {
	total := 0.0
	for _, p := range pools {
		total += p.TotalMonthly
	}
	return total
}

// AllocateNamespaceCosts distributes node pool costs to namespaces proportionally
func (npa *NodePoolCostAnalyzer) AllocateNamespaceCosts(
	pools []models.NodePoolCost,
	namespaces []models.NamespaceResourceUsage,
	totalClusterCPU, totalClusterMem float64,
) []models.NamespaceCostInfo {
	totalNodeCost := TotalClusterCostFromPools(pools)

	var nsCosts []models.NamespaceCostInfo
	for _, ns := range namespaces {
		// Weighted share of cluster resources
		cpuShare := 0.0
		if totalClusterCPU > 0 {
			cpuShare = ns.CPUCoresRequested / totalClusterCPU
		}
		memShare := 0.0
		if totalClusterMem > 0 {
			memShare = ns.MemoryGBRequested / totalClusterMem
		}
		weightedShare := (cpuShare + memShare) / 2.0

		// Allocate costs proportionally
		baseCost := totalNodeCost * weightedShare

		// Calculate range based on pricing uncertainty (±10% for known VMs)
		lowFactor := 0.90
		highFactor := 1.10
		if ns.SpotEligiblePods > 0 {
			spotRatio := float64(ns.SpotEligiblePods) / float64(ns.PodCount)
			lowFactor -= spotRatio * 0.6 // spot could reduce cost up to 60%
		}
		if ns.WasteScore > 50 {
			highFactor += 0.15 // waste adds uncertainty
		}

		nsCosts = append(nsCosts, models.NamespaceCostInfo{
			Name: ns.Name,
			EstimatedCost: models.CostRange{
				Low:  baseCost * lowFactor,
				Best: baseCost,
				High: baseCost * highFactor,
			},
			CPUShare:      cpuShare,
			MemoryShare:   memShare,
			WeightedShare: weightedShare,
			CPUCores:      ns.CPUCoresRequested,
			MemoryGB:      ns.MemoryGBRequested,
			PodCount:      ns.PodCount,
			IdlePods:      ns.IdlePods,
			WasteScore:    ns.WasteScore,
		})
	}

	return nsCosts
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// nodePoolBuilder collects nodes for a single pool
type nodePoolBuilder struct {
	name     string
	vmSize   string
	os       string
	priority string
	region   string
	provider CloudProvider
	nodes    []models.NodeInfo
}

func (b *nodePoolBuilder) build(npa *NodePoolCostAnalyzer) models.NodePoolCost {
	nodeCount := len(b.nodes)

	vmSize := b.vmSize
	if vmSize == "" && len(b.nodes) > 0 {
		vmSize = b.nodes[0].VMSize
	}

	// Capacity comes from the Kubernetes node snapshot. Monetary values come
	// only from the configured provider API. A provider miss/error stays
	// unavailable; static catalogs and closest-SKU estimates must never become
	// production dollar values.
	var cpuPerNode, memPerNode float64
	if len(b.nodes) > 0 {
		cpuPerNode = b.nodes[0].CPUCapacity
		memPerNode = b.nodes[0].MemGBCapacity
	}

	region := b.region
	if region == "" {
		region = npa.region
	}
	providerImpl, configured := npa.providers[b.provider]
	priceResult, priceErr := PriceResult{}, fmt.Errorf("pricing is unavailable for provider %s", b.provider)
	if configured {
		priceResult, priceErr = providerImpl.LookupOnDemandPrice(npa.ctx, PriceRequest{
			InstanceType: vmSize, Region: region, OS: b.os, CapacityType: b.priority,
		})
	}

	var pricePerHour, pricePerMonth float64
	if priceErr == nil && priceResult.HourlyPrice > 0 {
		pricePerHour = priceResult.HourlyPrice
		pricePerMonth = pricePerHour * 730
		if !priceResult.RefreshedAt.IsZero() && priceResult.RefreshedAt.After(npa.lastPriceRefresh) {
			npa.lastPriceRefresh = priceResult.RefreshedAt
		}
	} else if priceErr == nil {
		priceErr = fmt.Errorf("pricing provider returned a non-positive hourly price for %q", vmSize)
	}

	// Sum utilization across all nodes in pool
	var totalCPUCap, totalMemCap, totalCPUReq, totalMemReq float64
	for _, n := range b.nodes {
		totalCPUCap += n.CPUCapacity
		totalMemCap += n.MemGBCapacity
		totalCPUReq += n.CPURequested
		totalMemReq += n.MemGBRequested
	}

	cpuUtil := 0.0
	if totalCPUCap > 0 {
		cpuUtil = (totalCPUReq / totalCPUCap) * 100
	}
	memUtil := 0.0
	if totalMemCap > 0 {
		memUtil = (totalMemReq / totalMemCap) * 100
	}

	// Determine mode (System vs User pool)
	mode := "User"
	for _, n := range b.nodes {
		if n.NodePool == "system" || strings.HasPrefix(n.NodePool, "system") {
			mode = "System"
			break
		}
	}

	return models.NodePoolCost{
		Name:                 b.name,
		VMSize:               vmSize,
		NodeCount:            nodeCount,
		Priority:             b.priority,
		OS:                   b.os,
		Mode:                 mode,
		Provider:             string(b.provider),
		Region:               region,
		PricingAvailable:     priceErr == nil && pricePerMonth > 0,
		CPUCoresPerNode:      cpuPerNode,
		MemoryGBPerNode:      memPerNode,
		PricePerNodeHour:     pricePerHour,
		PricePerNodeMonth:    pricePerMonth,
		TotalMonthly:         pricePerMonth * float64(nodeCount),
		SpotDiscount:         0,
		TotalCPUCapacity:     totalCPUCap,
		TotalMemoryCapacity:  totalMemCap,
		CPURequested:         totalCPUReq,
		MemoryRequested:      totalMemReq,
		CPUUtilizationPct:    cpuUtil,
		MemoryUtilizationPct: memUtil,
		RISavings:            0,
		RISavings3yr:         0,
		PricingWarning: func() string {
			if priceErr != nil {
				return priceErr.Error()
			}
			return ""
		}(),
	}
}

// NodeInfoFromNode derives cost/scheduling identity and capacity for a
// single Kubernetes Node from its labels, status, and allocatable
// resources — no clientset, cluster-wide state, or pricing-provider
// lookup. It is a pure function of node, so callers outside this analyzer
// (e.g. docs/08 Phase 4C's Node Optimization migration, which needs this
// same evidence sourced from a ClusterSnapshot generation rather than a
// live List call) can reuse it directly instead of reimplementing label
// parsing that must otherwise be kept in sync by hand in two places.
//
// It intentionally leaves two things to the caller, exactly as
// AnalyzeNodePoolCosts already does below:
//   - CPURequested/MemGBRequested, which are pod-derived (a cluster-wide
//     Pod list, not anything reachable from a single Node).
//   - A manual cloud-provider override, which is analyzer configuration —
//     not evidence observable on the Node itself — and gets applied by the
//     caller afterward (see AnalyzeNodePoolCosts' own providerOverride
//     step).
func NodeInfoFromNode(node corev1.Node) models.NodeInfo {
	labels := node.Labels

	info := models.NodeInfo{
		Name:     node.Name,
		Provider: string(DetectNodeProvider(node)),
	}

	info.NodePool = kube.NodePoolName(node)

	// VM Size
	if vmSize, ok := labels["node.kubernetes.io/instance-type"]; ok {
		info.VMSize = vmSize
	} else if vmSize, ok := labels["beta.kubernetes.io/instance-type"]; ok {
		info.VMSize = vmSize
	}

	// Region / Zone
	if region, ok := labels["topology.kubernetes.io/region"]; ok {
		info.Region = region
	} else if region, ok := labels["failure-domain.beta.kubernetes.io/region"]; ok {
		info.Region = region
	}
	if zone, ok := labels["topology.kubernetes.io/zone"]; ok {
		info.Zone = zone
	}

	// OS
	if os, ok := labels["kubernetes.io/os"]; ok {
		info.OS = os
	} else {
		info.OS = "linux"
	}

	// Architecture is part of canonical cost-pool identity. Preserve only
	// observed node metadata; do not infer a default architecture.
	if arch, ok := labels["kubernetes.io/arch"]; ok {
		info.Architecture = arch
	} else {
		info.Architecture = node.Status.NodeInfo.Architecture
	}

	// Capacity type / priority.
	if priority, ok := labels["kubernetes.azure.com/scalesetpriority"]; ok {
		info.Priority = priority // "spot" or "regular"
	} else if capacity, ok := labels["eks.amazonaws.com/capacityType"]; ok {
		info.Priority = capacity
	} else {
		info.Priority = "Regular"
	}

	// Capacity
	cpuQ := node.Status.Allocatable[corev1.ResourceCPU]
	info.CPUCapacity = float64(cpuQ.MilliValue()) / 1000.0

	memQ := node.Status.Allocatable[corev1.ResourceMemory]
	info.MemGBCapacity = float64(memQ.Value()) / (1024 * 1024 * 1024)

	return info
}

// extractNodeInfo reads node labels/metadata to populate NodeInfo.
func (npa *NodePoolCostAnalyzer) extractNodeInfo(node corev1.Node) models.NodeInfo {
	return NodeInfoFromNode(node)
}

// extractRegion gets region from any node
func (npa *NodePoolCostAnalyzer) extractRegion(node corev1.Node) string {
	labels := node.Labels
	if r, ok := labels["topology.kubernetes.io/region"]; ok {
		return r
	}
	if r, ok := labels["failure-domain.beta.kubernetes.io/region"]; ok {
		return r
	}
	return ""
}
