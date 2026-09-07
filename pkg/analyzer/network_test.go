package analyzer

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
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

func captureNetworkAuditOutput(t *testing.T, render func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	originalStdout := os.Stdout
	os.Stdout = writer
	defer func() {
		os.Stdout = originalStdout
		reader.Close()
		writer.Close()
	}()

	render()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	os.Stdout = originalStdout
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(output)
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
	if status.FullyCoveredPodCount != 1 || status.CoverageGapPodCount != 2 {
		t.Fatalf("same-pod directional coverage counts wrong: %+v", status)
	}
	if status.HasIngressRestriction || status.HasEgressRestriction {
		t.Fatalf("partial selector coverage must not set namespace-wide restriction flags: %+v", status)
	}
}

func TestAuditNetworkPoliciesIngressOnlyCountsEveryPodAsDirectionalGap(t *testing.T) {
	na := newTestNetworkAuditor(
		nsObj("payments-prod"),
		podWithLabels("payments-prod", "api-1", map[string]string{"app": "api"}),
		podWithLabels("payments-prod", "api-2", map[string]string{"app": "api"}),
		policy("payments-prod", "deny-ingress", metav1.LabelSelector{},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, nil, nil),
	)
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	status, ok := findStatus(audit.UnprotectedNamespaces, "payments-prod")
	if !ok {
		t.Fatalf("ingress-only coverage must remain a directional finding: %+v", audit)
	}
	if status.CoveredPodCount != 2 || status.UncoveredPodCount != 0 {
		t.Fatalf("existing selector-coverage semantics changed: %+v", status)
	}
	if status.IngressCoveredPods != 2 || status.EgressCoveredPods != 0 ||
		status.FullyCoveredPodCount != 0 || status.CoverageGapPodCount != 2 {
		t.Fatalf("directional coverage counts wrong: %+v", status)
	}
	if !status.HasIngressRestriction || status.HasEgressRestriction {
		t.Fatalf("namespace-wide direction flags wrong: %+v", status)
	}
	if !strings.Contains(status.RiskReason, "2 of 2 observed pods") ||
		strings.Contains(status.RiskReason, "0 of 2") {
		t.Fatalf("risk reason does not describe the actual directional gap: %q", status.RiskReason)
	}
}

func TestAuditNetworkPoliciesDisjointDirectionsDoNotCountAsFullyCovered(t *testing.T) {
	na := newTestNetworkAuditor(
		nsObj("split-prod"),
		podWithLabels("split-prod", "ingress-pod", map[string]string{"role": "ingress"}),
		podWithLabels("split-prod", "egress-pod", map[string]string{"role": "egress"}),
		policy("split-prod", "ingress-only",
			metav1.LabelSelector{MatchLabels: map[string]string{"role": "ingress"}},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, nil, nil),
		policy("split-prod", "egress-only",
			metav1.LabelSelector{MatchLabels: map[string]string{"role": "egress"}},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil, nil),
	)
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	status, ok := findStatus(audit.UnprotectedNamespaces, "split-prod")
	if !ok {
		t.Fatalf("disjoint directions must not be reported fully covered: %+v", audit)
	}
	if status.CoveredPodCount != 2 || status.UncoveredPodCount != 0 ||
		status.IngressCoveredPods != 1 || status.EgressCoveredPods != 1 ||
		status.FullyCoveredPodCount != 0 || status.CoverageGapPodCount != 2 {
		t.Fatalf("same-pod directional intersection was not preserved: %+v", status)
	}
	if status.HasIngressRestriction || status.HasEgressRestriction {
		t.Fatalf("partial directional coverage must not set namespace-wide flags: %+v", status)
	}
	output := captureNetworkAuditOutput(t, func() { printUnprotectedNamespace(status) })
	if !strings.Contains(output, "0 of 2 pods have full ingress+egress coverage") ||
		strings.Contains(output, "1 of 2 pods have full ingress+egress coverage") {
		t.Fatalf("CLI used directional totals instead of same-pod coverage: %s", output)
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
	if status.FullyCoveredPodCount != 0 || status.CoverageGapPodCount != 1 || status.HasIngressRestriction {
		t.Errorf("allow-all must remain a directional coverage gap: %+v", status)
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

func TestAuditNetworkPoliciesSharedPodsPreservesFindings(t *testing.T) {
	objects := []interface{}{
		nsObj("payments"),
		podWithLabels("payments", "api", map[string]string{"app": "api"}),
		podWithLabels("payments", "worker", map[string]string{"app": "worker"}),
		policy("payments", "api-only", metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{}}}},
			[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{}}}}),
	}
	legacy, err := newTestNetworkAuditor(objects...).AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("legacy audit: %v", err)
	}
	shared, err := newTestNetworkAuditor(objects...).AuditNetworkPoliciesWithPods("", []corev1.Pod{
		*objects[1].(*corev1.Pod), *objects[2].(*corev1.Pod),
	})
	if err != nil {
		t.Fatalf("shared audit: %v", err)
	}
	if !reflect.DeepEqual(shared, legacy) {
		t.Fatalf("shared snapshot changed findings:\nshared=%+v\nlegacy=%+v", shared, legacy)
	}
}

