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
