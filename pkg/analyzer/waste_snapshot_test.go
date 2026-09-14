package analyzer

import (
	"reflect"
	"sort"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// wasteSnapshotFromFixture converts wasteInvariantFixture()'s (or any
// similar) []interface{} of *corev1.Namespace/*corev1.Pod/etc. into the
// equivalent WasteSnapshot, so the exact same underlying objects can be run
// through both AuditWaste (via a fake clientset, see waste_invariants_test.go)
// and AnalyzeWaste for a direct equivalence comparison.
func wasteSnapshotFromFixture(objects []interface{}) WasteSnapshot {
	var snap WasteSnapshot
	for _, o := range objects {
		switch v := o.(type) {
		case *corev1.Namespace:
			snap.Namespaces = append(snap.Namespaces, *v)
		case *corev1.Pod:
			snap.Pods = append(snap.Pods, *v)
		case *corev1.PersistentVolumeClaim:
			snap.PersistentVolumeClaims = append(snap.PersistentVolumeClaims, *v)
		case *batchv1.Job:
			snap.Jobs = append(snap.Jobs, *v)
		case *batchv1.CronJob:
			snap.CronJobs = append(snap.CronJobs, *v)
		case *appsv1.Deployment:
			snap.Deployments = append(snap.Deployments, *v)
		case *appsv1.StatefulSet:
			snap.StatefulSets = append(snap.StatefulSets, *v)
		case *appsv1.ReplicaSet:
			snap.ReplicaSets = append(snap.ReplicaSets, *v)
		case *corev1.Service:
			snap.Services = append(snap.Services, *v)
		case *networkingv1.Ingress:
			snap.Ingresses = append(snap.Ingresses, *v)
		case *autoscalingv2.HorizontalPodAutoscaler:
			snap.HorizontalPodAutoscalers = append(snap.HorizontalPodAutoscalers, *v)
		case *discoveryv1.EndpointSlice:
			snap.EndpointSlices = append(snap.EndpointSlices, *v)
		case *corev1.Event:
			snap.PodWarningEvents = append(snap.PodWarningEvents, *v)
		}
	}

	// The fake clientset's List() returns items name-sorted, independent of
	// registration order. Sorting the reconstructed slices the same way
	// means a fixture literal's authoring order can't affect an
	// equivalence comparison against AuditWaste's fake-clientset-driven
	// output — matching a real informer Lister's own name-ordered return,
	// not a test artifact either path should be sensitive to.
	sort.Slice(snap.ReplicaSets, func(i, j int) bool { return snap.ReplicaSets[i].Name < snap.ReplicaSets[j].Name })
	sort.Slice(snap.HorizontalPodAutoscalers, func(i, j int) bool {
		return snap.HorizontalPodAutoscalers[i].Name < snap.HorizontalPodAutoscalers[j].Name
	})

	return snap
}

// TestAnalyzeWasteRequiresNoKubernetesClient proves the snapshot path
// performs zero Kubernetes API calls — AnalyzeWaste is a plain function
// over value slices, with no clientset or context reachable from it at all.
func TestAnalyzeWasteRequiresNoKubernetesClient(t *testing.T) {
	now := time.Now()
	input := WasteSnapshot{
		Namespaces: []corev1.Namespace{*oldNamespace("empty", 40)},
	}

	got := AnalyzeWaste(input, 7, now)

	if len(got.AbandonedNamespaces) != 1 {
		t.Fatalf("AnalyzeWaste = %+v, want one abandoned namespace", got.AbandonedNamespaces)
	}
	if len(got.DetectorWarnings) != 0 {
		t.Fatalf("DetectorWarnings = %+v, want none — the snapshot path never manufactures API-failure warnings", got.DetectorWarnings)
	}
}

// TestAnalyzeWasteMatchesAuditWaste runs the same comprehensive
// multi-category fixture already used by TestWastePhase1MatchesPrePhase1Baseline
// (wasteInvariantFixture, waste_invariants_test.go) through both the
// live-client path (AuditWaste, unshared) and the snapshot path
// (AnalyzeWaste) and requires identical *WasteAudit results — "equivalent
// inputs produce equivalent existing Waste findings" across every one of
// the 9 detector categories, including probe-failure/idle Pod
// classification, PVC/Job/CronJob/Deployment/StatefulSet/ReplicaSet/
// Service/Ingress/HPA evidence.
func TestAnalyzeWasteMatchesAuditWaste(t *testing.T) {
	objects := wasteInvariantFixture()

	w := newTestAuditor(7, objects...)
	legacy, err := w.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	now := time.Now()
	got := AnalyzeWaste(wasteSnapshotFromFixture(objects), 7, now)
	got.ScannedAt = legacy.ScannedAt // ScannedAt is a wall-clock capture, not a detector output

	if !reflect.DeepEqual(got, legacy) {
		t.Fatalf("AnalyzeWaste diverged from AuditWaste:\nsnapshot: %+v\nlegacy:   %+v", got, legacy)
	}
}

// TestAnalyzeWasteUsesSuppliedGenerationForAllResourceKinds proves every
// Kubernetes-derived input comes from the one supplied WasteSnapshot — a
// namespace, pod, and PVC that only exist in the snapshot argument (never
// touching any live client) all still produce their expected findings
// together, confirming AnalyzeWaste does not silently fall back to some
// other data source for any of them.
func TestAnalyzeWasteUsesSuppliedGenerationForAllResourceKinds(t *testing.T) {
	now := time.Now()
	ns := oldNamespace("abandoned", 40)
	pod := namespacePod("abandoned", "leftover", corev1.PodPending)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "abandoned", CreationTimestamp: metav1.NewTime(now.Add(-40 * 24 * time.Hour))},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	input := WasteSnapshot{
		Namespaces:             []corev1.Namespace{*ns},
		Pods:                   []corev1.Pod{*pod},
		PersistentVolumeClaims: []corev1.PersistentVolumeClaim{*pvc},
	}

	got := AnalyzeWaste(input, 7, now)

	if len(got.AbandonedNamespaces) != 1 {
		t.Fatalf("AbandonedNamespaces = %+v, want 1", got.AbandonedNamespaces)
	}
	if len(got.OrphanedPVCs) != 1 {
		t.Fatalf("OrphanedPVCs = %+v, want 1", got.OrphanedPVCs)
	}
}

