package analyzer

import (
	"fmt"
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

	for _, pod := range validationPods {
		if pod.EligibleNodeNames != nil && len(pod.EligibleNodeNames) == 0 {
			result.Status = NodeOptimizationBlockedEligibility
			result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
				Reason:  "no_eligible_candidate_nodes",
				Pod:     podDisplayName(pod),
				Message: "pod has no eligible candidate nodes after applying modeled scheduling constraints",
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
