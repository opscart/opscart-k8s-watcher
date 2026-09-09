package analyzer

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

type nodeOptimizationTopologyPod struct {
	Namespace                 string
	Name                      string
	NodeName                  string
	Labels                    map[string]string
	RequiredAffinityTerms     []nodeOptimizationRequiredPodAffinityTerm
	RequiredAntiAffinityTerms []nodeOptimizationRequiredPodAffinityTerm
}

type nodeOptimizationRequiredPodAffinityTerm struct {
	TopologyKey         string
	TopologyRequirement nodeOptimizationTopologyRequirement
	Namespaces          []string
	Selector            labels.Selector
	SortKey             string
}

type nodeOptimizationInterPodAffinityState struct {
	Nodes       []NodeOptimizationSchedulingNode
	FixedPods   []nodeOptimizationTopologyPod
	MovablePods map[string]nodeOptimizationTopologyPod
}

type nodeOptimizationTopologySpreadConstraint struct {
	TopologyKey string
	MaxSkew     int32
	MinDomains  int32
	SortKey     string

	DomainNodes              []NodeOptimizationSchedulingNode
	TopologyRequirement      nodeOptimizationTopologyRequirement
	ExistingMatchingPodNodes []string
	MatchingMovablePodKeys   map[string]struct{}
}

// nodeOptimizationTopologyRequirement identifies the topology dimensions a
// future scheduling check needs. Current CPU/memory placement does not request
// any topology dimensions and therefore remains independent of this gate.
type nodeOptimizationTopologyRequirement struct {
	Hostname bool
	Zone     bool
	Region   bool
}

// topologyEvidenceRequirementReason fails closed for the requested topology
// dimensions without treating unrelated missing dimensions as blockers.
func topologyEvidenceRequirementReason(
	nodes []NodeOptimizationSchedulingNode,
	requirement nodeOptimizationTopologyRequirement,
) string {
	if !requirement.Hostname && !requirement.Zone && !requirement.Region {
		return ""
	}

	ordered := append([]NodeOptimizationSchedulingNode(nil), nodes...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Name < ordered[j].Name
	})
	if len(ordered) == 0 {
		return "topology evidence contains no candidate nodes"
	}

	for _, node := range ordered {
		if requirement.Hostname && !node.Topology.hasHostnameEvidence() {
			return fmt.Sprintf("candidate node %s is missing hostname topology evidence", node.Name)
		}
		if requirement.Zone {
			if node.Topology.ZoneContradictory {
				return fmt.Sprintf("candidate node %s has contradictory zone topology evidence", node.Name)
			}
			if !node.Topology.hasZoneEvidence() {
				return fmt.Sprintf("candidate node %s is missing zone topology evidence", node.Name)
			}
		}
		if requirement.Region {
			if node.Topology.RegionContradictory {
				return fmt.Sprintf("candidate node %s has contradictory region topology evidence", node.Name)
			}
			if !node.Topology.hasRegionEvidence() {
				return fmt.Sprintf("candidate node %s is missing region topology evidence", node.Name)
			}
		}
	}

	if requirement.Hostname {
		hostnameNodes := make(map[string][]string, len(ordered))
		for _, node := range ordered {
			hostnameNodes[node.Topology.Hostname] = append(hostnameNodes[node.Topology.Hostname], node.Name)
		}
		hostnames := make([]string, 0, len(hostnameNodes))
		for hostname := range hostnameNodes {
			hostnames = append(hostnames, hostname)
		}
		sort.Strings(hostnames)
		for _, hostname := range hostnames {
			if len(hostnameNodes[hostname]) > 1 {
				return fmt.Sprintf(
					"hostname topology evidence %q is ambiguous across candidate nodes %s",
					hostname,
					strings.Join(hostnameNodes[hostname], ", "),
				)
			}
		}
	}

	return ""
}

func hasRequiredPodAntiAffinity(pod corev1.Pod) bool {
	return pod.Spec.Affinity != nil &&
		pod.Spec.Affinity.PodAntiAffinity != nil &&
		len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0
}

func hasRequiredPodAffinity(pod corev1.Pod) bool {
	return pod.Spec.Affinity != nil &&
		pod.Spec.Affinity.PodAffinity != nil &&
		len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0
}

func buildNodeOptimizationRequiredAntiAffinityTerms(pod corev1.Pod) ([]nodeOptimizationRequiredPodAffinityTerm, string) {
	if !hasRequiredPodAntiAffinity(pod) {
		return nil, ""
	}
	return buildNodeOptimizationRequiredPodAffinityTerms(
		pod,
		pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		"required pod anti-affinity",
	)
}

