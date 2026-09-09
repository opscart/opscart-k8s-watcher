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
			"does not model scheduling constraints beyond supplied eligible-node sets, compiled hard topology spread constraints, and compiled required pod anti-affinity",
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
		if eligibleI, eligibleJ := strings.Join(pods[i].EligibleNodeNames, "\x00"), strings.Join(pods[j].EligibleNodeNames, "\x00"); eligibleI != eligibleJ {
			return eligibleI < eligibleJ
		}
		if topologyI, topologyJ := topologySpreadConstraintSortValue(pods[i]), topologySpreadConstraintSortValue(pods[j]); topologyI != topologyJ {
			return topologyI < topologyJ
		}
		return requiredAntiAffinitySortValue(pods[i]) < requiredAntiAffinitySortValue(pods[j])
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
		topologyBlockedReason := ""
		antiAffinityBlockedReason := ""
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

			topologyAllowed, topologyReason, topologyInvalidReason := topologySpreadAllowsPlacement(
				pod,
				bins[i].name,
				result.Placements,
				input.removedNodeName,
			)
			if topologyInvalidReason != "" {
				result = invalidNodeOptimizationResult(result, "topology_spread_evidence", topologyInvalidReason)
				result.Blockers[len(result.Blockers)-1].Pod = podDisplayName(pod)
				return result
			}
			if !topologyAllowed {
				if topologyBlockedReason == "" {
					topologyBlockedReason = topologyReason
				}
				continue
			}

			antiAffinityAllowed, antiAffinityReason, antiAffinityInvalidReason := requiredPodAntiAffinityAllowsPlacement(
				pod,
				bins[i].name,
				result.Placements,
				input.removedNodeName,
			)
			if antiAffinityInvalidReason != "" {
				result = invalidNodeOptimizationResult(result, "pod_anti_affinity_evidence", antiAffinityInvalidReason)
				result.Blockers[len(result.Blockers)-1].Pod = podDisplayName(pod)
				return result
			}
			if !antiAffinityAllowed {
				if antiAffinityBlockedReason == "" {
					antiAffinityBlockedReason = antiAffinityReason
				}
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
			reason := "heuristic_placement_failed"
			message := "deterministic CPU/memory best-fit placement could not place this pod; this does not prove that no valid Kubernetes placement exists"
			if topologyBlockedReason != "" {
				reason = "topology_spread_constraint"
				message = topologyBlockedReason + "; deterministic placement could not prove a valid assignment"
			} else if antiAffinityBlockedReason != "" {
				reason = "required_pod_anti_affinity"
				message = antiAffinityBlockedReason + "; deterministic placement could not prove a valid assignment"
			}
			result.Blockers = append(result.Blockers, NodeOptimizationBlocker{
				Reason:  reason,
				Pod:     podDisplayName(pod),
				Message: message,
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
	if eligibleA, eligibleB := strings.Join(a.EligibleNodeNames, "\x00"), strings.Join(b.EligibleNodeNames, "\x00"); eligibleA != eligibleB {
		return eligibleA < eligibleB
	}
	if topologyA, topologyB := topologySpreadConstraintSortValue(a), topologySpreadConstraintSortValue(b); topologyA != topologyB {
		return topologyA < topologyB
	}
	return requiredAntiAffinitySortValue(a) < requiredAntiAffinitySortValue(b)
}

func topologySpreadConstraintSortValue(pod NodeOptimizationPodInput) string {
	keys := make([]string, 0, len(pod.topologySpreadConstraints))
	for _, constraint := range pod.topologySpreadConstraints {
		keys = append(keys, constraint.SortKey)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x00")
}

func requiredAntiAffinitySortValue(pod NodeOptimizationPodInput) string {
	keys := make([]string, 0, len(pod.requiredAntiAffinityTerms))
	for _, term := range pod.requiredAntiAffinityTerms {
		keys = append(keys, term.SortKey)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x00")
}

func requiredPodAntiAffinityAllowsPlacement(
	pod NodeOptimizationPodInput,
	candidateNodeName string,
	placements []NodeOptimizationPlacement,
	removedNodeName string,
) (bool, string, string) {
	state := pod.antiAffinityState
	if state == nil {
		if len(pod.requiredAntiAffinityTerms) > 0 {
			return false, "", fmt.Sprintf("pod %s has required anti-affinity without compiled scheduling state", podDisplayName(pod))
		}
		return true, "", ""
	}

	retainedNodes := make([]NodeOptimizationSchedulingNode, 0, len(state.Nodes))
	nodesByName := make(map[string]NodeOptimizationSchedulingNode, len(state.Nodes))
	for _, node := range state.Nodes {
		if node.Name == removedNodeName {
			continue
		}
		retainedNodes = append(retainedNodes, node)
		nodesByName[node.Name] = node
	}
	if _, ok := nodesByName[candidateNodeName]; !ok {
		return false, "", fmt.Sprintf("candidate node %s is absent from required anti-affinity topology evidence", candidateNodeName)
	}

	incomingKey := namespacedKey(pod.Namespace, pod.Name)
	incoming, ok := state.MovablePods[incomingKey]
	if !ok {
		return false, "", fmt.Sprintf("pod %s is absent from required anti-affinity movable-Pod evidence", podDisplayName(pod))
	}
	incoming.NodeName = candidateNodeName

	residents := make([]nodeOptimizationTopologyPod, 0, len(state.FixedPods)+len(placements))
	for _, fixed := range state.FixedPods {
		if fixed.NodeName != removedNodeName {
			residents = append(residents, fixed)
		}
	}
	for _, placement := range placements {
		key := namespacedKey(placement.Namespace, placement.PodName)
		placedPod, found := state.MovablePods[key]
		if !found {
			return false, "", fmt.Sprintf(
				"placed pod %s/%s is absent from required anti-affinity movable-Pod evidence",
				placement.Namespace,
				placement.PodName,
			)
		}
		placedPod.NodeName = placement.NodeName
		residents = append(residents, placedPod)
	}
	sort.Slice(residents, func(i, j int) bool {
		return nodeOptimizationTopologyPodSortKey(residents[i]) < nodeOptimizationTopologyPodSortKey(residents[j])
	})

	for _, term := range pod.requiredAntiAffinityTerms {
		if reason := topologyEvidenceRequirementReason(retainedNodes, term.TopologyRequirement); reason != "" {
			return false, "", fmt.Sprintf(
				"required pod anti-affinity for pod %s cannot use %s: %s",
				podDisplayName(pod),
				term.TopologyKey,
				reason,
			)
		}
	}
	for _, resident := range residents {
		for _, term := range resident.RequiredAntiAffinityTerms {
			if !requiredAntiAffinityTermMatchesPod(term, incoming) {
				continue
			}
			if reason := topologyEvidenceRequirementReason(retainedNodes, term.TopologyRequirement); reason != "" {
				return false, "", fmt.Sprintf(
					"required pod anti-affinity on resident pod %s/%s cannot use %s: %s",
					resident.Namespace,
					resident.Name,
					term.TopologyKey,
					reason,
				)
			}
		}
	}

	for _, term := range pod.requiredAntiAffinityTerms {
		candidateDomain := topologyValueForKey(nodesByName[candidateNodeName].Topology, term.TopologyKey)
		for _, resident := range residents {
			if !requiredAntiAffinityTermMatchesPod(term, resident) {
				continue
			}
			residentNode, found := nodesByName[resident.NodeName]
			if !found {
				return false, "", fmt.Sprintf(
					"matching resident pod %s/%s references node %s outside required anti-affinity evidence",
					resident.Namespace,
					resident.Name,
					resident.NodeName,
				)
			}
			if topologyValueForKey(residentNode.Topology, term.TopologyKey) == candidateDomain {
				return false, fmt.Sprintf(
					"pod %s required anti-affinity conflicts with resident pod %s/%s in %s=%q",
					podDisplayName(pod),
					resident.Namespace,
					resident.Name,
					term.TopologyKey,
					candidateDomain,
				), ""
			}
		}
	}

	for _, resident := range residents {
		residentNode, found := nodesByName[resident.NodeName]
		if !found {
			return false, "", fmt.Sprintf(
				"resident pod %s/%s references node %s outside required anti-affinity evidence",
				resident.Namespace,
				resident.Name,
				resident.NodeName,
			)
		}
		for _, term := range resident.RequiredAntiAffinityTerms {
			if !requiredAntiAffinityTermMatchesPod(term, incoming) {
				continue
			}
			candidateDomain := topologyValueForKey(nodesByName[candidateNodeName].Topology, term.TopologyKey)
			if topologyValueForKey(residentNode.Topology, term.TopologyKey) == candidateDomain {
				return false, fmt.Sprintf(
					"resident pod %s/%s required anti-affinity rejects pod %s in %s=%q",
					resident.Namespace,
					resident.Name,
					podDisplayName(pod),
					term.TopologyKey,
					candidateDomain,
				), ""
			}
		}
	}

	return true, "", ""
}

func topologySpreadAllowsPlacement(
	pod NodeOptimizationPodInput,
	candidateNodeName string,
	placements []NodeOptimizationPlacement,
	removedNodeName string,
) (bool, string, string) {
	for _, constraint := range pod.topologySpreadConstraints {
		if constraint.MaxSkew <= 0 || constraint.MinDomains <= 0 {
			return false, "", fmt.Sprintf("pod %s has invalid normalized topology spread values", podDisplayName(pod))
		}

		retainedDomainNodes := make([]NodeOptimizationSchedulingNode, 0, len(constraint.DomainNodes))
		for _, node := range constraint.DomainNodes {
			if node.Name != removedNodeName {
				retainedDomainNodes = append(retainedDomainNodes, node)
			}
		}
		if reason := topologyEvidenceRequirementReason(retainedDomainNodes, constraint.TopologyRequirement); reason != "" {
			return false, "", fmt.Sprintf(
				"hard topology spread for pod %s cannot use %s: %s",
				podDisplayName(pod),
				constraint.TopologyKey,
				reason,
			)
		}

		nodeDomains := make(map[string]string, len(retainedDomainNodes))
		domainCounts := make(map[string]int64)
		for _, node := range retainedDomainNodes {
			domain := topologyValueForKey(node.Topology, constraint.TopologyKey)
			nodeDomains[node.Name] = domain
			domainCounts[domain] = 0
		}

		candidateDomain, ok := nodeDomains[candidateNodeName]
		if !ok || candidateNodeName == removedNodeName {
			return false, fmt.Sprintf(
				"candidate node %s has no eligible %s topology domain for pod %s",
				candidateNodeName,
				constraint.TopologyKey,
				podDisplayName(pod),
			), ""
		}

		for _, nodeName := range constraint.ExistingMatchingPodNodes {
			if nodeName == removedNodeName {
				continue
			}
			domain, domainKnown := nodeDomains[nodeName]
			if !domainKnown {
				continue
			}
			next, countOK := checkedAddInt64(domainCounts[domain], 1)
			if !countOK {
				return false, "", fmt.Sprintf("topology domain count overflows int64 for %s=%s", constraint.TopologyKey, domain)
			}
			domainCounts[domain] = next
		}

		for _, placement := range placements {
			if _, matches := constraint.MatchingMovablePodKeys[namespacedKey(placement.Namespace, placement.PodName)]; !matches {
				continue
			}
			domain, domainKnown := nodeDomains[placement.NodeName]
			if !domainKnown {
				continue
			}
			next, countOK := checkedAddInt64(domainCounts[domain], 1)
			if !countOK {
				return false, "", fmt.Sprintf("topology domain count overflows int64 for %s=%s", constraint.TopologyKey, domain)
			}
			domainCounts[domain] = next
		}

		domains := make([]string, 0, len(domainCounts))
		for domain := range domainCounts {
			domains = append(domains, domain)
		}
		sort.Strings(domains)
		globalMinimum := domainCounts[domains[0]]
		for _, domain := range domains[1:] {
			if domainCounts[domain] < globalMinimum {
				globalMinimum = domainCounts[domain]
			}
		}
		if int64(len(domains)) < int64(constraint.MinDomains) {
			globalMinimum = 0
		}

		candidateCount := domainCounts[candidateDomain]
		if _, selfMatches := constraint.MatchingMovablePodKeys[namespacedKey(pod.Namespace, pod.Name)]; selfMatches {
			var countOK bool
			candidateCount, countOK = checkedAddInt64(candidateCount, 1)
			if !countOK {
				return false, "", fmt.Sprintf("topology candidate-domain count overflows int64 for %s=%s", constraint.TopologyKey, candidateDomain)
			}
		}
		skew, skewOK := checkedSubInt64(candidateCount, globalMinimum)
		if !skewOK {
			return false, "", fmt.Sprintf("topology skew cannot be represented for %s=%s", constraint.TopologyKey, candidateDomain)
		}
		if skew > int64(constraint.MaxSkew) {
			return false, fmt.Sprintf(
				"hard topology spread %s=%q would have skew %d greater than maxSkew %d for pod %s",
				constraint.TopologyKey,
				candidateDomain,
				skew,
				constraint.MaxSkew,
				podDisplayName(pod),
			), ""
		}
	}

	return true, "", ""
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
			removedNodeName:         removedNodeName,
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
