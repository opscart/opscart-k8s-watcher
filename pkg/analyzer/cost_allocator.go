package analyzer

import (
	"fmt"
	"sort"
)

// CostPoolAllocationInput is the priced, observed capacity of one canonical
// compute pool. MonthlyCost must come from the provider pricing layer; this
// allocator never performs pricing lookups.
type CostPoolAllocationInput struct {
	Key                 CostPoolKey
	MonthlyCost         float64
	CPUCapacityMilli    int64
	MemoryCapacityBytes int64
}

// PodCostAllocation is the point-in-time monthly run-rate attributed to one Pod.
type PodCostAllocation struct {
	Namespace string
	PodName   string

	WorkloadKind string
	WorkloadName string
	WorkloadUID  string

	ParentWorkloadKind string
	ParentWorkloadName string
	ParentWorkloadUID  string

	PoolKey CostPoolKey

	CPURequestMilli    int64
	MemoryRequestBytes int64

	CPUAllocatedMonthly    float64
	MemoryAllocatedMonthly float64
	AllocatedMonthly       float64
}

// NamespaceCostAllocation aggregates canonical Pod allocations.
type NamespaceCostAllocation struct {
	Namespace        string
	AllocatedMonthly float64
	PodCount         int
}

// WorkloadCostAllocation aggregates canonical Pod allocations to exactly one
// leaf workload. Parent workload metadata is a subtotal label only and is not
// included in the leaf key, preventing Job + CronJob double counting.
type WorkloadCostAllocation struct {
	Namespace string

	WorkloadKind string
	WorkloadName string
	WorkloadUID  string

	ParentWorkloadKind string
	ParentWorkloadName string
	ParentWorkloadUID  string

	AllocatedMonthly float64
	PodCount         int
}

// CostPoolAllocationResult reconciles one priced pool.
type CostPoolAllocationResult struct {
	Key CostPoolKey

	ResolvedMonthlyCost float64
	AllocatedMonthly    float64
	IdleMonthly         float64
	UnallocatedMonthly  float64

	CPURequestedMilli    int64
	MemoryRequestedBytes int64

	CPUCapacityMilli    int64
	MemoryCapacityBytes int64

	Pods []PodCostAllocation

	Warnings []string
}

// CanonicalCostAllocationResult is the pure result of allocating already-priced
// pool cost over already-observed Pod inputs.
type CanonicalCostAllocationResult struct {
	Pools      []CostPoolAllocationResult
	Pods       []PodCostAllocation
	Namespaces []NamespaceCostAllocation
	Workloads  []WorkloadCostAllocation

	ResolvedMonthlyCost float64
	AllocatedMonthly    float64
	IdleMonthly         float64
	UnallocatedMonthly  float64

	ExcludedPodCount   int
	UnresolvedPodCount int
	Warnings           []string
}

