package analyzer

import (
	"fmt"
	"math"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// NodeOptimizationRecommendationStatus describes a read-only recommendation
// stage. No status in this taxonomy authorizes execution.
type NodeOptimizationRecommendationStatus string

const (
	// NodeOptimizationRecommendationObservation records measured snapshot facts
	// before a removable-node candidate exists.
	NodeOptimizationRecommendationObservation NodeOptimizationRecommendationStatus = "OBSERVATION"
	// NodeOptimizationRecommendationCandidate identifies a deterministic N-1
	// candidate that has not yet passed all modeled pre-checks.
	NodeOptimizationRecommendationCandidate NodeOptimizationRecommendationStatus = "CANDIDATE"
	// NodeOptimizationRecommendationPreCheckPassed means supported evidence and
	// input checks passed, but a complete placement proof is not yet present.
	NodeOptimizationRecommendationPreCheckPassed NodeOptimizationRecommendationStatus = "PRE_CHECK_PASSED"
	// NodeOptimizationRecommendationSimulationPassed means scheduling feasibility
	// was proved under the supported model. It does not authorize execution.
	NodeOptimizationRecommendationSimulationPassed NodeOptimizationRecommendationStatus = "SIMULATION_PASSED"
	// NodeOptimizationRecommendationBlocked means sufficient supported evidence
	// proved that a hard modeled constraint is not satisfied.
	NodeOptimizationRecommendationBlocked NodeOptimizationRecommendationStatus = "BLOCKED"
	// NodeOptimizationRecommendationPartial means incomplete or unsupported
	// evidence prevented either a positive proof or a supported hard rejection.
	NodeOptimizationRecommendationPartial NodeOptimizationRecommendationStatus = "PARTIAL"
)

// NodeOptimizationVerifiedCheck is a stable identifier for a check the
// snapshot builder actually evaluated for a scenario.
type NodeOptimizationVerifiedCheck string

const (
	NodeOptimizationCheckCPUCapacity             NodeOptimizationVerifiedCheck = "cpu_capacity"
	NodeOptimizationCheckMemoryCapacity          NodeOptimizationVerifiedCheck = "memory_capacity"
	NodeOptimizationCheckPodResourceRequests     NodeOptimizationVerifiedCheck = "pod_resource_requests"
	NodeOptimizationCheckNodeSelector            NodeOptimizationVerifiedCheck = "node_selector"
	NodeOptimizationCheckRequiredNodeAffinity    NodeOptimizationVerifiedCheck = "required_node_affinity"
	NodeOptimizationCheckTaintsTolerations       NodeOptimizationVerifiedCheck = "taints_tolerations"
	NodeOptimizationCheckCordonState             NodeOptimizationVerifiedCheck = "cordon_state"
	NodeOptimizationCheckDaemonSetOverhead       NodeOptimizationVerifiedCheck = "daemonset_overhead"
	NodeOptimizationCheckTopologySpread          NodeOptimizationVerifiedCheck = "hard_topology_spread"
	NodeOptimizationCheckRequiredPodAffinity     NodeOptimizationVerifiedCheck = "required_pod_affinity"
	NodeOptimizationCheckRequiredPodAntiAffinity NodeOptimizationVerifiedCheck = "required_pod_anti_affinity"
	NodeOptimizationCheckPersistentVolume        NodeOptimizationVerifiedCheck = "pvc_pv_storage_topology"
)

// NodeOptimizationRecommendationAssignment is auditable placement evidence for
// one movable Pod. SourceNode is snapshot evidence; DestinationNode is the
// simulator's concrete retained-node assignment.
type NodeOptimizationRecommendationAssignment struct {
	Namespace       string
	PodName         string
	SourceNode      string
	DestinationNode string
}

// NodeOptimizationRecommendationReason is a concise product-facing reason
// while retaining the simulator's raw reason identifier and diagnostic text.
type NodeOptimizationRecommendationReason struct {
	Code      string
	Pod       string
	Node      string
	Message   string
	RawReason string
}

// NodeOptimizationRecommendationEvidence summarizes either pool-scoped or
// snapshot-wide evidence. Recommendation.Evidence is status-bearing for its
// pool; SnapshotEvidence is audit context and does not independently downgrade
// an unrelated pool.
type NodeOptimizationRecommendationEvidence struct {
	Complete                      bool
	EvidenceIncomplete            bool
	UnscopedEvidenceIncomplete    bool
	SkippedPoolCount              int
	ExcludedPodCount              int
	UnresolvedPodCount            int
	UnresolvedNodeCount           int
	DuplicatePodCount             int
	DuplicateNodeCount            int
	UnsupportedConstraintPodCount int
	SchedulingBlockedPodCount     int
	Warnings                      []string
}

// NodeOptimizationRecommendation is a read-only explanation of one evaluated
// scenario, or an aggregate observation/partial result when no scenario exists.
// SIMULATION_PASSED means scheduling feasibility was proved under the supported
// model. It never means that immediate drain, eviction, or deletion is safe.
type NodeOptimizationRecommendation struct {
	Status  NodeOptimizationRecommendationStatus
	Summary string

	PoolKey              CostPoolKey
	CandidateRemovedNode string
	CurrentNodeCount     int
	CandidateNodeCount   int

	PodsConsidered int
	PodsAssigned   int

	RetainedCPUCapacityMilli      int64
	RetainedMemoryCapacityBytes   int64
	AggregateCPURequestedMilli    int64
	AggregateMemoryRequestedBytes int64
	CPUHeadroomMilli              int64
	MemoryHeadroomBytes           int64
	CPUHeadroomPercent            *float64
	MemoryHeadroomPercent         *float64

	Assignments    []NodeOptimizationRecommendationAssignment
	VerifiedChecks []NodeOptimizationVerifiedCheck
	Blockers       []NodeOptimizationRecommendationReason
	Caveats        []string
	NotEvaluated   []string
	// Evidence contains only evidence relevant to this recommendation's pool.
	Evidence NodeOptimizationRecommendationEvidence
	// SnapshotEvidence is scan-wide health context. Only Evidence and genuinely
	// unscoped corruption participate in this pool recommendation's status.
	SnapshotEvidence NodeOptimizationRecommendationEvidence
}

var nodeOptimizationNotEvaluated = []string{
	"application-level readiness",
	"cloud node provisioning or termination",
	"CSI volume attach or detach execution",
	"drain or eviction execution",
	"PodDisruptionBudget and disruption timing",
}

// BuildNodeOptimizationRecommendations converts simulator output into a stable
// read-only product contract. It performs no placement, Kubernetes, cloud, or
// pricing work.
func BuildNodeOptimizationRecommendations(
	summary NodeOptimizationSnapshotSummary,
	pods []corev1.Pod,
) []NodeOptimizationRecommendation {
	podSources, podSourceCounts := nodeOptimizationPodSourceIndex(pods)
	snapshotEvidence := nodeOptimizationRecommendationEvidence(summary)
	poolEvidence := nodeOptimizationPoolRecommendationEvidence(summary.PoolEvidence)
	recommendations := make([]NodeOptimizationRecommendation, 0, len(summary.Scenarios)+len(poolEvidence))
	evaluatedPools := make(map[CostPoolKey]struct{}, len(summary.Scenarios))
	for _, scenario := range summary.Scenarios {
		evidence, found := poolEvidence[scenario.PoolKey]
		if !found {
			evidence = completeNodeOptimizationPoolRecommendationEvidence()
		}
		recommendation := recommendationFromScenario(
			scenario,
			podSources,
			podSourceCounts,
			evidence,
			snapshotEvidence,
			summary.UnscopedEvidenceIncomplete || !evidence.Complete,
		)
		sortRecommendationReasons(recommendation.Blockers)
		recommendations = append(recommendations, recommendation)
		evaluatedPools[scenario.PoolKey] = struct{}{}
	}

	for _, scoped := range summary.PoolEvidence {
		if _, evaluated := evaluatedPools[scoped.PoolKey]; evaluated {
			continue
		}
		evidence := poolEvidence[scoped.PoolKey]
		blockers := recommendationEvidenceReasons(evidence.Warnings)
		if len(blockers) == 0 {
			blockers = append(blockers, NodeOptimizationRecommendationReason{
				Code:    "incomplete_or_unsupported_evidence",
				Message: "pool evidence was incomplete or unsupported",
			})
		}
		sortRecommendationReasons(blockers)
		recommendations = append(recommendations, NodeOptimizationRecommendation{
			Status:           NodeOptimizationRecommendationPartial,
			Summary:          "Scheduling feasibility could not be proved or rejected because required pool evidence or semantics were incomplete.",
			PoolKey:          scoped.PoolKey,
			Blockers:         blockers,
			NotEvaluated:     nodeOptimizationNotEvaluatedList(),
			Evidence:         evidence,
			SnapshotEvidence: snapshotEvidence,
		})
		evaluatedPools[scoped.PoolKey] = struct{}{}
	}

	if len(recommendations) == 0 {
		status := NodeOptimizationRecommendationObservation
		summaryText := "No same-shape N-1 candidate was evaluated."
		var blockers []NodeOptimizationRecommendationReason
		if summary.UnscopedEvidenceIncomplete || !snapshotEvidence.Complete {
			status = NodeOptimizationRecommendationPartial
			summaryText = "Scheduling feasibility could not be proved or rejected because required evidence or semantics were incomplete."
			blockers = recommendationEvidenceReasons(snapshotEvidence.Warnings)
		}
		recommendations = append(recommendations, NodeOptimizationRecommendation{
			Status:           status,
			Summary:          summaryText,
			Blockers:         blockers,
			NotEvaluated:     nodeOptimizationNotEvaluatedList(),
			Evidence:         snapshotEvidence,
			SnapshotEvidence: snapshotEvidence,
		})
	}

	sort.Slice(recommendations, func(i, j int) bool {
		if poolI, poolJ := costPoolKeySortValue(recommendations[i].PoolKey), costPoolKeySortValue(recommendations[j].PoolKey); poolI != poolJ {
			return poolI < poolJ
		}
		return recommendations[i].CandidateRemovedNode < recommendations[j].CandidateRemovedNode
	})
	return recommendations
}

func recommendationFromScenario(
	scenario NodeOptimizationScenario,
	podSources map[string]string,
	podSourceCounts map[string]int,
	evidence NodeOptimizationRecommendationEvidence,
	snapshotEvidence NodeOptimizationRecommendationEvidence,
	partialEvidence bool,
) NodeOptimizationRecommendation {
	simulation := scenario.Simulation
	recommendation := NodeOptimizationRecommendation{
		PoolKey:                       scenario.PoolKey,
		CandidateRemovedNode:          scenario.RemovedNodeName,
		CurrentNodeCount:              simulation.CurrentNodes,
		CandidateNodeCount:            simulation.CandidateNodes,
		PodsConsidered:                simulation.TotalPodCount,
		RetainedCPUCapacityMilli:      simulation.CandidateCPUCapacityMilli,
		RetainedMemoryCapacityBytes:   simulation.CandidateMemoryCapacityBytes,
		AggregateCPURequestedMilli:    simulation.TotalCPURequestMilli,
		AggregateMemoryRequestedBytes: simulation.TotalMemoryRequestBytes,
		CPUHeadroomMilli:              simulation.CPUHeadroomMilli,
		MemoryHeadroomBytes:           simulation.MemoryHeadroomBytes,
		VerifiedChecks:                normalizeVerifiedChecks(scenario.VerifiedChecks),
		Caveats:                       recommendationCaveats(scenario),
		NotEvaluated:                  nodeOptimizationNotEvaluatedList(),
		Evidence:                      evidence,
		SnapshotEvidence:              snapshotEvidence,
		Blockers:                      normalizeSimulationBlockers(simulation.Blockers, scenario.RemovedNodeName),
	}

	if partialEvidence || simulation.Status == NodeOptimizationInvalidInput {
		recommendation.Status = NodeOptimizationRecommendationPartial
		recommendation.Summary = "Scheduling feasibility could not be proved or rejected because required evidence or semantics were incomplete."
		recommendation.Blockers = append(recommendation.Blockers, recommendationEvidenceReasons(evidence.Warnings)...)
		return recommendation
	}

	if simulation.PlacementFound {
		if simulation.Status != NodeOptimizationFit || len(simulation.Blockers) != 0 {
			recommendation.Status = NodeOptimizationRecommendationPartial
			recommendation.Summary = "Scheduling feasibility could not be proved because the simulation result was internally inconsistent."
			recommendation.Assignments = nil
			recommendation.PodsAssigned = 0
			recommendation.Blockers = append(recommendation.Blockers, NodeOptimizationRecommendationReason{
				Code: "simulation_evidence_inconsistent", Node: scenario.RemovedNodeName,
				Message: "placement was reported with a non-fit status or one or more blockers",
			})
			return recommendation
		}
		assignments, reason := completeRecommendationAssignments(scenario, podSources, podSourceCounts)
		if reason != "" {
			recommendation.Status = NodeOptimizationRecommendationPartial
			recommendation.Summary = "Scheduling feasibility could not be proved because assignment evidence was incomplete."
			recommendation.Blockers = append(recommendation.Blockers, NodeOptimizationRecommendationReason{
				Code: "assignment_evidence_incomplete", Node: scenario.RemovedNodeName, Message: reason,
			})
			return recommendation
		}

		cpuRemaining, cpuOK := checkedSubInt64(simulation.CandidateCPUCapacityMilli, simulation.TotalCPURequestMilli)
		memoryRemaining, memoryOK := checkedSubInt64(simulation.CandidateMemoryCapacityBytes, simulation.TotalMemoryRequestBytes)
		cpuPercent, cpuPercentOK := checkedHeadroomPercent(cpuRemaining, simulation.CandidateCPUCapacityMilli)
		memoryPercent, memoryPercentOK := checkedHeadroomPercent(memoryRemaining, simulation.CandidateMemoryCapacityBytes)
		if !cpuOK || !memoryOK || cpuRemaining < 0 || memoryRemaining < 0 || !cpuPercentOK || !memoryPercentOK ||
			cpuRemaining != simulation.CPUHeadroomMilli || memoryRemaining != simulation.MemoryHeadroomBytes {
			recommendation.Status = NodeOptimizationRecommendationPartial
			recommendation.Summary = "Scheduling feasibility could not be proved because headroom evidence was inconsistent."
			recommendation.Blockers = append(recommendation.Blockers, NodeOptimizationRecommendationReason{
				Code: "headroom_evidence_incomplete", Node: scenario.RemovedNodeName,
				Message: "retained capacity, requested resources, and reported headroom do not reconcile",
			})
			return recommendation
		}

		recommendation.Status = NodeOptimizationRecommendationSimulationPassed
		recommendation.Summary = "Scheduling feasibility proved under the supported model."
		recommendation.Assignments = assignments
		recommendation.PodsAssigned = len(assignments)
		recommendation.CPUHeadroomPercent = &cpuPercent
		recommendation.MemoryHeadroomPercent = &memoryPercent
		return recommendation
	}

	if simulation.Status == NodeOptimizationPlacementNotFound && onlyHeuristicPlacementFailure(simulation.Blockers) {
		recommendation.Status = NodeOptimizationRecommendationPartial
		recommendation.Summary = "The deterministic placement heuristic could not prove or reject this candidate."
		return recommendation
	}

	recommendation.Status = NodeOptimizationRecommendationBlocked
	recommendation.Summary = "The candidate does not satisfy a supported hard constraint under the modeled checks."
	return recommendation
}

func completeRecommendationAssignments(
	scenario NodeOptimizationScenario,
	podSources map[string]string,
	podSourceCounts map[string]int,
) ([]NodeOptimizationRecommendationAssignment, string) {
	simulation := scenario.Simulation
	if simulation.CurrentNodes <= 0 || simulation.CandidateNodes <= 0 ||
		simulation.CandidateNodes >= simulation.CurrentNodes ||
		simulation.CurrentNodes-simulation.CandidateNodes != 1 {
		return nil, fmt.Sprintf(
			"node counts %d current and %d candidate do not describe an N-1 scenario",
			simulation.CurrentNodes,
			simulation.CandidateNodes,
		)
	}
	candidateNodeNames, candidateReason := normalizeUniqueNodeNames(scenario.CandidateNodeNames, false)
	if candidateReason != "" || len(candidateNodeNames) != simulation.CandidateNodes {
		return nil, fmt.Sprintf(
			"candidate node identity evidence does not reconcile with %d candidate nodes: %s",
			simulation.CandidateNodes,
			candidateReason,
		)
	}
	if scenario.RemovedNodeName == "" {
		return nil, "removed node identity is empty"
	}
	if simulation.TotalPodCount != scenario.EligiblePodCount ||
		len(scenario.movablePodKeys) != scenario.EligiblePodCount ||
		simulation.PlacedPodCount != simulation.TotalPodCount ||
		len(simulation.Placements) != simulation.TotalPodCount {
		return nil, fmt.Sprintf(
			"assignment count %d does not reconcile with %d placed, %d considered, and %d movable Pods",
			len(simulation.Placements),
			simulation.PlacedPodCount,
			simulation.TotalPodCount,
			len(scenario.movablePodKeys),
		)
	}

	candidateNodes := make(map[string]struct{}, len(candidateNodeNames))
	for _, node := range candidateNodeNames {
		if node == scenario.RemovedNodeName {
			return nil, fmt.Sprintf("removed node %q remains in candidate node evidence", scenario.RemovedNodeName)
		}
		candidateNodes[node] = struct{}{}
	}
	expectedPods := make(map[string]struct{}, len(scenario.movablePodKeys))
	for _, key := range scenario.movablePodKeys {
		if _, duplicate := expectedPods[key]; duplicate {
			return nil, fmt.Sprintf("movable Pod identity %q is duplicated", displayNamespacedKey(key))
		}
		expectedPods[key] = struct{}{}
	}
	seenPods := make(map[string]struct{}, len(simulation.Placements))
	assignments := make([]NodeOptimizationRecommendationAssignment, 0, len(simulation.Placements))
	for _, placement := range simulation.Placements {
		key := namespacedKey(placement.Namespace, placement.PodName)
		if _, duplicate := seenPods[key]; duplicate {
			return nil, fmt.Sprintf("Pod %s/%s has duplicate destination assignments", placement.Namespace, placement.PodName)
		}
		if _, expected := expectedPods[key]; !expected {
			return nil, fmt.Sprintf("Pod %s/%s is not part of the movable Pod set", placement.Namespace, placement.PodName)
		}
		seenPods[key] = struct{}{}
		delete(expectedPods, key)
		if _, valid := candidateNodes[placement.NodeName]; !valid || placement.NodeName == scenario.RemovedNodeName {
			return nil, fmt.Sprintf("Pod %s/%s has invalid destination node %q", placement.Namespace, placement.PodName, placement.NodeName)
		}
		sourceNode, found := podSources[key]
		if !found || podSourceCounts[key] != 1 {
			return nil, fmt.Sprintf(
				"Pod %s/%s appears %d times in assignment source evidence; exactly one source is required",
				placement.Namespace,
				placement.PodName,
				podSourceCounts[key],
			)
		}
		assignments = append(assignments, NodeOptimizationRecommendationAssignment{
			Namespace:       placement.Namespace,
			PodName:         placement.PodName,
			SourceNode:      sourceNode,
			DestinationNode: placement.NodeName,
		})
	}
	if len(expectedPods) != 0 {
		missing := sortedStringSetKeys(expectedPods)
		return nil, fmt.Sprintf("movable Pod %q has no destination assignment", displayNamespacedKey(missing[0]))
	}
	sort.Slice(assignments, func(i, j int) bool {
		if assignments[i].Namespace != assignments[j].Namespace {
			return assignments[i].Namespace < assignments[j].Namespace
		}
		if assignments[i].PodName != assignments[j].PodName {
			return assignments[i].PodName < assignments[j].PodName
		}
		if assignments[i].SourceNode != assignments[j].SourceNode {
			return assignments[i].SourceNode < assignments[j].SourceNode
		}
		return assignments[i].DestinationNode < assignments[j].DestinationNode
	})
	return assignments, ""
}

func nodeOptimizationMovablePodKeys(pods []corev1.Pod) []string {
	keys := make([]string, 0, len(pods))
	for _, pod := range pods {
		keys = append(keys, namespacedKey(pod.Namespace, pod.Name))
	}
	sort.Strings(keys)
	return keys
}

func nodeOptimizationPodSourceIndex(pods []corev1.Pod) (map[string]string, map[string]int) {
	counts := make(map[string]int, len(pods))
	sources := make(map[string]string, len(pods))
	for _, pod := range pods {
		key := namespacedKey(pod.Namespace, pod.Name)
		counts[key]++
		sources[key] = pod.Spec.NodeName
	}
	return sources, counts
}

func completeNodeOptimizationPoolRecommendationEvidence() NodeOptimizationRecommendationEvidence {
	return NodeOptimizationRecommendationEvidence{Complete: true}
}

func nodeOptimizationPoolRecommendationEvidence(
	poolEvidence []NodeOptimizationPoolEvidence,
) map[CostPoolKey]NodeOptimizationRecommendationEvidence {
	result := make(map[CostPoolKey]NodeOptimizationRecommendationEvidence, len(poolEvidence))
	for _, scoped := range poolEvidence {
		evidence := result[scoped.PoolKey]
		evidence.EvidenceIncomplete = evidence.EvidenceIncomplete || scoped.EvidenceIncomplete
		if scoped.Skipped {
			evidence.SkippedPoolCount = 1
		}
		evidence.UnresolvedPodCount += scoped.UnresolvedPodCount
		evidence.UnresolvedNodeCount += scoped.UnresolvedNodeCount
		evidence.DuplicatePodCount += scoped.DuplicatePodCount
		evidence.DuplicateNodeCount += scoped.DuplicateNodeCount
		evidence.UnsupportedConstraintPodCount += scoped.UnsupportedConstraintPodCount
		evidence.Warnings = append(evidence.Warnings, scoped.Warnings...)
		result[scoped.PoolKey] = evidence
	}
	for key, evidence := range result {
		sort.Strings(evidence.Warnings)
		evidence.Complete = !evidence.EvidenceIncomplete &&
			evidence.SkippedPoolCount == 0 &&
			evidence.UnresolvedPodCount == 0 &&
			evidence.UnresolvedNodeCount == 0 &&
			evidence.DuplicatePodCount == 0 &&
			evidence.DuplicateNodeCount == 0 &&
			evidence.UnsupportedConstraintPodCount == 0
		result[key] = evidence
	}
	return result
}

func nodeOptimizationRecommendationEvidence(summary NodeOptimizationSnapshotSummary) NodeOptimizationRecommendationEvidence {
	evidence := NodeOptimizationRecommendationEvidence{
		EvidenceIncomplete:            summary.EvidenceIncomplete,
		UnscopedEvidenceIncomplete:    summary.UnscopedEvidenceIncomplete,
		SkippedPoolCount:              summary.SkippedPoolCount,
		ExcludedPodCount:              summary.ExcludedPodCount,
		UnresolvedPodCount:            summary.UnresolvedPodCount,
		UnresolvedNodeCount:           summary.UnresolvedNodeCount,
		DuplicatePodCount:             summary.DuplicatePodCount,
		DuplicateNodeCount:            summary.DuplicateNodeCount,
		UnsupportedConstraintPodCount: summary.UnsupportedConstraintPodCount,
		SchedulingBlockedPodCount:     summary.SchedulingBlockedPodCount,
		Warnings:                      append([]string(nil), summary.Warnings...),
	}
	sort.Strings(evidence.Warnings)
	evidence.Complete = !summary.EvidenceIncomplete &&
		summary.UnresolvedPodCount == 0 &&
		summary.UnresolvedNodeCount == 0 &&
		summary.DuplicatePodCount == 0 &&
		summary.DuplicateNodeCount == 0 &&
		summary.UnsupportedConstraintPodCount == 0 &&
		summary.SkippedPoolCount == 0
	return evidence
}

func checkedHeadroomPercent(headroom, capacity int64) (float64, bool) {
	if headroom < 0 || capacity <= 0 || headroom > capacity {
		return 0, false
	}
	percent := (float64(headroom) / float64(capacity)) * 100
	if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
		return 0, false
	}
	return percent, true
}

