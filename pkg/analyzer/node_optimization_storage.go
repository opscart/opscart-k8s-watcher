package analyzer

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

// NodeOptimizationStorageEvidence is an immutable-by-convention index over
// already-acquired PVC and PV snapshots. Building and consuming it performs no
// Kubernetes, CSI, or cloud API calls.
type NodeOptimizationStorageEvidence struct {
	PersistentVolumeClaimCount          int
	PersistentVolumeCount               int
	DuplicatePersistentVolumeClaimCount int
	DuplicatePersistentVolumeCount      int
	EvidenceIncomplete                  bool
	Warnings                            []string

	claimsByKey          map[string]corev1.PersistentVolumeClaim
	volumesByName        map[string]corev1.PersistentVolume
	duplicateClaimKeys   map[string]struct{}
	duplicateVolumeNames map[string]struct{}
}

// BuildNodeOptimizationStorageEvidence indexes caller-provided snapshots. The
// stored API objects are deep-copied so later caller mutation cannot change a
// simulation.
func BuildNodeOptimizationStorageEvidence(
	claims []corev1.PersistentVolumeClaim,
	volumes []corev1.PersistentVolume,
) NodeOptimizationStorageEvidence {
	result := NodeOptimizationStorageEvidence{
		PersistentVolumeClaimCount: len(claims),
		PersistentVolumeCount:      len(volumes),
		claimsByKey:                make(map[string]corev1.PersistentVolumeClaim, len(claims)),
		volumesByName:              make(map[string]corev1.PersistentVolume, len(volumes)),
		duplicateClaimKeys:         make(map[string]struct{}),
		duplicateVolumeNames:       make(map[string]struct{}),
	}

	claimCounts := make(map[string]int, len(claims))
	invalidClaimIdentities := make(map[string]struct{})
	for _, claim := range claims {
		key := namespacedKey(claim.Namespace, claim.Name)
		claimCounts[key]++
		if strings.TrimSpace(claim.Namespace) == "" || strings.TrimSpace(claim.Name) == "" {
			invalidClaimIdentities[key] = struct{}{}
		}
	}
	claimKeys := sortedStringIntMapKeys(claimCounts)
	for _, key := range claimKeys {
		if claimCounts[key] > 1 {
			result.duplicateClaimKeys[key] = struct{}{}
			result.DuplicatePersistentVolumeClaimCount++
			result.EvidenceIncomplete = true
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"duplicate PersistentVolumeClaim identity %q appears %d times in storage evidence",
				displayNamespacedKey(key),
				claimCounts[key],
			))
		}
	}
	invalidClaimKeys := sortedStringSetKeys(invalidClaimIdentities)
	for _, key := range invalidClaimKeys {
		result.EvidenceIncomplete = true
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"PersistentVolumeClaim storage evidence contains incomplete identity %q",
			displayNamespacedKey(key),
		))
	}
	for _, claim := range claims {
		key := namespacedKey(claim.Namespace, claim.Name)
		if claimCounts[key] != 1 {
			continue
		}
		if _, invalid := invalidClaimIdentities[key]; invalid {
			continue
		}
		result.claimsByKey[key] = *claim.DeepCopy()
	}

	volumeCounts := make(map[string]int, len(volumes))
	for _, volume := range volumes {
		volumeCounts[volume.Name]++
	}
	volumeNames := sortedStringIntMapKeys(volumeCounts)
	for _, name := range volumeNames {
		if strings.TrimSpace(name) == "" {
			result.EvidenceIncomplete = true
			result.Warnings = append(result.Warnings, "PersistentVolume storage evidence contains an empty identity")
		}
		if volumeCounts[name] > 1 {
			result.duplicateVolumeNames[name] = struct{}{}
			result.DuplicatePersistentVolumeCount++
			result.EvidenceIncomplete = true
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"duplicate PersistentVolume identity %q appears %d times in storage evidence",
				name,
				volumeCounts[name],
			))
		}
	}
	for _, volume := range volumes {
		if strings.TrimSpace(volume.Name) == "" || volumeCounts[volume.Name] != 1 {
			continue
		}
		result.volumesByName[volume.Name] = *volume.DeepCopy()
	}

	sort.Strings(result.Warnings)
	return result
}

