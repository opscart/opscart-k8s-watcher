package analyzer

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testPVC(namespace, name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-30 * 24 * time.Hour)),
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// PVCSnapshot lets other scan-pipeline consumers (Node Optimization storage
// evidence) reuse the PVC list AuditWaste already fetched, instead of
// listing PersistentVolumeClaims again.
func TestWasteAuditorPVCSnapshotReusesClusterWideFetch(t *testing.T) {
	pvcA := testPVC("app", "data-a")
	pvcB := testPVC("other", "data-b")
	wa := newTestAuditor(7, pvcA, pvcB)

	if _, err := wa.AuditWaste(""); err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	snapshot := wa.PVCSnapshot()
	if len(snapshot) != 2 {
		t.Fatalf("PVCSnapshot() len = %d, want 2", len(snapshot))
	}
	names := map[string]bool{}
	for _, pvc := range snapshot {
		names[pvc.Namespace+"/"+pvc.Name] = true
	}
	if !names["app/data-a"] || !names["other/data-b"] {
		t.Fatalf("PVCSnapshot() missing expected PVCs: %+v", snapshot)
	}

	// Exactly one PersistentVolumeClaim LIST must have happened — the
	// snapshot is a read of already-fetched data, not a second fetch.
	count := 0
	for _, action := range wa.clientset.(*fake.Clientset).Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "persistentvolumeclaims" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("PersistentVolumeClaim LIST count = %d, want 1 (PVCSnapshot must not trigger another list)", count)
	}
}

// A namespace-scoped AuditWaste call does not populate the cluster-wide
// snapshot — mirrors WithPodSnapshot's "only genuinely cluster-wide" rule,
// so a partial-scope fetch is never mistaken for the full cluster picture.
func TestWasteAuditorPVCSnapshotEmptyForNamespaceScopedAudit(t *testing.T) {
	pvc := testPVC("app", "data-a")
	wa := newTestAuditor(7, pvc)

	if _, err := wa.AuditWaste("app"); err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	if snapshot := wa.PVCSnapshot(); len(snapshot) != 0 {
		t.Fatalf("PVCSnapshot() = %+v, want empty for a namespace-scoped audit", snapshot)
	}
}

// PVCSnapshot returns a defensive copy; mutating it must not corrupt the
// auditor's retained snapshot (mirrors ResourceAnalyzer.PodSnapshot()).
func TestWasteAuditorPVCSnapshotIsDefensiveCopy(t *testing.T) {
	pvc := testPVC("app", "data-a")
	wa := newTestAuditor(7, pvc)
	if _, err := wa.AuditWaste(""); err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	snapshot := wa.PVCSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("PVCSnapshot() len = %d, want 1", len(snapshot))
	}
	snapshot[0].Name = "mutated"

	again := wa.PVCSnapshot()
	if again[0].Name != "data-a" {
		t.Fatalf("PVCSnapshot() was mutated by caller: %+v", again)
	}
}
