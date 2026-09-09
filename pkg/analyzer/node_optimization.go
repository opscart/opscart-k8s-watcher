package analyzer

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// NodeOptimizationStatus describes the outcome of a same-shape placement
// simulation. The simulator is intentionally narrower than the Kubernetes
// scheduler: it models CPU and memory requests only in this first slice.
type NodeOptimizationStatus string

const (
	NodeOptimizationFit                NodeOptimizationStatus = "fit"
	NodeOptimizationBlockedAggregate   NodeOptimizationStatus = "blocked_aggregate"
	NodeOptimizationBlockedPodSize     NodeOptimizationStatus = "blocked_pod_exceeds_node"
	NodeOptimizationBlockedEligibility NodeOptimizationStatus = "blocked_no_eligible_nodes"
	NodeOptimizationPlacementNotFound  NodeOptimizationStatus = "placement_not_found"
	NodeOptimizationInvalidInput       NodeOptimizationStatus = "invalid_input"
)

// NodeOptimizationPodInput is the scheduler-independent Pod demand consumed by
// the pure simulator. It deliberately contains no Kubernetes client or API
// object dependency.
type NodeOptimizationPodInput struct {
	Namespace          string
	Name               string
	CPURequestMilli    int64
	MemoryRequestBytes int64

	// EligibleNodeNames is the explicit set of candidate nodes on which this Pod
	// may be placed. Nil preserves the legacy all-candidate-nodes behavior; a
	// non-nil empty set is a hard scheduling blocker.
	EligibleNodeNames []string

	// topologySpreadConstraints is compiled from already-acquired Pod and Node
	// snapshots. It remains internal so the public simulator contract does not
	// expose Kubernetes selector types.
	topologySpreadConstraints []nodeOptimizationTopologySpreadConstraint

	// Required inter-Pod affinity terms and occupancy are compiled from the same
	// snapshots. The shared state distinguishes fixed residents from movable Pods
	// so placement never counts a movable Pod at both its old and new node.
	requiredAffinityTerms     []nodeOptimizationRequiredPodAffinityTerm
	requiredAntiAffinityTerms []nodeOptimizationRequiredPodAffinityTerm
	interPodAffinityState     *nodeOptimizationInterPodAffinityState
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

	// CandidateNodeNames gives the otherwise same-shape bins stable identities.
	// When present, it must contain exactly CandidateNodes unique, non-empty names.
	// Nil preserves the legacy anonymous-bin behavior for direct simulator callers.
	CandidateNodeNames []string

	// removedNodeName identifies the node absent from an internally constructed
	// N-1 attempt. Topology domain and fixed-Pod counts must exclude it.
	removedNodeName string

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
	Placements     []NodeOptimizationPlacement

	Blockers []NodeOptimizationBlocker
	Caveats  []string
}

// NodeOptimizationPlacement records the concrete candidate node selected for a
// Pod by a successful or partially completed deterministic simulation.
type NodeOptimizationPlacement struct {
	Namespace string
	PodName   string
	NodeName  string
}

// NodeOptimizationSnapshotSummary bridges already-acquired Kubernetes snapshots
// into the pure placement simulator. It performs no Kubernetes or cloud API calls.
type NodeOptimizationSnapshotSummary struct {
	Scenarios                     []NodeOptimizationScenario
	PoolEvidence                  []NodeOptimizationPoolEvidence
	ExcludedPodCount              int
	UnresolvedPodCount            int
	UnresolvedNodeCount           int
	DuplicateNodeCount            int
	DuplicatePodCount             int
	UnsupportedConstraintPodCount int
	SchedulingBlockedPodCount     int
	EvidenceIncomplete            bool
	UnscopedEvidenceIncomplete    bool
	SkippedPoolCount              int
	Warnings                      []string
}

// NodeOptimizationPoolEvidence records incomplete or unsupported evidence for
// one canonical pool. SnapshotSummary counters remain the scan-wide health
// view; recommendation status uses this scoped evidence instead.
type NodeOptimizationPoolEvidence struct {
	PoolKey                       CostPoolKey
	EvidenceIncomplete            bool
	Skipped                       bool
	UnresolvedPodCount            int
	UnresolvedNodeCount           int
	DuplicatePodCount             int
	DuplicateNodeCount            int
	UnsupportedConstraintPodCount int
	Warnings                      []string
}

// NodeOptimizationScenario is one N-1 same-shape pool simulation derived from
// an already-observed cluster snapshot.
type NodeOptimizationScenario struct {
	PoolKey                     CostPoolKey
	RemovedNodeName             string
	CandidateNodeNames          []string
	EligiblePodCount            int
	DaemonSetCount              int
	DaemonSetCPUPerNodeMilli    int64
	DaemonSetMemoryPerNodeBytes int64
	SchedulingCaveats           []string
	VerifiedChecks              []NodeOptimizationVerifiedCheck
	// movablePodKeys is the deterministic identity proof used to ensure that a
	// positive recommendation exposes exactly one assignment per movable Pod.
	movablePodKeys []string
	Simulation     NodeOptimizationResult
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
	return buildNMinusOneNodeOptimizationScenarios(nodeInfos, pods, nil, nil)
}

// BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence adds
// scheduler-relevant Node evidence to the N-1 simulation without performing
// Kubernetes or cloud API calls.
func BuildNMinusOneNodeOptimizationScenariosWithSchedulingEvidence(
	nodeInfos []models.NodeInfo,
	pods []corev1.Pod,
	evidence NodeOptimizationSchedulingEvidence,
) NodeOptimizationSnapshotSummary {
	return buildNMinusOneNodeOptimizationScenarios(nodeInfos, pods, &evidence, nil)
}

// BuildNMinusOneNodeOptimizationScenariosWithSchedulingAndStorageEvidence adds
// caller-acquired PVC/PV binding and topology evidence to scheduling-aware N-1
// simulation. It performs no Kubernetes, CSI, or cloud API calls.
func BuildNMinusOneNodeOptimizationScenariosWithSchedulingAndStorageEvidence(
	nodeInfos []models.NodeInfo,
	pods []corev1.Pod,
	schedulingEvidence NodeOptimizationSchedulingEvidence,
	storageEvidence NodeOptimizationStorageEvidence,
) NodeOptimizationSnapshotSummary {
	return buildNMinusOneNodeOptimizationScenarios(nodeInfos, pods, &schedulingEvidence, &storageEvidence)
}

func buildNMinusOneNodeOptimizationScenarios(
	nodeInfos []models.NodeInfo,
	pods []corev1.Pod,
	evidence *NodeOptimizationSchedulingEvidence,
	storageEvidence *NodeOptimizationStorageEvidence,
) (summary NodeOptimizationSnapshotSummary) {
	type poolSnapshot struct {
		key       CostPoolKey
		nodeCount int
		nodeNames []string
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

	poolEvidenceByKey := make(map[CostPoolKey]*NodeOptimizationPoolEvidence)
	poolEvidenceFor := func(key CostPoolKey) *NodeOptimizationPoolEvidence {
		if scoped := poolEvidenceByKey[key]; scoped != nil {
			return scoped
		}
		scoped := &NodeOptimizationPoolEvidence{PoolKey: key}
		poolEvidenceByKey[key] = scoped
		return scoped
	}
	markPoolSkipped := func(key CostPoolKey) *NodeOptimizationPoolEvidence {
		scoped := poolEvidenceFor(key)
		if !scoped.Skipped {
			scoped.Skipped = true
			summary.SkippedPoolCount++
		}
		return scoped
	}
	defer func() {
		sort.Strings(summary.Warnings)
		summary.PoolEvidence = sortedNodeOptimizationPoolEvidence(poolEvidenceByKey)
	}()

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
		warning := fmt.Sprintf(
			"duplicate NodeInfo identity %q appears %d times; node optimization evidence is incomplete",
			name,
			nodeNameCounts[name],
		)
		summary.DuplicateNodeCount++
		summary.EvidenceIncomplete = true
		summary.Warnings = append(summary.Warnings, warning)
		scopedKeys := make(map[CostPoolKey]struct{})
		for _, info := range nodeInfos {
			if info.Name != name {
				continue
			}
			key := costPoolKeyFromNodeInfo(info)
			if len(missingCostPoolKeyFields(key)) == 0 {
				scopedKeys[key] = struct{}{}
			}
		}
		if len(scopedKeys) == 0 {
			summary.UnscopedEvidenceIncomplete = true
		}
		for key := range scopedKeys {
			scoped := markPoolSkipped(key)
			scoped.EvidenceIncomplete = true
			scoped.DuplicateNodeCount++
			scoped.Warnings = append(scoped.Warnings, warning)
		}
	}

	for _, info := range nodeInfos {
		if nodeNameCounts[info.Name] > 1 {
			continue
		}
		knownNodes[info.Name] = struct{}{}

		key := costPoolKeyFromNodeInfo(info)

		if missing := missingCostPoolKeyFields(key); len(missing) > 0 {
			warning := fmt.Sprintf(
				"node %s excluded from node optimization because canonical pool identity is missing: %s",
				info.Name,
				strings.Join(missing, ", "),
			)
			summary.UnresolvedNodeCount++
			summary.EvidenceIncomplete = true
			summary.UnscopedEvidenceIncomplete = true
			summary.Warnings = append(summary.Warnings, warning)
			continue
		}

		cpuMilli, cpuValid := finiteNonNegativeToInt64(info.CPUCapacity * 1000)
		if !cpuValid {
			warning := fmt.Sprintf(
				"node %s excluded from node optimization because observed CPU capacity is non-finite, negative, or outside the int64 representable range",
				info.Name,
			)
			summary.UnresolvedNodeCount++
			summary.EvidenceIncomplete = true
			summary.Warnings = append(summary.Warnings, warning)
			scoped := markPoolSkipped(key)
			scoped.EvidenceIncomplete = true
			scoped.UnresolvedNodeCount++
			scoped.Warnings = append(scoped.Warnings, warning)
			continue
		}
		memBytes, memoryValid := finiteNonNegativeToInt64(info.MemGBCapacity * 1024 * 1024 * 1024)
		if !memoryValid {
			warning := fmt.Sprintf(
				"node %s excluded from node optimization because observed memory capacity is non-finite, negative, or outside the int64 representable range",
				info.Name,
			)
			summary.UnresolvedNodeCount++
			summary.EvidenceIncomplete = true
			summary.Warnings = append(summary.Warnings, warning)
			scoped := markPoolSkipped(key)
			scoped.EvidenceIncomplete = true
			scoped.UnresolvedNodeCount++
			scoped.Warnings = append(scoped.Warnings, warning)
			continue
		}
		if cpuMilli <= 0 || memBytes <= 0 {
			warning := fmt.Sprintf(
				"node %s excluded from node optimization because observed CPU or memory capacity is unavailable",
				info.Name,
			)
			summary.UnresolvedNodeCount++
			summary.EvidenceIncomplete = true
			summary.Warnings = append(summary.Warnings, warning)
			scoped := markPoolSkipped(key)
			scoped.EvidenceIncomplete = true
			scoped.UnresolvedNodeCount++
			scoped.Warnings = append(scoped.Warnings, warning)
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
		pool.nodeNames = append(pool.nodeNames, info.Name)
		if pool.cpuMilli != cpuMilli || pool.memBytes != memBytes {
			pool.invalid = true
		}
	}

	podsByPool := make(map[CostPoolKey][]NodeOptimizationPodInput)
	movablePodsByPool := make(map[CostPoolKey][]corev1.Pod)
	eligibleTopologyPods := make([]nodeOptimizationTopologyPod, 0, len(pods))
	daemonPodsByPool := make(map[CostPoolKey][]corev1.Pod)
	eligibilityWarningsByPool := make(map[CostPoolKey][]string)
	daemonSetsByPool := make(map[CostPoolKey]map[daemonSetKey]*daemonSetObservation)
	unsupportedConstraintsByPool := make(map[CostPoolKey][]string)

	podNameCounts := make(map[string]int, len(pods))
	persistentVolumeClaimPodCounts := make(map[string]int)
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
		warning := fmt.Sprintf(
			"duplicate Pod identity %q appears %d times; node optimization evidence is incomplete",
			strings.ReplaceAll(name, "\x00", "/"),
			podNameCounts[name],
		)
		summary.DuplicatePodCount++
		summary.EvidenceIncomplete = true
		summary.Warnings = append(summary.Warnings, warning)
		scopedKeys := make(map[CostPoolKey]struct{})
		for _, pod := range pods {
			if namespacedKey(pod.Namespace, pod.Name) != name {
				continue
			}
			if key := nodeKeys[pod.Spec.NodeName]; key != nil {
				scopedKeys[*key] = struct{}{}
			}
		}
		if len(scopedKeys) == 0 {
			summary.UnscopedEvidenceIncomplete = true
		}
		for key := range scopedKeys {
			scoped := markPoolSkipped(key)
			scoped.EvidenceIncomplete = true
			scoped.DuplicatePodCount++
			scoped.Warnings = append(scoped.Warnings, warning)
		}
	}

	for _, pod := range pods {
		if podNameCounts[namespacedKey(pod.Namespace, pod.Name)] > 1 {
			continue
		}
		if eligibility, _, _ := ClassifyPodCostEligibility(pod, knownNodes); eligibility == PodCostExcluded {
			summary.ExcludedPodCount++
			continue
		}
		if claimNames, reason := podPersistentVolumeClaimNames(pod); reason == "" {
			for _, claimName := range claimNames {
				persistentVolumeClaimPodCounts[namespacedKey(pod.Namespace, claimName)]++
			}
		}
		cpuMilli, memoryBytes, numericReason := checkedEffectivePodRequests(pod)
		if numericReason != "" {
			warning := fmt.Sprintf(
				"pod %s/%s is unresolved for node optimization because effective resource requests are invalid: %s",
				pod.Namespace,
				pod.Name,
				numericReason,
			)
			summary.UnresolvedPodCount++
			summary.EvidenceIncomplete = true
			summary.Warnings = append(summary.Warnings, warning)
			if key := nodeKeys[pod.Spec.NodeName]; key != nil {
				scoped := markPoolSkipped(*key)
				scoped.EvidenceIncomplete = true
				scoped.UnresolvedPodCount++
				scoped.Warnings = append(scoped.Warnings, warning)
			} else {
				summary.UnscopedEvidenceIncomplete = true
			}
			continue
		}
		input := BuildPodCostInput(pod, knownNodes, nodeKeys, ControllerIndexes{})
		input.CPURequestMilli = cpuMilli
		input.MemoryRequestBytes = memoryBytes

		switch input.Eligibility {
		case PodCostExcluded:
			// Excluded Pods return before numeric request evaluation above.
			continue
		case PodCostUnresolved:
			summary.UnresolvedPodCount++
			summary.EvidenceIncomplete = true
			reason := input.EligibilityWarning
			if reason == "" {
				reason = string(input.EligibilityReason)
			}
			warning := fmt.Sprintf(
				"pod %s/%s is unresolved for node optimization: %s",
				input.Namespace,
				input.PodName,
				reason,
			)
			summary.Warnings = append(summary.Warnings, warning)
			if key := nodeKeys[pod.Spec.NodeName]; key != nil {
				scoped := markPoolSkipped(*key)
				scoped.EvidenceIncomplete = true
				scoped.UnresolvedPodCount++
				scoped.Warnings = append(scoped.Warnings, warning)
			} else {
				summary.UnscopedEvidenceIncomplete = true
			}
			continue
		case PodCostEligible:
			if input.PoolKey == nil {
				warning := fmt.Sprintf(
					"pod %s/%s is eligible but has no canonical pool identity",
					input.Namespace,
					input.PodName,
				)
				summary.UnresolvedPodCount++
				summary.EvidenceIncomplete = true
				summary.UnscopedEvidenceIncomplete = true
				summary.Warnings = append(summary.Warnings, warning)
				continue
			}
		default:
			warning := fmt.Sprintf(
				"pod %s/%s has unknown optimization eligibility %q",
				input.Namespace,
				input.PodName,
				input.Eligibility,
			)
			summary.UnresolvedPodCount++
			summary.EvidenceIncomplete = true
			summary.Warnings = append(summary.Warnings, warning)
			if key := nodeKeys[pod.Spec.NodeName]; key != nil {
				scoped := markPoolSkipped(*key)
				scoped.EvidenceIncomplete = true
				scoped.UnresolvedPodCount++
				scoped.Warnings = append(scoped.Warnings, warning)
			} else {
				summary.UnscopedEvidenceIncomplete = true
			}
			continue
		}

		key := *input.PoolKey
		requiredAffinityTerms, affinityReason := buildNodeOptimizationRequiredAffinityTerms(pod)
		requiredAntiAffinityTerms, antiAffinityReason := buildNodeOptimizationRequiredAntiAffinityTerms(pod)
		interPodAffinityReasons := make([]string, 0, 2)
		if affinityReason != "" {
			interPodAffinityReasons = append(interPodAffinityReasons, affinityReason)
		}
		if antiAffinityReason != "" {
			interPodAffinityReasons = append(interPodAffinityReasons, antiAffinityReason)
		}
		if len(interPodAffinityReasons) > 0 {
			summary.UnsupportedConstraintPodCount++
			summary.EvidenceIncomplete = true
			summary.UnscopedEvidenceIncomplete = true
			sort.Strings(interPodAffinityReasons)
			warning := fmt.Sprintf(
				"pod %s/%s: %s",
				input.Namespace,
				input.PodName,
				interPodAffinityReasons[0],
			)
			summary.Warnings = append(summary.Warnings, warning)
			unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], warning)
			continue
		}
		eligibleTopologyPods = append(eligibleTopologyPods, nodeOptimizationTopologyPod{
			Namespace:                 input.Namespace,
			Name:                      input.PodName,
			NodeName:                  input.NodeName,
			Labels:                    cloneStringMap(pod.Labels),
			RequiredAffinityTerms:     requiredAffinityTerms,
			RequiredAntiAffinityTerms: requiredAntiAffinityTerms,
		})
		if input.EligibilityWarning != "" {
			eligibilityWarningsByPool[key] = append(eligibilityWarningsByPool[key], fmt.Sprintf(
				"pod %s/%s: %s",
				input.Namespace,
				input.PodName,
				input.EligibilityWarning,
			))
		}

		if input.WorkloadKind != "DaemonSet" {
			if reason := unsupportedPodSchedulingConstraintReason(pod, storageEvidence != nil); reason != "" {
				summary.UnsupportedConstraintPodCount++
				unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], fmt.Sprintf(
					"pod %s/%s: %s",
					input.Namespace,
					input.PodName,
					reason,
				))
				continue
			}
		} else {
			if podReferencesPersistentVolumeClaim(pod) {
				summary.UnsupportedConstraintPodCount++
				unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], fmt.Sprintf(
					"DaemonSet pod %s/%s: PVC-backed DaemonSet storage placement is not modeled",
					input.Namespace,
					input.PodName,
				))
				continue
			}
			if reason := unsupportedPodStorageSchedulingReason(pod, true); reason != "" {
				summary.UnsupportedConstraintPodCount++
				unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], fmt.Sprintf(
					"DaemonSet pod %s/%s: %s",
					input.Namespace,
					input.PodName,
					reason,
				))
				continue
			}
			if hasHardTopologySpreadConstraints(pod) {
				summary.UnsupportedConstraintPodCount++
				unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], fmt.Sprintf(
					"DaemonSet pod %s/%s: hard topology spread is not modeled for per-node overhead",
					input.Namespace,
					input.PodName,
				))
				continue
			}
		}

		if evidence == nil && (hasRequiredPodAffinity(pod) || hasRequiredPodAntiAffinity(pod)) {
			summary.UnsupportedConstraintPodCount++
			summary.EvidenceIncomplete = true
			summary.UnscopedEvidenceIncomplete = true
			warning := fmt.Sprintf(
				"pod %s/%s: required inter-Pod affinity needs scheduling topology evidence",
				input.Namespace,
				input.PodName,
			)
			summary.Warnings = append(summary.Warnings, warning)
			unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], warning)
			continue
		}

		if evidence == nil && input.WorkloadKind != "DaemonSet" {
			if reason := nodeSelectorCompatibilityReason(pod.Spec.NodeSelector, key); reason != "" {
				summary.UnsupportedConstraintPodCount++
				unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], fmt.Sprintf(
					"pod %s/%s: %s",
					input.Namespace,
					input.PodName,
					reason,
				))
				continue
			}
		}

		if input.WorkloadKind == "DaemonSet" {
			daemonPodsByPool[key] = append(daemonPodsByPool[key], pod)
			ownerUID, ok := daemonSetControllerUID(pod)
			if !ok {
				warning := fmt.Sprintf(
					"pod %s/%s identified as DaemonSet workload %s/%s but controller UID evidence is missing",
					input.Namespace,
					input.PodName,
					input.Namespace,
					input.WorkloadName,
				)
				summary.EvidenceIncomplete = true
				summary.Warnings = append(summary.Warnings, warning)
				scoped := markPoolSkipped(key)
				scoped.EvidenceIncomplete = true
				scoped.UnresolvedPodCount++
				scoped.Warnings = append(scoped.Warnings, warning)
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
		movablePodsByPool[key] = append(movablePodsByPool[key], pod)
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
		if evidence.UnmatchedSchedulingNodeCount > 0 {
			summary.UnscopedEvidenceIncomplete = true
		}
	}
	if storageEvidence != nil && storageEvidence.EvidenceIncomplete {
		summary.EvidenceIncomplete = true
		summary.Warnings = append(summary.Warnings, storageEvidence.Warnings...)
	}
	if summary.UnscopedEvidenceIncomplete {
		for _, key := range keys {
			if pools[key].nodeCount >= 2 {
				scoped := markPoolSkipped(key)
				scoped.EvidenceIncomplete = true
				scoped.Warnings = append(scoped.Warnings, "pool could not be evaluated because snapshot evidence could not be scoped to a canonical pool")
			}
		}
		sort.Strings(summary.Warnings)
		return summary
	}

	for _, key := range keys {
		pool := pools[key]
		sort.Strings(pool.nodeNames)
		if nodeOptimizationPodsRequireClusterWideSchedulingEvidence(movablePodsByPool[key]) &&
			(summary.UnresolvedPodCount > 0 ||
				summary.UnresolvedNodeCount > 0 ||
				summary.DuplicatePodCount > 0 ||
				summary.DuplicateNodeCount > 0) {
			warning := fmt.Sprintf(
				"pool %s skipped because its topology or inter-Pod constraints require complete cluster-wide Pod and Node occupancy evidence",
				key.PoolName,
			)
			summary.Warnings = append(summary.Warnings, warning)
			scoped := markPoolSkipped(key)
			scoped.EvidenceIncomplete = true
			scoped.Warnings = append(scoped.Warnings, warning)
			continue
		}
		if scoped := poolEvidenceByKey[key]; scoped != nil && scoped.EvidenceIncomplete {
			markPoolSkipped(key)
			continue
		}

		var schedulingCaveats []string
		scenarioPods := append([]NodeOptimizationPodInput(nil), podsByPool[key]...)
		if evidence != nil {
			nodes := schedulingNodesForPool(*evidence, key)
			if len(nodes) != pool.nodeCount {
				warning := fmt.Sprintf(
					"pool %s skipped because scheduling evidence covers %d of %d nodes",
					key.PoolName,
					len(nodes),
					pool.nodeCount,
				)
				summary.Warnings = append(summary.Warnings, warning)
				scoped := markPoolSkipped(key)
				scoped.EvidenceIncomplete = true
				scoped.UnresolvedNodeCount += pool.nodeCount - len(nodes)
				scoped.Warnings = append(scoped.Warnings, warning)
				continue
			}
			interPodAffinityState := buildNodeOptimizationInterPodAffinityState(
				allSchedulingNodes(*evidence),
				eligibleTopologyPods,
				movablePodsByPool[key],
			)

			if poolHasPreferNoSchedule(nodes) {
				schedulingCaveats = append(schedulingCaveats,
					"PreferNoSchedule taints are advisory and are not modeled as hard placement blockers")
			}

			var unsupported []string
			for i, pod := range movablePodsByPool[key] {
				eligible, reason := podEligibleNodeNames(pod, nodes, true)
				if reason != "" {
					summary.UnsupportedConstraintPodCount++
					unsupported = append(unsupported, fmt.Sprintf("pod %s/%s: %s", pod.Namespace, pod.Name, reason))
					continue
				}
				storageEligible, hasPersistentStorage, storageReason := podStorageEligibleNodeNames(
					pod,
					nodes,
					storageEvidence,
					persistentVolumeClaimPodCounts,
				)
				if storageReason != "" {
					summary.UnsupportedConstraintPodCount++
					unsupported = append(unsupported, fmt.Sprintf("pod %s/%s: %s", pod.Namespace, pod.Name, storageReason))
					continue
				}
				if hasPersistentStorage {
					eligible = intersectSortedNodeNames(eligible, storageEligible)
				}
				scenarioPods[i].EligibleNodeNames = eligible
				if len(eligible) == 0 {
					summary.SchedulingBlockedPodCount++
				}

				topologyConstraints, topologyReason := buildNodeOptimizationTopologySpreadConstraints(
					pod,
					allSchedulingNodes(*evidence),
					eligibleTopologyPods,
					movablePodsByPool[key],
				)
				if topologyReason != "" {
					summary.SchedulingBlockedPodCount++
					unsupported = append(unsupported, fmt.Sprintf("pod %s/%s: %s", pod.Namespace, pod.Name, topologyReason))
					continue
				}
				scenarioPods[i].topologySpreadConstraints = topologyConstraints
				interPodAffinityPod := interPodAffinityState.MovablePods[namespacedKey(pod.Namespace, pod.Name)]
				scenarioPods[i].requiredAffinityTerms = append(
					[]nodeOptimizationRequiredPodAffinityTerm(nil),
					interPodAffinityPod.RequiredAffinityTerms...,
				)
				scenarioPods[i].requiredAntiAffinityTerms = append(
					[]nodeOptimizationRequiredPodAffinityTerm(nil),
					interPodAffinityPod.RequiredAntiAffinityTerms...,
				)
				scenarioPods[i].interPodAffinityState = interPodAffinityState
			}

			// DaemonSet Pods remain modeled as per-node overhead. Preserve the
			// existing fail-closed guarantee that their hard taints and required
			// affinity are compatible with every node on which that overhead is
			// assumed. Their nodeSelector was already evidenced by distinct-node
			// observed coverage and remains outside this check.
			for _, pod := range daemonPodsByPool[key] {
				eligible, reason := podEligibleNodeNames(pod, nodes, false)
				if reason != "" {
					summary.UnsupportedConstraintPodCount++
					unsupported = append(unsupported, fmt.Sprintf("DaemonSet pod %s/%s: %s", pod.Namespace, pod.Name, reason))
					continue
				}
				if len(eligible) != len(nodes) {
					summary.SchedulingBlockedPodCount++
					unsupported = append(unsupported, fmt.Sprintf(
						"DaemonSet pod %s/%s is eligible on %d of %d pool nodes",
						pod.Namespace,
						pod.Name,
						len(eligible),
						len(nodes),
					))
				}
			}

			if len(unsupported) > 0 {
				sort.Strings(unsupported)
				warning := fmt.Sprintf(
					"pool %s skipped because scheduling constraints are not fully modeled: %s",
					key.PoolName,
					strings.Join(unsupported, "; "),
				)
				summary.Warnings = append(summary.Warnings, warning)
				scoped := markPoolSkipped(key)
				scoped.UnsupportedConstraintPodCount += len(unsupported)
				scoped.Warnings = append(scoped.Warnings, warning)
				continue
			}
		} else {
			for i, pod := range movablePodsByPool[key] {
				if podReferencesPersistentVolumeClaim(pod) {
					summary.UnsupportedConstraintPodCount++
					unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], fmt.Sprintf(
						"pod %s/%s: persistent volume topology requires scheduling Node and PVC/PV storage evidence",
						pod.Namespace,
						pod.Name,
					))
					continue
				}
				if hasHardTopologySpreadConstraints(pod) {
					summary.UnsupportedConstraintPodCount++
					unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], fmt.Sprintf(
						"pod %s/%s: hard topology spread constraints require scheduling topology evidence",
						pod.Namespace,
						pod.Name,
					))
					continue
				}
				if hasRequiredNodeAffinity(pod) {
					summary.UnsupportedConstraintPodCount++
					unsupportedConstraintsByPool[key] = append(unsupportedConstraintsByPool[key], fmt.Sprintf(
						"pod %s/%s: required node affinity cannot be evaluated without scheduling evidence",
						pod.Namespace,
						pod.Name,
					))
					continue
				}
				scenarioPods[i].EligibleNodeNames = append([]string{}, pool.nodeNames...)
			}
		}

		if blockers := unsupportedConstraintsByPool[key]; len(blockers) > 0 {
			sort.Strings(blockers)
			warning := fmt.Sprintf(
				"pool %s skipped because scheduling constraints are unsupported or lack evidence: %s",
				key.PoolName,
				strings.Join(blockers, "; "),
			)
			summary.Warnings = append(summary.Warnings, warning)
			scoped := markPoolSkipped(key)
			scoped.UnsupportedConstraintPodCount += len(blockers)
			scoped.Warnings = append(scoped.Warnings, warning)
			continue
		}

		if pool.invalid {
			warning := fmt.Sprintf(
				"pool %s skipped because nodes with the same canonical identity expose inconsistent CPU or memory capacity",
				key.PoolName,
			)
			summary.EvidenceIncomplete = true
			summary.Warnings = append(summary.Warnings, warning)
			scoped := markPoolSkipped(key)
			scoped.EvidenceIncomplete = true
			scoped.UnresolvedNodeCount += pool.nodeCount
			scoped.Warnings = append(scoped.Warnings, warning)
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
				warning := fmt.Sprintf(
					"pool %s skipped because DaemonSet %s/%s replicas expose inconsistent effective CPU or memory requests",
					key.PoolName,
					dsKey.namespace,
					dsKey.name,
				)
				summary.Warnings = append(summary.Warnings, warning)
				scoped := markPoolSkipped(key)
				scoped.EvidenceIncomplete = true
				scoped.Warnings = append(scoped.Warnings, warning)
				continue
			}
			if len(obs.nodes) != pool.nodeCount {
				daemonEvidenceComplete = false
				warning := fmt.Sprintf(
					"pool %s skipped because DaemonSet %s/%s is observed on %d of %d distinct nodes; placement scope is not modeled yet",
					key.PoolName,
					dsKey.namespace,
					dsKey.name,
					len(obs.nodes),
					pool.nodeCount,
				)
				summary.Warnings = append(summary.Warnings, warning)
				scoped := markPoolSkipped(key)
				scoped.EvidenceIncomplete = true
				scoped.Warnings = append(scoped.Warnings, warning)
				continue
			}
			nextDaemonCPU, cpuOK := checkedAddInt64(daemonCPUPerNode, obs.cpuMilli)
			if !cpuOK {
				daemonEvidenceComplete = false
				summary.EvidenceIncomplete = true
				warning := fmt.Sprintf(
					"pool %s skipped because DaemonSet per-node CPU overhead exceeds the int64 representable range while adding %s/%s",
					key.PoolName,
					dsKey.namespace,
					dsKey.name,
				)
				summary.Warnings = append(summary.Warnings, warning)
				scoped := markPoolSkipped(key)
				scoped.EvidenceIncomplete = true
				scoped.Warnings = append(scoped.Warnings, warning)
				break
			}
			nextDaemonMemory, memoryOK := checkedAddInt64(daemonMemPerNode, obs.memBytes)
			if !memoryOK {
				daemonEvidenceComplete = false
				summary.EvidenceIncomplete = true
				warning := fmt.Sprintf(
					"pool %s skipped because DaemonSet per-node memory overhead exceeds the int64 representable range while adding %s/%s",
					key.PoolName,
					dsKey.namespace,
					dsKey.name,
				)
				summary.Warnings = append(summary.Warnings, warning)
				scoped := markPoolSkipped(key)
				scoped.EvidenceIncomplete = true
				scoped.Warnings = append(scoped.Warnings, warning)
				break
			}
			daemonCPUPerNode = nextDaemonCPU
			daemonMemPerNode = nextDaemonMemory
			daemonSetCount++
		}

		if !daemonEvidenceComplete {
			markPoolSkipped(key)
			continue
		}

		usableCPUPerNode, cpuOK := checkedSubInt64(pool.cpuMilli, daemonCPUPerNode)
		usableMemPerNode, memoryOK := checkedSubInt64(pool.memBytes, daemonMemPerNode)
		if !cpuOK || !memoryOK {
			warning := fmt.Sprintf(
				"pool %s skipped because allocatable capacity minus DaemonSet overhead cannot be represented as int64",
				key.PoolName,
			)
			summary.EvidenceIncomplete = true
			summary.Warnings = append(summary.Warnings, warning)
			scoped := markPoolSkipped(key)
			scoped.EvidenceIncomplete = true
			scoped.Warnings = append(scoped.Warnings, warning)
			continue
		}
		if usableCPUPerNode <= 0 || usableMemPerNode <= 0 {
			summary.EvidenceIncomplete = true
			warning := fmt.Sprintf(
				"pool %s skipped because observed DaemonSet per-node overhead leaves no positive CPU or memory capacity for movable workloads",
				key.PoolName,
			)
			summary.Warnings = append(summary.Warnings, warning)
			scoped := markPoolSkipped(key)
			scoped.EvidenceIncomplete = true
			scoped.Warnings = append(scoped.Warnings, warning)
			continue
		}

		removedNodeName, candidateNodeNames, simulation := simulateNMinusOneNodeRemoval(
			key,
			pool.nodeNames,
			usableCPUPerNode,
			usableMemPerNode,
			scenarioPods,
		)

		caveats := append([]string(nil), schedulingCaveats...)
		caveats = append(caveats, eligibilityWarningsByPool[key]...)
		sort.Strings(caveats)

		summary.Scenarios = append(summary.Scenarios, NodeOptimizationScenario{
			PoolKey:                     key,
			RemovedNodeName:             removedNodeName,
			CandidateNodeNames:          append([]string(nil), candidateNodeNames...),
			EligiblePodCount:            len(scenarioPods),
			DaemonSetCount:              daemonSetCount,
			DaemonSetCPUPerNodeMilli:    daemonCPUPerNode,
			DaemonSetMemoryPerNodeBytes: daemonMemPerNode,
			SchedulingCaveats:           caveats,
			VerifiedChecks:              nodeOptimizationScenarioVerifiedChecks(movablePodsByPool[key], evidence != nil, storageEvidence != nil, daemonSetCount),
			movablePodKeys:              nodeOptimizationMovablePodKeys(movablePodsByPool[key]),
			Simulation:                  simulation,
		})
	}

	return summary
}

