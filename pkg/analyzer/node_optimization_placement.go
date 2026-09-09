package analyzer

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

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
			"does not model scheduling constraints beyond supplied eligible-node sets",
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

	candidateCount := int64(input.CandidateNodes)
	candidateCPUCapacity, ok := checkedMulInt64(candidateCount, input.NodeCPUCapacityMilli)
	if !ok {
		return invalidNodeOptimizationResult(result, "candidate_cpu_capacity_overflow", "candidate CPU capacity exceeds the int64 representable range")
	}
	candidateMemoryCapacity, ok := checkedMulInt64(candidateCount, input.NodeMemoryCapacityBytes)
	if !ok {
		return invalidNodeOptimizationResult(result, "candidate_memory_capacity_overflow", "candidate memory capacity exceeds the int64 representable range")
	}
	result.CandidateCPUCapacityMilli = candidateCPUCapacity
	result.CandidateMemoryCapacityBytes = candidateMemoryCapacity

	candidateNodeNames, explicitCandidateNames, err := normalizedCandidateNodeNames(input)
	if err != "" {
		return invalidNodeOptimizationResult(result, "candidate_node_identity", err)
	}
	candidateNodeSet := make(map[string]struct{}, len(candidateNodeNames))
	for _, name := range candidateNodeNames {
		candidateNodeSet[name] = struct{}{}
	}

	normalizedPods := append([]NodeOptimizationPodInput(nil), input.Pods...)
	sort.Slice(normalizedPods, func(i, j int) bool {
		return nodeOptimizationPodValidationLess(normalizedPods[i], normalizedPods[j])
	})
	for i := range normalizedPods {
		pod := &normalizedPods[i]
		if pod.EligibleNodeNames == nil {
			continue
		}
		if !explicitCandidateNames {
			return invalidNodeOptimizationResult(
				result,
				"eligible_node_identity",
				fmt.Sprintf("pod %s has an explicit eligible-node set but candidate node identities are unavailable", podDisplayName(*pod)),
			)
		}
		normalized, normalizeErr := normalizeUniqueNodeNames(pod.EligibleNodeNames, true)
		if normalizeErr != "" {
			return invalidNodeOptimizationResult(
				result,
				"eligible_node_identity",
				fmt.Sprintf("pod %s has invalid eligible-node evidence: %s", podDisplayName(*pod), normalizeErr),
			)
		}
		for _, name := range normalized {
			if _, ok := candidateNodeSet[name]; !ok {
				return invalidNodeOptimizationResult(
					result,
					"eligible_node_identity",
					fmt.Sprintf("pod %s references unknown candidate node %q", podDisplayName(*pod), name),
				)
			}
		}
		pod.EligibleNodeNames = normalized
	}

	validationPods := append([]NodeOptimizationPodInput(nil), normalizedPods...)

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
	var totalCPURequestMilli, totalMemoryRequestBytes int64
	for i := range validationPods {
		pod := validationPods[i]
		var ok bool
		totalCPURequestMilli, ok = checkedAddInt64(totalCPURequestMilli, pod.CPURequestMilli)
		if !ok {
			result = invalidNodeOptimizationResult(
				result,
				"aggregate_cpu_request_overflow",
				fmt.Sprintf("aggregate CPU request exceeds the int64 representable range while adding pod %s", podDisplayName(pod)),
			)
			result.Blockers[len(result.Blockers)-1].Pod = podDisplayName(pod)
			return result
		}
		totalMemoryRequestBytes, ok = checkedAddInt64(totalMemoryRequestBytes, pod.MemoryRequestBytes)
		if !ok {
			result = invalidNodeOptimizationResult(
				result,
				"aggregate_memory_request_overflow",
				fmt.Sprintf("aggregate memory request exceeds the int64 representable range while adding pod %s", podDisplayName(pod)),
			)
			result.Blockers[len(result.Blockers)-1].Pod = podDisplayName(pod)
			return result
		}

		if oversizedPod == nil &&
			(pod.CPURequestMilli > input.NodeCPUCapacityMilli || pod.MemoryRequestBytes > input.NodeMemoryCapacityBytes) {
			copy := pod
			oversizedPod = &copy
		}
	}
	result.TotalCPURequestMilli = totalCPURequestMilli
	result.TotalMemoryRequestBytes = totalMemoryRequestBytes
	result = finalizeNodeOptimizationCapacity(result)
	if result.Status == NodeOptimizationInvalidInput {
		return result
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
		return result
	}

	for _, pod := range validationPods {
		if pod.EligibleNodeNames != nil && len(pod.EligibleNodeNames) == 0 {
			result.Status = NodeOptimizationBlockedEligibility
			result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
				Reason:  "no_eligible_candidate_nodes",
				Pod:     podDisplayName(pod),
				Message: "pod has no eligible candidate nodes after applying modeled scheduling constraints",
			})
			return result
		}
	}

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

	pods := append([]NodeOptimizationPodInput(nil), normalizedPods...)
	sort.Slice(pods, func(i, j int) bool {
		ei := eligibleNodeCount(pods[i], len(candidateNodeNames))
		ej := eligibleNodeCount(pods[j], len(candidateNodeNames))
		if ei != ej {
			return ei < ej
		}
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
		if pods[i].Name != pods[j].Name {
			return pods[i].Name < pods[j].Name
		}
		return strings.Join(pods[i].EligibleNodeNames, "\x00") < strings.Join(pods[j].EligibleNodeNames, "\x00")
	})

	type nodeBin struct {
		name    string
		cpuUsed int64
		memUsed int64
	}
	bins := make([]nodeBin, input.CandidateNodes)
	for i, name := range candidateNodeNames {
		bins[i].name = name
	}

	for _, pod := range pods {
		bestIndex := -1
		bestScore := 0.0
		var bestCPUUsed, bestMemoryUsed int64
		eligible := make(map[string]struct{}, len(pod.EligibleNodeNames))
		for _, name := range pod.EligibleNodeNames {
			eligible[name] = struct{}{}
		}

		for i := range bins {
			if pod.EligibleNodeNames != nil {
				if _, ok := eligible[bins[i].name]; !ok {
					continue
				}
			}
			nextCPU, ok := checkedAddInt64(bins[i].cpuUsed, pod.CPURequestMilli)
			if !ok {
				result = invalidNodeOptimizationResult(
					result,
					"placement_cpu_request_overflow",
					fmt.Sprintf("CPU placement arithmetic exceeds the int64 representable range for pod %s", podDisplayName(pod)),
				)
				result.Blockers[len(result.Blockers)-1].Pod = podDisplayName(pod)
				return result
			}
			nextMem, ok := checkedAddInt64(bins[i].memUsed, pod.MemoryRequestBytes)
			if !ok {
				result = invalidNodeOptimizationResult(
					result,
					"placement_memory_request_overflow",
					fmt.Sprintf("memory placement arithmetic exceeds the int64 representable range for pod %s", podDisplayName(pod)),
				)
				result.Blockers[len(result.Blockers)-1].Pod = podDisplayName(pod)
				return result
			}
			if nextCPU > input.NodeCPUCapacityMilli || nextMem > input.NodeMemoryCapacityBytes {
				continue
			}

			remainingCPUValue, ok := checkedSubInt64(input.NodeCPUCapacityMilli, nextCPU)
			if !ok {
				return invalidNodeOptimizationResult(result, "remaining_cpu_capacity_overflow", "remaining CPU capacity cannot be represented as int64")
			}
			remainingMemoryValue, ok := checkedSubInt64(input.NodeMemoryCapacityBytes, nextMem)
			if !ok {
				return invalidNodeOptimizationResult(result, "remaining_memory_capacity_overflow", "remaining memory capacity cannot be represented as int64")
			}
			remainingCPU := float64(remainingCPUValue) / float64(input.NodeCPUCapacityMilli)
			remainingMem := float64(remainingMemoryValue) / float64(input.NodeMemoryCapacityBytes)
			score := remainingCPU + remainingMem

			if bestIndex == -1 || score < bestScore || (score == bestScore && i < bestIndex) {
				bestIndex = i
				bestScore = score
				bestCPUUsed = nextCPU
				bestMemoryUsed = nextMem
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

		bins[bestIndex].cpuUsed = bestCPUUsed
		bins[bestIndex].memUsed = bestMemoryUsed
		result.PlacedPodCount++
		result.Placements = append(result.Placements, NodeOptimizationPlacement{
			Namespace: pod.Namespace,
			PodName:   pod.Name,
			NodeName:  bins[bestIndex].name,
		})
	}

	result.Status = NodeOptimizationFit
	result.PlacementFound = true
	return result
}

func nodeOptimizationPodValidationLess(a, b NodeOptimizationPodInput) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.CPURequestMilli != b.CPURequestMilli {
		return a.CPURequestMilli < b.CPURequestMilli
	}
	if a.MemoryRequestBytes != b.MemoryRequestBytes {
		return a.MemoryRequestBytes < b.MemoryRequestBytes
	}
	return strings.Join(a.EligibleNodeNames, "\x00") < strings.Join(b.EligibleNodeNames, "\x00")
}

