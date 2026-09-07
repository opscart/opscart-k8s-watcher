package analyzer

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// newTestAuditor creates a WasteAuditor backed by a fake Kubernetes client.
func newTestAuditor(minAgeDays int, objs ...interface{}) *WasteAuditor {
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o.(runtime.Object))
	}
	return &WasteAuditor{
		clientset:  fake.NewSimpleClientset(runtimeObjs...),
		minAgeDays: minAgeDays,
		ctx:        context.Background(),
	}
}

func oldNamespace(name string, ageDays int) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)),
	}}
}

func namespacePod(namespace, name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Status: corev1.PodStatus{Phase: phase}}
}

func runAbandonedNamespaceDetector(t *testing.T, auditor *WasteAuditor) []AbandonedNamespace {
	t.Helper()
	audit := &WasteAudit{}
	if err := auditor.detectAbandonedNamespaces(audit, ""); err != nil {
		t.Fatalf("detectAbandonedNamespaces: %v", err)
	}
	return audit.AbandonedNamespaces
}

func TestAbandonedNamespacesSharedSnapshotMatchesLegacy(t *testing.T) {
	objects := []interface{}{
		oldNamespace("empty", 30), oldNamespace("stopped", 20), oldNamespace("active", 15),
		namespacePod("stopped", "failed", corev1.PodFailed),
		namespacePod("stopped", "pending", corev1.PodPending),
		namespacePod("active", "running", corev1.PodRunning),
	}
	legacy := runAbandonedNamespaceDetector(t, newTestAuditor(7, objects...))
	sharedAuditor := newTestAuditor(7, objects...)
	sharedAuditor.WithPodSnapshot([]corev1.Pod{
		*objects[3].(*corev1.Pod), *objects[4].(*corev1.Pod), *objects[5].(*corev1.Pod),
	}, true)
	shared := runAbandonedNamespaceDetector(t, sharedAuditor)
	if !reflect.DeepEqual(shared, legacy) {
		t.Fatalf("shared snapshot changed abandoned namespaces:\nshared=%+v\nlegacy=%+v", shared, legacy)
	}
}

func TestAbandonedNamespacesSharedSnapshotPreservesEmptyOldNamespace(t *testing.T) {
	wa := newTestAuditor(7, oldNamespace("empty", 30))
	wa.WithPodSnapshot([]corev1.Pod{}, true)
	got := runAbandonedNamespaceDetector(t, wa)
	if len(got) != 1 || got[0].Name != "empty" || got[0].PodCount != 0 || !strings.Contains(got[0].Reason, "No pods found") {
		t.Fatalf("empty old namespace changed: %+v", got)
	}
}

func TestAbandonedNamespacesSharedSnapshotExcludesNamespaceWithRunningPods(t *testing.T) {
	wa := newTestAuditor(7, oldNamespace("active", 30))
	wa.WithPodSnapshot([]corev1.Pod{*namespacePod("active", "api", corev1.PodRunning)}, true)
	if got := runAbandonedNamespaceDetector(t, wa); len(got) != 0 {
		t.Fatalf("namespace with running Pods reported abandoned: %+v", got)
	}
}

func TestAbandonedNamespacesSharedSnapshotIncludesOnlyNonRunningPods(t *testing.T) {
	wa := newTestAuditor(7, oldNamespace("stopped", 30))
	wa.WithPodSnapshot([]corev1.Pod{
		*namespacePod("stopped", "failed", corev1.PodFailed),
		*namespacePod("stopped", "pending", corev1.PodPending),
	}, true)
	got := runAbandonedNamespaceDetector(t, wa)
	if len(got) != 1 || got[0].PodCount != 2 || !strings.Contains(got[0].Reason, "none are in Running phase") {
		t.Fatalf("non-running Pod classification changed: %+v", got)
	}
}

func TestAbandonedNamespacesSharedSnapshotPreservesEligibilityFiltering(t *testing.T) {
	wa := newTestAuditor(7,
		oldNamespace("eligible", 30), oldNamespace("default", 30),
		oldNamespace("kube-system", 30), oldNamespace("young", 2),
	)
	wa.WithPodSnapshot([]corev1.Pod{}, true)
	got := runAbandonedNamespaceDetector(t, wa)
	if len(got) != 1 || got[0].Name != "eligible" {
		t.Fatalf("namespace eligibility filtering changed: %+v", got)
	}
}

func TestAbandonedNamespacesClusterWideSnapshotPerformsNoPodLists(t *testing.T) {
	wa := newTestAuditor(7, oldNamespace("one", 30), oldNamespace("two", 30))
	wa.WithPodSnapshot([]corev1.Pod{}, true)
	runAbandonedNamespaceDetector(t, wa)
	for _, action := range wa.clientset.(*fake.Clientset).Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			t.Fatalf("unexpected Pod LIST for namespace %q", action.GetNamespace())
		}
	}
}

func TestAbandonedNamespacesNamespaceScopedSnapshotFallsBackToPodLists(t *testing.T) {
	apiPod := namespacePod("stopped", "failed", corev1.PodFailed)
	wa := newTestAuditor(7, oldNamespace("stopped", 30), apiPod)
	// This deliberately conflicts with the API Pod. A namespace-scoped
	// snapshot must be ignored, leaving the legacy request and result intact.
	wa.WithPodSnapshot([]corev1.Pod{*namespacePod("stopped", "running", corev1.PodRunning)}, false)
	got := runAbandonedNamespaceDetector(t, wa)
	if len(got) != 1 || got[0].PodCount != 1 {
		t.Fatalf("namespace-scoped snapshot did not use legacy result: %+v", got)
	}
	podLists := 0
	for _, action := range wa.clientset.(*fake.Clientset).Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			podLists++
		}
	}
	if podLists != 1 {
		t.Fatalf("Pod LIST calls = %d, want 1", podLists)
	}
}

