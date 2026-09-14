package analyzer

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ================================================================
// Types
// ================================================================

type NetworkPolicyAuditor struct {
	clientset      kubernetes.Interface
	ctx            context.Context
	skipNamespaces []string // user-provided additional namespaces to skip
}

type NetworkPolicyAudit struct {
	TotalNamespaces       int
	ProtectedNamespaces   []NamespaceNetworkStatus // every observed pod has configured ingress and egress coverage
	UnprotectedNamespaces []NamespaceNetworkStatus // at least one observed pod lacks full directional coverage
	TotalPolicies         int
	HighRiskNamespaces    int
	Warnings              []NetworkAuditWarning
}

// NetworkAuditWarning records a namespace-scoped audit failure (e.g. an API
// error listing NetworkPolicies or Pods) so it's visible instead of being
// silently skipped. A namespace with a warning may be entirely missing from
// both Protected/UnprotectedNamespaces — callers that need to know whether
// the audit is complete must check len(Warnings), not just the two slices.
type NetworkAuditWarning struct {
	Namespace string
	Operation string
	Message   string
}

type NamespaceNetworkStatus struct {
	Name        string
	Environment string
	PodCount    int
	PolicyCount int

	// CoveredPodCount/UncoveredPodCount: how many of this namespace's pods
	// are matched by at least one policy's PodSelector (empty selector
	// matches all pods). A namespace with any policy is not automatically
	// "protected" — coverage must be computed per pod.
	CoveredPodCount   int
	UncoveredPodCount int

	// FullyCoveredPodCount counts pods for which the same pod has both
	// ingress and egress coverage. CoverageGapPodCount is its complement;
	// unlike UncoveredPodCount, it also includes pods selected by a policy
	// that covers only one direction or explicitly allows all traffic.
	FullyCoveredPodCount int
	CoverageGapPodCount  int

	// IngressCoveredPods/EgressCoveredPods: of the covered pods, how many
	// are covered by a policy that actually restricts that direction
	// (PolicyTypes includes it) AND that direction isn't rendered moot by
	// an allow-all rule in any policy covering the pod (NetworkPolicies
	// are additive — one allow-all policy permits everything regardless
	// of how restrictive other policies covering the same pods are).
	IngressCoveredPods int
	EgressCoveredPods  int

	// HasDefaultDenyIngress/HasDefaultDenyEgress: true only when a policy
	// with an empty PodSelector (selects all pods) declares that
	// PolicyType with zero rules for it — the actual Kubernetes
	// default-deny-all convention, evaluated per direction. An
	// egress-only default-deny policy sets HasDefaultDenyEgress without
	// implying anything about ingress.
	HasDefaultDenyIngress bool
	HasDefaultDenyEgress  bool

	// HasIngressRestriction/HasEgressRestriction describe namespace-wide
	// configured coverage: every observed pod must be covered in that
	// direction, after accounting for additive allow-all policies.
	HasIngressRestriction bool
	HasEgressRestriction  bool

	Policies   []PolicyDetail
	RiskLevel  string // HIGH, MEDIUM, LOW
	RiskReason string
}

type PolicyDetail struct {
	Name             string
	Types            []string
	IngressRules     int
	EgressRules      int
	IsDefaultDeny    bool // true only if this policy alone is a full deny-all (podSelector empty, that direction declared, zero rules)
	AllowsAllIngress bool // podSelector empty/matches-all AND an ingress rule with no From/Ports restriction
	AllowsAllEgress  bool
}

// ================================================================
// Constructor
// ================================================================

func NewNetworkPolicyAuditor(clientset kubernetes.Interface) *NetworkPolicyAuditor {
	return &NetworkPolicyAuditor{
		clientset:      clientset,
		ctx:            context.Background(),
		skipNamespaces: []string{},
	}
}

func (n *NetworkPolicyAuditor) WithSkipNamespaces(namespaces []string) *NetworkPolicyAuditor {
	n.skipNamespaces = namespaces
	return n
}

// ================================================================
// Main Audit
// ================================================================

func (n *NetworkPolicyAuditor) AuditNetworkPolicies(filterNamespace string) (*NetworkPolicyAudit, error) {
	return n.auditNetworkPolicies(filterNamespace, nil, true)
}

