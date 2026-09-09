package analyzer

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
)

// NodeOptimizationStatus describes the outcome of a same-shape placement
// simulation. The simulator is intentionally narrower than the Kubernetes
// scheduler: it models CPU and memory requests only in this first slice.
type NodeOptimizationStatus string

const (
	NodeOptimizationFit               NodeOptimizationStatus = "fit"
	NodeOptimizationBlockedAggregate  NodeOptimizationStatus = "blocked_aggregate"
	NodeOptimizationBlockedPodSize    NodeOptimizationStatus = "blocked_pod_exceeds_node"
	NodeOptimizationPlacementNotFound NodeOptimizationStatus = "placement_not_found"
	NodeOptimizationInvalidInput      NodeOptimizationStatus = "invalid_input"
)

// NodeOptimizationPodInput is the scheduler-independent Pod demand consumed by
// the pure simulator. It deliberately contains no Kubernetes client or API
// object dependency.
type NodeOptimizationPodInput struct {
	Namespace          string
	Name               string
	CPURequestMilli    int64
	MemoryRequestBytes int64
}

// NodeOptimizationInput describes a same-SKU node-count scenario.
//
// PoolKey is identity/evidence only. Pricing and Kubernetes acquisition remain
// outside this pure placement engine.
type NodeOptimizationInput struct {
	PoolKey CostPoolKey

	CurrentNodes   int
	CandidateNodes int

	NodeCPUCapacityMilli    int64
	NodeMemoryCapacityBytes int64

	Pods []NodeOptimizationPodInput
}

// NodeOptimizationBlocker is an explicit reason a scenario could not be
// established by the modeled checks.
type NodeOptimizationBlocker struct {
	Reason  string
	Pod     string
	Message string
}

// NodeOptimizationResult is the deterministic result of a same-shape
// CPU/memory placement simulation.
//
// PlacementFound means this simulator found a placement under the modeled
// constraints. It must not be presented as equivalent to Kubernetes scheduler
// feasibility until scheduler constraints are modeled separately.
type NodeOptimizationResult struct {
	Status         NodeOptimizationStatus
	PlacementFound bool

	CurrentNodes   int
	CandidateNodes int

	TotalCPURequestMilli    int64
	TotalMemoryRequestBytes int64

	CandidateCPUCapacityMilli    int64
	CandidateMemoryCapacityBytes int64

	CPUHeadroomMilli    int64
	MemoryHeadroomBytes int64

	PlacedPodCount int
	TotalPodCount  int

	Blockers []NodeOptimizationBlocker
	Caveats  []string
}