func podReferencesPersistentVolumeClaim(pod corev1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

func podPersistentVolumeClaimNames(pod corev1.Pod) ([]string, string) {
	claimSet := make(map[string]struct{})
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		claimName := strings.TrimSpace(volume.PersistentVolumeClaim.ClaimName)
		if claimName == "" {
			return nil, fmt.Sprintf("pod %s/%s has a PersistentVolumeClaim volume with an empty claimName", pod.Namespace, pod.Name)
		}
		claimSet[claimName] = struct{}{}
	}
	return sortedStringSetKeys(claimSet), ""
}

// podStorageEligibleNodeNames resolves Pod -> PVC -> PV and returns the exact
// candidate-node intersection imposed by all bound PV node affinities.
func podStorageEligibleNodeNames(
	pod corev1.Pod,
	nodes []NodeOptimizationSchedulingNode,
	evidence *NodeOptimizationStorageEvidence,
	claimPodCounts map[string]int,
) ([]string, bool, string) {
	claimNames, reason := podPersistentVolumeClaimNames(pod)
	if reason != "" {
		return nil, true, reason
	}
	if len(claimNames) == 0 {
		return nil, false, ""
	}
	if evidence == nil {
		return nil, true, fmt.Sprintf("pod %s/%s references persistent storage, but PVC/PV storage evidence is unavailable", pod.Namespace, pod.Name)
	}

	eligible := make(map[string]struct{}, len(nodes))
	nodesByName := make(map[string]NodeOptimizationSchedulingNode, len(nodes))
	for _, node := range nodes {
		eligible[node.Name] = struct{}{}
		nodesByName[node.Name] = node
	}

	for _, claimName := range claimNames {
		claimKey := namespacedKey(pod.Namespace, claimName)
		if _, duplicate := evidence.duplicateClaimKeys[claimKey]; duplicate {
			return nil, true, fmt.Sprintf("pod %s/%s references duplicate PersistentVolumeClaim identity %s/%s", pod.Namespace, pod.Name, pod.Namespace, claimName)
		}
		claim, found := evidence.claimsByKey[claimKey]
		if !found {
			return nil, true, fmt.Sprintf("pod %s/%s references missing PersistentVolumeClaim %s/%s", pod.Namespace, pod.Name, pod.Namespace, claimName)
		}
		if claim.DeletionTimestamp != nil {
			return nil, true, fmt.Sprintf("PersistentVolumeClaim %s/%s is terminating", claim.Namespace, claim.Name)
		}
		if claim.Status.Phase != corev1.ClaimBound {
			return nil, true, fmt.Sprintf("PersistentVolumeClaim %s/%s is %s, not Bound; late binding is not modeled", claim.Namespace, claim.Name, claim.Status.Phase)
		}
		volumeName := strings.TrimSpace(claim.Spec.VolumeName)
		if volumeName == "" {
			return nil, true, fmt.Sprintf("Bound PersistentVolumeClaim %s/%s has no spec.volumeName", claim.Namespace, claim.Name)
		}
		if _, duplicate := evidence.duplicateVolumeNames[volumeName]; duplicate {
			return nil, true, fmt.Sprintf("PersistentVolumeClaim %s/%s references duplicate PersistentVolume identity %q", claim.Namespace, claim.Name, volumeName)
		}
		volume, found := evidence.volumesByName[volumeName]
		if !found {
			return nil, true, fmt.Sprintf("PersistentVolumeClaim %s/%s references missing PersistentVolume %q", claim.Namespace, claim.Name, volumeName)
		}
		if bindingReason := persistentVolumeBindingEvidenceReason(claim, volume); bindingReason != "" {
			return nil, true, bindingReason
		}
		if sourceReason := persistentVolumeSourceUnsupportedReason(volume); sourceReason != "" {
			return nil, true, sourceReason
		}
		if persistentVolumeUsesReadWriteOncePod(claim, volume) {
			return nil, true, fmt.Sprintf("PersistentVolumeClaim %s/%s uses ReadWriteOncePod, whose exclusive-use semantics are not modeled", claim.Namespace, claim.Name)
		}
		if persistentVolumeUsesReadWriteOnce(claim, volume) && claimPodCounts[claimKey] > 1 {
			return nil, true, fmt.Sprintf(
				"PersistentVolumeClaim %s/%s uses ReadWriteOnce and is referenced by %d continuing Pods; shared multi-attach placement is not modeled",
				claim.Namespace,
				claim.Name,
				claimPodCounts[claimKey],
			)
		}
		if volume.Spec.Local != nil && (volume.Spec.NodeAffinity == nil || volume.Spec.NodeAffinity.Required == nil) {
			return nil, true, fmt.Sprintf("local PersistentVolume %s has no required node affinity and cannot be treated as portable", volume.Name)
		}

		required := (*corev1.NodeSelector)(nil)
		if volume.Spec.NodeAffinity != nil {
			required = volume.Spec.NodeAffinity.Required
		}
		if required == nil {
			continue
		}
		if affinityReason := persistentVolumeNodeAffinityUnsupportedReason(volume.Name, required.NodeSelectorTerms); affinityReason != "" {
			return nil, true, affinityReason
		}
		if topologyReason := persistentVolumeTopologyEvidenceReason(nodes, required.NodeSelectorTerms); topologyReason != "" {
			return nil, true, fmt.Sprintf("PersistentVolume %s cannot evaluate node affinity: %s", volume.Name, topologyReason)
		}
		for _, node := range nodes {
			if _, stillEligible := eligible[node.Name]; !stillEligible {
				continue
			}
			if !nodeMatchesPersistentVolumeNodeAffinity(node.Labels, required.NodeSelectorTerms) {
				delete(eligible, node.Name)
			}
		}
	}

	if _, observedNodeFound := nodesByName[pod.Spec.NodeName]; !observedNodeFound {
		return nil, true, fmt.Sprintf("pod %s/%s observed node %s is absent from its storage scheduling scope", pod.Namespace, pod.Name, pod.Spec.NodeName)
	}
	if _, observedNodeEligible := eligible[pod.Spec.NodeName]; len(eligible) > 0 && !observedNodeEligible {
		return nil, true, fmt.Sprintf(
			"pod %s/%s is observed on node %s, which does not satisfy its PersistentVolume node-affinity intersection",
			pod.Namespace,
			pod.Name,
			pod.Spec.NodeName,
		)
	}

	return sortedStringSetKeys(eligible), true, ""
}

