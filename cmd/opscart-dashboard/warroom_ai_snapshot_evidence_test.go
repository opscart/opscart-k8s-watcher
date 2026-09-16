package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWarRoomAIPodEvidenceAllowsOnlySanitizedFields(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	pod, events, forbidden := richWarRoomAIPodFixture(now)
	scan, db := warRoomAICrashFixture("prod", 17)
	scan.aiPodEvidence = buildWarRoomAIPodEvidenceIndex(
		[]*corev1.Pod{pod}, events, true, true, now,
	)
	selection := collectWarRoomAISelections(scan, "prod", db)[0]
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(capture.Request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, sentinel := range forbidden {
		if strings.Contains(text, sentinel) {
			t.Fatalf("serialized AnalysisRequest contains forbidden sentinel %q: %s", sentinel, text)
		}
	}
	for _, allowed := range []string{
		"pod_phase=Running",
		"target_container=app",
		"container=app; ready=false; current_state=waiting; waiting_reason=CrashLoopBackOff",
		"last_termination_reason=Error; last_termination_exit_code=137; restart_count=17",
		"cpu_request=250m; memory_request=128Mi; cpu_limit=1; memory_limit=512Mi",
		"liveness_probe=exec; readiness_probe=http_get; startup_probe=grpc",
		"reason=Unhealthy; count=7; age_seconds=300",
		"reason=FailedMount; count=2; age_seconds=600",
	} {
		if !strings.Contains(text, allowed) {
			t.Fatalf("serialized AnalysisRequest missing allowed observation %q: %s", allowed, text)
		}
	}
	wantOrder := []string{
		"Selected issue identity",
		"Observed pod counts",
		"Pod snapshot observation",
		"Target container runtime observation",
		"Target container resources and probes",
		"Warning Event observation",
		"Warning Event observation",
	}
	gotOrder := make([]string, len(capture.Request.Evidence))
	for i, item := range capture.Request.Evidence {
		gotOrder[i] = item.Summary
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("evidence order = %v, want %v", gotOrder, wantOrder)
	}
	if len(encoded) > warRoomAIMaxRequestBytes || len(capture.Request.Evidence) > warRoomAIMaxEvidenceItems {
		t.Fatalf("request exceeded existing bounds: bytes=%d evidence=%d", len(encoded), len(capture.Request.Evidence))
	}
}

func TestWarRoomAIPodEvidenceOrderingAndHashAreDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	pod, events, _ := richWarRoomAIPodFixture(now)
	otherPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "payments"}}
	base, db := warRoomAICrashFixture("prod", 17)
	base.aiPodEvidence = buildWarRoomAIPodEvidenceIndex([]*corev1.Pod{otherPod, pod}, events, true, true, now)
	selection := collectWarRoomAISelections(base, "prod", db)[0]
	first, err := captureWarRoomAIEvidence(base, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}

	reorderedPod := pod.DeepCopy()
	reverseContainers(reorderedPod.Spec.Containers)
	reverseContainerStatuses(reorderedPod.Status.ContainerStatuses)
	reorderedEvents := append([]*corev1.Event(nil), events...)
	reverseEvents(reorderedEvents)
	reordered := *base
	reordered.aiPodEvidence = buildWarRoomAIPodEvidenceIndex(
		[]*corev1.Pod{reorderedPod, otherPod}, reorderedEvents, true, true, now,
	)
	second, err := captureWarRoomAIEvidence(&reordered, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Request.Evidence, second.Request.Evidence) || first.Hash != second.Hash {
		t.Fatalf("source ordering changed evidence/hash:\nfirst=%+v %s\nsecond=%+v %s", first.Request.Evidence, first.Hash, second.Request.Evidence, second.Hash)
	}

	later := *base
	laterReport := *base.report
	laterReport.Timestamp = now.Add(time.Minute)
	later.report = &laterReport
	later.aiPodEvidence = buildWarRoomAIPodEvidenceIndex(
		[]*corev1.Pod{pod}, events, true, true, laterReport.Timestamp,
	)
	third, err := captureWarRoomAIEvidence(&later, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != third.Hash {
		t.Fatal("capture-time-only Event age change invalidated the evidence hash")
	}
	if reflect.DeepEqual(first.Request.Evidence, third.Request.Evidence) {
		t.Fatal("Event age did not remain relative to the evidence capture timestamp")
	}

	changedEvents := deepCopyEvents(events)
	changedEvents[1].Series.Count++
	changed := *base
	changed.aiPodEvidence = buildWarRoomAIPodEvidenceIndex(
		[]*corev1.Pod{pod}, changedEvents, true, true, now,
	)
	fourth, err := captureWarRoomAIEvidence(&changed, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash == fourth.Hash {
		t.Fatal("sanitized Warning Event count change did not invalidate evidence hash")
	}

	changedPod := pod.DeepCopy()
	changedPod.Status.ContainerStatuses[1].RestartCount++
	changed.aiPodEvidence = buildWarRoomAIPodEvidenceIndex(
		[]*corev1.Pod{changedPod}, events, true, true, now,
	)
	fifth, err := captureWarRoomAIEvidence(&changed, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash == fifth.Hash {
		t.Fatal("sanitized container restart count change did not invalidate evidence hash")
	}
}

func TestWarRoomAIPodEvidenceRepresentsUnavailableOptionalData(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-0", Namespace: "payments"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	scan := &clusterScan{
		wasteAudit: &analyzer.WasteAudit{StalePods: []analyzer.StalePod{{
			Name: "payments-0", Namespace: "payments", Kind: analyzer.StalePodZombie,
			Status: "CrashLoopBackOff", RestartCount: 1,
		}}},
		aiPodEvidence: buildWarRoomAIPodEvidenceIndex([]*corev1.Pod{pod}, nil, true, false, now),
	}
	issue := warRoomIssue{
		Type: store.IssueCrashLoop, Namespace: "payments", Resource: "payments-0", Container: "app",
		WorkloadKind: "StatefulSet", WorkloadName: "payments-0",
	}
	evidence, err := warRoomAIEvidenceItems(scan, issue)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(evidence)
	text := string(encoded)
	for _, expected := range []string{
		"pod_phase=unknown",
		"warning_events=unavailable",
		"ready=unknown; current_state=unknown; waiting_reason=unknown",
		"last_termination_reason=unknown; last_termination_exit_code=unknown; restart_count=unknown",
		"cpu_request=unspecified; memory_request=unspecified; cpu_limit=unspecified; memory_limit=unspecified",
		"liveness_probe=absent; readiness_probe=absent; startup_probe=absent",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing explicit unavailable evidence %q: %s", expected, text)
		}
	}

	unknownEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "unknown-time", Namespace: "payments"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "payments", Name: "payments-0"},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackOff",
	}
	scan.aiPodEvidence = buildWarRoomAIPodEvidenceIndex(
		[]*corev1.Pod{pod}, []*corev1.Event{unknownEvent}, true, true, now,
	)
	evidence, err = warRoomAIEvidenceItems(scan, issue)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(evidence)
	if !strings.Contains(string(encoded), "reason=BackOff; count=unknown; age_seconds=unknown") {
		t.Fatalf("missing explicit unknown Event evidence: %s", encoded)
	}

	scan.aiPodEvidence = buildWarRoomAIPodEvidenceIndex(nil, nil, false, false, now)
	evidence, err = warRoomAIEvidenceItems(scan, issue)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(evidence)
	if !strings.Contains(string(encoded), "pod_snapshot=unavailable; warning_events=unavailable") {
		t.Fatalf("missing unavailable pod snapshot evidence: %s", encoded)
	}
}