// AllocateCanonicalCosts performs no Kubernetes or cloud API calls.
//
// Each pool's monthly cost is split 50/50 between CPU and memory capacity.
// Within each dimension:
//   - requested fraction below capacity leaves explicit idle cost;
//   - requested fraction at/above capacity consumes the full dimension budget;
//   - when requests exceed capacity, that dimension is normalized only across
//     the competing requests so allocated dollars never exceed pool dollars.
//
// Therefore:
//
//	allocated + idle + unallocated == resolved pool compute cost.
//
// Unallocated is reserved for cost that cannot be allocated because a required
// capacity dimension is absent/non-positive. It is intentionally distinct from
// idle capacity.
func AllocateCanonicalCosts(pools []CostPoolAllocationInput, pods []PodCostInput) CanonicalCostAllocationResult {
	result := CanonicalCostAllocationResult{}

	poolInputs := make(map[CostPoolKey]CostPoolAllocationInput, len(pools))
	podsByPool := make(map[CostPoolKey][]PodCostInput, len(pools))

	for _, pool := range pools {
		if pool.MonthlyCost < 0 {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("pool %s has negative monthly cost and was excluded", formatPoolKey(pool.Key)))
			continue
		}
		if _, exists := poolInputs[pool.Key]; exists {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("duplicate pool identity %s; later duplicate ignored", formatPoolKey(pool.Key)))
			continue
		}
		poolInputs[pool.Key] = pool
	}

	for _, pod := range pods {
		switch pod.Eligibility {
		case PodCostExcluded:
			result.ExcludedPodCount++
			continue
		case PodCostUnresolved:
			result.UnresolvedPodCount++
			continue
		case PodCostEligible:
			if pod.PoolKey == nil {
				result.UnresolvedPodCount++
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("eligible pod %s/%s has no resolved pool key", pod.Namespace, pod.PodName))
				continue
			}
			if _, ok := poolInputs[*pod.PoolKey]; !ok {
				result.UnresolvedPodCount++
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("eligible pod %s/%s references unresolved pool %s",
						pod.Namespace, pod.PodName, formatPoolKey(*pod.PoolKey)))
				continue
			}
			podsByPool[*pod.PoolKey] = append(podsByPool[*pod.PoolKey], pod)
		default:
			result.UnresolvedPodCount++
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("pod %s/%s has unknown cost eligibility %q",
					pod.Namespace, pod.PodName, pod.Eligibility))
		}
	}

	keys := make([]CostPoolKey, 0, len(poolInputs))
	for key := range poolInputs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return formatPoolKey(keys[i]) < formatPoolKey(keys[j])
	})

	namespaceMap := make(map[string]*NamespaceCostAllocation)
	workloadMap := make(map[string]*WorkloadCostAllocation)

	for _, key := range keys {
		pool := poolInputs[key]
		poolResult := allocatePool(pool, podsByPool[key])

		result.Pools = append(result.Pools, poolResult)
		result.ResolvedMonthlyCost += poolResult.ResolvedMonthlyCost
		result.AllocatedMonthly += poolResult.AllocatedMonthly
		result.IdleMonthly += poolResult.IdleMonthly
		result.UnallocatedMonthly += poolResult.UnallocatedMonthly
		result.Warnings = append(result.Warnings, poolResult.Warnings...)

		for _, pod := range poolResult.Pods {
			result.Pods = append(result.Pods, pod)

			ns := namespaceMap[pod.Namespace]
			if ns == nil {
				ns = &NamespaceCostAllocation{Namespace: pod.Namespace}
				namespaceMap[pod.Namespace] = ns
			}
			ns.AllocatedMonthly += pod.AllocatedMonthly
			ns.PodCount++

			wk := workloadAggregationKey(pod)
			workload := workloadMap[wk]
			if workload == nil {
				workload = &WorkloadCostAllocation{
					Namespace:          pod.Namespace,
					WorkloadKind:       pod.WorkloadKind,
					WorkloadName:       pod.WorkloadName,
					WorkloadUID:        pod.WorkloadUID,
					ParentWorkloadKind: pod.ParentWorkloadKind,
					ParentWorkloadName: pod.ParentWorkloadName,
					ParentWorkloadUID:  pod.ParentWorkloadUID,
				}
				workloadMap[wk] = workload
			}
			workload.AllocatedMonthly += pod.AllocatedMonthly
			workload.PodCount++
		}
	}

	for _, ns := range namespaceMap {
		result.Namespaces = append(result.Namespaces, *ns)
	}
	sort.Slice(result.Namespaces, func(i, j int) bool {
		return result.Namespaces[i].Namespace < result.Namespaces[j].Namespace
	})

	for _, workload := range workloadMap {
		result.Workloads = append(result.Workloads, *workload)
	}
	result.AllocatedMonthly = normalizeAllocationNoise(result.AllocatedMonthly)
	result.IdleMonthly = normalizeAllocationNoise(result.IdleMonthly)
	result.UnallocatedMonthly = normalizeAllocationNoise(result.UnallocatedMonthly)
	sort.Slice(result.Workloads, func(i, j int) bool {
		a, b := result.Workloads[i], result.Workloads[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.WorkloadKind != b.WorkloadKind {
			return a.WorkloadKind < b.WorkloadKind
		}
		if a.WorkloadName != b.WorkloadName {
			return a.WorkloadName < b.WorkloadName
		}
		return a.WorkloadUID < b.WorkloadUID
	})

	return result
}

