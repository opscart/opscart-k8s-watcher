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
	return n.auditNetworkPolicies(filterNamespace, nil)
}

// AuditNetworkPoliciesWithPods performs the same audit using a previously
// successful cluster-wide Pod snapshot. NetworkPolicies remain fetched once
// per eligible namespace so their existing failure behavior is unchanged.
func (n *NetworkPolicyAuditor) AuditNetworkPoliciesWithPods(filterNamespace string, pods []corev1.Pod) (*NetworkPolicyAudit, error) {
	podsByNamespace := make(map[string][]corev1.Pod)
	for _, pod := range pods {
		podsByNamespace[pod.Namespace] = append(podsByNamespace[pod.Namespace], pod)
	}
	return n.auditNetworkPolicies(filterNamespace, podsByNamespace)
}

// A nil podsByNamespace retains the original behavior of listing Pods in
// each namespace. A non-nil map, including an empty map, is a supplied
// snapshot and therefore performs no Pod LIST calls.
func (n *NetworkPolicyAuditor) auditNetworkPolicies(filterNamespace string, podsByNamespace map[string][]corev1.Pod) (*NetworkPolicyAudit, error) {
	audit := &NetworkPolicyAudit{}

	// Get namespaces
	nsList, err := n.clientset.CoreV1().Namespaces().List(n.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}

	for _, ns := range nsList.Items {
		nsName := ns.Name

		// Skip system/infrastructure namespaces using 3 strategies:
		// 1. Kubernetes official label: kubernetes.io/metadata.name on system namespaces
		// 2. Well-known name patterns for infrastructure components
		// 3. User-provided skip list via --skip-namespaces flag
		if n.shouldSkipNamespace(nsName, ns.Labels) {
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
		policies, err := n.clientset.NetworkingV1().NetworkPolicies(nsName).List(n.ctx, metav1.ListOptions{})
		if err != nil {
			audit.Warnings = append(audit.Warnings, NetworkAuditWarning{
				Namespace: nsName, Operation: "list NetworkPolicies", Message: err.Error(),
			})
			continue
		}

		env := detectEnvironment(nsName)
		status := NamespaceNetworkStatus{
			Name:                nsName,
			Environment:         env,
			PodCount:            len(namespacePods),
			PolicyCount:         len(policies.Items),
			UncoveredPodCount:   len(namespacePods),
			CoverageGapPodCount: len(namespacePods),
		}

		if len(policies.Items) > 0 {
			audit.TotalPolicies += len(policies.Items)
			n.analyzeCoverage(&status, namespacePods, policies.Items)
		}
		n.analyzeRisk(&status)
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
func (n *NetworkPolicyAuditor) analyzeCoverage(status *NamespaceNetworkStatus, pods []corev1.Pod, policies []networkingv1.NetworkPolicy) {
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

func (n *NetworkPolicyAuditor) analyzeRisk(status *NamespaceNetworkStatus) {
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
func (n *NetworkPolicyAuditor) shouldSkipNamespace(name string, labels map[string]string) bool {
	// Strategy 1: User-provided skip list (highest priority)
	for _, skip := range n.skipNamespaces {
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

// ================================================================
// Print Functions
// ================================================================

func PrintNetworkPolicyAudit(audit *NetworkPolicyAudit) {
	// Header
	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║              NETWORK POLICY ANALYSIS                      ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Println("║  • Shows NetworkPolicy coverage across all namespaces      ║")
	fmt.Println("║  • Reports observed pod selectors and policy directions    ║")
	fmt.Println("║  • Use with kube-bench for full network security audit     ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Summary
	protected := len(audit.ProtectedNamespaces)
	unprotected := len(audit.UnprotectedNamespaces)
	total := audit.TotalNamespaces
	if total == 0 {
		total = protected + unprotected + len(audit.Warnings)
	}

	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("NETWORK POLICY SUMMARY")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("Total Namespaces:         %d\n", total)
	fmt.Printf("Protected (full coverage): %d\n", protected)
	fmt.Printf("Unprotected (gap found):  %d\n", unprotected)
	fmt.Printf("Total NetworkPolicies:    %d\n", audit.TotalPolicies)
	fmt.Printf("High Risk Namespaces:     %d\n", audit.HighRiskNamespaces)
	if len(audit.Warnings) > 0 {
		fmt.Printf("Audit Warnings:           %d namespace(s) could not be fully checked\n", len(audit.Warnings))
	}
	fmt.Println()

	// Coverage bar
	if total > 0 {
		pct := (protected * 100) / total
		printCoverageBar(pct)
	}

	// Protected namespaces
	if len(audit.ProtectedNamespaces) > 0 {
		fmt.Println("\n🟢 PROTECTED NAMESPACES:")
		fmt.Println("───────────────────────────────────────────────────────────")
		for _, ns := range audit.ProtectedNamespaces {
			printProtectedNamespace(ns)
		}
	}

	// Unprotected namespaces
	if len(audit.UnprotectedNamespaces) > 0 {
		fmt.Println("\n🔴 UNPROTECTED NAMESPACES (sorted by risk):")
		fmt.Println("───────────────────────────────────────────────────────────")
		for _, ns := range audit.UnprotectedNamespaces {
			printUnprotectedNamespace(ns)
		}
	}

	// Recommendations
	printNetworkRecommendations(audit)
}

func printCoverageBar(pct int) {
	barWidth := 40
	filled := (pct * barWidth) / 100
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	status := "🔴 Poor"
	if pct >= 80 {
		status = "🟢 Good"
	} else if pct >= 50 {
		status = "🟡 Partial"
	}

	fmt.Printf("Coverage: [%s] %d%% %s\n", bar, pct, status)
	fmt.Println()
}

func printProtectedNamespace(ns NamespaceNetworkStatus) {
	envLabel := envEmoji(ns.Environment)
	fmt.Printf("  ✅ %s %s (%s, %d policies)\n", envLabel, ns.Name, podLabel(ns.PodCount), ns.PolicyCount)

	ingressStatus := "❌ None"
	if ns.HasIngressRestriction {
		ingressStatus = "✅ Restricted"
	}
	egressStatus := "❌ None"
	if ns.HasEgressRestriction {
		egressStatus = "✅ Restricted"
	}
	defaultDeny := ""
	if ns.HasDefaultDenyIngress && ns.HasDefaultDenyEgress {
		defaultDeny = " | Default-Deny: ✅ (ingress+egress)"
	} else if ns.HasDefaultDenyIngress {
		defaultDeny = " | Default-Deny: ✅ (ingress only)"
	} else if ns.HasDefaultDenyEgress {
		defaultDeny = " | Default-Deny: ✅ (egress only)"
	}

	fmt.Printf("     Ingress: %s | Egress: %s%s\n", ingressStatus, egressStatus, defaultDeny)

	for _, p := range ns.Policies {
		types := strings.Join(p.Types, "+")
		if types == "" {
			types = "Ingress"
		}
		denyNote := ""
		if p.IsDefaultDeny {
			denyNote = " [default-deny]"
		}
		fmt.Printf("     Policy: %s (%s)%s\n", p.Name, types, denyNote)
	}
	fmt.Println()
}

func printUnprotectedNamespace(ns NamespaceNetworkStatus) {
	riskEmoji := "🟢"
	if ns.RiskLevel == "HIGH" {
		riskEmoji = "🔴"
	} else if ns.RiskLevel == "MEDIUM" {
		riskEmoji = "🟡"
	}

	envLabel := envEmoji(ns.Environment)
	fmt.Printf("  %s %s %s (%s) - %s RISK\n", riskEmoji, envLabel, ns.Name, podLabel(ns.PodCount), ns.RiskLevel)
	if ns.PolicyCount > 0 {
		fmt.Printf("     📊 %d of %d pods have full ingress+egress coverage (%d policies present)\n",
			ns.FullyCoveredPodCount, ns.PodCount, ns.PolicyCount)
	}
	fmt.Printf("     ⚠️  %s\n", ns.RiskReason)
	fmt.Println()
}

func printNetworkRecommendations(audit *NetworkPolicyAudit) {
	if len(audit.UnprotectedNamespaces) == 0 {
		if len(audit.Warnings) > 0 {
			printNetworkAuditWarnings(audit.Warnings)
			return
		}
		fmt.Println("\n✅ All observed pods have full configured NetworkPolicy coverage.")
		return
	}

	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("💡 RECOMMENDATIONS")
	fmt.Println("═══════════════════════════════════════════════════════════")

	// Count high risk
	highRisk := []NamespaceNetworkStatus{}
	for _, ns := range audit.UnprotectedNamespaces {
		if ns.RiskLevel == "HIGH" {
			highRisk = append(highRisk, ns)
		}
	}

	if len(highRisk) > 0 {
		fmt.Printf("\n🔴 IMMEDIATE ACTION - %d high-risk namespaces:\n", len(highRisk))
		for i, ns := range highRisk {
			if ns.PolicyCount == 0 {
				fmt.Printf("  %d. Add NetworkPolicy to '%s' (%s, %s)\n", i+1, ns.Name, ns.Environment, podLabel(ns.PodCount))
			} else {
				fmt.Printf("  %d. Review NetworkPolicy selectors and directions in '%s' (%s, %s)\n", i+1, ns.Name, ns.Environment, podLabel(ns.PodCount))
			}
		}
	}

	fmt.Println("\n📋 QUICK START - Default deny policy template:")
	fmt.Println()
	fmt.Println("  cat <<EOF | kubectl apply -f -")
	fmt.Println("  apiVersion: networking.k8s.io/v1")
	fmt.Println("  kind: NetworkPolicy")
	fmt.Println("  metadata:")
	fmt.Println("    name: default-deny-all")
	fmt.Println("    namespace: YOUR_NAMESPACE")
	fmt.Println("  spec:")
	fmt.Println("    podSelector: {}   # Applies to all pods")
	fmt.Println("    policyTypes:")
	fmt.Println("    - Ingress")
	fmt.Println("    - Egress")
	fmt.Println("  EOF")
	fmt.Println()
	fmt.Println("  ⚠️  Apply default-deny CAREFULLY - test in staging first!")
	fmt.Println("  📚 Full guide: https://kubernetes.io/docs/concepts/services-networking/network-policies/")

	if len(audit.ProtectedNamespaces) > 0 && !allHaveDefaultDeny(audit.ProtectedNamespaces) {
		fmt.Println("\n🟡 ENHANCEMENT - Protected namespaces missing default-deny:")
		for _, ns := range audit.ProtectedNamespaces {
			if !(ns.HasDefaultDenyIngress && ns.HasDefaultDenyEgress) {
				fmt.Printf("  • %s: Has policies but no default-deny rule\n", ns.Name)
			}
		}
	}

	printNetworkAuditWarnings(audit.Warnings)
}

func printNetworkAuditWarnings(warnings []NetworkAuditWarning) {
	if len(warnings) == 0 {
		return
	}
	fmt.Println("\n⚠️  AUDIT INCOMPLETE - the following namespaces could not be checked:")
	for _, warning := range warnings {
		fmt.Printf("  • %s (%s): %s\n", warning.Namespace, warning.Operation, warning.Message)
	}
}

func allHaveDefaultDeny(namespaces []NamespaceNetworkStatus) bool {
	for _, ns := range namespaces {
		if !(ns.HasDefaultDenyIngress && ns.HasDefaultDenyEgress) {
			return false
		}
	}
	return true
}

func envEmoji(env string) string {
	switch env {
	case "PRODUCTION":
		return "[PROD]"
	case "STAGING":
		return "[STAGE]"
	case "SYSTEM":
		return "[SYS]"
	default:
		return "[DEV]"
	}
}