// AuditNetworkPoliciesWithPods performs the same audit using a previously
// successful cluster-wide Pod snapshot. NetworkPolicies remain fetched once
// per eligible namespace so their existing failure behavior is unchanged.
func (n *NetworkPolicyAuditor) AuditNetworkPoliciesWithPods(filterNamespace string, pods []corev1.Pod) (*NetworkPolicyAudit, error) {
	podsByNamespace := make(map[string][]corev1.Pod)
	for _, pod := range pods {
		podsByNamespace[pod.Namespace] = append(podsByNamespace[pod.Namespace], pod)
	}
	return n.auditNetworkPolicies(filterNamespace, podsByNamespace, true)
}

// A nil podsByNamespace retains the original behavior of listing Pods in
// each namespace. A non-nil map, including an empty map, is a supplied
// snapshot and therefore performs no Pod LIST calls.
// When usePolicySnapshot is true, a successful cluster-wide NetworkPolicy
// LIST supplies every namespace. If it fails, the loop retains the original
// namespace-scoped retrieval and warning behavior.
func (n *NetworkPolicyAuditor) auditNetworkPolicies(filterNamespace string, podsByNamespace map[string][]corev1.Pod, usePolicySnapshot bool) (*NetworkPolicyAudit, error) {
	audit := &NetworkPolicyAudit{}

	// Get namespaces
	nsList, err := n.clientset.CoreV1().Namespaces().List(n.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}

	var policiesByNamespace map[string][]networkingv1.NetworkPolicy
	if usePolicySnapshot {
		if policies, listErr := n.clientset.NetworkingV1().NetworkPolicies("").List(n.ctx, metav1.ListOptions{}); listErr == nil {
			policiesByNamespace = make(map[string][]networkingv1.NetworkPolicy)
			for _, policy := range policies.Items {
				policiesByNamespace[policy.Namespace] = append(policiesByNamespace[policy.Namespace], policy)
			}
		}
	}

	for _, ns := range nsList.Items {
		nsName := ns.Name

		// Skip system/infrastructure namespaces using 3 strategies:
		// 1. Kubernetes official label: kubernetes.io/metadata.name on system namespaces
		// 2. Well-known name patterns for infrastructure components
		// 3. User-provided skip list via --skip-namespaces flag
		if shouldSkipNamespace(nsName, ns.Labels, n.skipNamespaces) {
			continue
		}

		// Apply namespace filter if specified
		if filterNamespace != "" && filterNamespace != nsName {
			continue
		}
		// Include namespaces whose API requests later fail: they remain in
		// scope even though their policy coverage cannot be established.
		audit.TotalNamespaces++

		// Get pods — needed for real coverage (which pods each policy
		// selector actually matches), not just a count.
		var namespacePods []corev1.Pod
		if podsByNamespace == nil {
			pods, err := n.clientset.CoreV1().Pods(nsName).List(n.ctx, metav1.ListOptions{})
			if err != nil {
				audit.Warnings = append(audit.Warnings, NetworkAuditWarning{
					Namespace: nsName, Operation: "list Pods", Message: err.Error(),
				})
				continue // no pod data means coverage can't be computed at all for this namespace
			}
			namespacePods = pods.Items
		} else {
			namespacePods = podsByNamespace[nsName]
		}

		// Get NetworkPolicies in this namespace
		var namespacePolicies []networkingv1.NetworkPolicy
		if policiesByNamespace == nil {
			policies, err := n.clientset.NetworkingV1().NetworkPolicies(nsName).List(n.ctx, metav1.ListOptions{})
			if err != nil {
				audit.Warnings = append(audit.Warnings, NetworkAuditWarning{
					Namespace: nsName, Operation: "list NetworkPolicies", Message: err.Error(),
				})
				continue
			}
			namespacePolicies = policies.Items
		} else {
			namespacePolicies = policiesByNamespace[nsName]
		}

		env := detectEnvironment(nsName)
		status := NamespaceNetworkStatus{
			Name:                nsName,
			Environment:         env,
			PodCount:            len(namespacePods),
			PolicyCount:         len(namespacePolicies),
			UncoveredPodCount:   len(namespacePods),
			CoverageGapPodCount: len(namespacePods),
		}

		if len(namespacePolicies) > 0 {
			audit.TotalPolicies += len(namespacePolicies)
			analyzeCoverage(&status, namespacePods, namespacePolicies)
		}
		analyzeRisk(&status)
		if status.RiskLevel == "HIGH" {
			audit.HighRiskNamespaces++
		}

		// A namespace with no pods has nothing to protect — vacuously
		// protected, not a finding. Otherwise, require full coverage in
		// both directions across every pod actually present.
		if status.PodCount == 0 || status.FullyCoveredPodCount == status.PodCount {
			audit.ProtectedNamespaces = append(audit.ProtectedNamespaces, status)
		} else {
			audit.UnprotectedNamespaces = append(audit.UnprotectedNamespaces, status)
		}
	}

	// Sort unprotected by risk: HIGH first, then by pod count
	sort.Slice(audit.UnprotectedNamespaces, func(i, j int) bool {
		ri, rj := riskScore(audit.UnprotectedNamespaces[i].RiskLevel),
			riskScore(audit.UnprotectedNamespaces[j].RiskLevel)
		if ri != rj {
			return ri > rj
		}
		return audit.UnprotectedNamespaces[i].PodCount > audit.UnprotectedNamespaces[j].PodCount
	})

	return audit, nil
}