func persistentVolumeBindingEvidenceReason(
	claim corev1.PersistentVolumeClaim,
	volume corev1.PersistentVolume,
) string {
	if volume.DeletionTimestamp != nil {
		return fmt.Sprintf("PersistentVolume %s is terminating", volume.Name)
	}
	if volume.Status.Phase != corev1.VolumeBound {
		return fmt.Sprintf("PersistentVolume %s is %s, not Bound", volume.Name, volume.Status.Phase)
	}
	if volume.Spec.ClaimRef == nil {
		return fmt.Sprintf("PersistentVolume %s has no claimRef for Bound PersistentVolumeClaim %s/%s", volume.Name, claim.Namespace, claim.Name)
	}
	claimRef := volume.Spec.ClaimRef
	if claimRef.Namespace != claim.Namespace || claimRef.Name != claim.Name {
		return fmt.Sprintf(
			"PersistentVolume %s claimRef %s/%s does not match PersistentVolumeClaim %s/%s",
			volume.Name,
			claimRef.Namespace,
			claimRef.Name,
			claim.Namespace,
			claim.Name,
		)
	}
	if claim.UID == "" || claimRef.UID == "" {
		return fmt.Sprintf("PersistentVolume %s and PersistentVolumeClaim %s/%s lack complete binding UID evidence", volume.Name, claim.Namespace, claim.Name)
	}
	if claimRef.UID != claim.UID {
		return fmt.Sprintf(
			"PersistentVolume %s claimRef UID %q does not match PersistentVolumeClaim %s/%s UID %q",
			volume.Name,
			claimRef.UID,
			claim.Namespace,
			claim.Name,
			claim.UID,
		)
	}
	return ""
}

func persistentVolumeSourceUnsupportedReason(volume corev1.PersistentVolume) string {
	switch {
	case volume.Spec.NFS != nil:
		return ""
	case volume.Spec.Local != nil:
		return ""
	case volume.Spec.CSI != nil:
		return fmt.Sprintf(
			"PersistentVolume %s uses CSI; CSI attachment limits and driver-specific scheduling constraints are not modeled",
			volume.Name,
		)
	default:
		return fmt.Sprintf(
			"PersistentVolume %s uses a volume source whose attachment and driver-specific scheduling constraints are not modeled",
			volume.Name,
		)
	}
}

