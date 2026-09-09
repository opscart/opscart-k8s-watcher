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

	validationPods := append([]NodeOptimizationPodInput(nil), input.Pods...)
	sort.Slice(validationPods, func(i, j int) bool {
		if validationPods[i].Namespace != validationPods[j].Namespace {
			return validationPods[i].Namespace < validationPods[j].Namespace
		}
		if validationPods[i].Name != validationPods[j].Name {
			return validationPods[i].Name < validationPods[j].Name
		}
		if validationPods[i].CPURequestMilli != validationPods[j].CPURequestMilli {
			return validationPods[i].CPURequestMilli < validationPods[j].CPURequestMilli
		}
		return validationPods[i].MemoryRequestBytes < validationPods[j].MemoryRequestBytes
	})

	for _, pod := range validationPods {
		if pod.CPURequestMilli < 0 || pod.MemoryRequestBytes < 0 {
			result = invalidNodeOptimizationResult(
				result,
				"negative_pod_request",
				fmt.Sprintf("pod %s has a negative resource request", podDisplayName(pod)),
			)
			result.Blockers[len(result.Blockers)-1].Pod = podDisplayName(pod)
			return result
		}
	}

	var oversizedPod *NodeOptimizationPodInput
	for i := range validationPods {
		pod := validationPods[i]
		result.TotalCPURequestMilli += pod.CPURequestMilli
		result.TotalMemoryRequestBytes += pod.MemoryRequestBytes

		if oversizedPod == nil &&
			(pod.CPURequestMilli > input.NodeCPUCapacityMilli || pod.MemoryRequestBytes > input.NodeMemoryCapacityBytes) {
			copy := pod
			oversizedPod = &copy
		}
	}

	if oversizedPod != nil {
		result.Status = NodeOptimizationBlockedPodSize
		result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
			Reason: "pod_exceeds_single_node",
			Pod:    podDisplayName(*oversizedPod),
			Message: fmt.Sprintf(
				"pod requires %dm CPU and %d bytes memory; one candidate node provides %dm CPU and %d bytes memory",
				oversizedPod.CPURequestMilli,
				oversizedPod.MemoryRequestBytes,
				input.NodeCPUCapacityMilli,
				input.NodeMemoryCapacityBytes,
			),
		})
		return finalizeNodeOptimizationCapacity(result, input)
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
	Scenarios                     []NodeOptimizationScenario
	ExcludedPodCount              int
	UnresolvedPodCount            int
	UnresolvedNodeCount           int
	DuplicateNodeCount            int
	DuplicatePodCount             int
	UnsupportedConstraintPodCount int
	SchedulingBlockedPodCount     int
	EvidenceIncomplete            bool
	SkippedPoolCount              int
	Warnings                      []string
}

// NodeOptimizationScenario is one N-1 same-shape pool simulation derived from
// an already-observed cluster snapshot.
type NodeOptimizationScenario struct {
	PoolKey                     CostPoolKey
	EligiblePodCount            int
	DaemonSetCount              int
	DaemonSetCPUPerNodeMilli    int64
	DaemonSetMemoryPerNodeBytes int64
	SchedulingCaveats           []string
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
	return buildNMinusOneNodeOptimizationScenarios(nodeInfos, pods, nil)
}

// BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence adds
// scheduler-relevant Node evidence to the N-1 simulation without performing
// Kubernetes or cloud API calls.
func BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
	nodeInfos []models.NodeInfo,
	pods []corev1.Pod,
	evidence NodeOptimizationSchedulingEvidence,
) NodeOptimizationSnapshotSummary {
	return buildNMinusOneNodeOptimizationScenarios(nodeInfos, pods, &evidence)
}