func nodeOptimizationPodsRequireClusterWideSchedulingEvidence(pods []corev1.Pod) bool {
	for _, pod := range pods {
		if hasHardTopologySpreadConstraints(pod) || hasRequiredPodAffinity(pod) || hasRequiredPodAntiAffinity(pod) {
			return true
		}
	}
	return false
}

func sortedNodeOptimizationPoolEvidence(
	byPool map[CostPoolKey]*NodeOptimizationPoolEvidence,
) []NodeOptimizationPoolEvidence {
	result := make([]NodeOptimizationPoolEvidence, 0, len(byPool))
	for _, scoped := range byPool {
		copy := *scoped
		sort.Strings(copy.Warnings)
		result = append(result, copy)
	}
	sort.Slice(result, func(i, j int) bool {
		return costPoolKeySortValue(result[i].PoolKey) < costPoolKeySortValue(result[j].PoolKey)
	})
	return result
}

// checkedEffectivePodRequests mirrors Kubernetes effective CPU/memory request
// semantics while rejecting negative quantities and every intermediate or final
// int64 overflow. It is used only at the snapshot-to-simulator boundary; the
// pure placement engine continues to consume validated integer requests.
func checkedEffectivePodRequests(pod corev1.Pod) (int64, int64, string) {
	appCPU, appMemory := int64(0), int64(0)
	for _, container := range pod.Spec.Containers {
		cpu, memory, reason := checkedOptimizationContainerRequests(container)
		if reason != "" {
			return 0, 0, reason
		}
		var ok bool
		appCPU, ok = checkedAddInt64(appCPU, cpu)
		if !ok {
			return 0, 0, fmt.Sprintf("application CPU request overflows int64 while adding container %q", container.Name)
		}
		appMemory, ok = checkedAddInt64(appMemory, memory)
		if !ok {
			return 0, 0, fmt.Sprintf("application memory request overflows int64 while adding container %q", container.Name)
		}
	}

	restartableCPU, restartableMemory := int64(0), int64(0)
	peakInitCPU, peakInitMemory := int64(0), int64(0)
	for _, container := range pod.Spec.InitContainers {
		cpu, memory, reason := checkedOptimizationContainerRequests(container)
		if reason != "" {
			return 0, 0, reason
		}

		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			var ok bool
			restartableCPU, ok = checkedAddInt64(restartableCPU, cpu)
			if !ok {
				return 0, 0, fmt.Sprintf("restartable init-container CPU request overflows int64 while adding container %q", container.Name)
			}
			restartableMemory, ok = checkedAddInt64(restartableMemory, memory)
			if !ok {
				return 0, 0, fmt.Sprintf("restartable init-container memory request overflows int64 while adding container %q", container.Name)
			}
			appCPU, ok = checkedAddInt64(appCPU, cpu)
			if !ok {
				return 0, 0, fmt.Sprintf("effective application CPU request overflows int64 while adding restartable init container %q", container.Name)
			}
			appMemory, ok = checkedAddInt64(appMemory, memory)
			if !ok {
				return 0, 0, fmt.Sprintf("effective application memory request overflows int64 while adding restartable init container %q", container.Name)
			}
			if restartableCPU > peakInitCPU {
				peakInitCPU = restartableCPU
			}
			if restartableMemory > peakInitMemory {
				peakInitMemory = restartableMemory
			}
			continue
		}

		stageCPU, ok := checkedAddInt64(restartableCPU, cpu)
		if !ok {
			return 0, 0, fmt.Sprintf("init-container stage CPU request overflows int64 at container %q", container.Name)
		}
		stageMemory, ok := checkedAddInt64(restartableMemory, memory)
		if !ok {
			return 0, 0, fmt.Sprintf("init-container stage memory request overflows int64 at container %q", container.Name)
		}
		if stageCPU > peakInitCPU {
			peakInitCPU = stageCPU
		}
		if stageMemory > peakInitMemory {
			peakInitMemory = stageMemory
		}
	}

	cpuMilli := maxInt64(appCPU, peakInitCPU)
	memoryBytes := maxInt64(appMemory, peakInitMemory)
	if quantity, ok := pod.Spec.Overhead[corev1.ResourceCPU]; ok {
		overhead, reason := checkedOptimizationQuantityValue(quantity, resource.Milli)
		if reason != "" {
			return 0, 0, "Pod CPU overhead " + reason
		}
		cpuMilli, ok = checkedAddInt64(cpuMilli, overhead)
		if !ok {
			return 0, 0, "effective CPU request overflows int64 while adding Pod overhead"
		}
	}
	if quantity, ok := pod.Spec.Overhead[corev1.ResourceMemory]; ok {
		overhead, reason := checkedOptimizationQuantityValue(quantity, 0)
		if reason != "" {
			return 0, 0, "Pod memory overhead " + reason
		}
		memoryBytes, ok = checkedAddInt64(memoryBytes, overhead)
		if !ok {
			return 0, 0, "effective memory request overflows int64 while adding Pod overhead"
		}
	}
	return cpuMilli, memoryBytes, ""
}

func checkedOptimizationContainerRequests(container corev1.Container) (int64, int64, string) {
	var cpuMilli, memoryBytes int64
	if quantity, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
		value, reason := checkedOptimizationQuantityValue(quantity, resource.Milli)
		if reason != "" {
			return 0, 0, fmt.Sprintf("container %q CPU request %s", container.Name, reason)
		}
		cpuMilli = value
	}
	if quantity, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
		value, reason := checkedOptimizationQuantityValue(quantity, 0)
		if reason != "" {
			return 0, 0, fmt.Sprintf("container %q memory request %s", container.Name, reason)
		}
		memoryBytes = value
	}
	return cpuMilli, memoryBytes, ""
}

func checkedOptimizationQuantityValue(quantity resource.Quantity, scale resource.Scale) (int64, string) {
	if quantity.Sign() < 0 {
		return 0, "is negative"
	}
	maximum := resource.NewScaledQuantity(math.MaxInt64, scale)
	if quantity.Cmp(*maximum) > 0 {
		return 0, "exceeds the int64 representable range"
	}
	value := quantity.ScaledValue(scale)
	if value < 0 {
		return 0, "cannot be represented as a nonnegative int64"
	}
	return value, ""
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