func countPodLists(actions []ktesting.Action) int {
	count := 0
	for _, action := range actions {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			count++
		}
	}
	return count
}

func TestWasteClusterWideSnapshotAvoidsThreeDetectorPodLists(t *testing.T) {
	pod := servicePod("api-1", "app", map[string]string{"app": "api"}, corev1.PodRunning, true)
	wa := newTestAuditor(7, pod, oldService("api", "app", map[string]string{"app": "api"}, corev1.ServiceTypeClusterIP))
	wa.WithPodSnapshot([]corev1.Pod{*pod}, true)
	if _, err := wa.AuditWaste(""); err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}
	if got := countPodLists(wa.clientset.(*fake.Clientset).Actions()); got != 0 {
		t.Fatalf("Pod LISTs with cluster-wide snapshot = %d, want 0", got)
	}
}

func TestWasteSharedPodSnapshotPreservesDetectorResults(t *testing.T) {
	t.Run("stale pods", func(t *testing.T) {
		pod := crashLoopPod("api", "app")
		event := probeFailureEvent("api.probe", "api", "app")
		legacyAudit := &WasteAudit{}
		if err := newTestAuditor(1, pod, event).detectStalePods(legacyAudit, ""); err != nil {
			t.Fatalf("legacy stale pods: %v", err)
		}
		sharedAuditor := newTestAuditor(1, event)
		sharedAuditor.WithPodSnapshot([]corev1.Pod{*pod}, true)
		sharedAudit := &WasteAudit{}
		if err := sharedAuditor.detectStalePods(sharedAudit, ""); err != nil {
			t.Fatalf("shared stale pods: %v", err)
		}
		if !reflect.DeepEqual(sharedAudit.StalePods, legacyAudit.StalePods) {
			t.Fatalf("shared snapshot changed stale Pods:\nshared=%+v\nlegacy=%+v", sharedAudit.StalePods, legacyAudit.StalePods)
		}
	})

	t.Run("orphaned PVCs", func(t *testing.T) {
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app", CreationTimestamp: metav1.NewTime(time.Now().Add(-30 * 24 * time.Hour))}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
		pod := namespacePod("other", "api", corev1.PodRunning)
		pod.Spec.Volumes = []corev1.Volume{{VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
		legacyAudit := &WasteAudit{}
		if err := newTestAuditor(7, pvc, pod).detectOrphanedPVCs(legacyAudit, ""); err != nil {
			t.Fatalf("legacy PVCs: %v", err)
		}
		sharedAuditor := newTestAuditor(7, pvc)
		sharedAuditor.WithPodSnapshot([]corev1.Pod{*pod}, true)
		sharedAudit := &WasteAudit{}
		if err := sharedAuditor.detectOrphanedPVCs(sharedAudit, ""); err != nil {
			t.Fatalf("shared PVCs: %v", err)
		}
		if !reflect.DeepEqual(sharedAudit.OrphanedPVCs, legacyAudit.OrphanedPVCs) {
			t.Fatalf("shared snapshot changed orphaned PVCs:\nshared=%+v\nlegacy=%+v", sharedAudit.OrphanedPVCs, legacyAudit.OrphanedPVCs)
		}
	})

	t.Run("orphaned services", func(t *testing.T) {
		service := oldService("api", "app", map[string]string{"app": "api"}, corev1.ServiceTypeClusterIP)
		pod := servicePod("api-1", "other", map[string]string{"app": "api"}, corev1.PodRunning, true)
		legacyAudit := &WasteAudit{}
		if err := newTestAuditor(7, service, pod).detectOrphanedServices(legacyAudit, ""); err != nil {
			t.Fatalf("legacy services: %v", err)
		}
		sharedAuditor := newTestAuditor(7, service)
		sharedAuditor.WithPodSnapshot([]corev1.Pod{*pod}, true)
		sharedAudit := &WasteAudit{}
		if err := sharedAuditor.detectOrphanedServices(sharedAudit, ""); err != nil {
			t.Fatalf("shared services: %v", err)
		}
		if !reflect.DeepEqual(sharedAudit.OrphanedServices, legacyAudit.OrphanedServices) {
			t.Fatalf("shared snapshot changed orphaned Services:\nshared=%+v\nlegacy=%+v", sharedAudit.OrphanedServices, legacyAudit.OrphanedServices)
		}
	})
}

func TestWastePodSnapshotFallbackPaths(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*WasteAuditor)
	}{
		{name: "absent"},
		{name: "namespace scoped", configure: func(wa *WasteAuditor) {
			wa.WithPodSnapshot([]corev1.Pod{*namespacePod("app", "snapshot-only", corev1.PodRunning)}, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wa := newTestAuditor(7)
			if tc.configure != nil {
				tc.configure(wa)
			}
			if _, err := wa.AuditWaste(""); err != nil {
				t.Fatalf("AuditWaste: %v", err)
			}
			if got := countPodLists(wa.clientset.(*fake.Clientset).Actions()); got != 3 {
				t.Fatalf("legacy Pod LISTs = %d, want 3", got)
			}
		})
	}
}

func TestWastePodListErrorsPreserveFallbackWarnings(t *testing.T) {
	wa := newTestAuditor(7)
	wa.clientset.(*fake.Clientset).PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	audit, err := wa.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}
	warnings := make(map[string]bool)
	for _, warning := range audit.DetectorWarnings {
		warnings[warning.Category] = true
	}
	for _, category := range []string{"Zombie and idle/unmanaged pods", "Unattached PVC candidates", "Orphaned services"} {
		if !warnings[category] {
			t.Errorf("missing legacy detector warning %q: %+v", category, audit.DetectorWarnings)
		}
	}
	if got := countPodLists(wa.clientset.(*fake.Clientset).Actions()); got != 3 {
		t.Fatalf("failed legacy Pod LISTs = %d, want 3", got)
	}
}

