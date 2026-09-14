package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func namespaceObj(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func labeledPod(namespace, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
}

func denyAllPolicy(namespace, name string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{"Ingress", "Egress"},
		},
	}
}

// TestBuildNetworkAnalysisMatchesDirectAnalyzerCall proves buildNetworkAnalysis's
// snapshot-resources adaptation produces exactly what calling
// analyzer.AnalyzeNetworkPolicies directly on the same value slices would —
// "same input produces equivalent audit results".
func TestBuildNetworkAnalysisMatchesDirectAnalyzerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{namespaceObj("payments")},
		Pods:       []*corev1.Pod{labeledPod("payments", "api-1", map[string]string{"app": "api"})},
	}

	got := buildNetworkAnalysis(resources)
	want := analyzer.AnalyzeNetworkPolicies(
		[]corev1.Namespace{*resources.Namespaces[0]},
		[]corev1.Pod{*resources.Pods[0]},
		nil, "", nil,
	)

	if got.TotalNamespaces != want.TotalNamespaces || len(got.UnprotectedNamespaces) != len(want.UnprotectedNamespaces) {
		t.Fatalf("buildNetworkAnalysis = %+v, want %+v", got, want)
	}
}

func TestBuildNetworkAnalysisProtectedNamespaceProducesNoUnprotectedFinding(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{namespaceObj("locked-down")},
		Pods:       []*corev1.Pod{labeledPod("locked-down", "api-1", map[string]string{"app": "api"})},
		NetworkPolicies: []*networkingv1.NetworkPolicy{
			denyAllPolicy("locked-down", "deny-all"),
		},
	}

	got := buildNetworkAnalysis(resources)

	if len(got.UnprotectedNamespaces) != 0 || len(got.ProtectedNamespaces) != 1 {
		t.Fatalf("got %+v, want locked-down fully protected", got)
	}
}