func buildNMinusOneNodeOptimizationScenarios(
	nodeInfos []models.NodeInfo,
	pods []corev1.Pod,
	evidence *NodeOptimizationSchedulingEvidence,
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
		uid       string
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

	nodeNameCounts := make(map[string]int, len(nodeInfos))
	for _, info := range nodeInfos {
		nodeNameCounts[info.Name]++
	}
	duplicateNodeNames := make([]string, 0)
	for name, count := range nodeNameCounts {
		if count > 1 {
			duplicateNodeNames = append(duplicateNodeNames, name)
		}
	}
	sort.Strings(duplicateNodeNames)
	for _, name := range duplicateNodeNames {
		summary.DuplicateNodeCount++
		summary.EvidenceIncomplete = true
		summary.Warnings = append(summary.Warnings, fmt.Sprintf(
			"duplicate NodeInfo identity %q appears %d times; node optimization evidence is incomplete",
			name,
			nodeNameCounts[name],
		))
	}

	for _, info := range nodeInfos {
		if nodeNameCounts[info.Name] > 1 {
			continue
		}
		knownNodes[info.Name] = struct{}{}

		key := costPoolKeyFromNodeInfo(info)

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
	eligiblePodsByPool := make(map[CostPoolKey][]corev1.Pod)
	eligibilityWarningsByPool := make(map[CostPoolKey][]string)
	daemonSetsByPool := make(map[CostPoolKey]map[daemonSetKey]*daemonSetObservation)
	unsupportedSelectorsByPool := make(map[CostPoolKey][]string)

	podNameCounts := make(map[string]int, len(pods))
	for _, pod := range pods {
		podNameCounts[namespacedKey(pod.Namespace, pod.Name)]++
	}
	duplicatePodNames := make([]string, 0)
	for name, count := range podNameCounts {
		if count > 1 {
			duplicatePodNames = append(duplicatePodNames, name)
		}
	}
	sort.Strings(duplicatePodNames)
	for _, name := range duplicatePodNames {
		summary.DuplicatePodCount++
		summary.EvidenceIncomplete = true
		summary.Warnings = append(summary.Warnings, fmt.Sprintf(
			"duplicate Pod identity %q appears %d times; node optimization evidence is incomplete",
			strings.ReplaceAll(name, "\x00", "/"),
			podNameCounts[name],
		))
	}

	for _, pod := range pods {
		if podNameCounts[namespacedKey(pod.Namespace, pod.Name)] > 1 {
			continue
		}
		input := BuildPodCostInput(pod, knownNodes, nodeKeys, ControllerIndexes{})

		switch input.Eligibility {
		case PodCostExcluded:
			summary.ExcludedPodCount++
			continue
		case PodCostUnresolved:
			summary.UnresolvedPodCount++
			summary.EvidenceIncomplete = true
			reason := input.EligibilityWarning
			if reason == "" {
				reason = string(input.EligibilityReason)
			}
			summary.Warnings = append(summary.Warnings, fmt.Sprintf(
				"pod %s/%s is unresolved for node optimization: %s",
				input.Namespace,
				input.PodName,
				reason,
			))
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
		eligiblePodsByPool[key] = append(eligiblePodsByPool[key], pod)
		if input.EligibilityWarning != "" {
			eligibilityWarningsByPool[key] = append(eligibilityWarningsByPool[key], fmt.Sprintf(
				"pod %s/%s: %s",
				input.Namespace,
				input.PodName,
				input.EligibilityWarning,
			))
		}

		if input.WorkloadKind != "DaemonSet" {
			if reason := nodeSelectorCompatibilityReason(pod.Spec.NodeSelector, key); reason != "" {
				summary.UnsupportedConstraintPodCount++
				unsupportedSelectorsByPool[key] = append(unsupportedSelectorsByPool[key], fmt.Sprintf(
					"pod %s/%s: %s",
					input.Namespace,
					input.PodName,
					reason,
				))
				continue
			}
		}

		if input.WorkloadKind == "DaemonSet" {
			ownerUID, ok := daemonSetControllerUID(pod)
			if !ok {
				summary.EvidenceIncomplete = true
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"pod %s/%s identified as DaemonSet workload %s/%s but controller UID evidence is missing",
					input.Namespace,
					input.PodName,
					input.Namespace,
					input.WorkloadName,
				))
				continue
			}

			dsKey := daemonSetKey{
				namespace: input.Namespace,
				name:      input.WorkloadName,
				uid:       ownerUID,
			}
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

	if evidence != nil {
		if evidence.EvidenceIncomplete ||
			evidence.UnresolvedNodeCount > 0 ||
			evidence.UnmatchedSchedulingNodeCount > 0 ||
			evidence.DuplicateNodeCount > 0 {
			summary.EvidenceIncomplete = true
			summary.UnresolvedNodeCount += evidence.UnresolvedNodeCount
			summary.DuplicateNodeCount += evidence.DuplicateNodeCount
			summary.Warnings = append(summary.Warnings, evidence.Warnings...)
		}
	}

	if summary.EvidenceIncomplete {
		for _, key := range keys {
			if pools[key].nodeCount >= 2 {
				summary.SkippedPoolCount++
			}
		}
		sort.Strings(summary.Warnings)
		return summary
	}

	for _, key := range keys {
		pool := pools[key]

		var schedulingCaveats []string
		if evidence != nil {
			nodes := schedulingNodesForPool(*evidence, key)
			if len(nodes) != pool.nodeCount {
				summary.SkippedPoolCount++
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"pool %s skipped because scheduling evidence covers %d of %d nodes",
					key.PoolName,
					len(nodes),
					pool.nodeCount,
				))
				continue
			}

			hardTaints, homogeneous := homogeneousHardTaints(nodes)
			if !homogeneous {
				summary.SkippedPoolCount++
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"pool %s skipped because hard taints differ across nodes; per-node taint placement is not modeled yet",
					key.PoolName,
				))
				continue
			}

			if poolHasPreferNoSchedule(nodes) {
				schedulingCaveats = append(schedulingCaveats,
					"PreferNoSchedule taints are advisory and are not modeled as hard placement blockers")
			}

			var blockedPods []string
			for _, pod := range eligiblePodsByPool[key] {
				if reason := podHardTaintCompatibilityReason(pod, hardTaints); reason != "" {
					summary.SchedulingBlockedPodCount++
					blockedPods = append(blockedPods, fmt.Sprintf(
						"%s/%s: %s",
						pod.Namespace,
						pod.Name,
						reason,
					))
				}
			}
			if len(blockedPods) > 0 {
				summary.SkippedPoolCount++
				sort.Strings(blockedPods)
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"pool %s skipped because Pods do not have durable toleration for required hard taints: %s",
					key.PoolName,
					strings.Join(blockedPods, "; "),
				))
				continue
			}

			var affinityBlocked, affinityPartial, affinityUnsupported []string
			for _, pod := range eligiblePodsByPool[key] {
				matchCount, unsupportedReason := requiredNodeAffinityMatchCount(pod, nodes)
				podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

				if unsupportedReason != "" {
					summary.UnsupportedConstraintPodCount++
					affinityUnsupported = append(affinityUnsupported, fmt.Sprintf(
						"%s: %s",
						podName,
						unsupportedReason,
					))
					continue
				}
				if matchCount == 0 {
					summary.SchedulingBlockedPodCount++
					affinityBlocked = append(affinityBlocked, fmt.Sprintf(
						"%s: required node affinity matches no node in the pool",
						podName,
					))
					continue
				}
				if matchCount != len(nodes) {
					summary.UnsupportedConstraintPodCount++
					affinityPartial = append(affinityPartial, fmt.Sprintf(
						"%s: required node affinity matches %d of %d nodes",
						podName,
						matchCount,
						len(nodes),
					))
				}
			}

			if len(affinityBlocked) > 0 || len(affinityPartial) > 0 || len(affinityUnsupported) > 0 {
				summary.SkippedPoolCount++
				parts := make([]string, 0, 3)
				if len(affinityBlocked) > 0 {
					sort.Strings(affinityBlocked)
					parts = append(parts, strings.Join(affinityBlocked, "; "))
				}
				if len(affinityPartial) > 0 {
					sort.Strings(affinityPartial)
					parts = append(parts, strings.Join(affinityPartial, "; "))
				}
				if len(affinityUnsupported) > 0 {
					sort.Strings(affinityUnsupported)
					parts = append(parts, strings.Join(affinityUnsupported, "; "))
				}
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"pool %s skipped because required node affinity is not fully compatible with identical-bin simulation: %s",
					key.PoolName,
					strings.Join(parts, "; "),
				))
				continue
			}
		}

		if blockers := unsupportedSelectorsByPool[key]; len(blockers) > 0 {
			summary.SkippedPoolCount++
			sort.Strings(blockers)
			summary.Warnings = append(summary.Warnings, fmt.Sprintf(
				"pool %s skipped because nodeSelector compatibility is not fully modeled: %s",
				key.PoolName,
				strings.Join(blockers, "; "),
			))
			continue
		}

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
		daemonKeys := make([]daemonSetKey, 0, len(daemonSetsByPool[key]))
		for dsKey := range daemonSetsByPool[key] {
			daemonKeys = append(daemonKeys, dsKey)
		}
		sort.Slice(daemonKeys, func(i, j int) bool {
			if daemonKeys[i].namespace != daemonKeys[j].namespace {
				return daemonKeys[i].namespace < daemonKeys[j].namespace
			}
			if daemonKeys[i].name != daemonKeys[j].name {
				return daemonKeys[i].name < daemonKeys[j].name
			}
			return daemonKeys[i].uid < daemonKeys[j].uid
		})

		for _, dsKey := range daemonKeys {
			obs := daemonSetsByPool[key][dsKey]
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

		caveats := append([]string(nil), schedulingCaveats...)
		caveats = append(caveats, eligibilityWarningsByPool[key]...)
		sort.Strings(caveats)

		summary.Scenarios = append(summary.Scenarios, NodeOptimizationScenario{
			PoolKey:                     key,
			EligiblePodCount:            len(scenarioInput.Pods),
			DaemonSetCount:              daemonSetCount,
			DaemonSetCPUPerNodeMilli:    daemonCPUPerNode,
			DaemonSetMemoryPerNodeBytes: daemonMemPerNode,
			SchedulingCaveats:           caveats,
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

func daemonSetControllerUID(pod corev1.Pod) (string, bool) {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind != "DaemonSet" {
			continue
		}
		if owner.Controller != nil && !*owner.Controller {
			continue
		}
		if owner.UID == "" {
			return "", false
		}
		return string(owner.UID), true
	}
	return "", false
}

func nodeSelectorCompatibilityReason(selector map[string]string, pool CostPoolKey) string {
	if len(selector) == 0 {
		return ""
	}

	keys := make([]string, 0, len(selector))
	for key := range selector {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := selector[key]
		switch key {
		case corev1.LabelOSStable, "beta.kubernetes.io/os":
			if value != pool.OS {
				return fmt.Sprintf(
					"nodeSelector %s=%s does not match pool OS %s",
					key,
					value,
					pool.OS,
				)
			}
		case corev1.LabelArchStable, "beta.kubernetes.io/arch":
			if value != pool.Architecture {
				return fmt.Sprintf(
					"nodeSelector %s=%s does not match pool architecture %s",
					key,
					value,
					pool.Architecture,
				)
			}
		default:
			return fmt.Sprintf(
				"nodeSelector key %s is not modeled by the same-shape simulator",
				key,
			)
		}
	}

	return ""
}

// NodeOptimizationSchedulingNode preserves scheduler-relevant evidence from an
// already-acquired corev1.Node snapshot while retaining the canonical pool
// identity derived from NodeInfo. Maps and slices are copied so simulator code
// cannot mutate the Kubernetes snapshot.
type NodeOptimizationSchedulingNode struct {
	Name    string
	PoolKey CostPoolKey
	Labels  map[string]string
	Taints  []corev1.Taint
}

// NodeOptimizationSchedulingEvidence is the join between the cost/capacity
// NodeInfo snapshot and the raw Kubernetes Node snapshot.
//
// Building it performs no Kubernetes API calls. A node is usable as scheduling
// evidence only when it is present in both snapshots and has a complete
// canonical pool identity.
type NodeOptimizationSchedulingEvidence struct {
	Nodes                        map[string]NodeOptimizationSchedulingNode
	UnresolvedNodeCount          int
	UnmatchedSchedulingNodeCount int
	DuplicateNodeCount           int
	EvidenceIncomplete           bool
	Warnings                     []string
}

// BuildNodeOptimizationSchedulingEvidence joins already-acquired NodeInfo and
// corev1.Node snapshots by node name. It is the scheduling evidence boundary
// used by later taint/toleration, affinity, label, and topology simulation.
func BuildNodeOptimizationSchedulingEvidence(
	nodeInfos []models.NodeInfo,
	nodes []corev1.Node,
) NodeOptimizationSchedulingEvidence {
	result := NodeOptimizationSchedulingEvidence{
		Nodes: make(map[string]NodeOptimizationSchedulingNode),
	}

	rawNameCounts := make(map[string]int, len(nodes))
	for _, node := range nodes {
		rawNameCounts[node.Name]++
	}
	infoNameCounts := make(map[string]int, len(nodeInfos))
	for _, info := range nodeInfos {
		infoNameCounts[info.Name]++
	}

	duplicateNames := make(map[string]struct{})
	for name, count := range rawNameCounts {
		if count > 1 {
			duplicateNames[name] = struct{}{}
			result.EvidenceIncomplete = true
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"duplicate Kubernetes Node identity %q appears %d times in scheduling evidence",
				name,
				count,
			))
		}
	}
	for name, count := range infoNameCounts {
		if count > 1 {
			duplicateNames[name] = struct{}{}
			result.EvidenceIncomplete = true
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"duplicate NodeInfo identity %q appears %d times in scheduling evidence",
				name,
				count,
			))
		}
	}
	result.DuplicateNodeCount = len(duplicateNames)

	rawByName := make(map[string]corev1.Node, len(nodes))
	for _, node := range nodes {
		if _, duplicate := duplicateNames[node.Name]; duplicate {
			continue
		}
		rawByName[node.Name] = node
	}

	infoNames := make(map[string]struct{}, len(nodeInfos))
	for _, info := range nodeInfos {
		if _, duplicate := duplicateNames[info.Name]; duplicate {
			continue
		}
		infoNames[info.Name] = struct{}{}

		key := costPoolKeyFromNodeInfo(info)
		if missing := missingCostPoolKeyFields(key); len(missing) > 0 {
			result.UnresolvedNodeCount++
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"node %s excluded from scheduling evidence because canonical pool identity is missing: %s",
				info.Name,
				strings.Join(missing, ", "),
			))
			continue
		}

		node, ok := rawByName[info.Name]
		if !ok {
			result.UnresolvedNodeCount++
			result.EvidenceIncomplete = true
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"node %s excluded from scheduling evidence because it is missing from the Kubernetes Node snapshot",
				info.Name,
			))
			continue
		}

		if reason := nodeInfoSchedulingIdentityMismatch(info, node); reason != "" {
			result.UnresolvedNodeCount++
			result.EvidenceIncomplete = true
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"node %s excluded from scheduling evidence because %s",
				info.Name,
				reason,
			))
			continue
		}

		result.Nodes[info.Name] = NodeOptimizationSchedulingNode{
			Name:    info.Name,
			PoolKey: key,
			Labels:  cloneStringMap(node.Labels),
			Taints:  append([]corev1.Taint(nil), node.Spec.Taints...),
		}
	}

	extraNames := make([]string, 0)
	for _, node := range nodes {
		if _, ok := infoNames[node.Name]; ok {
			continue
		}
		if _, duplicate := duplicateNames[node.Name]; duplicate {
			continue
		}
		result.UnmatchedSchedulingNodeCount++
		result.EvidenceIncomplete = true
		extraNames = append(extraNames, node.Name)
	}
	sort.Strings(extraNames)
	for _, name := range extraNames {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"Kubernetes Node %s has no matching NodeInfo entry and is excluded from scheduling evidence",
			name,
		))
	}

	sort.Strings(result.Warnings)
	return result
}

