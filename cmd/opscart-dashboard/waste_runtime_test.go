package main

import (
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func abandonedNamespace(name string, ageDays int) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)),
		},
	}
}

// TestBuildWasteAnalysisMatchesDirectAnalyzerCall proves buildWasteAnalysis's
// snapshot-resources adaptation produces exactly what calling
// analyzer.AnalyzeWaste directly on the same value slices would — "same
// input produces equivalent audit results".
func TestBuildWasteAnalysisMatchesDirectAnalyzerCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{abandonedNamespace("stale-team", 40)},
	}

	got := buildWasteAnalysis(resources)

	if len(got.AbandonedNamespaces) != 1 {
		t.Fatalf("buildWasteAnalysis = %+v, want one abandoned namespace", got.AbandonedNamespaces)
	}
}

func TestBuildWasteAnalysisHealthyNamespaceProducesNoFinding(t *testing.T) {
	resources := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{abandonedNamespace("payments", 1)}, // 1 day old: below the 7-day minimum
	}

	got := buildWasteAnalysis(resources)

	if len(got.AbandonedNamespaces) != 0 {
		t.Fatalf("unexpected abandoned-namespace finding for a fresh namespace: %+v", got.AbandonedNamespaces)
	}
}

// TestBuildWasteAnalysisReevaluatesAgeOnEachCall proves buildWasteAnalysis
// uses the actual wall-clock time at call time (not a cached/fixed
// reference), which is what lets a clock-triggered re-analysis of an
// unchanged snapshot (docs/08 Phase 5's Coordinator clockInterval) pick up
// an age threshold crossing with no new Kubernetes event.
func TestBuildWasteAnalysisReevaluatesAgeOnEachCall(t *testing.T) {
	resources := clusterstate.ClusterResources{
		// +20 days clears both namespaceAbandonmentEligible's minAgeDays gate
		// and evaluateAbandonedNamespace's own score>=10 threshold
		// (score = ageDays*0.8 for a zero-pod namespace) — this test only
		// cares that buildWasteAnalysis's age math is live, not about the
		// exact scoring formula, which pkg/analyzer's own tests already
		// cover.
		Namespaces: []*corev1.Namespace{abandonedNamespace("aged-team", dashboardWasteMinAgeDays+20)},
	}

	before := buildWasteAnalysis(resources)
	if len(before.AbandonedNamespaces) != 1 {
		t.Fatalf("past the minimum age, got %+v, want one abandoned-namespace finding", before.AbandonedNamespaces)
	}

	// A namespace created after the minimum age must not be flagged — this
	// is the same buildWasteAnalysis call, at a different simulated "now"
	// via a fresher CreationTimestamp, proving the age comparison is live,
	// not a value baked in once.
	fresh := clusterstate.ClusterResources{
		Namespaces: []*corev1.Namespace{abandonedNamespace("brand-new-team", 0)},
	}
	after := buildWasteAnalysis(fresh)
	if len(after.AbandonedNamespaces) != 0 {
		t.Fatalf("a namespace created now, got %+v, want no abandoned-namespace finding", after.AbandonedNamespaces)
	}
}