func TestAuditWastePreservesDetectorWarnings(t *testing.T) {
	wa := newTestAuditor(7)
	wa.clientset.(*fake.Clientset).PrependReactor("list", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	audit, err := wa.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}
	for _, warning := range audit.DetectorWarnings {
		if warning.Category == "Unattached PVC candidates" {
			return
		}
	}
	t.Fatalf("PVC detector warning not preserved: %+v", audit.DetectorWarnings)
}

func oldService(name, namespace string, selector map[string]string, serviceType corev1.ServiceType) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-30 * 24 * time.Hour)},
		},
		Spec: corev1.ServiceSpec{Selector: selector, Type: serviceType},
	}
}

func servicePod(name, namespace string, podLabels map[string]string, phase corev1.PodPhase, ready bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: podLabels},
		Status: corev1.PodStatus{
			Phase: phase,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: ready,
			}},
		},
	}
}

func TestOrphanedServiceUsesNamespaceLocalSelectorMatches(t *testing.T) {
	tests := []struct {
		name      string
		service   *corev1.Service
		pods      []*corev1.Pod
		wantCount int
	}{
		{
			name:      "selector matches no pods",
			service:   oldService("api", "app", map[string]string{"app": "api"}, corev1.ServiceTypeClusterIP),
			wantCount: 1,
		},
		{
			name:    "selector matches healthy pod",
			service: oldService("api", "app", map[string]string{"app": "api"}, corev1.ServiceTypeClusterIP),
			pods: []*corev1.Pod{
				servicePod("api-1", "app", map[string]string{"app": "api"}, corev1.PodRunning, true),
			},
		},
		{
			name:    "selector matches unhealthy pod",
			service: oldService("api", "app", map[string]string{"app": "api"}, corev1.ServiceTypeClusterIP),
			pods: []*corev1.Pod{
				servicePod("api-1", "app", map[string]string{"app": "api"}, corev1.PodFailed, false),
			},
		},
		{
			name:    "selectorless Service",
			service: oldService("manual", "app", nil, corev1.ServiceTypeClusterIP),
		},
		{
			name:    "ExternalName Service",
			service: oldService("external", "app", map[string]string{"app": "api"}, corev1.ServiceTypeExternalName),
		},
		{
			name:      "matching pod in another namespace does not count",
			service:   oldService("api", "app", map[string]string{"app": "api"}, corev1.ServiceTypeClusterIP),
			pods:      []*corev1.Pod{servicePod("api-1", "other", map[string]string{"app": "api"}, corev1.PodRunning, true)},
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []interface{}{tt.service}
			for _, pod := range tt.pods {
				objs = append(objs, pod)
			}
			wa := newTestAuditor(7, objs...)
			audit := &WasteAudit{}
			if err := wa.detectOrphanedServices(audit, ""); err != nil {
				t.Fatalf("detectOrphanedServices: %v", err)
			}
			if got := len(audit.OrphanedServices); got != tt.wantCount {
				t.Fatalf("orphan candidates = %d, want %d: %+v", got, tt.wantCount, audit.OrphanedServices)
			}
		})
	}
}

func TestPVCReferenceEvidenceIsNamespaceScopedAndPhaseAgnostic(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "data",
			Namespace:         "app",
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-30 * 24 * time.Hour)},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	otherNamespacePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "other"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
		}}},
	}
	wa := newTestAuditor(7, pvc, otherNamespacePod)
	audit := &WasteAudit{}
	if err := wa.detectOrphanedPVCs(audit, ""); err != nil {
		t.Fatalf("detectOrphanedPVCs: %v", err)
	}
	if len(audit.OrphanedPVCs) != 1 {
		t.Fatalf("candidates = %+v, want namespace-isolated PVC candidate", audit.OrphanedPVCs)
	}
	reason := audit.OrphanedPVCs[0].Reason
	if !strings.Contains(reason, `no currently listed pod in namespace "app" references it`) {
		t.Fatalf("reason lacks namespace-local evidence: %q", reason)
	}
	for _, forbidden := range []string{"running pod", "checked 1 pods in namespace"} {
		if strings.Contains(strings.ToLower(reason), forbidden) {
			t.Fatalf("reason contains unsupported evidence %q: %q", forbidden, reason)
		}
	}

	referencingFailedPod := otherNamespacePod.DeepCopy()
	referencingFailedPod.Name = "stopped"
	referencingFailedPod.Namespace = "app"
	referencingFailedPod.Status.Phase = corev1.PodFailed
	wa = newTestAuditor(7, pvc, referencingFailedPod)
	audit = &WasteAudit{}
	if err := wa.detectOrphanedPVCs(audit, "app"); err != nil {
		t.Fatalf("detectOrphanedPVCs: %v", err)
	}
	if len(audit.OrphanedPVCs) != 0 {
		t.Fatalf("non-running referencing pod must count as a reference: %+v", audit.OrphanedPVCs)
	}
}