func buildNodeOptimizationRequiredAffinityTerms(pod corev1.Pod) ([]nodeOptimizationRequiredPodAffinityTerm, string) {
	if !hasRequiredPodAffinity(pod) {
		return nil, ""
	}
	return buildNodeOptimizationRequiredPodAffinityTerms(
		pod,
		pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		"required pod affinity",
	)
}

func buildNodeOptimizationRequiredPodAffinityTerms(
	pod corev1.Pod,
	podAffinityTerms []corev1.PodAffinityTerm,
	description string,
) ([]nodeOptimizationRequiredPodAffinityTerm, string) {
	terms := make([]nodeOptimizationRequiredPodAffinityTerm, 0, len(podAffinityTerms))
	reasons := make([]string, 0)
	for _, term := range podAffinityTerms {
		termIdentity := fmt.Sprintf("%s term with topologyKey %q", description, term.TopologyKey)
		if term.NamespaceSelector != nil {
			reasons = append(reasons, termIdentity+" uses namespaceSelector, but namespace-label evidence is unavailable")
		}
		if len(term.MatchLabelKeys) > 0 {
			reasons = append(reasons, termIdentity+" uses matchLabelKeys, which is not modeled yet")
		}
		if len(term.MismatchLabelKeys) > 0 {
			reasons = append(reasons, termIdentity+" uses mismatchLabelKeys, which is not modeled yet")
		}

		requirement := topologyRequirementForKey(term.TopologyKey)
		if !requirement.Hostname && !requirement.Zone && !requirement.Region {
			reasons = append(reasons, termIdentity+" uses an unsupported topology key")
		}

		selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s has an invalid labelSelector: %v", termIdentity, err))
			continue
		}

		namespaces := append([]string(nil), term.Namespaces...)
		if len(namespaces) == 0 {
			namespaces = []string{pod.Namespace}
		} else {
			sort.Strings(namespaces)
			normalized := namespaces[:0]
			for _, namespace := range namespaces {
				if namespace == "" {
					reasons = append(reasons, termIdentity+" contains an empty namespace")
					continue
				}
				if len(normalized) == 0 || normalized[len(normalized)-1] != namespace {
					normalized = append(normalized, namespace)
				}
			}
			namespaces = normalized
		}

		terms = append(terms, nodeOptimizationRequiredPodAffinityTerm{
			TopologyKey:         term.TopologyKey,
			TopologyRequirement: requirement,
			Namespaces:          append([]string(nil), namespaces...),
			Selector:            selector,
			SortKey: fmt.Sprintf(
				"%s\x00%s\x00%s",
				term.TopologyKey,
				strings.Join(namespaces, "\x00"),
				selector.String(),
			),
		})
	}

	if len(reasons) > 0 {
		sort.Strings(reasons)
		return nil, reasons[0]
	}
	sort.Slice(terms, func(i, j int) bool {
		return terms[i].SortKey < terms[j].SortKey
	})
	return terms, ""
}

func buildNodeOptimizationInterPodAffinityState(
	nodes []NodeOptimizationSchedulingNode,
	eligiblePods []nodeOptimizationTopologyPod,
	movablePods []corev1.Pod,
) *nodeOptimizationInterPodAffinityState {
	movableKeys := make(map[string]struct{}, len(movablePods))
	for _, pod := range movablePods {
		movableKeys[namespacedKey(pod.Namespace, pod.Name)] = struct{}{}
	}

	state := &nodeOptimizationInterPodAffinityState{
		Nodes:       append([]NodeOptimizationSchedulingNode(nil), nodes...),
		MovablePods: make(map[string]nodeOptimizationTopologyPod, len(movablePods)),
	}
	for _, pod := range eligiblePods {
		key := namespacedKey(pod.Namespace, pod.Name)
		if _, movable := movableKeys[key]; movable {
			state.MovablePods[key] = pod
			continue
		}
		state.FixedPods = append(state.FixedPods, pod)
	}
	sort.Slice(state.FixedPods, func(i, j int) bool {
		return nodeOptimizationTopologyPodSortKey(state.FixedPods[i]) < nodeOptimizationTopologyPodSortKey(state.FixedPods[j])
	})
	return state
}