func allocatePool(pool CostPoolAllocationInput, pods []PodCostInput) CostPoolAllocationResult {
	result := CostPoolAllocationResult{
		Key:                 pool.Key,
		ResolvedMonthlyCost: pool.MonthlyCost,
		CPUCapacityMilli:    pool.CPUCapacityMilli,
		MemoryCapacityBytes: pool.MemoryCapacityBytes,
	}

	for _, pod := range pods {
		if pod.CPURequestMilli > 0 {
			result.CPURequestedMilli += pod.CPURequestMilli
		}
		if pod.MemoryRequestBytes > 0 {
			result.MemoryRequestedBytes += pod.MemoryRequestBytes
		}
	}

	cpuBudget := pool.MonthlyCost / 2.0
	memBudget := pool.MonthlyCost - cpuBudget

	cpuAllocBudget, cpuIdle, cpuUnallocated := dimensionBudget(
		cpuBudget, result.CPURequestedMilli, pool.CPUCapacityMilli)
	memAllocBudget, memIdle, memUnallocated := dimensionBudget(
		memBudget, result.MemoryRequestedBytes, pool.MemoryCapacityBytes)

	result.IdleMonthly = cpuIdle + memIdle
	result.UnallocatedMonthly = cpuUnallocated + memUnallocated

	if pool.CPUCapacityMilli <= 0 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("pool %s has no positive CPU capacity; CPU-side cost is unallocated", formatPoolKey(pool.Key)))
	}
	if pool.MemoryCapacityBytes <= 0 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("pool %s has no positive memory capacity; memory-side cost is unallocated", formatPoolKey(pool.Key)))
	}

	for _, pod := range pods {
		podAllocation := PodCostAllocation{
			Namespace:          pod.Namespace,
			PodName:            pod.PodName,
			WorkloadKind:       pod.WorkloadKind,
			WorkloadName:       pod.WorkloadName,
			WorkloadUID:        string(pod.WorkloadUID),
			ParentWorkloadKind: pod.ParentWorkloadKind,
			ParentWorkloadName: pod.ParentWorkloadName,
			ParentWorkloadUID:  string(pod.ParentWorkloadUID),
			PoolKey:            pool.Key,
			CPURequestMilli:    pod.CPURequestMilli,
			MemoryRequestBytes: pod.MemoryRequestBytes,
		}

		if result.CPURequestedMilli > 0 && pod.CPURequestMilli > 0 {
			podAllocation.CPUAllocatedMonthly =
				cpuAllocBudget * float64(pod.CPURequestMilli) / float64(result.CPURequestedMilli)
		}
		if result.MemoryRequestedBytes > 0 && pod.MemoryRequestBytes > 0 {
			podAllocation.MemoryAllocatedMonthly =
				memAllocBudget * float64(pod.MemoryRequestBytes) / float64(result.MemoryRequestedBytes)
		}

		podAllocation.AllocatedMonthly =
			podAllocation.CPUAllocatedMonthly + podAllocation.MemoryAllocatedMonthly
		result.AllocatedMonthly += podAllocation.AllocatedMonthly
		result.Pods = append(result.Pods, podAllocation)
	}

	// Floating point arithmetic may introduce tiny binary residuals. Reconcile
	// first, then canonicalize sub-nanodollar noise to positive zero so every
	// production surface (JSON, HTML, CSV/XLSX later) sees the same monetary
	// value instead of "-0".
	residual := result.ResolvedMonthlyCost -
		(result.AllocatedMonthly + result.IdleMonthly + result.UnallocatedMonthly)
	if residual != 0 {
		result.UnallocatedMonthly += residual
	}
	result.AllocatedMonthly = normalizeAllocationNoise(result.AllocatedMonthly)
	result.IdleMonthly = normalizeAllocationNoise(result.IdleMonthly)
	result.UnallocatedMonthly = normalizeAllocationNoise(result.UnallocatedMonthly)

	return result
}

const allocationNoiseEpsilon = 1e-9

func normalizeAllocationNoise(value float64) float64 {
	if value > -allocationNoiseEpsilon && value < allocationNoiseEpsilon {
		return 0
	}
	return value
}

func dimensionBudget(monthlyBudget float64, requested, capacity int64) (allocated, idle, unallocated float64) {
	if monthlyBudget <= 0 {
		return 0, 0, 0
	}
	if capacity <= 0 {
		return 0, 0, monthlyBudget
	}
	if requested <= 0 {
		return 0, monthlyBudget, 0
	}

	fraction := float64(requested) / float64(capacity)
	if fraction >= 1 {
		return monthlyBudget, 0, 0
	}
	allocated = monthlyBudget * fraction
	idle = monthlyBudget - allocated
	return allocated, idle, 0
}

func workloadAggregationKey(pod PodCostAllocation) string {
	return pod.Namespace + "\x00" +
		pod.WorkloadKind + "\x00" +
		pod.WorkloadName + "\x00" +
		pod.WorkloadUID
}

func formatPoolKey(key CostPoolKey) string {
	return key.Provider + "/" +
		key.PoolName + "/" +
		key.InstanceType + "/" +
		key.CapacityType + "/" +
		key.Region + "/" +
		key.OS + "/" +
		key.Architecture
}