func TestZombieBypassesMinAge(t *testing.T) {
	tests := []struct {
		name          string
		waitingReason string
		wantZombie    bool
	}{
		{"CrashLoopBackOff", "CrashLoopBackOff", true},
		{"OOMKilled", "OOMKilled", true},
		{"ImagePullBackOff", "ImagePullBackOff", true},
		{"Error", "Error", true},
		{"healthy running pod not flagged", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "crash-pod",
					Namespace: "default",
					// Age = 0 — well below minAgeDays=7
					CreationTimestamp: metav1.Now(),
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			}

			if tc.waitingReason != "" {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{
					{
						Name:         "app",
						RestartCount: 5,
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{
								Reason: tc.waitingReason,
							},
						},
					},
				}
			}

			wa := newTestAuditor(7, pod)
			audit, err := wa.AuditWaste("")
			if err != nil {
				t.Fatalf("AuditWaste: %v", err)
			}

			found := false
			for _, sp := range audit.StalePods {
				if sp.Kind == StalePodZombie && sp.Name == "crash-pod" {
					found = true
					break
				}
			}
			if found != tc.wantZombie {
				t.Errorf("zombie found = %v, want %v; StalePods = %+v", found, tc.wantZombie, audit.StalePods)
			}
		})
	}
}

func TestDefaultNamespaceNotSkipped(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-crasher",
			Namespace: "default",
			// 2 days old — above minAgeDays=1 but the zombie path ignores age anyway
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-48 * time.Hour)},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 20,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
				},
			},
		},
	}

	wa := newTestAuditor(1, pod)
	audit, err := wa.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	found := false
	for _, sp := range audit.StalePods {
		if sp.Namespace == "default" && sp.Name == "default-crasher" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("pod in 'default' namespace not detected; StalePods = %+v", audit.StalePods)
	}
}

// crashLoopPod builds a pod in Waiting/CrashLoopBackOff state with a high
// restart count and a not-ready container — the state a probe-killed pod
// lands in when a scan happens to catch it mid-restart.
func crashLoopPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-48 * time.Hour)},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 20,
					Ready:        false,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
				},
			},
		},
	}
}

// runningNotReadyPod builds a pod caught mid-Running with a high restart
// count and a not-ready container — the alternate state the same
// probe-killed pod flickers into on the next scan.
func runningNotReadyPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-48 * time.Hour)},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 20,
					Ready:        false,
				},
			},
		},
	}
}

// probeFailureEvent builds a Pod event whose message matches the
// probe-failure signature (mirrors hasProbeFailureSignature's matched
// strings in cmd/opscart-scan/emergency.go).
func probeFailureEvent(name, podName, namespace string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      podName,
			Namespace: namespace,
		},
		Message: "Liveness probe failed: HTTP probe failed with statuscode: 500",
		Type:    "Warning",
		Reason:  "Unhealthy",
	}
}

func findStalePod(pods []StalePod, namespace, name string) (StalePod, bool) {
	for _, sp := range pods {
		if sp.Namespace == namespace && sp.Name == name {
			return sp, true
		}
	}
	return StalePod{}, false
}

func TestProbeFailureEventOverridesCrashLoopClassification(t *testing.T) {
	pod := crashLoopPod("stream-processor", "default")
	ev := probeFailureEvent("stream-processor.probe1", "stream-processor", "default")

	wa := newTestAuditor(1, pod, ev)
	audit, err := wa.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	sp, found := findStalePod(audit.StalePods, "default", "stream-processor")
	if !found {
		t.Fatalf("pod not found in StalePods; got %+v", audit.StalePods)
	}
	if sp.Status != "ProbeFailure" {
		t.Errorf("Status = %q, want %q (event evidence must override instantaneous CrashLoopBackOff state)", sp.Status, "ProbeFailure")
	}
	if strings.Contains(sp.Reason, "process itself is not the problem") {
		t.Fatalf("probe evidence made an unsupported causal claim: %q", sp.Reason)
	}
	if !strings.Contains(sp.Reason, "Kubernetes events show repeated startup/liveness probe failures followed by container restarts") {
		t.Fatalf("probe evidence wording missing observed event semantics: %q", sp.Reason)
	}
}

func TestCrashLoopWithoutProbeEventStaysCrashLoop(t *testing.T) {
	pod := crashLoopPod("real-crasher", "default")

	wa := newTestAuditor(1, pod)
	audit, err := wa.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	sp, found := findStalePod(audit.StalePods, "default", "real-crasher")
	if !found {
		t.Fatalf("pod not found in StalePods; got %+v", audit.StalePods)
	}
	if sp.Status != "CrashLoopBackOff" {
		t.Errorf("Status = %q, want %q (no probe-failure evidence, must keep today's behavior)", sp.Status, "CrashLoopBackOff")
	}
}

func TestProbeFailureFallbackWithoutEvents(t *testing.T) {
	pod := runningNotReadyPod("flaky-runner", "default")

	wa := newTestAuditor(1, pod)
	audit, err := wa.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	sp, found := findStalePod(audit.StalePods, "default", "flaky-runner")
	if !found {
		t.Fatalf("pod not found in StalePods; got %+v", audit.StalePods)
	}
	if sp.Status != "ProbeFailure" {
		t.Errorf("Status = %q, want %q (fallback phase-based path with no event evidence)", sp.Status, "ProbeFailure")
	}
}