func nodeOptimizationTopologyPodSortKey(pod nodeOptimizationTopologyPod) string {
	return namespacedKey(pod.Namespace, pod.Name) + "\x00" + pod.NodeName
}

func requiredPodAffinityTermMatchesPod(term nodeOptimizationRequiredPodAffinityTerm, pod nodeOptimizationTopologyPod) bool {
	namespaceIndex := sort.SearchStrings(term.Namespaces, pod.Namespace)
	if namespaceIndex >= len(term.Namespaces) || term.Namespaces[namespaceIndex] != pod.Namespace {
		return false
	}
	return term.Selector.Matches(labels.Set(pod.Labels))
}

func hasHardTopologySpreadConstraints(pod corev1.Pod) bool {
	for _, constraint := range pod.Spec.TopologySpreadConstraints {
		if constraint.WhenUnsatisfiable == corev1.DoNotSchedule {
			return true
		}
	}
	return false
}

func hardTopologySpreadUnsupportedReason(pod corev1.Pod) string {
	reasons := make([]string, 0)
	for i, constraint := range pod.Spec.TopologySpreadConstraints {
		switch constraint.WhenUnsatisfiable {
		case corev1.ScheduleAnyway:
			continue
		case corev1.DoNotSchedule:
		default:
			reasons = append(reasons, fmt.Sprintf(
				"topology spread constraint %d has unsupported whenUnsatisfiable value %q",
				i,
				constraint.WhenUnsatisfiable,
			))
			continue
		}

		if constraint.MaxSkew <= 0 {
			reasons = append(reasons, fmt.Sprintf(
				"hard topology spread constraint %d has nonpositive maxSkew %d",
				i,
				constraint.MaxSkew,
			))
		}
		switch constraint.TopologyKey {
		case corev1.LabelHostname, corev1.LabelTopologyZone, corev1.LabelTopologyRegion:
		default:
			reasons = append(reasons, fmt.Sprintf(
				"hard topology spread constraint %d uses unsupported topologyKey %q",
				i,
				constraint.TopologyKey,
			))
		}
		if constraint.MinDomains != nil && *constraint.MinDomains <= 0 {
			reasons = append(reasons, fmt.Sprintf(
				"hard topology spread constraint %d has nonpositive minDomains %d",
				i,
				*constraint.MinDomains,
			))
		}
		if len(constraint.MatchLabelKeys) > 0 {
			reasons = append(reasons, fmt.Sprintf(
				"hard topology spread constraint %d uses matchLabelKeys, which is not modeled yet",
				i,
			))
		}
		if constraint.NodeAffinityPolicy != nil && *constraint.NodeAffinityPolicy != corev1.NodeInclusionPolicyHonor {
			reasons = append(reasons, fmt.Sprintf(
				"hard topology spread constraint %d uses nodeAffinityPolicy %q; only the default Honor policy is modeled",
				i,
				*constraint.NodeAffinityPolicy,
			))
		}
		if constraint.NodeTaintsPolicy != nil && *constraint.NodeTaintsPolicy != corev1.NodeInclusionPolicyIgnore {
			reasons = append(reasons, fmt.Sprintf(
				"hard topology spread constraint %d uses nodeTaintsPolicy %q; only the default Ignore policy is modeled",
				i,
				*constraint.NodeTaintsPolicy,
			))
		}
		if _, err := metav1.LabelSelectorAsSelector(constraint.LabelSelector); err != nil {
			reasons = append(reasons, fmt.Sprintf(
				"hard topology spread constraint %d has an invalid labelSelector: %v",
				i,
				err,
			))
		}
	}

	if len(reasons) == 0 {
		return ""
	}
	sort.Strings(reasons)
	return reasons[0]
}