func costPoolKeyFromNodeInfo(info models.NodeInfo) CostPoolKey {
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
	return key
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func schedulingNodesForPool(
	evidence NodeOptimizationSchedulingEvidence,
	poolKey CostPoolKey,
) []NodeOptimizationSchedulingNode {
	nodes := make([]NodeOptimizationSchedulingNode, 0)
	for _, node := range evidence.Nodes {
		if node.PoolKey == poolKey {
			nodes = append(nodes, node)
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Name < nodes[j].Name
	})
	return nodes
}

func hardTaintsForNode(node NodeOptimizationSchedulingNode) []corev1.Taint {
	taints := make([]corev1.Taint, 0)
	for _, taint := range node.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		taints = append(taints, corev1.Taint{
			Key:    taint.Key,
			Value:  taint.Value,
			Effect: taint.Effect,
		})
	}
	sort.Slice(taints, func(i, j int) bool {
		if taints[i].Key != taints[j].Key {
			return taints[i].Key < taints[j].Key
		}
		if taints[i].Value != taints[j].Value {
			return taints[i].Value < taints[j].Value
		}
		return taints[i].Effect < taints[j].Effect
	})
	return taints
}

func homogeneousHardTaints(nodes []NodeOptimizationSchedulingNode) ([]corev1.Taint, bool) {
	if len(nodes) == 0 {
		return nil, true
	}

	expected := hardTaintsForNode(nodes[0])
	for _, node := range nodes[1:] {
		got := hardTaintsForNode(node)
		if !sameHardTaints(expected, got) {
			return nil, false
		}
	}
	return expected, true
}