// SimulateSameShapeNodeCount evaluates whether the provided Pod requests can be
// placed onto CandidateNodes identical nodes.
//
// The implementation is intentionally pure:
//   - no Kubernetes API calls
//   - no cloud API calls
//   - no persistence
//
// It performs hard aggregate checks first, then deterministic multi-dimensional
// best-fit-decreasing placement using CPU and memory requests.
//
// A placement failure from the heuristic is reported as placement_not_found,
// not as proof that no valid Kubernetes placement exists.
func SimulateSameShapeNodeCount(input NodeOptimizationInput) NodeOptimizationResult {
	result := NodeOptimizationResult{
		CurrentNodes:   input.CurrentNodes,
		CandidateNodes: input.CandidateNodes,
		TotalPodCount:  len(input.Pods),
		Caveats: []string{
			"models CPU and memory requests only",
			"does not yet model Kubernetes scheduling constraints",
		},
	}

	if input.CurrentNodes <= 0 {
		return invalidNodeOptimizationResult(result, "current_nodes", "current node count must be greater than zero")
	}
	if input.CandidateNodes <= 0 {
		return invalidNodeOptimizationResult(result, "candidate_nodes", "candidate node count must be greater than zero")
	}
	if input.NodeCPUCapacityMilli <= 0 {
		return invalidNodeOptimizationResult(result, "node_cpu_capacity", "node CPU capacity must be greater than zero")
	}
	if input.NodeMemoryCapacityBytes <= 0 {
		return invalidNodeOptimizationResult(result, "node_memory_capacity", "node memory capacity must be greater than zero")
	}

	for _, pod := range input.Pods {
		if pod.CPURequestMilli < 0 || pod.MemoryRequestBytes < 0 {
			return invalidNodeOptimizationResult(
				result,
				"negative_pod_request",
				fmt.Sprintf("pod %s has a negative resource request", podDisplayName(pod)),
			)
		}

		result.TotalCPURequestMilli += pod.CPURequestMilli
		result.TotalMemoryRequestBytes += pod.MemoryRequestBytes

		if pod.CPURequestMilli > input.NodeCPUCapacityMilli || pod.MemoryRequestBytes > input.NodeMemoryCapacityBytes {
			result.Status = NodeOptimizationBlockedPodSize
			result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
				Reason: "pod_exceeds_single_node",
				Pod:    podDisplayName(pod),
				Message: fmt.Sprintf(
					"pod requires %dm CPU and %d bytes memory; one candidate node provides %dm CPU and %d bytes memory",
					pod.CPURequestMilli,
					pod.MemoryRequestBytes,
					input.NodeCPUCapacityMilli,
					input.NodeMemoryCapacityBytes,
				),
			})
			return finalizeNodeOptimizationCapacity(result, input)
		}
	}

	result = finalizeNodeOptimizationCapacity(result, input)

	if result.TotalCPURequestMilli > result.CandidateCPUCapacityMilli {
		result.Status = NodeOptimizationBlockedAggregate
		result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
			Reason: "aggregate_cpu_capacity",
			Message: fmt.Sprintf(
				"pod CPU requests total %dm but candidate capacity is %dm",
				result.TotalCPURequestMilli,
				result.CandidateCPUCapacityMilli,
			),
		})
	}
	if result.TotalMemoryRequestBytes > result.CandidateMemoryCapacityBytes {
		result.Status = NodeOptimizationBlockedAggregate
		result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
			Reason: "aggregate_memory_capacity",
			Message: fmt.Sprintf(
				"pod memory requests total %d bytes but candidate capacity is %d bytes",
				result.TotalMemoryRequestBytes,
				result.CandidateMemoryCapacityBytes,
			),
		})
	}
	if len(result.Blockers) > 0 {
		return result
	}

	pods := append([]NodeOptimizationPodInput(nil), input.Pods...)
	sort.Slice(pods, func(i, j int) bool {
		di := dominantRequestFraction(pods[i], input.NodeCPUCapacityMilli, input.NodeMemoryCapacityBytes)
		dj := dominantRequestFraction(pods[j], input.NodeCPUCapacityMilli, input.NodeMemoryCapacityBytes)
		if di != dj {
			return di > dj
		}
		if pods[i].CPURequestMilli != pods[j].CPURequestMilli {
			return pods[i].CPURequestMilli > pods[j].CPURequestMilli
		}
		if pods[i].MemoryRequestBytes != pods[j].MemoryRequestBytes {
			return pods[i].MemoryRequestBytes > pods[j].MemoryRequestBytes
		}
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})

	type nodeBin struct {
		cpuUsed int64
		memUsed int64
	}
	bins := make([]nodeBin, input.CandidateNodes)

	for _, pod := range pods {
		bestIndex := -1
		bestScore := 0.0

		for i := range bins {
			nextCPU := bins[i].cpuUsed + pod.CPURequestMilli
			nextMem := bins[i].memUsed + pod.MemoryRequestBytes
			if nextCPU > input.NodeCPUCapacityMilli || nextMem > input.NodeMemoryCapacityBytes {
				continue
			}

			remainingCPU := float64(input.NodeCPUCapacityMilli-nextCPU) / float64(input.NodeCPUCapacityMilli)
			remainingMem := float64(input.NodeMemoryCapacityBytes-nextMem) / float64(input.NodeMemoryCapacityBytes)
			score := remainingCPU + remainingMem

			if bestIndex == -1 || score < bestScore || (score == bestScore && i < bestIndex) {
				bestIndex = i
				bestScore = score
			}
		}

		if bestIndex == -1 {
			result.Status = NodeOptimizationPlacementNotFound
			result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
				Reason:  "heuristic_placement_failed",
				Pod:     podDisplayName(pod),
				Message: "deterministic CPU/memory best-fit placement could not place this pod; this does not prove that no valid Kubernetes placement exists",
			})
			return result
		}

		bins[bestIndex].cpuUsed += pod.CPURequestMilli
		bins[bestIndex].memUsed += pod.MemoryRequestBytes
		result.PlacedPodCount++
	}

	result.Status = NodeOptimizationFit
	result.PlacementFound = true
	return result
}

func invalidNodeOptimizationResult(result NodeOptimizationResult, reason, message string) NodeOptimizationResult {
	result.Status = NodeOptimizationInvalidInput
	result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
		Reason:  reason,
		Message: message,
	})
	return result
}

func finalizeNodeOptimizationCapacity(result NodeOptimizationResult, input NodeOptimizationInput) NodeOptimizationResult {
	result.CandidateCPUCapacityMilli = int64(input.CandidateNodes) * input.NodeCPUCapacityMilli
	result.CandidateMemoryCapacityBytes = int64(input.CandidateNodes) * input.NodeMemoryCapacityBytes
	result.CPUHeadroomMilli = result.CandidateCPUCapacityMilli - result.TotalCPURequestMilli
	result.MemoryHeadroomBytes = result.CandidateMemoryCapacityBytes - result.TotalMemoryRequestBytes
	return result
}

