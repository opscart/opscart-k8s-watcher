package analyzer

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func newTestNetworkAuditor(objs ...interface{}) *NetworkPolicyAuditor {
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o.(runtime.Object))
	}
	return &NetworkPolicyAuditor{
		clientset: fake.NewSimpleClientset(runtimeObjs...),
		ctx:       context.Background(),
	}
}

func nsObj(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func podWithLabels(namespace, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
}

func policy(namespace, name string, selector metav1.LabelSelector, types []networkingv1.PolicyType,
	ingress []networkingv1.NetworkPolicyIngressRule, egress []networkingv1.NetworkPolicyEgressRule) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: selector,
			PolicyTypes: types,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

func findStatus(list []NamespaceNetworkStatus, name string) (NamespaceNetworkStatus, bool) {
	for _, ns := range list {
		if ns.Name == name {
			return ns, true
		}
	}
	return NamespaceNetworkStatus{}, false
}

// ── Core reported bug: partial selector coverage ───────────────────────────

func TestAuditNetworkPoliciesPartialSelectorCoverageIsUnprotected(t *testing.T) {
	// One policy selects only 1 of 3 pods. The old behavior (any policy
	// count > 0 = protected) would mark this namespace fully protected.
	na := newTestNetworkAuditor(
		nsObj("payments"),
		podWithLabels("payments", "api-1", map[string]string{"app": "api"}),
		podWithLabels("payments", "worker-1", map[string]string{"app": "worker"}),
		podWithLabels("payments", "worker-2", map[string]string{"app": "worker"}),
		policy("payments", "api-policy",
			metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			[]networkingv1.PolicyType{"Ingress", "Egress"},
			[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{}}}},
			[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{}}}},
		),
	)
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	status, ok := findStatus(audit.UnprotectedNamespaces, "payments")
	if !ok {
		t.Fatalf("expected 'payments' in UnprotectedNamespaces (partial coverage), got protected=%v unprotected=%v",
			audit.ProtectedNamespaces, audit.UnprotectedNamespaces)
	}
	if status.PodCount != 3 || status.CoveredPodCount != 1 || status.UncoveredPodCount != 2 {
		t.Fatalf("coverage counts wrong: %+v", status)
	}
}

// ── Allow-all detection ──────────────────────────────────────────────────

func TestAuditNetworkPoliciesAllowAllIngressNotCovered(t *testing.T) {
	// podSelector: {} ; policyTypes: [Ingress] ; ingress: [{}]
	// This explicitly allows ALL ingress — a policy existing here must NOT
	// count as coverage, even though every pod matches its selector.
	na := newTestNetworkAuditor(
		nsObj("open"),
		podWithLabels("open", "api-1", map[string]string{"app": "api"}),
		policy("open", "allow-all-ingress",
			metav1.LabelSelector{},
			[]networkingv1.PolicyType{"Ingress"},
			[]networkingv1.NetworkPolicyIngressRule{{}}, // empty rule = allow from anywhere, any port
			nil,
		),
	)
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	status, ok := findStatus(audit.UnprotectedNamespaces, "open")
	if !ok {
		t.Fatalf("expected 'open' in UnprotectedNamespaces (allow-all ingress), got: %+v", audit.ProtectedNamespaces)
	}
	if status.CoveredPodCount != 1 {
		t.Errorf("selector still matched the pod (CoveredPodCount should be 1), got %d", status.CoveredPodCount)
	}
	if status.IngressCoveredPods != 0 {
		t.Errorf("IngressCoveredPods should be 0 (allow-all rule), got %d", status.IngressCoveredPods)
	}
}

// ── Additive policy semantics ───────────────────────────────────────────

func TestAuditNetworkPoliciesAdditiveAllowAllOverridesRestrictivePolicy(t *testing.T) {
	// Two policies select the same pod: one restrictive, one allow-all.
	// NetworkPolicies are additive — the allow-all wins for that pod's
	// ingress regardless of the other, more restrictive policy.
	na := newTestNetworkAuditor(
		nsObj("mixed"),
		podWithLabels("mixed", "api-1", map[string]string{"app": "api"}),
		policy("mixed", "restrictive",
			metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			[]networkingv1.PolicyType{"Ingress"},
			[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "frontend"}},
			}}}},
			nil,
		),
		policy("mixed", "allow-all",
			metav1.LabelSelector{},
			[]networkingv1.PolicyType{"Ingress"},
			[]networkingv1.NetworkPolicyIngressRule{{}},
			nil,
		),
	)
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	status, ok := findStatus(audit.UnprotectedNamespaces, "mixed")
	if !ok {
		t.Fatalf("expected 'mixed' in UnprotectedNamespaces (allow-all makes ingress open despite the restrictive policy), got protected=%+v", audit.ProtectedNamespaces)
	}
	if status.IngressCoveredPods != 0 {
		t.Errorf("IngressCoveredPods should be 0 — allow-all is additive and wins, got %d", status.IngressCoveredPods)
	}
}

// ── PolicyTypes omitted — Kubernetes default behavior ──────────────────────