// AnalyzeNetworkPolicies is auditNetworkPolicies' Kubernetes-free
// counterpart (docs/08 Phase 4D.3): the same coverage/risk analysis, from
// already-observed namespaces/pods/policies instead of the auditor's own
// LIST calls. It has no Kubernetes client, no context dependency, no
// persistence, and no presentation work, and is deterministic: the same
// inputs always produce the same *NetworkPolicyAudit.
//
// It is intentionally a free function, not a method — nothing here needs
// NetworkPolicyAuditor's clientset/ctx, and skipNamespaces is threaded
// through explicitly instead of read from auditor state, so this has no
// hidden dependency on a particular auditor instance.
//
// auditNetworkPolicies is NOT rewritten to call this: its own per-namespace
// LIST-with-fallback-to-warning behavior (see its doc comment) has no
// equivalent here, and forcing the two together would mean choosing
// between reimplementing that fallback in terms of pre-resolved slices (it
// cannot be, since a slice carries no "this failed" signal) or silently
// dropping it. The two now share only their pure analysis primitives
// (shouldSkipNamespace, analyzeCoverage, analyzeRisk, riskScore,
// detectEnvironment) — the actual algorithm — while each keeps its own
// acquisition-shaped orchestration loop. See docs/08 Phase 4D.3's
// legacy-coexistence decision for why auditNetworkPolicies itself is left
// running unchanged.
//
// Every input here is assumed to be a single trustworthy, already-successful
// snapshot (the coordinator refuses to call this at all against an
// untrustworthy ClusterSnapshot — see network_runtime.go), so there is no
// analogous failure mode to auditNetworkPolicies' per-namespace API errors:
// the returned audit's Warnings is always empty. A namespace with zero
// NetworkPolicy objects is recorded as PolicyCount == 0 — a real,
// successful observation of absence — and is never conflated with "failed
// to retrieve policies," which in the legacy path is represented only by a
// Warnings entry, never by PolicyCount.
func AnalyzeNetworkPolicies(
	namespaces []corev1.Namespace,
	pods []corev1.Pod,
	policies []networkingv1.NetworkPolicy,
	filterNamespace string,
	skipNamespaces []string,
) *NetworkPolicyAudit {
	audit := &NetworkPolicyAudit{}

	podsByNamespace := make(map[string][]corev1.Pod, len(namespaces))
	for _, pod := range pods {
		podsByNamespace[pod.Namespace] = append(podsByNamespace[pod.Namespace], pod)
	}
	policiesByNamespace := make(map[string][]networkingv1.NetworkPolicy, len(namespaces))
	for _, policy := range policies {
		policiesByNamespace[policy.Namespace] = append(policiesByNamespace[policy.Namespace], policy)
	}

	for _, ns := range namespaces {
		nsName := ns.Name

		if shouldSkipNamespace(nsName, ns.Labels, skipNamespaces) {
			continue
		}
		if filterNamespace != "" && filterNamespace != nsName {
			continue
		}
		audit.TotalNamespaces++

		namespacePods := podsByNamespace[nsName]
		namespacePolicies := policiesByNamespace[nsName]

		env := detectEnvironment(nsName)
		status := NamespaceNetworkStatus{
			Name:                nsName,
			Environment:         env,
			PodCount:            len(namespacePods),
			PolicyCount:         len(namespacePolicies),
			UncoveredPodCount:   len(namespacePods),
			CoverageGapPodCount: len(namespacePods),
		}

		if len(namespacePolicies) > 0 {
			audit.TotalPolicies += len(namespacePolicies)
			analyzeCoverage(&status, namespacePods, namespacePolicies)
		}
		analyzeRisk(&status)
		if status.RiskLevel == "HIGH" {
			audit.HighRiskNamespaces++
		}

		if status.PodCount == 0 || status.FullyCoveredPodCount == status.PodCount {
			audit.ProtectedNamespaces = append(audit.ProtectedNamespaces, status)
		} else {
			audit.UnprotectedNamespaces = append(audit.UnprotectedNamespaces, status)
		}
	}

	sort.Slice(audit.UnprotectedNamespaces, func(i, j int) bool {
		ri, rj := riskScore(audit.UnprotectedNamespaces[i].RiskLevel),
			riskScore(audit.UnprotectedNamespaces[j].RiskLevel)
		if ri != rj {
			return ri > rj
		}
		return audit.UnprotectedNamespaces[i].PodCount > audit.UnprotectedNamespaces[j].PodCount
	})

	return audit
}