// TestClassificationDeterministicAcrossPhaseFlicker is the core regression
// test for the incident: a single pod alternating Waiting/CrashLoopBackOff
// and Running from one scan to the next (because a failing probe keeps
// killing it) must classify identically both times once probe-failure
// event evidence is present — otherwise the fingerprint flips and incidents
// churn resolved/reoccurred every scan.
func TestClassificationDeterministicAcrossPhaseFlicker(t *testing.T) {
	ev := probeFailureEvent("stream-processor.probe1", "stream-processor", "default")

	waitingPod := crashLoopPod("stream-processor", "default")
	waWaiting := newTestAuditor(1, waitingPod, ev)
	auditWaiting, err := waWaiting.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste (waiting): %v", err)
	}
	spWaiting, found := findStalePod(auditWaiting.StalePods, "default", "stream-processor")
	if !found {
		t.Fatalf("pod not found in StalePods (waiting run); got %+v", auditWaiting.StalePods)
	}

	runningPod := runningNotReadyPod("stream-processor", "default")
	waRunning := newTestAuditor(1, runningPod, ev)
	auditRunning, err := waRunning.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste (running): %v", err)
	}
	spRunning, found := findStalePod(auditRunning.StalePods, "default", "stream-processor")
	if !found {
		t.Fatalf("pod not found in StalePods (running run); got %+v", auditRunning.StalePods)
	}

	if spWaiting.Status != spRunning.Status {
		t.Errorf("classification flickered across phases: waiting-scan Status=%q, running-scan Status=%q — fingerprint would churn", spWaiting.Status, spRunning.Status)
	}
	if spWaiting.Status != "ProbeFailure" {
		t.Errorf("Status = %q, want %q", spWaiting.Status, "ProbeFailure")
	}
}

func TestPodFailureClassificationUsesLastTerminationOOMAndSharedPriority(t *testing.T) {
	pod := crashLoopPod("oom-worker", "default")
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name:         "oom-sidecar",
		RestartCount: 3,
		Ready:        true,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "OOMKilled",
		}},
	})

	audit, err := newTestAuditor(7, pod).AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}
	sp, found := findStalePod(audit.StalePods, "default", "oom-worker")
	if !found {
		t.Fatalf("pod not found in StalePods; got %+v", audit.StalePods)
	}
	if sp.Status != "OOMKilled" || sp.Severity != "critical" {
		t.Fatalf("got Status=%q Severity=%q, want OOMKilled/critical", sp.Status, sp.Severity)
	}
	if !strings.Contains(sp.Reason, "termination state reports OOMKilled") {
		t.Fatalf("merged OOM evidence message missing: %q", sp.Reason)
	}
}

func TestPodFailureClassificationIndependentOfContainerOrder(t *testing.T) {
	statuses := []corev1.ContainerStatus{
		{
			Name: "puller", RestartCount: 1,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}},
		},
		{
			Name: "worker", RestartCount: 2,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
		},
	}

	classifyOrder := func(name string, containerStatuses []corev1.ContainerStatus) StalePod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: metav1.Now()},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: containerStatuses},
		}
		audit, err := newTestAuditor(7, pod).AuditWaste("")
		if err != nil {
			t.Fatalf("AuditWaste(%s): %v", name, err)
		}
		sp, found := findStalePod(audit.StalePods, "default", name)
		if !found {
			t.Fatalf("pod %s not found in StalePods; got %+v", name, audit.StalePods)
		}
		return sp
	}

	forward := classifyOrder("forward", statuses)
	reverse := classifyOrder("reverse", []corev1.ContainerStatus{statuses[1], statuses[0]})
	if forward.Status != "OOMKilled" || reverse.Status != forward.Status || reverse.Severity != forward.Severity {
		t.Fatalf("classification changed with API container order: forward=%+v reverse=%+v", forward, reverse)
	}
}

func TestImagePullAndHighRestartUseSharedSeverities(t *testing.T) {
	imagePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "image-pod", Namespace: "default", CreationTimestamp: metav1.Now()},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}},
		}}},
	}
	restartPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-pod", Namespace: "default", CreationTimestamp: metav1.Now()},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Ready: true, RestartCount: 12,
		}}},
	}

	audit, err := newTestAuditor(7, imagePod, restartPod).AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}
	imageFinding, imageFound := findStalePod(audit.StalePods, "default", "image-pod")
	restartFinding, restartFound := findStalePod(audit.StalePods, "default", "restart-pod")
	if !imageFound || imageFinding.Status != "ErrImagePull" || imageFinding.Severity != "high" {
		t.Fatalf("image finding = %+v found=%v, want ErrImagePull/high", imageFinding, imageFound)
	}
	if !restartFound || restartFinding.Status != "HighRestartCount" || restartFinding.Severity != "medium" {
		t.Fatalf("restart finding = %+v found=%v, want HighRestartCount/medium", restartFinding, restartFound)
	}
}

func TestEventsFetchedOncePerNamespaceNotPerPod(t *testing.T) {
	ev := probeFailureEvent("shared.probe1", "pod-a", "default")

	wa := newTestAuditor(1,
		crashLoopPod("pod-a", "default"),
		crashLoopPod("pod-b", "default"),
		crashLoopPod("pod-c", "default"),
		ev,
	)

	if _, err := wa.AuditWaste(""); err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}

	fakeClient, ok := wa.clientset.(*fake.Clientset)
	if !ok {
		t.Fatalf("clientset is not *fake.Clientset")
	}

	eventListCalls := 0
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "events" {
			eventListCalls++
		}
	}

	if eventListCalls != 1 {
		t.Errorf("events List called %d times for 3 pods in one namespace, want exactly 1 (must be batched per-namespace, not per-pod)", eventListCalls)
	}
}

func eventListScopes(actions []ktesting.Action) (cluster, namespace int) {
	for _, action := range actions {
		if action.GetVerb() != "list" || action.GetResource().Resource != "events" {
			continue
		}
		if action.GetNamespace() == "" {
			cluster++
		} else {
			namespace++
		}
	}
	return
}