func normalizeVerifiedChecks(checks []NodeOptimizationVerifiedCheck) []NodeOptimizationVerifiedCheck {
	set := make(map[NodeOptimizationVerifiedCheck]struct{}, len(checks))
	for _, check := range checks {
		if check != "" {
			set[check] = struct{}{}
		}
	}
	normalized := make([]NodeOptimizationVerifiedCheck, 0, len(set))
	for check := range set {
		normalized = append(normalized, check)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i] < normalized[j] })
	return normalized
}

func nodeOptimizationScenarioVerifiedChecks(
	pods []corev1.Pod,
	schedulingEvidenceAvailable bool,
	storageEvidenceAvailable bool,
	daemonSetCount int,
) []NodeOptimizationVerifiedCheck {
	checks := []NodeOptimizationVerifiedCheck{
		NodeOptimizationCheckCPUCapacity,
		NodeOptimizationCheckMemoryCapacity,
		NodeOptimizationCheckPodResourceRequests,
	}
	if daemonSetCount > 0 {
		checks = append(checks, NodeOptimizationCheckDaemonSetOverhead)
	}
	if schedulingEvidenceAvailable {
		checks = append(checks, NodeOptimizationCheckTaintsTolerations, NodeOptimizationCheckCordonState)
	}
	for _, pod := range pods {
		if len(pod.Spec.NodeSelector) > 0 {
			checks = append(checks, NodeOptimizationCheckNodeSelector)
		}
		if hasRequiredNodeAffinity(pod) {
			checks = append(checks, NodeOptimizationCheckRequiredNodeAffinity)
		}
		if hasHardTopologySpreadConstraints(pod) {
			checks = append(checks, NodeOptimizationCheckTopologySpread)
		}
		if hasRequiredPodAffinity(pod) {
			checks = append(checks, NodeOptimizationCheckRequiredPodAffinity)
		}
		if hasRequiredPodAntiAffinity(pod) {
			checks = append(checks, NodeOptimizationCheckRequiredPodAntiAffinity)
		}
		if storageEvidenceAvailable && podReferencesPersistentVolumeClaim(pod) {
			checks = append(checks, NodeOptimizationCheckPersistentVolume)
		}
	}
	return normalizeVerifiedChecks(checks)
}