func dominantRequestFraction(pod NodeOptimizationPodInput, nodeCPUMilli, nodeMemoryBytes int64) float64 {
	cpu := float64(pod.CPURequestMilli) / float64(nodeCPUMilli)
	mem := float64(pod.MemoryRequestBytes) / float64(nodeMemoryBytes)
	if cpu > mem {
		return cpu
	}
	return mem
}

func podDisplayName(pod NodeOptimizationPodInput) string {
	if pod.Namespace == "" {
		return pod.Name
	}
	return pod.Namespace + "/" + pod.Name
}

// NodeOptimizationSnapshotSummary bridges already-acquired Kubernetes snapshots
// into the pure placement simulator. It performs no Kubernetes or cloud API calls.
type NodeOptimizationSnapshotSummary struct {
	Scenarios           []NodeOptimizationScenario
	ExcludedPodCount    int
	UnresolvedPodCount  int
	UnresolvedNodeCount int
	SkippedPoolCount    int
	Warnings            []string
}

// NodeOptimizationScenario is one N-1 same-shape pool simulation derived from
// an already-observed cluster snapshot.
type NodeOptimizationScenario struct {
	PoolKey                     CostPoolKey
	EligiblePodCount            int
	DaemonSetCount              int
	DaemonSetCPUPerNodeMilli    int64
	DaemonSetMemoryPerNodeBytes int64
	Simulation                  NodeOptimizationResult
}