func persistentVolumeUsesReadWriteOncePod(
	claim corev1.PersistentVolumeClaim,
	volume corev1.PersistentVolume,
) bool {
	for _, mode := range claim.Spec.AccessModes {
		if mode == corev1.ReadWriteOncePod {
			return true
		}
	}
	for _, mode := range volume.Spec.AccessModes {
		if mode == corev1.ReadWriteOncePod {
			return true
		}
	}
	return false
}

func persistentVolumeUsesReadWriteOnce(
	claim corev1.PersistentVolumeClaim,
	volume corev1.PersistentVolume,
) bool {
	for _, mode := range claim.Spec.AccessModes {
		if mode == corev1.ReadWriteOnce {
			return true
		}
	}
	for _, mode := range volume.Spec.AccessModes {
		if mode == corev1.ReadWriteOnce {
			return true
		}
	}
	return false
}

func persistentVolumeNodeAffinityUnsupportedReason(name string, terms []corev1.NodeSelectorTerm) string {
	for termIndex, term := range terms {
		if len(term.MatchFields) > 0 {
			return fmt.Sprintf("PersistentVolume %s node affinity term %d uses matchFields, which is not modeled", name, termIndex)
		}
		for expressionIndex, expression := range term.MatchExpressions {
			if _, reason := nodeSelectorRequirement(expression); reason != "" {
				return fmt.Sprintf(
					"PersistentVolume %s node affinity term %d expression %d is invalid or unsupported: %s",
					name,
					termIndex,
					expressionIndex,
					reason,
				)
			}
		}
	}
	return ""
}

func persistentVolumeTopologyEvidenceReason(
	nodes []NodeOptimizationSchedulingNode,
	terms []corev1.NodeSelectorTerm,
) string {
	requirement := nodeOptimizationTopologyRequirement{}
	for _, term := range terms {
		for _, expression := range term.MatchExpressions {
			switch expression.Key {
			case corev1.LabelHostname:
				requirement.Hostname = true
			case corev1.LabelTopologyZone, corev1.LabelFailureDomainBetaZone:
				requirement.Zone = true
			case corev1.LabelTopologyRegion, corev1.LabelFailureDomainBetaRegion:
				requirement.Region = true
			}
		}
	}
	return topologyEvidenceRequirementReason(nodes, requirement)
}

func nodeSelectorRequirement(expression corev1.NodeSelectorRequirement) (*labels.Requirement, string) {
	var operator selection.Operator
	switch expression.Operator {
	case corev1.NodeSelectorOpIn:
		operator = selection.In
	case corev1.NodeSelectorOpNotIn:
		operator = selection.NotIn
	case corev1.NodeSelectorOpExists:
		operator = selection.Exists
	case corev1.NodeSelectorOpDoesNotExist:
		operator = selection.DoesNotExist
	case corev1.NodeSelectorOpGt:
		operator = selection.GreaterThan
	case corev1.NodeSelectorOpLt:
		operator = selection.LessThan
	default:
		return nil, fmt.Sprintf("unsupported node selector operator %q", expression.Operator)
	}
	requirement, err := labels.NewRequirement(expression.Key, operator, expression.Values)
	if err != nil {
		return nil, err.Error()
	}
	return requirement, ""
}

func nodeSelectorRequirementMatches(labelsMap map[string]string, expression corev1.NodeSelectorRequirement) bool {
	requirement, reason := nodeSelectorRequirement(expression)
	if reason != "" {
		return false
	}
	return requirement.Matches(labels.Set(labelsMap))
}

func nodeMatchesPersistentVolumeNodeAffinity(labelsMap map[string]string, terms []corev1.NodeSelectorTerm) bool {
	for _, term := range terms {
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue
		}
		if len(term.MatchFields) > 0 {
			continue
		}
		matches := true
		for _, expression := range term.MatchExpressions {
			if !nodeSelectorRequirementMatches(labelsMap, expression) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func sortedStringIntMapKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringSetKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func intersectSortedNodeNames(left, right []string) []string {
	rightSet := make(map[string]struct{}, len(right))
	for _, name := range right {
		rightSet[name] = struct{}{}
	}
	intersection := make([]string, 0, len(left))
	for _, name := range left {
		if _, found := rightSet[name]; found {
			intersection = append(intersection, name)
		}
	}
	sort.Strings(intersection)
	return intersection
}

func displayNamespacedKey(key string) string {
	return strings.ReplaceAll(key, "\x00", "/")
}