func TestClusterEventSnapshotUsesOneRequestWithRequiredSelector(t *testing.T) {
	wa := newTestAuditor(1, crashLoopPod("pod-a", "app"), crashLoopPod("pod-b", "other"))
	audit := &WasteAudit{}
	if err := wa.detectStalePods(audit, ""); err != nil {
		t.Fatalf("detectStalePods: %v", err)
	}
	client := wa.clientset.(*fake.Clientset)
	cluster, namespace := eventListScopes(client.Actions())
	if cluster != 1 || namespace != 0 {
		t.Fatalf("Event LISTs: cluster=%d namespace=%d, want 1/0", cluster, namespace)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "events" {
			selector := action.(ktesting.ListAction).GetListRestrictions().Fields.String()
			if selector != "involvedObject.kind=Pod,type=Warning" {
				t.Fatalf("aggregate Event selector = %q", selector)
			}
		}
	}
}

func TestClusterEventSnapshotMatchesLegacyResults(t *testing.T) {
	podA := crashLoopPod("api", "payments")
	podB := crashLoopPod("worker", "workers")
	eventA := probeFailureEvent("api.probe", "api", "payments")
	objects := []interface{}{podA, podB, eventA}
	legacyAudit := &WasteAudit{}
	if err := newTestAuditor(1, objects...).detectStalePodsWithClusterEvents(legacyAudit, "", false); err != nil {
		t.Fatalf("legacy detection: %v", err)
	}
	aggregateAudit := &WasteAudit{}
	if err := newTestAuditor(1, objects...).detectStalePodsWithClusterEvents(aggregateAudit, "", true); err != nil {
		t.Fatalf("aggregate detection: %v", err)
	}
	if !reflect.DeepEqual(aggregateAudit.StalePods, legacyAudit.StalePods) {
		t.Fatalf("aggregate Events changed findings:\naggregate=%+v\nlegacy=%+v", aggregateAudit.StalePods, legacyAudit.StalePods)
	}
}

func TestClusterEventSnapshotKeepsPodNamesNamespaceIsolated(t *testing.T) {
	wa := newTestAuditor(1,
		crashLoopPod("api", "with-probe"), crashLoopPod("api", "without-probe"),
		probeFailureEvent("api.probe", "api", "with-probe"),
	)
	audit := &WasteAudit{}
	if err := wa.detectStalePods(audit, ""); err != nil {
		t.Fatalf("detectStalePods: %v", err)
	}
	withProbe, _ := findStalePod(audit.StalePods, "with-probe", "api")
	withoutProbe, _ := findStalePod(audit.StalePods, "without-probe", "api")
	if withProbe.Status != "ProbeFailure" || withoutProbe.Status != "CrashLoopBackOff" {
		t.Fatalf("cross-namespace evidence contamination: with=%+v without=%+v", withProbe, withoutProbe)
	}
}

func TestClusterEventSnapshotExcludesInfraNamespaces(t *testing.T) {
	wa := newTestAuditor(1,
		crashLoopPod("api", "kube-system"),
		probeFailureEvent("api.probe", "api", "kube-system"),
	)
	audit := &WasteAudit{}
	if err := wa.detectStalePods(audit, ""); err != nil {
		t.Fatalf("detectStalePods: %v", err)
	}
	if len(audit.StalePods) != 0 {
		t.Fatalf("infrastructure Pod was analyzed: %+v", audit.StalePods)
	}
}

func TestProbeFailureEvidenceFiltersEventKindTypeAndMessage(t *testing.T) {
	matching := probeFailureEvent("matching", "matching", "app")
	secondSignature := probeFailureEvent("second", "second", "app")
	secondSignature.Message = "Startup probe, will be restarted after failure"
	normal := probeFailureEvent("normal", "normal", "app")
	normal.Type = corev1.EventTypeNormal
	nonPod := probeFailureEvent("node", "node", "app")
	nonPod.InvolvedObject.Kind = "Node"
	nonMatching := probeFailureEvent("other", "other", "app")
	nonMatching.Message = "Container image pull failed"
	got := probeFailurePodsFromEvents([]corev1.Event{*matching, *secondSignature, *normal, *nonPod, *nonMatching})
	if !got["matching"] || !got["second"] {
		t.Fatalf("probe signatures not recognized: %+v", got)
	}
	for _, excluded := range []string{"normal", "node", "other"} {
		if got[excluded] {
			t.Fatalf("Event %q incorrectly supplied probe evidence: %+v", excluded, got)
		}
	}
}

func TestClusterEventSnapshotNoMatchingEventsLeavesEvidenceEmpty(t *testing.T) {
	wa := newTestAuditor(1, crashLoopPod("api", "app"))
	audit := &WasteAudit{}
	if err := wa.detectStalePods(audit, ""); err != nil {
		t.Fatalf("detectStalePods: %v", err)
	}
	finding, ok := findStalePod(audit.StalePods, "app", "api")
	if !ok || finding.Status != "CrashLoopBackOff" {
		t.Fatalf("empty Event evidence changed classification: %+v", audit.StalePods)
	}
}

func TestProbeFailureEvidenceIgnoresDuplicatesCountOrderAndSeries(t *testing.T) {
	event := probeFailureEvent("api.probe", "api", "app")
	event.Count = 37
	event.Series = &corev1.EventSeries{Count: 91, LastObservedTime: metav1.MicroTime{Time: time.Now()}}
	duplicate := event.DeepCopy()
	duplicate.Name = "api.probe.duplicate"
	forward := probeFailurePodsFromEvents([]corev1.Event{*event, *duplicate})
	reverse := probeFailurePodsFromEvents([]corev1.Event{*duplicate, *event})
	if !reflect.DeepEqual(forward, map[string]bool{"api": true}) || !reflect.DeepEqual(reverse, forward) {
		t.Fatalf("count/order/series affected boolean evidence: forward=%+v reverse=%+v", forward, reverse)
	}
}

