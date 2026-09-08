package analyzer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
)

// CanonicalAllocationSummary is the dashboard/report-facing reconciliation
// produced from the canonical Pod allocator.
type CanonicalAllocationSummary struct {
	ResolvedMonthlyCost float64
	AllocatedMonthly    float64
	IdleMonthly         float64
	UnallocatedMonthly  float64
	ExcludedPodCount    int
	UnresolvedPodCount  int
	Namespaces          []models.NamespaceCostInfo
	Workloads           []WorkloadCostAllocation
	Warnings            []string
}

// BuildCanonicalAllocationFromSnapshots bridges already-acquired node-pool
// pricing, NodeInfo metadata, and the shared Pod snapshot into the pure
// canonical allocator. It performs no Kubernetes or cloud API calls.
func BuildCanonicalAllocationFromSnapshots(
	poolCosts []models.NodePoolCost,
	nodeInfos []models.NodeInfo,
	pods []corev1.Pod,
) CanonicalAllocationSummary {
	knownNodes := make(map[string]struct{}, len(nodeInfos))
	for _, info := range nodeInfos {
		knownNodes[info.Name] = struct{}{}
	}

	nodeKeys := make(map[string]*CostPoolKey, len(nodeInfos))
	nodesPerPoolKey := make(map[CostPoolKey]int)
	for _, info := range nodeInfos {
		key := CostPoolKey{
			Provider:     strings.TrimSpace(info.Provider),
			PoolName:     strings.TrimSpace(info.NodePool),
			InstanceType: strings.TrimSpace(info.VMSize),
			CapacityType: strings.TrimSpace(info.Priority),
			Region:       strings.TrimSpace(info.Region),
			OS:           strings.TrimSpace(info.OS),
			Architecture: strings.TrimSpace(info.Architecture),
		}
		if key.PoolName == "" {
			key.PoolName = "default"
		}
		if len(missingCostPoolKeyFields(key)) > 0 {
			continue
		}
		copied := key
		nodeKeys[info.Name] = &copied
		nodesPerPoolKey[key]++
	}

	type pricedGroupKey struct {
		Provider, Pool, Instance, Capacity, Region, OS string
	}
	canonicalKeysByPricedGroup := make(map[pricedGroupKey][]CostPoolKey)
	for key := range nodesPerPoolKey {
		g := pricedGroupKey{
			Provider: strings.ToLower(key.Provider),
			Pool:     key.PoolName,
			Instance: key.InstanceType,
			Capacity: strings.ToLower(key.CapacityType),
			Region:   strings.ToLower(key.Region),
			OS:       strings.ToLower(key.OS),
		}
		canonicalKeysByPricedGroup[g] = append(canonicalKeysByPricedGroup[g], key)
	}

	var allocationPools []CostPoolAllocationInput
	var warnings []string
	for _, pool := range poolCosts {
		if !pool.PricingAvailable {
			continue
		}
		g := pricedGroupKey{
			Provider: strings.ToLower(strings.TrimSpace(pool.Provider)),
			Pool:     strings.TrimSpace(pool.Name),
			Instance: strings.TrimSpace(pool.VMSize),
			Capacity: strings.ToLower(strings.TrimSpace(pool.Priority)),
			Region:   strings.ToLower(strings.TrimSpace(pool.Region)),
			OS:       strings.ToLower(strings.TrimSpace(pool.OS)),
		}
		keys := canonicalKeysByPricedGroup[g]
		if len(keys) == 0 {
			warnings = append(warnings, fmt.Sprintf(
				"priced pool %s could not be mapped to observed nodes for canonical allocation", pool.Name))
			continue
		}

		totalMappedNodes := 0
		for _, key := range keys {
			totalMappedNodes += nodesPerPoolKey[key]
		}
		if totalMappedNodes == 0 {
			continue
		}

		for _, key := range keys {
			nodeCount := nodesPerPoolKey[key]
			fraction := float64(nodeCount) / float64(totalMappedNodes)
			allocationPools = append(allocationPools, CostPoolAllocationInput{
				Key:                 key,
				MonthlyCost:         pool.TotalMonthly * fraction,
				CPUCapacityMilli:    int64(pool.TotalCPUCapacity * 1000 * fraction),
				MemoryCapacityBytes: int64(pool.TotalMemoryCapacity * 1024 * 1024 * 1024 * fraction),
			})
		}
	}

	indexes := ControllerIndexes{}
	inputs := make([]PodCostInput, 0, len(pods))
	for _, pod := range pods {
		inputs = append(inputs, BuildPodCostInput(pod, knownNodes, nodeKeys, indexes))
	}

	allocation := AllocateCanonicalCosts(allocationPools, inputs)
	warnings = append(warnings, allocation.Warnings...)

	usageByNS := make(map[string]struct {
		cpuMilli int64
		memBytes int64
		pods     int
	})
	for _, input := range inputs {
		if input.Eligibility != PodCostEligible || input.PoolKey == nil {
			continue
		}
		u := usageByNS[input.Namespace]
		u.cpuMilli += input.CPURequestMilli
		u.memBytes += input.MemoryRequestBytes
		u.pods++
		usageByNS[input.Namespace] = u
	}

	namespaces := make([]models.NamespaceCostInfo, 0, len(allocation.Namespaces))
	for _, ns := range allocation.Namespaces {
		u := usageByNS[ns.Namespace]
		share := 0.0
		if allocation.ResolvedMonthlyCost > 0 {
			share = ns.AllocatedMonthly / allocation.ResolvedMonthlyCost
		}
		namespaces = append(namespaces, models.NamespaceCostInfo{
			Name: ns.Namespace,
			EstimatedCost: models.CostRange{
				Low: ns.AllocatedMonthly, Best: ns.AllocatedMonthly, High: ns.AllocatedMonthly,
			},
			WeightedShare: share,
			CPUCores:      float64(u.cpuMilli) / 1000.0,
			MemoryGB:      float64(u.memBytes) / (1024 * 1024 * 1024),
			PodCount:      u.pods,
		})
	}
	sort.Slice(namespaces, func(i, j int) bool {
		if namespaces[i].EstimatedCost.Best != namespaces[j].EstimatedCost.Best {
			return namespaces[i].EstimatedCost.Best > namespaces[j].EstimatedCost.Best
		}
		return namespaces[i].Name < namespaces[j].Name
	})

	return CanonicalAllocationSummary{
		ResolvedMonthlyCost: allocation.ResolvedMonthlyCost,
		AllocatedMonthly:    allocation.AllocatedMonthly,
		IdleMonthly:         allocation.IdleMonthly,
		UnallocatedMonthly:  allocation.UnallocatedMonthly,
		ExcludedPodCount:    allocation.ExcludedPodCount,
		UnresolvedPodCount:  allocation.UnresolvedPodCount,
		Namespaces:          namespaces,
		Workloads:           allocation.Workloads,
		Warnings:            warnings,
	}
}

func missingCostPoolKeyFields(key CostPoolKey) []string {
	var missing []string
	if key.Provider == "" {
		missing = append(missing, "provider")
	}
	if key.PoolName == "" {
		missing = append(missing, "pool")
	}
	if key.InstanceType == "" {
		missing = append(missing, "instance type")
	}
	if key.CapacityType == "" {
		missing = append(missing, "capacity type")
	}
	if key.Region == "" {
		missing = append(missing, "region")
	}
	if key.OS == "" {
		missing = append(missing, "os")
	}
	if key.Architecture == "" {
		missing = append(missing, "architecture")
	}
	return missing
}
