package analyzer

import (
	"fmt"
	"strings"
)

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