// podSelectorMatches reports whether a policy's PodSelector matches a pod's
// labels. An empty selector (no MatchLabels, no MatchExpressions) matches
// every pod in the namespace — standard NetworkPolicy semantics.
func podSelectorMatches(selector metav1.LabelSelector, podLabels map[string]string) bool {
	if len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0 {
		return true
	}
	for k, v := range selector.MatchLabels {
		if podLabels[k] != v {
			return false
		}
	}
	for _, expr := range selector.MatchExpressions {
		val, has := podLabels[expr.Key]
		switch expr.Operator {
		case metav1.LabelSelectorOpIn:
			if !has || !containsStr(expr.Values, val) {
				return false
			}
		case metav1.LabelSelectorOpNotIn:
			if has && containsStr(expr.Values, val) {
				return false
			}
		case metav1.LabelSelectorOpExists:
			if !has {
				return false
			}
		case metav1.LabelSelectorOpDoesNotExist:
			if has {
				return false
			}
		}
	}
	return true
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// effectivePolicyTypes returns which directions a policy actually governs,
// applying Kubernetes' documented default when PolicyTypes is omitted:
// Ingress always applies; Egress applies only if the policy has at least
// one Egress rule. An explicit PolicyTypes list is used exactly as given.
func effectivePolicyTypes(policy networkingv1.NetworkPolicy) (ingress, egress bool) {
	if len(policy.Spec.PolicyTypes) > 0 {
		for _, t := range policy.Spec.PolicyTypes {
			if t == networkingv1.PolicyTypeIngress {
				ingress = true
			}
			if t == networkingv1.PolicyTypeEgress {
				egress = true
			}
		}
		return ingress, egress
	}
	return true, len(policy.Spec.Egress) > 0
}

// policyAllowsAllIngress reports whether the policy contains an ingress
// rule with no From and no Ports restriction — the standard "allow all
// ingress" pattern (policyTypes: [Ingress]; ingress: [{}]). NetworkPolicies
// are additive: if ANY policy selecting a pod allows all ingress, that
// pod's ingress is unrestricted regardless of other, more restrictive
// policies also selecting it.
func policyAllowsAllIngress(policy networkingv1.NetworkPolicy) bool {
	for _, rule := range policy.Spec.Ingress {
		if len(rule.From) == 0 && len(rule.Ports) == 0 {
			return true
		}
	}
	return false
}

func policyAllowsAllEgress(policy networkingv1.NetworkPolicy) bool {
	for _, rule := range policy.Spec.Egress {
		if len(rule.To) == 0 && len(rule.Ports) == 0 {
			return true
		}
	}
	return false
}

// policyIsDefaultDenyIngress reports whether this policy alone establishes
// a namespace-wide (or selector-wide) default-deny for ingress: empty
// PodSelector (selects all pods it applies to), Ingress declared via
// effective PolicyTypes, and zero ingress rules (nothing allowed).
func policyIsDefaultDenyIngress(policy networkingv1.NetworkPolicy) bool {
	ingress, _ := effectivePolicyTypes(policy)
	return ingress &&
		len(policy.Spec.PodSelector.MatchLabels) == 0 &&
		len(policy.Spec.PodSelector.MatchExpressions) == 0 &&
		len(policy.Spec.Ingress) == 0
}

func policyIsDefaultDenyEgress(policy networkingv1.NetworkPolicy) bool {
	_, egress := effectivePolicyTypes(policy)
	return egress &&
		len(policy.Spec.PodSelector.MatchLabels) == 0 &&
		len(policy.Spec.PodSelector.MatchExpressions) == 0 &&
		len(policy.Spec.Egress) == 0
}

// analyzeCoverage computes real per-pod, per-direction coverage: which
// pods are matched by at least one policy's selector, and — accounting for
// NetworkPolicies being additive across all policies matching a pod —
// whether that pod's ingress/egress is actually restricted or effectively
// open due to an allow-all rule in any one matching policy.
func analyzeCoverage(status *NamespaceNetworkStatus, pods []corev1.Pod, policies []networkingv1.NetworkPolicy) {
	for _, policy := range policies {
		detail := PolicyDetail{Name: policy.Name, Types: []string{}}
		ingress, egress := effectivePolicyTypes(policy)
		if ingress {
			detail.Types = append(detail.Types, "Ingress")
			detail.IngressRules = len(policy.Spec.Ingress)
		}
		if egress {
			detail.Types = append(detail.Types, "Egress")
			detail.EgressRules = len(policy.Spec.Egress)
		}
		detail.AllowsAllIngress = ingress && policyAllowsAllIngress(policy)
		detail.AllowsAllEgress = egress && policyAllowsAllEgress(policy)
		if policyIsDefaultDenyIngress(policy) {
			detail.IsDefaultDeny = true
			status.HasDefaultDenyIngress = true
		}
		if policyIsDefaultDenyEgress(policy) {
			detail.IsDefaultDeny = true
			status.HasDefaultDenyEgress = true
		}
		status.Policies = append(status.Policies, detail)
	}

	for _, pod := range pods {
		var matching []networkingv1.NetworkPolicy
		for _, policy := range policies {
			if podSelectorMatches(policy.Spec.PodSelector, pod.Labels) {
				matching = append(matching, policy)
			}
		}
		if len(matching) == 0 {
			continue
		}
		status.CoveredPodCount++

		ingressRestricted, egressRestricted := false, false
		ingressAllowedAll, egressAllowedAll := false, false
		for _, policy := range matching {
			ing, egr := effectivePolicyTypes(policy)
			if ing {
				ingressRestricted = true
				if policyAllowsAllIngress(policy) {
					ingressAllowedAll = true
				}
			}
			if egr {
				egressRestricted = true
				if policyAllowsAllEgress(policy) {
					egressAllowedAll = true
				}
			}
		}
		// Additive semantics: one allow-all policy makes that direction
		// unrestricted for this pod regardless of other, more restrictive
		// policies also selecting it.
		ingressCovered := ingressRestricted && !ingressAllowedAll
		egressCovered := egressRestricted && !egressAllowedAll
		if ingressCovered {
			status.IngressCoveredPods++
		}
		if egressCovered {
			status.EgressCoveredPods++
		}
		if ingressCovered && egressCovered {
			status.FullyCoveredPodCount++
		}
	}
	status.UncoveredPodCount = status.PodCount - status.CoveredPodCount
	status.CoverageGapPodCount = status.PodCount - status.FullyCoveredPodCount
	status.HasIngressRestriction = status.PodCount > 0 && status.IngressCoveredPods == status.PodCount
	status.HasEgressRestriction = status.PodCount > 0 && status.EgressCoveredPods == status.PodCount
}

func analyzeRisk(status *NamespaceNetworkStatus) {
	// Fully protected (or empty) namespaces are not a risk finding.
	if status.PodCount == 0 || status.FullyCoveredPodCount == status.PodCount {
		return
	}

	env := status.Environment
	var coverageClause string
	if status.PolicyCount == 0 {
		coverageClause = "no NetworkPolicy was observed"
	} else if status.CoveredPodCount == 0 {
		coverageClause = "no observed pods are selected by the existing NetworkPolicies"
	} else {
		coverageClause = fmt.Sprintf(
			"%d of %d observed pods lack configured ingress and egress coverage",
			status.CoverageGapPodCount, status.PodCount,
		)
	}

	switch env {
	case "PRODUCTION":
		status.RiskLevel = "HIGH"
		status.RiskReason = "Production namespace: " + coverageClause
	case "STAGING":
		status.RiskLevel = "HIGH"
		status.RiskReason = "Staging namespace: " + coverageClause
	case "SYSTEM":
		status.RiskLevel = "HIGH"
		status.RiskReason = "System namespace: " + coverageClause
	default:
		if status.PodCount > 10 {
			status.RiskLevel = "MEDIUM"
			status.RiskReason = "Development namespace: " + coverageClause
		} else {
			status.RiskLevel = "LOW"
			status.RiskReason = "Development/test namespace: " + coverageClause
		}
	}
}

// shouldSkipNamespace returns true if namespace should be excluded from analysis.
// Uses 3 strategies so it works across any Kubernetes distribution (AKS, EKS, GKE, k3s, etc.)
func shouldSkipNamespace(name string, labels map[string]string, skipNamespaces []string) bool {
	// Strategy 1: User-provided skip list (highest priority)
	for _, skip := range skipNamespaces {
		if skip == name {
			return true
		}
	}

	// Strategy 2: Well-known infrastructure namespace patterns
	// Covers: kube-system, kube-public, kube-node-lease, istio-system,
	// calico-system, calico-apiserver, tigera-operator, cert-manager,
	// ingress-nginx, flux-system, argocd, velero, longhorn-system,
	// cattle-system (Rancher), openshift-* etc.
	infraPatterns := []string{
		"kube-",         // kube-system, kube-public, kube-node-lease
		"istio-",        // istio-system, istio-ingress
		"calico-",       // calico-system, calico-apiserver
		"tigera-",       // tigera-operator
		"cert-manager",  // cert-manager
		"ingress-nginx", // ingress-nginx
		"flux-system",   // flux-system
		"argocd",        // argocd
		"velero",        // velero
		"longhorn-",     // longhorn-system
		"cattle-",       // cattle-system (Rancher)
		"openshift-",    // openshift-* namespaces
		"gke-",          // GKE system namespaces
		"azure-",        // AKS system namespaces
		"karpenter",     // karpenter
		"crossplane-",   // crossplane-system
	}

	for _, pattern := range infraPatterns {
		if strings.HasPrefix(name, pattern) {
			return true
		}
	}

	// Strategy 3: Kubernetes official label for system namespaces
	// kubernetes.io/metadata.name is set on all namespaces but
	// pod-security.kubernetes.io/enforce=privileged marks system namespaces
	if val, ok := labels["pod-security.kubernetes.io/enforce"]; ok && val == "privileged" {
		// Only skip if it also looks like an infrastructure namespace
		// (don't skip user namespaces that happen to use privileged PSA)
		if _, ok := labels["app.kubernetes.io/managed-by"]; !ok {
			return true
		}
	}

	return false
}

func podLabel(count int) string {
	if count == 1 {
		return "1 pod"
	}
	return fmt.Sprintf("%d pods", count)
}

func riskScore(level string) int {
	switch level {
	case "HIGH":
		return 3
	case "MEDIUM":
		return 2
	default:
		return 1
	}
}