func sameHardTaints(a, b []corev1.Taint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].Value != b[i].Value || a[i].Effect != b[i].Effect {
			return false
		}
	}
	return true
}

func poolHasPreferNoSchedule(nodes []NodeOptimizationSchedulingNode) bool {
	for _, node := range nodes {
		for _, taint := range node.Taints {
			if taint.Effect == corev1.TaintEffectPreferNoSchedule {
				return true
			}
		}
	}
	return false
}

func podHardTaintCompatibilityReason(pod corev1.Pod, taints []corev1.Taint) string {
	for _, taint := range taints {
		toleration, ok := matchingToleration(pod.Spec.Tolerations, taint)
		if !ok {
			return fmt.Sprintf(
				"does not tolerate %s=%s:%s",
				taint.Key,
				taint.Value,
				taint.Effect,
			)
		}
		if taint.Effect == corev1.TaintEffectNoExecute && toleration.TolerationSeconds != nil {
			return fmt.Sprintf(
				"tolerates %s=%s:%s only for %d seconds, which is not durable steady-state placement evidence",
				taint.Key,
				taint.Value,
				taint.Effect,
				*toleration.TolerationSeconds,
			)
		}
	}
	return ""
}

func matchingToleration(
	tolerations []corev1.Toleration,
	taint corev1.Taint,
) (corev1.Toleration, bool) {
	var finiteNoExecuteMatch *corev1.Toleration

	for i := range tolerations {
		toleration := tolerations[i]
		if toleration.Effect != "" && toleration.Effect != taint.Effect {
			continue
		}

		operator := toleration.Operator
		if operator == "" {
			operator = corev1.TolerationOpEqual
		}

		matches := false
		switch operator {
		case corev1.TolerationOpExists:
			matches = toleration.Key == "" || toleration.Key == taint.Key
		case corev1.TolerationOpEqual:
			matches = toleration.Key == taint.Key && toleration.Value == taint.Value
		}
		if !matches {
			continue
		}

		if taint.Effect == corev1.TaintEffectNoExecute && toleration.TolerationSeconds != nil {
			if finiteNoExecuteMatch == nil {
				copy := toleration
				finiteNoExecuteMatch = &copy
			}
			continue
		}

		return toleration, true
	}

	if finiteNoExecuteMatch != nil {
		return *finiteNoExecuteMatch, true
	}
	return corev1.Toleration{}, false
}