// BuildNMinusOneNodeOptimizationScenariosFromSnapshots constructs one same-shape
// N-1 scenario for each canonical pool with at least two observed nodes.
//
// Pod request and eligibility semantics reuse BuildPodCostInput so Cost and Node
// Optimization do not invent competing definitions of workload demand.
func BuildNMinusOneNodeOptimizationScenariosFromSnapshots(
	nodeInfos []models.NodeInfo,
	pods []corev1.Pod,
) NodeOptimizationSnapshotSummary {
	type poolSnapshot struct {
		key       CostPoolKey
		nodeCount int
		cpuMilli  int64
		memBytes  int64
		invalid   bool
	}

	type daemonSetKey struct {
		namespace string
		name      string
	}

	type daemonSetObservation struct {
		nodes        map[string]struct{}
		cpuMilli     int64
		memBytes     int64
		requestsSet  bool
		inconsistent bool
	}

	summary := NodeOptimizationSnapshotSummary{}
	knownNodes := make(map[string]struct{}, len(nodeInfos))
	nodeKeys := make(map[string]*CostPoolKey, len(nodeInfos))
	pools := make(map[CostPoolKey]*poolSnapshot)

	for _, info := range nodeInfos {
		knownNodes[info.Name] = struct{}{}

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

		if missing := missingCostPoolKeyFields(key); len(missing) > 0 {
			summary.UnresolvedNodeCount++
			summary.Warnings = append(summary.Warnings, fmt.Sprintf(
				"node %s excluded from node optimization because canonical pool identity is missing: %s",
				info.Name,
				strings.Join(missing, ", "),
			))
			continue
		}

		cpuMilli := int64(math.Round(info.CPUCapacity * 1000))
		memBytes := int64(math.Round(info.MemGBCapacity * 1024 * 1024 * 1024))
		if cpuMilli <= 0 || memBytes <= 0 {
			summary.UnresolvedNodeCount++
			summary.Warnings = append(summary.Warnings, fmt.Sprintf(
				"node %s excluded from node optimization because observed CPU or memory capacity is unavailable",
				info.Name,
			))
			continue
		}

		copied := key
		nodeKeys[info.Name] = &copied

		pool := pools[key]
		if pool == nil {
			pool = &poolSnapshot{
				key:      key,
				cpuMilli: cpuMilli,
				memBytes: memBytes,
			}
			pools[key] = pool
		}
		pool.nodeCount++
		if pool.cpuMilli != cpuMilli || pool.memBytes != memBytes {
			pool.invalid = true
		}
	}

	podsByPool := make(map[CostPoolKey][]NodeOptimizationPodInput)
	daemonSetsByPool := make(map[CostPoolKey]map[daemonSetKey]*daemonSetObservation)

	for _, pod := range pods {
		input := BuildPodCostInput(pod, knownNodes, nodeKeys, ControllerIndexes{})

		switch input.Eligibility {
		case PodCostExcluded:
			summary.ExcludedPodCount++
			continue
		case PodCostUnresolved:
			summary.UnresolvedPodCount++
			continue
		case PodCostEligible:
			if input.PoolKey == nil {
				summary.UnresolvedPodCount++
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"pod %s/%s is eligible but has no canonical pool identity",
					input.Namespace,
					input.PodName,
				))
				continue
			}
		default:
			summary.UnresolvedPodCount++
			summary.Warnings = append(summary.Warnings, fmt.Sprintf(
				"pod %s/%s has unknown optimization eligibility %q",
				input.Namespace,
				input.PodName,
				input.Eligibility,
			))
			continue
		}

		key := *input.PoolKey

		if input.WorkloadKind == "DaemonSet" {
			dsKey := daemonSetKey{namespace: input.Namespace, name: input.WorkloadName}
			byDaemonSet := daemonSetsByPool[key]
			if byDaemonSet == nil {
				byDaemonSet = make(map[daemonSetKey]*daemonSetObservation)
				daemonSetsByPool[key] = byDaemonSet
			}
			obs := byDaemonSet[dsKey]
			if obs == nil {
				obs = &daemonSetObservation{nodes: make(map[string]struct{})}
				byDaemonSet[dsKey] = obs
			}
			obs.nodes[input.NodeName] = struct{}{}
			if !obs.requestsSet {
				obs.cpuMilli = input.CPURequestMilli
				obs.memBytes = input.MemoryRequestBytes
				obs.requestsSet = true
			} else if obs.cpuMilli != input.CPURequestMilli || obs.memBytes != input.MemoryRequestBytes {
				obs.inconsistent = true
			}
			continue
		}

		podsByPool[key] = append(podsByPool[key], NodeOptimizationPodInput{
			Namespace:          input.Namespace,
			Name:               input.PodName,
			CPURequestMilli:    input.CPURequestMilli,
			MemoryRequestBytes: input.MemoryRequestBytes,
		})
	}

	keys := make([]CostPoolKey, 0, len(pools))
	for key := range pools {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return costPoolKeySortValue(keys[i]) < costPoolKeySortValue(keys[j])
	})

	for _, key := range keys {
		pool := pools[key]
		if pool.invalid {
			summary.SkippedPoolCount++
			summary.Warnings = append(summary.Warnings, fmt.Sprintf(
				"pool %s skipped because nodes with the same canonical identity expose inconsistent CPU or memory capacity",
				key.PoolName,
			))
			continue
		}
		if pool.nodeCount < 2 {
			continue
		}

		var daemonCPUPerNode, daemonMemPerNode int64
		daemonSetCount := 0
		daemonEvidenceComplete := true
		for dsKey, obs := range daemonSetsByPool[key] {
			if obs.inconsistent {
				daemonEvidenceComplete = false
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"pool %s skipped because DaemonSet %s/%s replicas expose inconsistent effective CPU or memory requests",
					key.PoolName,
					dsKey.namespace,
					dsKey.name,
				))
				continue
			}
			if len(obs.nodes) != pool.nodeCount {
				daemonEvidenceComplete = false
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"pool %s skipped because DaemonSet %s/%s is observed on %d of %d distinct nodes; placement scope is not modeled yet",
					key.PoolName,
					dsKey.namespace,
					dsKey.name,
					len(obs.nodes),
					pool.nodeCount,
				))
				continue
			}
			daemonSetCount++
			daemonCPUPerNode += obs.cpuMilli
			daemonMemPerNode += obs.memBytes
		}

		if !daemonEvidenceComplete {
			summary.SkippedPoolCount++
			continue
		}

		usableCPUPerNode := pool.cpuMilli - daemonCPUPerNode
		usableMemPerNode := pool.memBytes - daemonMemPerNode
		if usableCPUPerNode <= 0 || usableMemPerNode <= 0 {
			summary.SkippedPoolCount++
			summary.Warnings = append(summary.Warnings, fmt.Sprintf(
				"pool %s skipped because observed DaemonSet per-node overhead leaves no positive CPU or memory capacity for movable workloads",
				key.PoolName,
			))
			continue
		}

		scenarioInput := NodeOptimizationInput{
			PoolKey:                 key,
			CurrentNodes:            pool.nodeCount,
			CandidateNodes:          pool.nodeCount - 1,
			NodeCPUCapacityMilli:    usableCPUPerNode,
			NodeMemoryCapacityBytes: usableMemPerNode,
			Pods:                    append([]NodeOptimizationPodInput(nil), podsByPool[key]...),
		}

		summary.Scenarios = append(summary.Scenarios, NodeOptimizationScenario{
			PoolKey:                     key,
			EligiblePodCount:            len(scenarioInput.Pods),
			DaemonSetCount:              daemonSetCount,
			DaemonSetCPUPerNodeMilli:    daemonCPUPerNode,
			DaemonSetMemoryPerNodeBytes: daemonMemPerNode,
			Simulation:                  SimulateSameShapeNodeCount(scenarioInput),
		})
	}

	return summary
}

func costPoolKeySortValue(key CostPoolKey) string {
	return strings.Join([]string{
		key.Provider,
		key.PoolName,
		key.InstanceType,
		key.CapacityType,
		key.Region,
		key.OS,
		key.Architecture,
	}, "\x00")
}