func TestBuildClusterScanDerivesWarRoomAIEvidenceFromSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	pod, events, _ := richWarRoomAIPodFixture(now)
	state := &dashboardState{costAnalyzer: analyzer.NewNodePoolCostAnalyzer("")}
	clusterState := clusterstate.NewClusterState("cluster-a")
	clusterState.Update(clusterstate.ClusterResources{
		Pods:             []*corev1.Pod{pod},
		PodWarningEvents: events,
	})
	clusterState.SetResourceState(clusterstate.ResourcePods, clusterstate.ResourceState{Synced: true})
	clusterState.SetResourceState(clusterstate.ResourcePodWarningEvents, clusterstate.ResourceState{Synced: true})
	clusterState.SetAcquisitionState(clusterstate.AcquisitionHealthy)

	scan := buildClusterScan(state, clusterState.Publish())
	if scan.aiPodEvidence == nil || !scan.aiPodEvidence.podsAvailable || !scan.aiPodEvidence.eventsAvailable {
		t.Fatalf("buildClusterScan did not retain sanitized availability: %#v", scan.aiPodEvidence)
	}
	got, ok := scan.aiPodEvidence.pods["payments/payments-0"]
	if !ok || got.phase != "Running" || len(got.containers) != 2 || got.warningEventCount != 3 || len(got.warningEvents) != warRoomAIMaxPodWarningEvents {
		t.Fatalf("buildClusterScan evidence index = %#v", got)
	}

	emptyEventsState := clusterstate.NewClusterState("cluster-a")
	emptyEventsState.Update(clusterstate.ClusterResources{
		Pods:             []*corev1.Pod{pod},
		PodWarningEvents: []*corev1.Event{},
	})
	emptyEventsState.SetResourceState(clusterstate.ResourcePods, clusterstate.ResourceState{Synced: true})
	emptyEventsState.SetResourceState(clusterstate.ResourcePodWarningEvents, clusterstate.ResourceState{Synced: true})
	emptyEventsState.SetAcquisitionState(clusterstate.AcquisitionHealthy)
	emptyEventsScan := buildClusterScan(state, emptyEventsState.Publish())
	emptyEventsPod := emptyEventsScan.aiPodEvidence.pods["payments/payments-0"]
	if !emptyEventsScan.aiPodEvidence.eventsAvailable || emptyEventsPod.warningEventCount != 0 {
		t.Fatalf("synchronized empty Event cache became unavailable/nonempty: index=%#v pod=%#v", emptyEventsScan.aiPodEvidence, emptyEventsPod)
	}
}

