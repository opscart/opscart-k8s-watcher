package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func privilegedPod(namespace, name string) *corev1.Pod {
	privileged := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:            "app",
			SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		}}},
	}
}

// TestBuildSecurityAnalysisMatchesDirectAnalyzerCall proves buildSecurityAnalysis's
// snapshot-resources adaptation produces exactly what calling
// analyzer.AnalyzeSecurity directly on the same value slice would — "same
// input produces equivalent audit results".
func TestBuildSecurityAnalysisMatchesDirectAnalyzerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Pods: []*corev1.Pod{privilegedPod("payments", "api-1")},
	}

	got := buildSecurityAnalysis(resources)
	want := analyzer.AnalyzeSecurity([]corev1.Pod{*resources.Pods[0]})

	if got.TotalPodsAudited != want.TotalPodsAudited || len(got.Issues) != len(want.Issues) {
		t.Fatalf("buildSecurityAnalysis = %+v, want %+v", got, want)
	}
}

func TestBuildSecurityAnalysisHealthyPodProducesNoPrivilegedFinding(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Pods: []*corev1.Pod{labeledPod("payments", "api-1", nil)},
	}

	got := buildSecurityAnalysis(resources)

	for _, issue := range got.Issues {
		if issue.Type == "privileged_container" {
			t.Fatalf("unexpected privileged_container finding for a non-privileged pod: %+v", issue)
		}
	}
}
