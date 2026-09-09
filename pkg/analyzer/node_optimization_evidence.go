package analyzer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
)

// NodeOptimizationSchedulingNode preserves scheduler-relevant evidence from an
// already-acquired corev1.Node snapshot while retaining the canonical pool
// identity derived from NodeInfo. Maps and slices are copied so simulator code
// cannot mutate the Kubernetes snapshot.
type NodeOptimizationSchedulingNode struct {
	Name          string
	PoolKey       CostPoolKey
	Labels        map[string]string
	Taints        []corev1.Taint
	Unschedulable bool
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
			Name:          info.Name,
			PoolKey:       key,
			Labels:        cloneStringMap(node.Labels),
			Taints:        append([]corev1.Taint(nil), node.Spec.Taints...),
			Unschedulable: node.Spec.Unschedulable,
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