func TestAuditNetworkPoliciesSharedPodsPreservesEmptyNamespace(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("empty"))
	audit, err := na.AuditNetworkPoliciesWithPods("", []corev1.Pod{})
	if err != nil {
		t.Fatalf("AuditNetworkPoliciesWithPods: %v", err)
	}
	status, ok := findStatus(audit.ProtectedNamespaces, "empty")
	if !ok || status.PodCount != 0 || status.CoverageGapPodCount != 0 {
		t.Fatalf("empty eligible namespace behavior changed: %+v", audit)
	}
}

func TestAuditNetworkPoliciesSharedPodsPreservesSelectorBehavior(t *testing.T) {
	na := newTestNetworkAuditor(
		nsObj("payments"),
		policy("payments", "api-only", metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{}}}},
			[]networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{}}}}),
	)
	audit, err := na.AuditNetworkPoliciesWithPods("", []corev1.Pod{
		*podWithLabels("payments", "api", map[string]string{"app": "api"}),
		*podWithLabels("payments", "worker", map[string]string{"app": "worker"}),
	})
	if err != nil {
		t.Fatalf("AuditNetworkPoliciesWithPods: %v", err)
	}
	status, ok := findStatus(audit.UnprotectedNamespaces, "payments")
	if !ok || status.PodCount != 2 || status.CoveredPodCount != 1 || status.UncoveredPodCount != 1 || status.FullyCoveredPodCount != 1 {
		t.Fatalf("selector behavior changed with shared Pods: %+v", audit)
	}
}

func TestAuditNetworkPoliciesSharedPodsRetainsNetworkPolicyErrors(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("broken"))
	na.clientset.(*fake.Clientset).PrependReactor("list", "networkpolicies", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	audit, err := na.AuditNetworkPoliciesWithPods("", []corev1.Pod{})
	if err != nil {
		t.Fatalf("AuditNetworkPoliciesWithPods: %v", err)
	}
	if len(audit.Warnings) != 1 || audit.Warnings[0].Namespace != "broken" || audit.Warnings[0].Operation != "list NetworkPolicies" {
		t.Fatalf("NetworkPolicy error behavior changed: %+v", audit.Warnings)
	}
	if _, ok := findStatus(audit.ProtectedNamespaces, "broken"); ok {
		t.Fatal("namespace with a NetworkPolicy error appeared protected")
	}
	if _, ok := findStatus(audit.UnprotectedNamespaces, "broken"); ok {
		t.Fatal("namespace with a NetworkPolicy error appeared unprotected")
	}
}

