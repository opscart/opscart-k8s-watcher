package analyzer

import (
	"context"
	"fmt"
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ================================================================
// Types
// ================================================================

type NetworkPolicyAuditor struct {
	clientset      *kubernetes.Clientset
	ctx            context.Context
	skipNamespaces []string // user-provided additional namespaces to skip
}

type NetworkPolicyAudit struct {
	TotalNamespaces       int
	ProtectedNamespaces   []NamespaceNetworkStatus
	UnprotectedNamespaces []NamespaceNetworkStatus
	TotalPolicies         int
	HighRiskNamespaces    int
}

type NamespaceNetworkStatus struct {
	Name                  string
	Environment           string
	PodCount              int
	PolicyCount           int
	HasDefaultDeny        bool
	HasIngressRestriction bool
	HasEgressRestriction  bool
	Policies              []PolicyDetail
	RiskLevel             string // HIGH, MEDIUM, LOW
	RiskReason            string
}

type PolicyDetail struct {
	Name          string
	Types         []string
	IngressRules  int
	EgressRules   int
	IsDefaultDeny bool
}

// ================================================================
// Constructor
// ================================================================

func NewNetworkPolicyAuditor(clientset *kubernetes.Clientset) *NetworkPolicyAuditor {
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
	audit := &NetworkPolicyAudit{}

	// Get namespaces
	nsList, err := n.clientset.CoreV1().Namespaces().List(n.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}

	audit.TotalNamespaces = len(nsList.Items)

	for _, ns := range nsList.Items {
		nsName := ns.Name

		// Skip system/infrastructure namespaces using 3 strategies:
		// 1. Kubernetes official label: kubernetes.io/metadata.name on system namespaces
		// 2. Well-known name patterns for infrastructure components
		// 3. User-provided skip list via --skip-namespaces flag
		if n.shouldSkipNamespace(nsName, ns.Labels) {
			audit.TotalNamespaces--
			continue
		}

		// Apply namespace filter if specified
		if filterNamespace != "" && filterNamespace != nsName {
			continue
		}

		// Get pod count
		pods, _ := n.clientset.CoreV1().Pods(nsName).List(n.ctx, metav1.ListOptions{})
		podCount := 0
		if pods != nil {
			podCount = len(pods.Items)
		}

		// Get NetworkPolicies in this namespace
		policies, err := n.clientset.NetworkingV1().NetworkPolicies(nsName).List(n.ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}

		env := detectEnvironment(nsName)
		status := NamespaceNetworkStatus{
			Name:        nsName,
			Environment: env,
			PodCount:    podCount,
			PolicyCount: len(policies.Items),
		}

		if len(policies.Items) > 0 {
			audit.TotalPolicies += len(policies.Items)
			n.analyzeProtected(&status, policies.Items)
			audit.ProtectedNamespaces = append(audit.ProtectedNamespaces, status)
		} else {
			n.analyzeRisk(&status)
			if status.RiskLevel == "HIGH" {
				audit.HighRiskNamespaces++
			}
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

func (n *NetworkPolicyAuditor) analyzeProtected(status *NamespaceNetworkStatus, policies []networkingv1.NetworkPolicy) {
	for _, policy := range policies {
		detail := PolicyDetail{
			Name:  policy.Name,
			Types: []string{},
		}

		for _, pType := range policy.Spec.PolicyTypes {
			detail.Types = append(detail.Types, string(pType))
			if pType == networkingv1.PolicyTypeIngress {
				status.HasIngressRestriction = true
				detail.IngressRules = len(policy.Spec.Ingress)
			}
			if pType == networkingv1.PolicyTypeEgress {
				status.HasEgressRestriction = true
				detail.EgressRules = len(policy.Spec.Egress)
			}
		}

		// Detect default-deny: policy with empty podSelector and no rules
		if len(policy.Spec.PodSelector.MatchLabels) == 0 &&
			len(policy.Spec.PodSelector.MatchExpressions) == 0 &&
			(len(policy.Spec.Ingress) == 0 || len(policy.Spec.Egress) == 0) {
			detail.IsDefaultDeny = true
			status.HasDefaultDeny = true
		}

		status.Policies = append(status.Policies, detail)
	}
}

func (n *NetworkPolicyAuditor) analyzeRisk(status *NamespaceNetworkStatus) {
	env := status.Environment

	switch env {
	case "PRODUCTION":
		status.RiskLevel = "HIGH"
		status.RiskReason = "Production namespace with no network isolation - all pods can communicate freely"
	case "STAGING":
		status.RiskLevel = "HIGH"
		status.RiskReason = "Staging namespace unprotected - can be used as pivot point in attacks"
	case "SYSTEM":
		status.RiskLevel = "HIGH"
		status.RiskReason = "System namespace unprotected - critical infrastructure exposed"
	default:
		if status.PodCount > 10 {
			status.RiskLevel = "MEDIUM"
			status.RiskReason = fmt.Sprintf("Development namespace with %d pods - consider basic isolation", status.PodCount)
		} else {
			status.RiskLevel = "LOW"
			status.RiskReason = "Development/test namespace - low risk but isolation recommended"
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
	fmt.Println("║  • Missing policies = unrestricted pod-to-pod traffic      ║")
	fmt.Println("║  • Use with kube-bench for full network security audit     ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Summary
	protected := len(audit.ProtectedNamespaces)
	unprotected := len(audit.UnprotectedNamespaces)
	total := protected + unprotected

	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("NETWORK POLICY SUMMARY")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("Total Namespaces:         %d\n", total)
	fmt.Printf("Protected (policies):     %d\n", protected)
	fmt.Printf("Unprotected (no policy):  %d\n", unprotected)
	fmt.Printf("Total NetworkPolicies:    %d\n", audit.TotalPolicies)
	fmt.Printf("High Risk Namespaces:     %d\n", audit.HighRiskNamespaces)
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
	if ns.HasDefaultDeny {
		defaultDeny = " | Default-Deny: ✅"
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
	fmt.Printf("     ⚠️  %s\n", ns.RiskReason)
	fmt.Println()
}

func printNetworkRecommendations(audit *NetworkPolicyAudit) {
	if len(audit.UnprotectedNamespaces) == 0 {
		fmt.Println("\n🎉 All namespaces have NetworkPolicies! Great security posture.")
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
			fmt.Printf("  %d. Add NetworkPolicy to '%s' (%s, %s)\n", i+1, ns.Name, ns.Environment, podLabel(ns.PodCount))
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
			if !ns.HasDefaultDeny {
				fmt.Printf("  • %s: Has policies but no default-deny rule\n", ns.Name)
			}
		}
	}
}

func allHaveDefaultDeny(namespaces []NamespaceNetworkStatus) bool {
	for _, ns := range namespaces {
		if !ns.HasDefaultDeny {
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
