package analyzer

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

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

func podEligibleNodeNames(
	pod corev1.Pod,
	nodes []NodeOptimizationSchedulingNode,
	applyNodeSelector bool,
) ([]string, string) {
	if reason := requiredNodeAffinityUnsupportedReason(pod); reason != "" {
		return nil, reason
	}

	eligible := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.Unschedulable {
			continue
		}
		if applyNodeSelector && !nodeMatchesNodeSelector(node, pod.Spec.NodeSelector) {
			continue
		}
		if hasRequiredNodeAffinity(pod) {
			required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			if !nodeMatchesRequiredNodeAffinity(node.Labels, required.NodeSelectorTerms) {
				continue
			}
		}
		if podHardTaintCompatibilityReason(pod, hardTaintsForNode(node)) != "" {
			continue
		}
		eligible = append(eligible, node.Name)
	}
	sort.Strings(eligible)
	return eligible, ""
}

func nodeMatchesNodeSelector(node NodeOptimizationSchedulingNode, selector map[string]string) bool {
	for key, expected := range selector {
		actual, ok := node.Labels[key]
		if !ok {
			switch key {
			case corev1.LabelOSStable, "beta.kubernetes.io/os":
				actual, ok = node.PoolKey.OS, node.PoolKey.OS != ""
			case corev1.LabelArchStable, "beta.kubernetes.io/arch":
				actual, ok = node.PoolKey.Architecture, node.PoolKey.Architecture != ""
			}
		}
		if !ok || actual != expected {
			return false
		}
	}
	return true
}

func hasRequiredNodeAffinity(pod corev1.Pod) bool {
	return pod.Spec.Affinity != nil &&
		pod.Spec.Affinity.NodeAffinity != nil &&
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil
}

func unsupportedPodSchedulingConstraintReason(pod corev1.Pod) string {
	if schedulerName := strings.TrimSpace(pod.Spec.SchedulerName); schedulerName != "" && schedulerName != corev1.DefaultSchedulerName {
		return fmt.Sprintf("custom scheduler %q is not modeled yet", schedulerName)
	}
	if pod.Spec.RuntimeClassName != nil {
		return "RuntimeClass scheduling constraints are not modeled yet"
	}
	if len(pod.Spec.ResourceClaims) > 0 {
		return "dynamic resource claims are not modeled yet"
	}
	if resourceName := firstUnsupportedPodRequestResource(pod); resourceName != "" {
		return fmt.Sprintf("requested resource %s is not modeled yet", resourceName)
	}
	for _, container := range append(append([]corev1.Container(nil), pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, port := range container.Ports {
			if port.HostPort != 0 {
				return "host-port scheduling conflicts are not modeled yet"
			}
		}
	}
	if pod.Spec.Affinity != nil {
		if pod.Spec.Affinity.PodAffinity != nil &&
			len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
			return "required pod affinity is not modeled yet"
		}
		if pod.Spec.Affinity.PodAntiAffinity != nil &&
			len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
			return "required pod anti-affinity is not modeled yet"
		}
	}
	for _, constraint := range pod.Spec.TopologySpreadConstraints {
		if constraint.WhenUnsatisfiable == corev1.DoNotSchedule {
			return "hard topology spread constraints are not modeled yet"
		}
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			return "persistent volume topology is not modeled yet"
		}
	}
	return ""
}

func firstUnsupportedPodRequestResource(pod corev1.Pod) string {
	resources := make(map[string]struct{})
	collect := func(requests corev1.ResourceList) {
		for name, quantity := range requests {
			if quantity.IsZero() || name == corev1.ResourceCPU || name == corev1.ResourceMemory {
				continue
			}
			resources[string(name)] = struct{}{}
		}
	}
	for _, container := range pod.Spec.InitContainers {
		collect(container.Resources.Requests)
	}
	for _, container := range pod.Spec.Containers {
		collect(container.Resources.Requests)
	}
	collect(pod.Spec.Overhead)

	names := make([]string, 0, len(resources))
	for name := range resources {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func requiredNodeAffinityUnsupportedReason(pod corev1.Pod) string {
	if !hasRequiredNodeAffinity(pod) {
		return ""
	}

	required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	for _, term := range required.NodeSelectorTerms {
		if len(term.MatchFields) > 0 {
			return "required node affinity matchFields are not modeled yet"
		}
		for _, expression := range term.MatchExpressions {
			switch expression.Operator {
			case corev1.NodeSelectorOpIn, corev1.NodeSelectorOpExists:
			default:
				return fmt.Sprintf(
					"required node affinity operator %s is not modeled yet",
					expression.Operator,
				)
			}
		}
	}
	return ""
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