func TestAuditNetworkPoliciesSharedPodsPerformsNoPodLists(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("one"), nsObj("two"))
	podLists := 0
	na.clientset.(*fake.Clientset).PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		podLists++
		return true, nil, errors.New("unexpected Pod LIST")
	})
	_, err := na.AuditNetworkPoliciesWithPods("", []corev1.Pod{*podWithLabels("one", "api", nil)})
	if err != nil {
		t.Fatalf("AuditNetworkPoliciesWithPods: %v", err)
	}
	if podLists != 0 {
		t.Fatalf("Pod LIST calls = %d, want 0", podLists)
	}
}

func TestAuditNetworkPoliciesClusterPolicySnapshotMatchesLegacy(t *testing.T) {
	objects := []interface{}{
		nsObj("payments-prod"), nsObj("workers"), nsObj("empty"), nsObj("kube-system"),
		podWithLabels("payments-prod", "api", map[string]string{"app": "api"}),
		podWithLabels("payments-prod", "worker", map[string]string{"app": "worker"}),
		podWithLabels("workers", "worker", map[string]string{"app": "worker"}),
		policy("payments-prod", "api-ingress", metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, nil, nil),
		policy("payments-prod", "api-egress", metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil, nil),
		policy("workers", "worker-deny-all", metav1.LabelSelector{},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, nil, nil),
		policy("kube-system", "ignored", metav1.LabelSelector{},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, nil, nil),
	}
	pods := map[string][]corev1.Pod{
		"payments-prod": {*objects[4].(*corev1.Pod), *objects[5].(*corev1.Pod)},
		"workers":       {*objects[6].(*corev1.Pod)},
	}
	legacy, err := newTestNetworkAuditor(objects...).auditNetworkPolicies("", pods, false)
	if err != nil {
		t.Fatalf("legacy audit: %v", err)
	}
	fast, err := newTestNetworkAuditor(objects...).auditNetworkPolicies("", pods, true)
	if err != nil {
		t.Fatalf("snapshot audit: %v", err)
	}
	if !reflect.DeepEqual(fast, legacy) {
		t.Fatalf("cluster policy snapshot changed results:\nfast=%+v\nlegacy=%+v", fast, legacy)
	}
}

func TestAuditNetworkPoliciesClusterPolicySnapshotIsolatesNamespaces(t *testing.T) {
	na := newTestNetworkAuditor(
		nsObj("open"), nsObj("locked"),
		policy("locked", "deny-all", metav1.LabelSelector{},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, nil, nil),
	)
	audit, err := na.AuditNetworkPoliciesWithPods("", []corev1.Pod{
		*podWithLabels("open", "api", nil), *podWithLabels("locked", "api", nil),
	})
	if err != nil {
		t.Fatalf("AuditNetworkPoliciesWithPods: %v", err)
	}
	if _, ok := findStatus(audit.UnprotectedNamespaces, "open"); !ok {
		t.Fatalf("policy from another namespace protected open: %+v", audit)
	}
	if _, ok := findStatus(audit.ProtectedNamespaces, "locked"); !ok {
		t.Fatalf("locked namespace was not protected: %+v", audit)
	}
}

func TestAuditNetworkPoliciesClusterPolicySnapshotPreservesNamespaceFiltering(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("payments"), nsObj("billing"), nsObj("kube-system"))
	audit, err := na.AuditNetworkPoliciesWithPods("payments", []corev1.Pod{
		*podWithLabels("payments", "api", nil), *podWithLabels("billing", "api", nil),
	})
	if err != nil {
		t.Fatalf("AuditNetworkPoliciesWithPods: %v", err)
	}
	if audit.TotalNamespaces != 1 {
		t.Fatalf("TotalNamespaces = %d, want 1", audit.TotalNamespaces)
	}
	if _, ok := findStatus(audit.UnprotectedNamespaces, "payments"); !ok {
		t.Fatalf("filtered namespace missing: %+v", audit)
	}
	if _, ok := findStatus(audit.UnprotectedNamespaces, "billing"); ok {
		t.Fatalf("non-target namespace included: %+v", audit)
	}
}