func recommendationCaveats(scenario NodeOptimizationScenario) []string {
	caveats := append([]string(nil), scenario.SchedulingCaveats...)
	caveats = append(caveats, scenario.Simulation.Caveats...)
	caveats = append(caveats, "read-only simulation; execution safety was not evaluated")
	sort.Strings(caveats)
	return compactSortedStrings(caveats)
}

func normalizeSimulationBlockers(
	blockers []NodeOptimizationBlocker,
	removedNode string,
) []NodeOptimizationRecommendationReason {
	reasons := make([]NodeOptimizationRecommendationReason, 0, len(blockers))
	for _, blocker := range blockers {
		reasons = append(reasons, NodeOptimizationRecommendationReason{
			Code:      recommendationReasonCode(blocker.Reason),
			Pod:       blocker.Pod,
			Node:      removedNode,
			Message:   blocker.Message,
			RawReason: blocker.Reason,
		})
	}
	sortRecommendationReasons(reasons)
	return reasons
}

func recommendationReasonCode(raw string) string {
	switch raw {
	case "aggregate_cpu_capacity":
		return "insufficient_cpu"
	case "aggregate_memory_capacity":
		return "insufficient_memory"
	case "pod_exceeds_single_node":
		return "pod_exceeds_node_capacity"
	case "no_eligible_candidate_nodes":
		return "no_eligible_destination"
	case "topology_spread_constraint":
		return "topology_spread_conflict"
	case "required_pod_affinity":
		return "required_pod_affinity_unsatisfied"
	case "required_pod_anti_affinity":
		return "required_pod_anti_affinity_conflict"
	case "heuristic_placement_failed":
		return "simulation_inconclusive"
	default:
		if raw == "" {
			return "simulation_blocked"
		}
		return raw
	}
}

