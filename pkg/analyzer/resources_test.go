package analyzer

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ownedPod(namespace, name, ownerKind, ownerName string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: ownerKind, Name: ownerName},
			},
		},
	}
}

func TestWorkloadsFromPods(t *testing.T) {
	pods := []corev1.Pod{
		// Two replicas of the same Deployment — must dedupe to one entry,
		// with the ReplicaSet's own hash suffix stripped.
		ownedPod("payments", "payments-api-7d8f9c6b5-abc12", "ReplicaSet", "payments-api-7d8f9c6b5"),
		ownedPod("payments", "payments-api-7d8f9c6b5-def34", "ReplicaSet", "payments-api-7d8f9c6b5"),
		// StatefulSet-owned pods use the owner name verbatim (no suffix to strip).
		ownedPod("payments", "redis-0", "StatefulSet", "redis"),
		ownedPod("payments", "redis-1", "StatefulSet", "redis"),
		// DaemonSet-owned pod, different namespace.
		ownedPod("kube-system", "node-exporter-xz9kq", "DaemonSet", "node-exporter"),
		// Job-owned pod — not a Deployment/StatefulSet/DaemonSet, excluded.
		ownedPod("payments", "migrate-abc12", "Job", "migrate"),
		// Bare pod, no owner at all — excluded.
		{ObjectMeta: metav1.ObjectMeta{Name: "debug-shell", Namespace: "payments"}},
		// Same Deployment name in a different namespace — must stay distinct.
		ownedPod("checkout", "payments-api-9f1a2b3c4-zzz99", "ReplicaSet", "payments-api-9f1a2b3c4"),
	}

	got := workloadsFromPods(pods)

	want := []models.WorkloadRef{
		{Name: "payments-api", Kind: "Deployment", Namespace: "checkout"},
		{Name: "node-exporter", Kind: "DaemonSet", Namespace: "kube-system"},
		{Name: "payments-api", Kind: "Deployment", Namespace: "payments"},
		{Name: "redis", Kind: "StatefulSet", Namespace: "payments"},
	}

	if len(got) != len(want) {
		t.Fatalf("expected %d workloads, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v (full: %+v)", i, got[i], want[i], got)
		}
	}
}

func TestWorkloadsFromPods_EmptyInput(t *testing.T) {
	if got := workloadsFromPods(nil); len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

func TestStripHashSuffix(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"payments-api-7d8f9c6b5", "payments-api"},
		{"redis", "redis"},               // no hyphen at all
		{"payments-api", "payments-api"}, // "api" too short to look like a hash
		{"checkout-worker-9f1a2", "checkout-worker"},
	}
	for _, tc := range tests {
		if got := stripHashSuffix(tc.in); got != tc.want {
			t.Errorf("stripHashSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// ownedPodMulti builds a pod with multiple OwnerReferences, letting the
// caller mark exactly one as the controller — mirrors what the real
// Kubernetes API can return (rare, but not disallowed) and is the scenario
// workloadRefForPod's Controller-preference logic exists for.
func ownedPodMulti(namespace, name string, refs ...metav1.OwnerReference) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: refs},
	}
}

func TestWorkloadRefForPodPrefersExplicitController(t *testing.T) {
	// The controller (StatefulSet) is listed SECOND — a naive "first Kind
	// match" would have picked the non-controlling ReplicaSet reference
	// instead. This is exactly the "OwnerReferences[0] is not necessarily
	// the controlling owner" gap identified during review.
	pod := ownedPodMulti("payments", "redis-0",
		metav1.OwnerReference{Kind: "ReplicaSet", Name: "stale-rs-abc12", Controller: boolPtr(false)},
		metav1.OwnerReference{Kind: "StatefulSet", Name: "redis", Controller: boolPtr(true)},
	)
	ref, ok := workloadRefForPod(pod)
	if !ok || ref != (models.WorkloadRef{Name: "redis", Kind: "StatefulSet", Namespace: "payments"}) {
		t.Fatalf("workloadRefForPod = %+v, ok=%v; want the StatefulSet (explicit controller), not the non-controlling ReplicaSet", ref, ok)
	}
}

func TestWorkloadRefForPodControllerNotARollupKindReturnsFalse(t *testing.T) {
	// The controller is explicitly a Job (not rolled up). Even though a
	// second, non-controlling reference happens to be a StatefulSet, the
	// pod must NOT be attributed to it — the controller is authoritative,
	// and falling back to an unrelated non-controlling reference would be
	// its own misattribution bug.
	pod := ownedPodMulti("batch", "worker-0",
		metav1.OwnerReference{Kind: "Job", Name: "worker-job", Controller: boolPtr(true)},
		metav1.OwnerReference{Kind: "StatefulSet", Name: "worker", Controller: boolPtr(false)},
	)
	if _, ok := workloadRefForPod(pod); ok {
		t.Fatalf("expected no workload attribution when the actual controller (Job) isn't a rolled-up kind")
	}
}

func TestWorkloadRefForPodFallsBackWhenNoControllerMarked(t *testing.T) {
	// No reference is explicitly marked Controller (uncommon but not
	// guaranteed by the API) — falls back to the first recognized kind,
	// preserving the original behavior for this edge case.
	pod := ownedPod("payments", "redis-0", "StatefulSet", "redis")
	ref, ok := workloadRefForPod(pod)
	if !ok || ref != (models.WorkloadRef{Name: "redis", Kind: "StatefulSet", Namespace: "payments"}) {
		t.Fatalf("workloadRefForPod = %+v, ok=%v; want redis/StatefulSet", ref, ok)
	}
}

func TestPodWorkloadMap(t *testing.T) {
	pods := []corev1.Pod{
		ownedPod("monitoring", "prometheus-0", "StatefulSet", "prometheus"),
		ownedPod("apps", "worker-0", "ReplicaSet", "worker-0-7d8f9c6b5"),
		{ObjectMeta: metav1.ObjectMeta{Name: "bare-pod", Namespace: "default"}}, // no owner
	}
	got := podWorkloadMap(pods)

	if ref, ok := got["monitoring/prometheus-0"]; !ok || ref != (models.WorkloadRef{Name: "prometheus", Kind: "StatefulSet", Namespace: "monitoring"}) {
		t.Errorf("monitoring/prometheus-0 = %+v, ok=%v; want prometheus/StatefulSet", ref, ok)
	}
	// This is the exact naming-collision scenario: a Deployment whose
	// rolled-up name happens to be "worker-0" (i.e. the Deployment itself
	// is literally named "worker-0", pod suffixed by its own ReplicaSet
	// hash). The map must record the CONFIRMED owner ("worker-0",
	// Deployment) for this pod, distinct from any unrelated StatefulSet
	// that might also be named "worker" in the same namespace — nothing
	// in this map depends on name comparison at lookup time.
	if ref, ok := got["apps/worker-0"]; !ok || ref.Kind != "Deployment" || ref.Name != "worker-0" {
		t.Errorf("apps/worker-0 = %+v, ok=%v; want Deployment/worker-0", ref, ok)
	}
	if _, ok := got["default/bare-pod"]; ok {
		t.Error("bare pod with no owner should not appear in the map")
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries (bare pod excluded), got %d: %+v", len(got), got)
	}
}