func TestAuditNetworkPoliciesClusterPolicySnapshotUsesOneClusterList(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("one"), nsObj("two"))
	if _, err := na.AuditNetworkPoliciesWithPods("", nil); err != nil {
		t.Fatalf("AuditNetworkPoliciesWithPods: %v", err)
	}
	clusterLists, namespaceLists := 0, 0
	for _, action := range na.clientset.(*fake.Clientset).Actions() {
		if action.GetVerb() != "list" || action.GetResource().Resource != "networkpolicies" {
			continue
		}
		if action.GetNamespace() == "" {
			clusterLists++
		} else {
			namespaceLists++
		}
	}
	if clusterLists != 1 || namespaceLists != 0 {
		t.Fatalf("NetworkPolicy LISTs: cluster=%d namespace=%d, want 1/0", clusterLists, namespaceLists)
	}
}

func TestAuditNetworkPoliciesClusterListFailureFallsBack(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("one"), nsObj("two"))
	na.clientset.(*fake.Clientset).PrependReactor("list", "networkpolicies", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "" {
			return true, nil, errors.New("cluster-wide list forbidden")
		}
		return false, nil, nil
	})
	if _, err := na.AuditNetworkPoliciesWithPods("", nil); err != nil {
		t.Fatalf("AuditNetworkPoliciesWithPods: %v", err)
	}
	clusterLists, namespaceLists := 0, 0
	for _, action := range na.clientset.(*fake.Clientset).Actions() {
		if action.GetVerb() != "list" || action.GetResource().Resource != "networkpolicies" {
			continue
		}
		if action.GetNamespace() == "" {
			clusterLists++
		} else {
			namespaceLists++
		}
	}
	if clusterLists != 1 || namespaceLists != 2 {
		t.Fatalf("fallback NetworkPolicy LISTs: cluster=%d namespace=%d, want 1/2", clusterLists, namespaceLists)
	}
}

