package analyzer

import (
	"context"
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