func TestClusterEventListFailureFallsBackToNamespaceLists(t *testing.T) {
	wa := newTestAuditor(1, crashLoopPod("api", "one"), crashLoopPod("worker", "two"))
	wa.clientset.(*fake.Clientset).PrependReactor("list", "events", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "" {
			return true, nil, context.DeadlineExceeded
		}
		return false, nil, nil
	})
	audit := &WasteAudit{}
	if err := wa.detectStalePods(audit, ""); err != nil {
		t.Fatalf("fallback detection: %v", err)
	}
	cluster, namespace := eventListScopes(wa.clientset.(*fake.Clientset).Actions())
	if cluster != 1 || namespace != 2 {
		t.Fatalf("fallback Event LISTs: cluster=%d namespace=%d, want 1/2", cluster, namespace)
	}
}

func TestClusterEventFallbackPreservesPartialFindingsAndWarning(t *testing.T) {
	goodEvent := probeFailureEvent("api.probe", "api", "good")
	wa := newTestAuditor(1, crashLoopPod("api", "good"), crashLoopPod("api", "broken"), goodEvent)
	wa.clientset.(*fake.Clientset).PrependReactor("list", "events", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "" || action.GetNamespace() == "broken" {
			return true, nil, context.DeadlineExceeded
		}
		return false, nil, nil
	})
	audit, err := wa.AuditWaste("")
	if err != nil {
		t.Fatalf("AuditWaste: %v", err)
	}
	good, goodFound := findStalePod(audit.StalePods, "good", "api")
	broken, brokenFound := findStalePod(audit.StalePods, "broken", "api")
	if !goodFound || good.Status != "ProbeFailure" || !brokenFound || broken.Status != "CrashLoopBackOff" {
		t.Fatalf("fallback partial findings changed: good=%+v broken=%+v", good, broken)
	}
	foundWarning := false
	for _, warning := range audit.DetectorWarnings {
		if warning.Category == "Zombie and idle/unmanaged pods" && strings.Contains(warning.Error, `namespace "broken"`) {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("fallback detector warning missing: %+v", audit.DetectorWarnings)
	}
}

// ── Active-vs-age-gated malfunction detection ──────────────────────────────
//
// Currently-broken resources (Ingress backends with no ready endpoints,
// HPAs reporting ScalingActive=False) must be reported immediately, not
// hidden behind minAgeDays. Age-gating remains correct for findings
// inferred from a sustained pattern (AlwaysAtMin), which needs time to
// become a meaningful signal. The Service-availability detector (matched
// pods with no ready endpoints) was deliberately deferred to a separate PR
// pending a batched EndpointSlice query design — see PR discussion on
// per-Service API call cost at scale.

func TestFormatDurationSinceSubDay(t *testing.T) {
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "1m"},
		{45 * time.Minute, "45m"},
		{2 * time.Hour, "2h"},
		{25 * time.Hour, "1d"},
	}
	for _, c := range cases {
		got := formatDurationSince(time.Now().Add(-c.ago))
		if got != c.want {
			t.Errorf("formatDurationSince(-%s) = %q, want %q", c.ago, got, c.want)
		}
	}
}

func ingressFixture(name, namespace, host, svcName string, ageDays int) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: svcName,
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
}

func TestBrokenIngressNotAgeGated(t *testing.T) {
	// Ingress created today (age 0), backend has zero ready endpoints.
	// Before the fix, minAgeDays=7 hid this for a week.
	ing := ingressFixture("checkout", "shop", "checkout.example.com", "checkout-svc", 0)
	svc := oldService("checkout-svc", "shop", map[string]string{"app": "checkout"}, corev1.ServiceTypeClusterIP)
	svc.CreationTimestamp = metav1.Time{Time: time.Now()}

	wa := newTestAuditor(7, ing, svc)
	audit := &WasteAudit{}
	if err := wa.detectBrokenIngresses(audit, ""); err != nil {
		t.Fatalf("detectBrokenIngresses: %v", err)
	}
	if len(audit.BrokenIngresses) != 1 {
		t.Fatalf("broken ingresses = %d, want 1 (must not be age-gated): %+v", len(audit.BrokenIngresses), audit.BrokenIngresses)
	}
	got := audit.BrokenIngresses[0]
	if !got.IsActive {
		t.Errorf("broken ingress finding should be marked IsActive")
	}
	if strings.Contains(got.Reason, "will fail") || strings.Contains(got.Reason, "will return errors") {
		t.Errorf("reason overclaims an observed request outcome: %q", got.Reason)
	}
}

func TestBrokenIngressEndpointCheckErrorSurfacesAsWarningNotFalsePositive(t *testing.T) {
	ing := ingressFixture("checkout", "shop", "checkout.example.com", "checkout-svc", 0)
	svc := oldService("checkout-svc", "shop", map[string]string{"app": "checkout"}, corev1.ServiceTypeClusterIP)

	wa := newTestAuditor(7, ing, svc)
	wa.clientset.(*fake.Clientset).PrependReactor("list", "endpointslices", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	audit := &WasteAudit{}
	if err := wa.detectBrokenIngresses(audit, ""); err != nil {
		t.Fatalf("detectBrokenIngresses: %v", err)
	}
	if len(audit.BrokenIngresses) != 0 {
		t.Fatalf("an EndpointSlice API error must not fabricate a broken-ingress finding: %+v", audit.BrokenIngresses)
	}
	found := false
	for _, w := range audit.DetectorWarnings {
		if w.Category == "Broken ingresses" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a detector warning for the endpoint-check failure, got: %+v", audit.DetectorWarnings)
	}
}

func hpaFixture(name, namespace string, ageDays int, minReplicas, currentReplicas, desiredReplicas int32, conditions []autoscalingv2.HorizontalPodAutoscalerCondition) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "checkout"},
			MinReplicas:    &minReplicas,
			MaxReplicas:    10,
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			CurrentReplicas: currentReplicas,
			DesiredReplicas: desiredReplicas,
			Conditions:      conditions,
		},
	}
}

