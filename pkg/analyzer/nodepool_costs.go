package analyzer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodePoolCostAnalyzer detects AKS/K8s node pools and calculates real costs
type NodePoolCostAnalyzer struct {
	clientset         *kubernetes.Clientset
	ctx               context.Context
	region            string
	providers         map[CloudProvider]PricingProvider
	detectedProvider  CloudProvider
	effectiveProvider CloudProvider
	providerOverride  CloudProvider
	warnings          []string
	lastPriceRefresh  time.Time
}

// NewNodePoolCostAnalyzer creates a new node pool cost analyzer
func NewNodePoolCostAnalyzer(clientset *kubernetes.Clientset, region string) *NodePoolCostAnalyzer {
	return &NodePoolCostAnalyzer{
		clientset: clientset,
		ctx:       context.Background(),
		region:    region,
		providers: map[CloudProvider]PricingProvider{CloudProviderAzure: NewAzurePricingProvider()},
	}
}

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

func (npa *NodePoolCostAnalyzer) Provider() CloudProvider         { return npa.effectiveProvider }
func (npa *NodePoolCostAnalyzer) DetectedProvider() CloudProvider { return npa.detectedProvider }
func (npa *NodePoolCostAnalyzer) ProviderDetectionMode() string {
	if npa.providerOverride != "" {
		return "manual"
	}
	return "detected"
}
func (npa *NodePoolCostAnalyzer) ProviderWarning() string {
	if npa.providerOverride != "" && npa.providerOverride != npa.detectedProvider {
		return fmt.Sprintf("%s pricing is enabled by manual provider override; the cluster provider was not detected as %s.",
			providerDisplayName(npa.providerOverride), providerDisplayName(npa.providerOverride))
	}
	return ""
}
func (npa *NodePoolCostAnalyzer) Region() string { return npa.region }
func (npa *NodePoolCostAnalyzer) PricingWarnings() []string {
	return append([]string(nil), npa.warnings...)
}
func (npa *NodePoolCostAnalyzer) LastPriceRefresh() time.Time { return npa.lastPriceRefresh }

// AnalyzeNodePoolCosts discovers node pools and computes costs from VM SKU pricing
func (npa *NodePoolCostAnalyzer) AnalyzeNodePoolCosts() ([]models.NodePoolCost, []models.NodeInfo, error) {
	nodeList, err := npa.clientset.CoreV1().Nodes().List(npa.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing nodes: %w", err)
	}
	npa.detectedProvider = DetectClusterProvider(nodeList.Items)
	npa.effectiveProvider = npa.detectedProvider
	if npa.providerOverride != "" {
		npa.effectiveProvider = npa.providerOverride
	}

	// Get all pods to calculate per-node resource requests
	podList, err := npa.clientset.CoreV1().Pods("").List(npa.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing pods: %w", err)
	}

	// Map node name → total requests on that node
	nodeRequests := make(map[string]models.ResourceCapacity)
	for _, pod := range podList.Items {
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

	// Discover region from first node if not set
	if npa.region == "" {
		for _, node := range nodeList.Items {
			if r := npa.extractRegion(node); r != "" {
				npa.region = r
				break
			}
		}
	}

	// Group nodes by pool
	poolMap := make(map[string]*nodePoolBuilder)
	var nodeInfos []models.NodeInfo

	for _, node := range nodeList.Items {
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

		poolKey := info.Provider + "\x00" + poolName + "\x00" + info.VMSize + "\x00" + info.Priority + "\x00" + info.Region
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

	return poolCosts, nodeInfos, nil
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

	// Determine VM size — use most common across nodes
	vmSize := b.vmSize
	if vmSize == "" && len(b.nodes) > 0 {
		vmSize = b.nodes[0].VMSize
	}

	// Look up pricing
	var pricePerHour, pricePerMonth float64
	var cpuPerNode, memPerNode float64
	var spotDiscount float64
	var riSavings, riSavings3yr float64

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
	pricing, azureCatalogMatch := LookupVMPrice(vmSize, region)
	if priceErr == nil {
		pricePerHour = priceResult.HourlyPrice
		pricePerMonth = pricePerHour * 730
		if !priceResult.RefreshedAt.IsZero() && priceResult.RefreshedAt.After(npa.lastPriceRefresh) {
			npa.lastPriceRefresh = priceResult.RefreshedAt
		}
	}
	if b.provider == CloudProviderAzure && azureCatalogMatch {
		cpuPerNode = float64(pricing.CPUCores)
		memPerNode = pricing.MemoryGB
		if strings.EqualFold(b.priority, "spot") && priceErr == nil {
			spotDiscount = 1.0 - (pricing.SpotHour / pricing.PayAsYouGoHour)
		}
		// RI savings potential (if not already spot)
		if !strings.EqualFold(b.priority, "spot") {
			if pricing.OneYearRI > 0 {
				riSavings = (pricing.PayAsYouGoMonth - pricing.OneYearRI) * float64(nodeCount)
			}
			if pricing.ThreeYearRI > 0 {
				riSavings3yr = (pricing.PayAsYouGoMonth - pricing.ThreeYearRI) * float64(nodeCount)
			}
		}
	} else if b.provider == CloudProviderAzure && npa.providerOverride == "" {
		// Preserve the existing Azure-only capacity fallback. It is never used
		// for AWS or unknown nodes.
		if len(b.nodes) > 0 {
			cpuPerNode = b.nodes[0].CPUCapacity
			memPerNode = b.nodes[0].MemGBCapacity
			// Try to estimate cost from closest SKU
			if estimated, ok := EstimateVMFromResources(cpuPerNode, memPerNode); ok {
				pricePerHour = estimated.PayAsYouGoHour
				pricePerMonth = estimated.PayAsYouGoMonth
				priceErr = nil
			}
		}
	} else if len(b.nodes) > 0 {
		cpuPerNode = b.nodes[0].CPUCapacity
		memPerNode = b.nodes[0].MemGBCapacity
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
		SpotDiscount:         spotDiscount,
		TotalCPUCapacity:     totalCPUCap,
		TotalMemoryCapacity:  totalMemCap,
		CPURequested:         totalCPUReq,
		MemoryRequested:      totalMemReq,
		CPUUtilizationPct:    cpuUtil,
		MemoryUtilizationPct: memUtil,
		RISavings:            riSavings,
		RISavings3yr:         riSavings3yr,
		PricingWarning: func() string {
			if priceErr != nil {
				return priceErr.Error()
			}
			return ""
		}(),
	}
}

// extractNodeInfo reads node labels/metadata to populate NodeInfo
func (npa *NodePoolCostAnalyzer) extractNodeInfo(node corev1.Node) models.NodeInfo {
	labels := node.Labels

	info := models.NodeInfo{
		Name:     node.Name,
		Provider: string(DetectNodeProvider(node)),
	}

	// AKS node pool labels
	if pool, ok := labels["agentpool"]; ok {
		info.NodePool = pool
	} else if pool, ok := labels["kubernetes.azure.com/agentpool"]; ok {
		info.NodePool = pool
	} else if pool, ok := labels["node.kubernetes.io/instance-type"]; ok {
		// GKE / EKS style
		info.NodePool = labels["cloud.google.com/gke-nodepool"]
		if info.NodePool == "" {
			info.NodePool = labels["eks.amazonaws.com/nodegroup"]
		}
		_ = pool
	}

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