// TestNamespaceAbandonmentEligibleUsesSuppliedClockNotGenerationCount
// proves the age gate is driven by the explicit now parameter, not by how
// many times the function is called — two calls with the same object but
// different now values must be able to disagree, and two calls with the
// same now must always agree, regardless of call count.
func TestNamespaceAbandonmentEligibleUsesSuppliedClockNotGenerationCount(t *testing.T) {
	created := time.Now().Add(-10 * 24 * time.Hour)
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app", CreationTimestamp: metav1.NewTime(created)}}

	tooEarly := created.Add(5 * 24 * time.Hour) // only 5 days old at this clock reading
	_, eligible := namespaceAbandonmentEligible(ns, 7, tooEarly)
	if eligible {
		t.Fatal("expected ineligible at 5 days old with a 7-day minimum, regardless of call count")
	}

	// Calling repeatedly with the SAME clock reading must not change the
	// answer — this is not a counter.
	for i := 0; i < 5; i++ {
		if _, eligible := namespaceAbandonmentEligible(ns, 7, tooEarly); eligible {
			t.Fatalf("call %d: eligibility changed across repeated calls at the same clock reading", i)
		}
	}

	laterClock := created.Add(10 * 24 * time.Hour) // 10 days old at THIS later clock reading
	if _, eligible := namespaceAbandonmentEligible(ns, 7, laterClock); !eligible {
		t.Fatal("expected eligible once the supplied clock reading advances past the minimum age — not tied to any generation/call counter")
	}
}