func TestHPAScalingInactiveNotAgeGated(t *testing.T) {
	hpa := hpaFixture("checkout-hpa", "shop", 0, 1, 1, 1, []autoscalingv2.HorizontalPodAutoscalerCondition{
		{Type: "ScalingActive", Status: "False", Reason: "FailedGetResourceMetric", Message: "unable to get metrics",
			LastTransitionTime: metav1.Time{Time: time.Now().Add(-2 * time.Hour)}},
	})
	wa := newTestAuditor(7, hpa)
	audit := &WasteAudit{}
	if err := wa.detectMisconfiguredHPAs(audit, ""); err != nil {
		t.Fatalf("detectMisconfiguredHPAs: %v", err)
	}
	if len(audit.MisconfiguredHPAs) != 1 {
		t.Fatalf("misconfigured HPAs = %d, want 1 (ScalingActive=False must not be age-gated): %+v", len(audit.MisconfiguredHPAs), audit.MisconfiguredHPAs)
	}
	got := audit.MisconfiguredHPAs[0]
	if !got.IsActive {
		t.Errorf("ScalingActive=False finding should be marked IsActive")
	}
	if !strings.Contains(got.Reason, "for 2h") {
		t.Errorf("reason should report real failure duration (2h), got: %q", got.Reason)
	}
	if strings.Contains(got.Reason, "inactive for 0 days") {
		t.Errorf("reason must not substitute object age for failure duration: %q", got.Reason)
	}
}

func TestHPAAlwaysAtMinStaysAgeGatedAndDoesNotOverclaimHistory(t *testing.T) {
	hpa := hpaFixture("checkout-hpa", "shop", 35, 1, 1, 1, nil)
	wa := newTestAuditor(7, hpa)
	audit := &WasteAudit{}
	if err := wa.detectMisconfiguredHPAs(audit, ""); err != nil {
		t.Fatalf("detectMisconfiguredHPAs: %v", err)
	}
	if len(audit.MisconfiguredHPAs) != 1 {
		t.Fatalf("misconfigured HPAs = %d, want 1: %+v", len(audit.MisconfiguredHPAs), audit.MisconfiguredHPAs)
	}
	got := audit.MisconfiguredHPAs[0]
	if got.IsActive {
		t.Errorf("AlwaysAtMin is a tuning candidate, not an active failure")
	}
	if strings.Contains(got.Reason, "never scaled up") {
		t.Errorf("reason overclaims history the detector never observed: %q", got.Reason)
	}
}

func TestHPAAlwaysAtMinSuppressedWhenScalingActiveAlreadyFired(t *testing.T) {
	hpa := hpaFixture("checkout-hpa", "shop", 35, 1, 1, 1, []autoscalingv2.HorizontalPodAutoscalerCondition{
		{Type: "ScalingActive", Status: "False", Reason: "FailedGetResourceMetric", Message: "unable to get metrics"},
	})
	wa := newTestAuditor(7, hpa)
	audit := &WasteAudit{}
	if err := wa.detectMisconfiguredHPAs(audit, ""); err != nil {
		t.Fatalf("detectMisconfiguredHPAs: %v", err)
	}
	if len(audit.MisconfiguredHPAs) != 1 {
		t.Fatalf("expected exactly one finding for one broken HPA (no AlwaysAtMin duplicate), got %d: %+v", len(audit.MisconfiguredHPAs), audit.MisconfiguredHPAs)
	}
	if audit.MisconfiguredHPAs[0].Condition == "AlwaysAtMin" {
		t.Errorf("ScalingActive=False finding should take precedence, not AlwaysAtMin")
	}
}

func TestHPAYoungAndHealthyProducesNoFinding(t *testing.T) {
	hpa := hpaFixture("checkout-hpa", "shop", 1, 1, 3, 3, nil)
	wa := newTestAuditor(7, hpa)
	audit := &WasteAudit{}
	if err := wa.detectMisconfiguredHPAs(audit, ""); err != nil {
		t.Fatalf("detectMisconfiguredHPAs: %v", err)
	}
	if len(audit.MisconfiguredHPAs) != 0 {
		t.Fatalf("healthy scaling HPA should produce no finding: %+v", audit.MisconfiguredHPAs)
	}
}

// TestSplitMisconfiguredHPAs guards the CLI summary-labeling fix: active
// failures (ScalingActive=False) must be counted separately from tuning
// candidates (AlwaysAtMin), so a live autoscaling failure never gets
// silently folded into a "warning" bucket or shown with a green scorecard
// emoji next to other, merely-worth-reviewing findings.
func TestSplitMisconfiguredHPAs(t *testing.T) {
	hpas := []MisconfiguredHPA{
		{Name: "a", IsActive: true},
		{Name: "b", IsActive: false},
		{Name: "c", IsActive: true},
		{Name: "d", IsActive: false},
		{Name: "e", IsActive: false},
	}
	active, tuning := splitMisconfiguredHPAs(hpas)
	if active != 2 {
		t.Errorf("active = %d, want 2", active)
	}
	if tuning != 3 {
		t.Errorf("tuning = %d, want 3", tuning)
	}
}

func TestSplitMisconfiguredHPAsEmpty(t *testing.T) {
	active, tuning := splitMisconfiguredHPAs(nil)
	if active != 0 || tuning != 0 {
		t.Errorf("splitMisconfiguredHPAs(nil) = (%d, %d), want (0, 0)", active, tuning)
	}
}