func recommendationEvidenceReasons(warnings []string) []NodeOptimizationRecommendationReason {
	ordered := append([]string(nil), warnings...)
	sort.Strings(ordered)
	reasons := make([]NodeOptimizationRecommendationReason, 0, len(ordered))
	for _, warning := range ordered {
		reasons = append(reasons, NodeOptimizationRecommendationReason{
			Code: recommendationEvidenceReasonCode(warning), Message: warning,
		})
	}
	return reasons
}

func recommendationEvidenceReasonCode(warning string) string {
	normalized := strings.ToLower(warning)
	switch {
	case strings.Contains(normalized, "persistentvolumeclaim"),
		strings.Contains(normalized, "persistentvolume"),
		strings.Contains(normalized, "pvc"),
		strings.Contains(normalized, "csi"),
		strings.Contains(normalized, "storage"):
		return "pvc_storage_mobility_unproven"
	case strings.Contains(normalized, "unsupported"), strings.Contains(normalized, "not modeled"):
		return "unsupported_hard_scheduling_constraint"
	case strings.Contains(normalized, "missing"),
		strings.Contains(normalized, "incomplete"),
		strings.Contains(normalized, "duplicate"),
		strings.Contains(normalized, "inconsistent"),
		strings.Contains(normalized, "unresolved"):
		return "incomplete_scheduling_evidence"
	default:
		return "incomplete_or_unsupported_evidence"
	}
}