func requiredNodeAffinityMatchCount(
	pod corev1.Pod,
	nodes []NodeOptimizationSchedulingNode,
) (int, string) {
	if pod.Spec.Affinity == nil ||
		pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return len(nodes), ""
	}

	required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(required.NodeSelectorTerms) == 0 {
		return 0, ""
	}

	for _, term := range required.NodeSelectorTerms {
		if len(term.MatchFields) > 0 {
			return 0, "required node affinity matchFields are not modeled yet"
		}
		for _, expression := range term.MatchExpressions {
			switch expression.Operator {
			case corev1.NodeSelectorOpIn, corev1.NodeSelectorOpExists:
			default:
				return 0, fmt.Sprintf(
					"required node affinity operator %s is not modeled yet",
					expression.Operator,
				)
			}
		}
	}

	matchCount := 0
	for _, node := range nodes {
		if nodeMatchesRequiredNodeAffinity(node.Labels, required.NodeSelectorTerms) {
			matchCount++
		}
	}
	return matchCount, ""
}

func nodeMatchesRequiredNodeAffinity(
	labels map[string]string,
	terms []corev1.NodeSelectorTerm,
) bool {
	for _, term := range terms {
		if nodeMatchesAffinityTerm(labels, term) {
			return true
		}
	}
	return false
}