func normalizedCandidateNodeNames(input NodeOptimizationInput) ([]string, bool, string) {
	if input.CandidateNodeNames == nil {
		names := make([]string, input.CandidateNodes)
		for i := range names {
			names[i] = fmt.Sprintf("candidate-%09d", i)
		}
		return names, false, ""
	}
	if len(input.CandidateNodeNames) != input.CandidateNodes {
		return nil, true, fmt.Sprintf(
			"candidate node identity count is %d but candidate node count is %d",
			len(input.CandidateNodeNames),
			input.CandidateNodes,
		)
	}
	names, err := normalizeUniqueNodeNames(input.CandidateNodeNames, false)
	return names, true, err
}

func normalizeUniqueNodeNames(names []string, allowEmptySet bool) ([]string, string) {
	if len(names) == 0 && !allowEmptySet {
		return nil, "candidate node identities must not be empty"
	}
	normalized := append([]string(nil), names...)
	sort.Strings(normalized)
	for i, name := range normalized {
		if name == "" {
			return nil, "node identity must not be empty"
		}
		if i > 0 && normalized[i-1] == name {
			return nil, fmt.Sprintf("duplicate node identity %q", name)
		}
	}
	if normalized == nil && allowEmptySet {
		return []string{}, ""
	}
	return normalized, ""
}