func onlyHeuristicPlacementFailure(blockers []NodeOptimizationBlocker) bool {
	if len(blockers) == 0 {
		return false
	}
	for _, blocker := range blockers {
		if blocker.Reason != "heuristic_placement_failed" {
			return false
		}
	}
	return true
}

func sortRecommendationReasons(reasons []NodeOptimizationRecommendationReason) {
	sort.Slice(reasons, func(i, j int) bool {
		if reasons[i].Code != reasons[j].Code {
			return reasons[i].Code < reasons[j].Code
		}
		if reasons[i].Pod != reasons[j].Pod {
			return reasons[i].Pod < reasons[j].Pod
		}
		if reasons[i].Node != reasons[j].Node {
			return reasons[i].Node < reasons[j].Node
		}
		if reasons[i].RawReason != reasons[j].RawReason {
			return reasons[i].RawReason < reasons[j].RawReason
		}
		return reasons[i].Message < reasons[j].Message
	})
}

func nodeOptimizationNotEvaluatedList() []string {
	return append([]string(nil), nodeOptimizationNotEvaluated...)
}

func compactSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	compacted := values[:0]
	for _, value := range values {
		if value == "" || (len(compacted) > 0 && compacted[len(compacted)-1] == value) {
			continue
		}
		compacted = append(compacted, value)
	}
	return compacted
}