func richWarRoomAIPodFixture(now time.Time) (*corev1.Pod, []*corev1.Event, []string) {
	forbidden := []string{
		"event-message-secret", "env-value-secret", "secret-key-ref-secret", "config-map-key-ref-secret",
		"annotation-secret", "label-secret", "command-secret", "argument-secret", "image-registry-secret",
		"image-pull-secret", "probe-command-secret", "probe-url-secret", "probe-header-secret", "probe-service-secret",
		"waiting-message-secret", "termination-message-secret",
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "payments-0", Namespace: "payments",
			Annotations: map[string]string{"annotation-secret": "annotation-secret"},
			Labels:      map[string]string{"label-secret": "label-secret"},
		},
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "image-pull-secret"}},
			Containers: []corev1.Container{
				{Name: "sidecar", Image: "image-registry-secret/sidecar:latest"},
				{
					Name: "app", Image: "image-registry-secret/app:latest",
					Command: []string{"command-secret"}, Args: []string{"argument-secret"},
					Env: []corev1.EnvVar{
						{Name: "DIRECT", Value: "env-value-secret"},
						{Name: "SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "secret-key-ref-secret"}, Key: "secret-key-ref-secret",
						}}},
						{Name: "CONFIG", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "config-map-key-ref-secret"}, Key: "config-map-key-ref-secret",
						}}},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
					},
					LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
						Exec: &corev1.ExecAction{Command: []string{"probe-command-secret"}},
					}},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/probe-url-secret", HTTPHeaders: []corev1.HTTPHeader{{Name: "probe-header-secret", Value: "probe-header-secret"}}},
					}},
					StartupProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
						GRPC: &corev1.GRPCAction{Service: stringPointer("probe-service-secret")},
					}},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "sidecar", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Hour))}}},
				{
					Name: "app", Ready: false, RestartCount: 17,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "waiting-message-secret"}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason: "Error", ExitCode: 137, Message: "termination-message-secret",
					}},
				},
			},
		},
	}
	events := []*corev1.Event{
		{
			ObjectMeta:     metav1.ObjectMeta{Name: "mount", Namespace: "payments", Annotations: map[string]string{"annotation-secret": "annotation-secret"}},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "payments", Name: "payments-0"},
			Type:           corev1.EventTypeWarning, Reason: "FailedMount", Message: "event-message-secret", Count: 2,
			LastTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
		},
		{
			ObjectMeta:     metav1.ObjectMeta{Name: "probe", Namespace: "payments"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "payments", Name: "payments-0"},
			Type:           corev1.EventTypeWarning, Reason: "Unhealthy", Message: "event-message-secret",
			Series: &corev1.EventSeries{Count: 7, LastObservedTime: metav1.NewMicroTime(now.Add(-5 * time.Minute))},
		},
		{
			ObjectMeta:     metav1.ObjectMeta{Name: "backoff", Namespace: "payments"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "payments", Name: "payments-0"},
			Type:           corev1.EventTypeWarning, Reason: "BackOff", Message: "event-message-secret", Count: 9,
			LastTimestamp: metav1.NewTime(now.Add(-20 * time.Minute)),
		},
	}
	return pod, events, forbidden
}

func reverseContainers(values []corev1.Container) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseContainerStatuses(values []corev1.ContainerStatus) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseEvents(values []*corev1.Event) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func deepCopyEvents(values []*corev1.Event) []*corev1.Event {
	result := make([]*corev1.Event, len(values))
	for i, value := range values {
		result[i] = value.DeepCopy()
	}
	return result
}

func stringPointer(value string) *string {
	return &value
}