func TestEffectivePolicyTypesOmittedDefaultsIngressAlwaysEgressIfRulesPresent(t *testing.T) {
	// No PolicyTypes specified, no Egress rules: Ingress applies, Egress doesn't.
	p1 := policy("ns", "p1", metav1.LabelSelector{}, nil,
		[]networkingv1.NetworkPolicyIngressRule{{}}, nil)
	ing, egr := effectivePolicyTypes(*p1)
	if !ing || egr {
		t.Errorf("no PolicyTypes, no Egress rules: got ingress=%v egress=%v, want true/false", ing, egr)
	}

	// No PolicyTypes specified, Egress rules present: both apply.
	p2 := policy("ns", "p2", metav1.LabelSelector{}, nil,
		nil, []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{}}}})
	ing2, egr2 := effectivePolicyTypes(*p2)
	if !ing2 || !egr2 {
		t.Errorf("no PolicyTypes, Egress rules present: got ingress=%v egress=%v, want true/true", ing2, egr2)
	}

	// Explicit PolicyTypes: [Egress] only — Ingress does NOT apply, even
	// though the zero-value Ingress list also has len 0.
	p3 := policy("ns", "p3", metav1.LabelSelector{}, []networkingv1.PolicyType{"Egress"}, nil, nil)
	ing3, egr3 := effectivePolicyTypes(*p3)
	if ing3 || !egr3 {
		t.Errorf("explicit PolicyTypes=[Egress]: got ingress=%v egress=%v, want false/true", ing3, egr3)
	}
}

// ── Directional default-deny ────────────────────────────────────────────

func TestPolicyIsDefaultDenyDirectional(t *testing.T) {
	// Egress-only default-deny: must set egress deny, NOT ingress deny —
	// this is the exact bug from the original || check.
	egressOnly := policy("ns", "deny-egress", metav1.LabelSelector{},
		[]networkingv1.PolicyType{"Egress"}, nil, []networkingv1.NetworkPolicyEgressRule{})
	if policyIsDefaultDenyIngress(*egressOnly) {
		t.Error("egress-only deny-all must NOT be reported as ingress default-deny")
	}
	if !policyIsDefaultDenyEgress(*egressOnly) {
		t.Error("egress-only deny-all should be reported as egress default-deny")
	}

	// The exact case from the design review: empty selector, PolicyTypes
	// declares only Ingress, with an explicit allow-all ingress rule —
	// must NOT be flagged default-deny at all (it explicitly allows).
	allowAllIngress := policy("ns", "allow-ingress", metav1.LabelSelector{},
		[]networkingv1.PolicyType{"Ingress"}, []networkingv1.NetworkPolicyIngressRule{{}}, nil)
	if policyIsDefaultDenyIngress(*allowAllIngress) {
		t.Error("a policy with an explicit allow-all ingress rule must not be default-deny")
	}
}

// ── API failure handling ────────────────────────────────────────────────

func TestAuditNetworkPoliciesAPIFailureRecordsWarning(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("broken"))
	na.clientset.(*fake.Clientset).PrependReactor("list", "networkpolicies", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	if len(audit.Warnings) != 1 || audit.Warnings[0].Namespace != "broken" || audit.Warnings[0].Operation != "list NetworkPolicies" {
		t.Fatalf("expected one warning for 'broken' namespace's NetworkPolicy listing, got: %+v", audit.Warnings)
	}
	if _, ok := findStatus(audit.ProtectedNamespaces, "broken"); ok {
		t.Error("a namespace with an audit warning must not appear in ProtectedNamespaces")
	}
	if _, ok := findStatus(audit.UnprotectedNamespaces, "broken"); ok {
		t.Error("a namespace with an audit warning must not appear in UnprotectedNamespaces either — its coverage is unknown, not confirmed bad")
	}
}

func TestAuditNetworkPoliciesPodListFailureRecordsWarning(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("broken"))
	na.clientset.(*fake.Clientset).PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	if len(audit.Warnings) != 1 || audit.Warnings[0].Operation != "list Pods" {
		t.Fatalf("expected a Pods-list warning, got: %+v", audit.Warnings)
	}
}

// ── Empty namespace and full coverage ───────────────────────────────────

func TestAuditNetworkPoliciesEmptyNamespaceIsProtected(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("empty"))
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	if _, ok := findStatus(audit.ProtectedNamespaces, "empty"); !ok {
		t.Errorf("a namespace with 0 pods has nothing to protect — should be vacuously protected, got unprotected=%+v", audit.UnprotectedNamespaces)
	}
}

func TestAuditNetworkPoliciesFullDenyAllIsProtected(t *testing.T) {
	na := newTestNetworkAuditor(
		nsObj("locked-down"),
		podWithLabels("locked-down", "api-1", map[string]string{"app": "api"}),
		policy("locked-down", "deny-all", metav1.LabelSelector{},
			[]networkingv1.PolicyType{"Ingress", "Egress"}, nil, nil),
	)
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	status, ok := findStatus(audit.ProtectedNamespaces, "locked-down")
	if !ok {
		t.Fatalf("expected full deny-all to result in Protected, got unprotected=%+v", audit.UnprotectedNamespaces)
	}
	if !status.HasDefaultDenyIngress || !status.HasDefaultDenyEgress {
		t.Errorf("expected both directional default-deny flags set: %+v", status)
	}
}