func nodeMatchesAffinityTerm(labels map[string]string, term corev1.NodeSelectorTerm) bool {
	if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
		return false
	}
	if len(term.MatchFields) > 0 {
		return false
	}

	for _, expression := range term.MatchExpressions {
		switch expression.Operator {
		case corev1.NodeSelectorOpIn:
			value, ok := labels[expression.Key]
			if !ok || !stringSliceContains(expression.Values, value) {
				return false
			}
		case corev1.NodeSelectorOpExists:
			if _, ok := labels[expression.Key]; !ok {
				return false
			}
		default:
			return false
		}
	}

	return true
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func nodeInfoSchedulingIdentityMismatch(info models.NodeInfo, node corev1.Node) string {
	rawOS := firstNonEmptyNodeLabel(node.Labels, corev1.LabelOSStable, "beta.kubernetes.io/os")
	if rawOS != "" && !strings.EqualFold(strings.TrimSpace(info.OS), strings.TrimSpace(rawOS)) {
		return fmt.Sprintf("NodeInfo OS %q disagrees with Kubernetes Node OS label %q", info.OS, rawOS)
	}

	rawArch := firstNonEmptyNodeLabel(node.Labels, corev1.LabelArchStable, "beta.kubernetes.io/arch")
	if rawArch != "" && !strings.EqualFold(strings.TrimSpace(info.Architecture), strings.TrimSpace(rawArch)) {
		return fmt.Sprintf("NodeInfo architecture %q disagrees with Kubernetes Node architecture label %q", info.Architecture, rawArch)
	}
	return ""
}

func firstNonEmptyNodeLabel(labels map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(labels[key]); value != "" {
			return value
		}
	}
	return ""
}
