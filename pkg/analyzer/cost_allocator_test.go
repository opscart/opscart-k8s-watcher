package analyzer

import (
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testPoolKey(name string) CostPoolKey {
	return CostPoolKey{
		Provider:     "azure",
		PoolName:     name,
		InstanceType: "Standard_D4s_v3",
		CapacityType: "regular",
		Region:       "centralus",
		OS:           "linux",
		Architecture: "amd64",
	}
}

func eligiblePod(namespace, name string, key CostPoolKey, cpuMilli, memoryBytes int64, workloadKind, workloadName string) PodCostInput {
	return PodCostInput{
		Namespace:          namespace,
		PodName:            name,
		NodeName:           "node-a",
		Phase:              corev1.PodRunning,
		Eligibility:        PodCostEligible,
		EligibilityReason:  PodCostReasonBoundRunning,
		WorkloadKind:       workloadKind,
		WorkloadName:       workloadName,
		WorkloadUID:        types.UID(workloadKind + "-" + workloadName),
		CPURequestMilli:    cpuMilli,
		MemoryRequestBytes: memoryBytes,
		PoolKey:            &key,
	}
}

func closeEnough(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func assertReconciles(t *testing.T, r CanonicalCostAllocationResult) {
	t.Helper()
	got := r.AllocatedMonthly + r.IdleMonthly + r.UnallocatedMonthly
	if !closeEnough(got, r.ResolvedMonthlyCost) {
		t.Fatalf("result does not reconcile: allocated=%f idle=%f unallocated=%f total=%f",
			r.AllocatedMonthly, r.IdleMonthly, r.UnallocatedMonthly, r.ResolvedMonthlyCost)
	}
	for _, pool := range r.Pools {
		gotPool := pool.AllocatedMonthly + pool.IdleMonthly + pool.UnallocatedMonthly
		if !closeEnough(gotPool, pool.ResolvedMonthlyCost) {
			t.Fatalf("pool %s does not reconcile: allocated=%f idle=%f unallocated=%f total=%f",
				pool.Key.PoolName, pool.AllocatedMonthly, pool.IdleMonthly, pool.UnallocatedMonthly, pool.ResolvedMonthlyCost)
		}
	}
}

func TestAllocateCanonicalCostsLeavesExplicitIdle(t *testing.T) {
	key := testPoolKey("userpool")
	result := AllocateCanonicalCosts(
		[]CostPoolAllocationInput{{
			Key: key, MonthlyCost: 1000,
			CPUCapacityMilli: 4000, MemoryCapacityBytes: 8 * 1024,
		}},
		[]PodCostInput{
			eligiblePod("app", "p1", key, 1000, 2*1024, "Deployment", "web"),
		},
	)

	assertReconciles(t, result)

	// 25% CPU request and 25% memory request => 25% of the pool cost allocated.
	if !closeEnough(result.AllocatedMonthly, 250) {
		t.Fatalf("allocated=%f, want 250", result.AllocatedMonthly)
	}
	if !closeEnough(result.IdleMonthly, 750) {
		t.Fatalf("idle=%f, want 750", result.IdleMonthly)
	}
	if !closeEnough(result.UnallocatedMonthly, 0) {
		t.Fatalf("unallocated=%f, want 0", result.UnallocatedMonthly)
	}
}

func TestAllocateCanonicalCostsWeightsCPUAndMemoryIndependently(t *testing.T) {
	key := testPoolKey("userpool")
	result := AllocateCanonicalCosts(
		[]CostPoolAllocationInput{{
			Key: key, MonthlyCost: 1000,
			CPUCapacityMilli: 4000, MemoryCapacityBytes: 1000,
		}},
		[]PodCostInput{
			eligiblePod("a", "cpu-heavy", key, 3000, 100, "Deployment", "cpu"),
			eligiblePod("b", "mem-heavy", key, 1000, 900, "Deployment", "mem"),
		},
	)

	assertReconciles(t, result)
	if !closeEnough(result.AllocatedMonthly, 1000) || !closeEnough(result.IdleMonthly, 0) {
		t.Fatalf("expected fully allocated pool, got allocated=%f idle=%f", result.AllocatedMonthly, result.IdleMonthly)
	}

	if len(result.Pods) != 2 {
		t.Fatalf("got %d pods, want 2", len(result.Pods))
	}
	// CPU-heavy gets 75% of CPU half + 10% of memory half = 425.
	if !closeEnough(result.Pods[0].AllocatedMonthly, 425) {
		t.Fatalf("cpu-heavy allocation=%f, want 425", result.Pods[0].AllocatedMonthly)
	}
	// Memory-heavy gets 25% of CPU half + 90% of memory half = 575.
	if !closeEnough(result.Pods[1].AllocatedMonthly, 575) {
		t.Fatalf("mem-heavy allocation=%f, want 575", result.Pods[1].AllocatedMonthly)
	}
}

func TestAllocateCanonicalCostsOvercommitDoesNotExceedPoolCost(t *testing.T) {
	key := testPoolKey("overcommitted")
	result := AllocateCanonicalCosts(
		[]CostPoolAllocationInput{{
			Key: key, MonthlyCost: 900,
			CPUCapacityMilli: 1000, MemoryCapacityBytes: 1000,
		}},
		[]PodCostInput{
			eligiblePod("ns", "p1", key, 1000, 1000, "Deployment", "web"),
			eligiblePod("ns", "p2", key, 2000, 2000, "Deployment", "api"),
		},
	)

	assertReconciles(t, result)
	if !closeEnough(result.AllocatedMonthly, 900) {
		t.Fatalf("allocated=%f, want 900", result.AllocatedMonthly)
	}
	if !closeEnough(result.IdleMonthly, 0) {
		t.Fatalf("idle=%f, want 0", result.IdleMonthly)
	}

	if !closeEnough(result.Pods[0].AllocatedMonthly, 300) {
		t.Fatalf("p1=%f, want 300", result.Pods[0].AllocatedMonthly)
	}
	if !closeEnough(result.Pods[1].AllocatedMonthly, 600) {
		t.Fatalf("p2=%f, want 600", result.Pods[1].AllocatedMonthly)
	}
}

func TestAllocateCanonicalCostsMissingCapacityIsUnallocatedNotIdle(t *testing.T) {
	key := testPoolKey("broken")
	result := AllocateCanonicalCosts(
		[]CostPoolAllocationInput{{
			Key: key, MonthlyCost: 800,
			CPUCapacityMilli: 0, MemoryCapacityBytes: 1000,
		}},
		[]PodCostInput{
			eligiblePod("ns", "p", key, 500, 500, "Deployment", "web"),
		},
	)

	assertReconciles(t, result)
	// CPU half cannot be allocated. Memory is 50% requested.
	if !closeEnough(result.AllocatedMonthly, 200) {
		t.Fatalf("allocated=%f, want 200", result.AllocatedMonthly)
	}
	if !closeEnough(result.IdleMonthly, 200) {
		t.Fatalf("idle=%f, want 200", result.IdleMonthly)
	}
	if !closeEnough(result.UnallocatedMonthly, 400) {
		t.Fatalf("unallocated=%f, want 400", result.UnallocatedMonthly)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("expected missing-capacity warning")
	}
}

func TestAllocateCanonicalCostsAggregatesNamespaceAndWorkloadFromSamePods(t *testing.T) {
	key := testPoolKey("userpool")
	result := AllocateCanonicalCosts(
		[]CostPoolAllocationInput{{
			Key: key, MonthlyCost: 1000,
			CPUCapacityMilli: 4000, MemoryCapacityBytes: 4000,
		}},
		[]PodCostInput{
			eligiblePod("checkout", "web-1", key, 1000, 1000, "Deployment", "web"),
			eligiblePod("checkout", "web-2", key, 1000, 1000, "Deployment", "web"),
			eligiblePod("payments", "api-1", key, 1000, 1000, "Deployment", "api"),
		},
	)

	assertReconciles(t, result)

	if len(result.Namespaces) != 2 {
		t.Fatalf("namespaces=%d, want 2", len(result.Namespaces))
	}
	if result.Namespaces[0].Namespace != "checkout" || !closeEnough(result.Namespaces[0].AllocatedMonthly, 500) || result.Namespaces[0].PodCount != 2 {
		t.Fatalf("unexpected checkout aggregate: %+v", result.Namespaces[0])
	}
	if result.Namespaces[1].Namespace != "payments" || !closeEnough(result.Namespaces[1].AllocatedMonthly, 250) || result.Namespaces[1].PodCount != 1 {
		t.Fatalf("unexpected payments aggregate: %+v", result.Namespaces[1])
	}

	if len(result.Workloads) != 2 {
		t.Fatalf("workloads=%d, want 2", len(result.Workloads))
	}
}

func TestAllocateCanonicalCostsCronJobParentDoesNotDoubleCount(t *testing.T) {
	key := testPoolKey("jobs")
	job := eligiblePod("ops", "backup-pod", key, 1000, 1000, "Job", "backup-123")
	job.ParentWorkloadKind = "CronJob"
	job.ParentWorkloadName = "backup"
	job.ParentWorkloadUID = "cron-uid"

	result := AllocateCanonicalCosts(
		[]CostPoolAllocationInput{{
			Key: key, MonthlyCost: 100,
			CPUCapacityMilli: 1000, MemoryCapacityBytes: 1000,
		}},
		[]PodCostInput{job},
	)

	assertReconciles(t, result)
	if len(result.Workloads) != 1 {
		t.Fatalf("workloads=%d, want one leaf Job aggregate", len(result.Workloads))
	}
	if result.Workloads[0].WorkloadKind != "Job" || result.Workloads[0].ParentWorkloadKind != "CronJob" {
		t.Fatalf("unexpected workload aggregate: %+v", result.Workloads[0])
	}
	if !closeEnough(result.Workloads[0].AllocatedMonthly, result.AllocatedMonthly) {
		t.Fatalf("workload subtotal double counted or lost: workload=%f total=%f",
			result.Workloads[0].AllocatedMonthly, result.AllocatedMonthly)
	}
}

func TestAllocateCanonicalCostsExcludedAndUnresolvedPodsReceiveNoDollars(t *testing.T) {
	key := testPoolKey("userpool")
	excluded := eligiblePod("ns", "done", key, 1000, 1000, "Job", "done")
	excluded.Eligibility = PodCostExcluded
	excluded.EligibilityReason = PodCostReasonSucceeded

	unresolved := eligiblePod("ns", "pending", key, 1000, 1000, "Pod", "pending")
	unresolved.Eligibility = PodCostUnresolved
	unresolved.EligibilityReason = PodCostReasonUnassignedPending
	unresolved.PoolKey = nil

	result := AllocateCanonicalCosts(
		[]CostPoolAllocationInput{{
			Key: key, MonthlyCost: 1000,
			CPUCapacityMilli: 4000, MemoryCapacityBytes: 4000,
		}},
		[]PodCostInput{excluded, unresolved},
	)

	assertReconciles(t, result)
	if result.AllocatedMonthly != 0 || len(result.Pods) != 0 {
		t.Fatalf("excluded/unresolved pods received allocation: %+v", result.Pods)
	}
	if result.ExcludedPodCount != 1 || result.UnresolvedPodCount != 1 {
		t.Fatalf("counts excluded=%d unresolved=%d, want 1/1",
			result.ExcludedPodCount, result.UnresolvedPodCount)
	}
	if !closeEnough(result.IdleMonthly, 1000) {
		t.Fatalf("idle=%f, want 1000", result.IdleMonthly)
	}
}

func TestAllocateCanonicalCostsUnknownPoolDoesNotBecomeZeroCostAllocation(t *testing.T) {
	key := testPoolKey("missing")
	pod := eligiblePod("ns", "p", key, 1000, 1000, "Deployment", "web")

	result := AllocateCanonicalCosts(nil, []PodCostInput{pod})

	if len(result.Pods) != 0 || result.UnresolvedPodCount != 1 {
		t.Fatalf("expected unresolved pod with no allocation, got %+v", result)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("expected unresolved-pool warning")
	}
}

func TestAllocateCanonicalCostsMultiplePoolsReconcileIndependently(t *testing.T) {
	a := testPoolKey("a")
	b := testPoolKey("b")
	result := AllocateCanonicalCosts(
		[]CostPoolAllocationInput{
			{Key: a, MonthlyCost: 300, CPUCapacityMilli: 1000, MemoryCapacityBytes: 1000},
			{Key: b, MonthlyCost: 700, CPUCapacityMilli: 2000, MemoryCapacityBytes: 2000},
		},
		[]PodCostInput{
			eligiblePod("a", "p1", a, 500, 500, "Deployment", "a"),
			eligiblePod("b", "p2", b, 2000, 2000, "Deployment", "b"),
		},
	)

	assertReconciles(t, result)
	if !closeEnough(result.ResolvedMonthlyCost, 1000) {
		t.Fatalf("resolved total=%f, want 1000", result.ResolvedMonthlyCost)
	}
	if len(result.Pools) != 2 {
		t.Fatalf("pools=%d, want 2", len(result.Pools))
	}
}

func TestNormalizeAllocationNoiseRemovesNegativeZero(t *testing.T) {
	got := normalizeAllocationNoise(-1e-12)
	if got != 0 {
		t.Fatalf("normalizeAllocationNoise(-1e-12)=%v, want 0", got)
	}
	if math.Signbit(got) {
		t.Fatal("normalized monetary zero must not retain a negative sign bit")
	}
}

func TestAllocateCanonicalCostsZeroRequestsLeavesPoolIdle(t *testing.T) {
	key := testPoolKey("idle")
	pod := eligiblePod("ns", "zero", key, 0, 0, "Deployment", "zero")

	result := AllocateCanonicalCosts(
		[]CostPoolAllocationInput{{
			Key: key, MonthlyCost: 420,
			CPUCapacityMilli: 1000, MemoryCapacityBytes: 1000,
		}},
		[]PodCostInput{pod},
	)

	assertReconciles(t, result)
	if !closeEnough(result.IdleMonthly, 420) || !closeEnough(result.AllocatedMonthly, 0) {
		t.Fatalf("allocated=%f idle=%f, want 0/420", result.AllocatedMonthly, result.IdleMonthly)
	}
	if len(result.Pods) != 1 || !closeEnough(result.Pods[0].AllocatedMonthly, 0) {
		t.Fatalf("zero-request pod should be retained with genuine zero allocation: %+v", result.Pods)
	}
}