func buildNodeOptimizationTopologySpreadConstraints(
	pod corev1.Pod,
	nodes []NodeOptimizationSchedulingNode,
	eligiblePods []nodeOptimizationTopologyPod,
	movablePods []corev1.Pod,
) ([]nodeOptimizationTopologySpreadConstraint, string) {
	constraints := make([]nodeOptimizationTopologySpreadConstraint, 0)
	for index, constraint := range pod.Spec.TopologySpreadConstraints {
		if constraint.WhenUnsatisfiable != corev1.DoNotSchedule {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(constraint.LabelSelector)
		if err != nil {
			return nil, fmt.Sprintf("hard topology spread constraint %d has an invalid labelSelector: %v", index, err)
		}
		minDomains := int32(1)
		if constraint.MinDomains != nil {
			minDomains = *constraint.MinDomains
		}

		domainNodes := make([]NodeOptimizationSchedulingNode, 0, len(nodes))
		for _, node := range nodes {
			if !nodeMatchesNodeSelector(node, pod.Spec.NodeSelector) {
				continue
			}
			if hasRequiredNodeAffinity(pod) {
				required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
				if !nodeMatchesRequiredNodeAffinity(node.Labels, required.NodeSelectorTerms) {
					continue
				}
			}
			domainNodes = append(domainNodes, node)
		}

		movableKeys := make(map[string]struct{}, len(movablePods))
		matchingMovableKeys := make(map[string]struct{})
		for _, movablePod := range movablePods {
			key := namespacedKey(movablePod.Namespace, movablePod.Name)
			movableKeys[key] = struct{}{}
			if movablePod.Namespace == pod.Namespace && selector.Matches(labels.Set(movablePod.Labels)) {
				matchingMovableKeys[key] = struct{}{}
			}
		}

		existingMatchingPodNodes := make([]string, 0)
		for _, existingPod := range eligiblePods {
			if existingPod.Namespace != pod.Namespace || !selector.Matches(labels.Set(existingPod.Labels)) {
				continue
			}
			if _, movable := movableKeys[namespacedKey(existingPod.Namespace, existingPod.Name)]; movable {
				continue
			}
			existingMatchingPodNodes = append(existingMatchingPodNodes, existingPod.NodeName)
		}
		sort.Strings(existingMatchingPodNodes)

		constraints = append(constraints, nodeOptimizationTopologySpreadConstraint{
			TopologyKey:              constraint.TopologyKey,
			MaxSkew:                  constraint.MaxSkew,
			MinDomains:               minDomains,
			SortKey:                  fmt.Sprintf("%s\x00%010d\x00%010d\x00%s", constraint.TopologyKey, constraint.MaxSkew, minDomains, selector.String()),
			DomainNodes:              append([]NodeOptimizationSchedulingNode(nil), domainNodes...),
			TopologyRequirement:      topologyRequirementForKey(constraint.TopologyKey),
			ExistingMatchingPodNodes: existingMatchingPodNodes,
			MatchingMovablePodKeys:   matchingMovableKeys,
		})
	}

	sort.Slice(constraints, func(i, j int) bool {
		return constraints[i].SortKey < constraints[j].SortKey
	})
	return constraints, ""
}

func topologyRequirementForKey(topologyKey string) nodeOptimizationTopologyRequirement {
	switch topologyKey {
	case corev1.LabelHostname:
		return nodeOptimizationTopologyRequirement{Hostname: true}
	case corev1.LabelTopologyZone:
		return nodeOptimizationTopologyRequirement{Zone: true}
	case corev1.LabelTopologyRegion:
		return nodeOptimizationTopologyRequirement{Region: true}
	default:
		return nodeOptimizationTopologyRequirement{}
	}
}

func topologyValueForKey(topology NodeOptimizationTopology, topologyKey string) string {
	switch topologyKey {
	case corev1.LabelHostname:
		return topology.Hostname
	case corev1.LabelTopologyZone:
		return topology.Zone
	case corev1.LabelTopologyRegion:
		return topology.Region
	default:
		return ""
	}
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

func unsupportedPodSchedulingConstraintReason(pod corev1.Pod, storageEvidenceAvailable bool) string {
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
	if reason := hardTopologySpreadUnsupportedReason(pod); reason != "" {
		return reason
	}
	if _, reason := buildNodeOptimizationRequiredAffinityTerms(pod); reason != "" {
		return reason
	}
	if _, reason := buildNodeOptimizationRequiredAntiAffinityTerms(pod); reason != "" {
		return reason
	}
	for _, container := range append(append([]corev1.Container(nil), pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, port := range container.Ports {
			if port.HostPort != 0 {
				return "host-port scheduling conflicts are not modeled yet"
			}
		}
	}
	return unsupportedPodStorageSchedulingReason(pod, storageEvidenceAvailable)
}

func unsupportedPodStorageSchedulingReason(pod corev1.Pod, storageEvidenceAvailable bool) string {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && !storageEvidenceAvailable {
			return "persistent volume topology is not modeled without PVC/PV storage evidence"
		}
		if volume.CSI != nil {
			return "inline CSI volume scheduling and topology are not modeled yet"
		}
		if volume.Ephemeral != nil {
			return "generic ephemeral volume scheduling and topology are not modeled yet"
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