func eligibleNodeCount(pod NodeOptimizationPodInput, candidateCount int) int {
	if pod.EligibleNodeNames == nil {
		return candidateCount
	}
	return len(pod.EligibleNodeNames)
}

func invalidNodeOptimizationResult(result NodeOptimizationResult, reason, message string) NodeOptimizationResult {
	result.Status = NodeOptimizationInvalidInput
	result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
		Reason:  reason,
		Message: message,
	})
	return result
}

func finalizeNodeOptimizationCapacity(result NodeOptimizationResult) NodeOptimizationResult {
	cpuHeadroom, ok := checkedSubInt64(result.CandidateCPUCapacityMilli, result.TotalCPURequestMilli)
	if !ok {
		return invalidNodeOptimizationResult(result, "cpu_headroom_overflow", "CPU headroom cannot be represented as int64")
	}
	memoryHeadroom, ok := checkedSubInt64(result.CandidateMemoryCapacityBytes, result.TotalMemoryRequestBytes)
	if !ok {
		return invalidNodeOptimizationResult(result, "memory_headroom_overflow", "memory headroom cannot be represented as int64")
	}
	result.CPUHeadroomMilli = cpuHeadroom
	result.MemoryHeadroomBytes = memoryHeadroom
	return result
}

func checkedAddInt64(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

func checkedSubInt64(a, b int64) (int64, bool) {
	if (b > 0 && a < math.MinInt64+b) || (b < 0 && a > math.MaxInt64+b) {
		return 0, false
	}
	return a - b, true
}

func checkedMulInt64(a, b int64) (int64, bool) {
	switch {
	case a == 0 || b == 0:
		return 0, true
	case a > 0 && b > 0:
		if a > math.MaxInt64/b {
			return 0, false
		}
	case a > 0 && b < 0:
		if b < math.MinInt64/a {
			return 0, false
		}
	case a < 0 && b > 0:
		if a < math.MinInt64/b {
			return 0, false
		}
	case a < 0 && b < 0:
		if a < math.MaxInt64/b {
			return 0, false
		}
	}
	return a * b, true
}

// finiteNonNegativeToInt64 rounds a nonnegative finite float using the
// simulator's existing nearest-integer semantics and rejects values that cannot
// be represented exactly as an int64 result.
func finiteNonNegativeToInt64(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	rounded := math.Round(value)
	// float64(math.MaxInt64) rounds to 2^63, so use that value as an
	// exclusive upper bound instead of converting math.MaxInt64 to float64.
	if rounded >= math.Ldexp(1, 63) {
		return 0, false
	}
	return int64(rounded), true
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

func simulateNMinusOneNodeRemoval(
	key CostPoolKey,
	currentNodeNames []string,
	nodeCPUCapacityMilli int64,
	nodeMemoryCapacityBytes int64,
	pods []NodeOptimizationPodInput,
) (string, []string, NodeOptimizationResult) {
	nodes := append([]string(nil), currentNodeNames...)
	sort.Strings(nodes)

	var firstRemoved string
	var firstCandidates []string
	var firstResult NodeOptimizationResult
	for removedIndex, removedNodeName := range nodes {
		candidateNodeNames := make([]string, 0, len(nodes)-1)
		for i, name := range nodes {
			if i != removedIndex {
				candidateNodeNames = append(candidateNodeNames, name)
			}
		}

		attemptPods := make([]NodeOptimizationPodInput, len(pods))
		candidateSet := make(map[string]struct{}, len(candidateNodeNames))
		for _, name := range candidateNodeNames {
			candidateSet[name] = struct{}{}
		}
		for i, pod := range pods {
			attemptPods[i] = pod
			if pod.EligibleNodeNames == nil {
				continue
			}
			attemptPods[i].EligibleNodeNames = make([]string, 0, len(pod.EligibleNodeNames))
			for _, name := range pod.EligibleNodeNames {
				if _, ok := candidateSet[name]; ok {
					attemptPods[i].EligibleNodeNames = append(attemptPods[i].EligibleNodeNames, name)
				}
			}
		}

		result := SimulateSameShapeNodeCount(NodeOptimizationInput{
			PoolKey:                 key,
			CurrentNodes:            len(nodes),
			CandidateNodes:          len(candidateNodeNames),
			NodeCPUCapacityMilli:    nodeCPUCapacityMilli,
			NodeMemoryCapacityBytes: nodeMemoryCapacityBytes,
			CandidateNodeNames:      candidateNodeNames,
			Pods:                    attemptPods,
		})
		if removedIndex == 0 {
			firstRemoved = removedNodeName
			firstCandidates = candidateNodeNames
			firstResult = result
		}
		if result.PlacementFound {
			return removedNodeName, candidateNodeNames, result
		}
	}

	return firstRemoved, firstCandidates, firstResult
}