func TestAuditNetworkPoliciesFallbackPreservesPartialResultsAndWarnings(t *testing.T) {
	na := newTestNetworkAuditor(nsObj("healthy"), nsObj("broken"))
	na.clientset.(*fake.Clientset).PrependReactor("list", "networkpolicies", func(action ktesting.Action) (bool, runtime.Object, error) {
		switch action.GetNamespace() {
		case "":
			return true, nil, errors.New("cluster-wide list forbidden")
		case "broken":
			return true, nil, context.DeadlineExceeded
		default:
			return false, nil, nil
		}
	})
	audit, err := na.AuditNetworkPoliciesWithPods("", nil)
	if err != nil {
		t.Fatalf("AuditNetworkPoliciesWithPods: %v", err)
	}
	if audit.TotalNamespaces != 2 || len(audit.Warnings) != 1 || audit.Warnings[0].Namespace != "broken" || audit.Warnings[0].Operation != "list NetworkPolicies" {
		t.Fatalf("fallback warnings changed: %+v", audit)
	}
	if _, ok := findStatus(audit.ProtectedNamespaces, "healthy"); !ok {
		t.Fatalf("successful namespace result was lost: %+v", audit)
	}
	if _, ok := findStatus(audit.ProtectedNamespaces, "broken"); ok {
		t.Fatalf("failed namespace appeared in partial results: %+v", audit)
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

func TestAuditNetworkPoliciesFilteredNamespaceCountIncludesFailedChecks(t *testing.T) {
	t.Run("filter excludes other and infrastructure namespaces", func(t *testing.T) {
		na := newTestNetworkAuditor(nsObj("payments"), nsObj("billing"), nsObj("kube-system"))
		audit, err := na.AuditNetworkPolicies("payments")
		if err != nil {
			t.Fatalf("AuditNetworkPolicies: %v", err)
		}
		if audit.TotalNamespaces != 1 || len(audit.ProtectedNamespaces) != 1 {
			t.Fatalf("filtered audit counted namespaces outside its scope: %+v", audit)
		}
	})

	t.Run("failed target remains in the filtered denominator", func(t *testing.T) {
		na := newTestNetworkAuditor(nsObj("payments"), nsObj("billing"))
		na.clientset.(*fake.Clientset).PrependReactor("list", "networkpolicies", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		audit, err := na.AuditNetworkPolicies("payments")
		if err != nil {
			t.Fatalf("AuditNetworkPolicies: %v", err)
		}
		if audit.TotalNamespaces != 1 || len(audit.Warnings) != 1 || audit.Warnings[0].Namespace != "payments" {
			t.Fatalf("failed namespace disappeared from the filtered audit scope: %+v", audit)
		}
	})
}

func TestPrintNetworkPolicyAuditWarningsDoNotProduceFalseAllClear(t *testing.T) {
	audit := &NetworkPolicyAudit{
		TotalNamespaces: 2,
		ProtectedNamespaces: []NamespaceNetworkStatus{{
			Name: "healthy", PodCount: 1, PolicyCount: 1,
			CoveredPodCount: 1, FullyCoveredPodCount: 1,
			IngressCoveredPods: 1, EgressCoveredPods: 1,
			HasIngressRestriction: true, HasEgressRestriction: true,
		}},
		Warnings: []NetworkAuditWarning{{
			Namespace: "blocked", Operation: "list NetworkPolicies", Message: "forbidden",
		}},
	}
	output := captureNetworkAuditOutput(t, func() { PrintNetworkPolicyAudit(audit) })
	for _, want := range []string{"Total Namespaces:         2", "50%", "AUDIT INCOMPLETE", "blocked", "forbidden"} {
		if !strings.Contains(output, want) {
			t.Errorf("incomplete CLI audit omitted %q: %s", want, output)
		}
	}
	for _, forbidden := range []string{"All observed pods have full", "Great security posture", "100%"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("incomplete CLI audit reported unsupported success %q: %s", forbidden, output)
		}
	}
}

func TestCISNetworkPolicyControlDoesNotPassIncompleteAudit(t *testing.T) {
	audit := &NetworkPolicyAudit{
		TotalNamespaces:     2,
		ProtectedNamespaces: []NamespaceNetworkStatus{{Name: "healthy"}},
		Warnings:            []NetworkAuditWarning{{Namespace: "blocked", Operation: "list Pods", Message: "forbidden"}},
	}
	result := CalculateCISScore(&models.SecurityAudit{}, audit)
	for _, control := range result.Controls {
		if control.ID != "5.7.3" {
			continue
		}
		if control.Passed || !strings.Contains(control.Finding, "Audit incomplete") {
			t.Fatalf("CIS 5.7.3 passed or omitted incomplete evidence: %+v", control)
		}
		return
	}
	t.Fatal("CIS result omitted control 5.7.3")
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
	if status.FullyCoveredPodCount != 1 || status.CoverageGapPodCount != 0 ||
		!status.HasIngressRestriction || !status.HasEgressRestriction {
		t.Errorf("full namespace-wide directional coverage was not preserved: %+v", status)
	}
}

func TestAuditNetworkPoliciesNoPolicyCountsEveryObservedPodAsUncovered(t *testing.T) {
	na := newTestNetworkAuditor(
		nsObj("apps"),
		podWithLabels("apps", "api-1", map[string]string{"app": "api"}),
		podWithLabels("apps", "api-2", map[string]string{"app": "api"}),
	)
	audit, err := na.AuditNetworkPolicies("")
	if err != nil {
		t.Fatalf("AuditNetworkPolicies: %v", err)
	}
	status, ok := findStatus(audit.UnprotectedNamespaces, "apps")
	if !ok {
		t.Fatalf("namespace without policies disappeared from findings: %+v", audit)
	}
	if status.CoveredPodCount != 0 || status.UncoveredPodCount != 2 ||
		status.FullyCoveredPodCount != 0 || status.CoverageGapPodCount != 2 {
		t.Fatalf("missing policies did not mark every observed pod as uncovered: %+v", status)
	}
	if !strings.Contains(status.RiskReason, "no NetworkPolicy was observed") ||
		strings.Contains(status.RiskReason, "communicate freely") {
		t.Fatalf("risk reason exceeded observed evidence: %q", status.RiskReason)
	}
}
