package main

import (
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file proves the stable/cache analysis-evidence hash's intended
// volatility contract, deliberately superseding the old assumption (see
// the now-updated TestWarRoomAIPodEvidenceOrderingAndHashAreDeterministic
// in warroom_ai_snapshot_evidence_test.go): restart_count and
// resource_age_days climb continuously for as long as one incident stays
// active and must never, by themselves, invalidate an otherwise-identical
// cached analysis — while every other meaningful piece of failure evidence
// (issue type, focus container, waiting/termination reason, exit code,
// probe configuration, a new incident episode, and the log-signal category
// set) still must.

// stableHashCase is every input buildStableHashCapture varies across these
// tests. A field left at its zero value uses baseStableHashCase's default.
type stableHashCase struct {
	namespace, podName, containerName string
	stalePodStatus                    string // analyzer.StalePod.Status — drives zombieTypeForStatus
	issueType                         string // canonical issue type — incident lookup and container-selection heuristic
	restartCount                      int32
	ageDays                           int
	containerState                    corev1.ContainerState
	lastTerminationState              corev1.ContainerState
	ready                             bool
	livenessProbe                     bool
	reopenCount                       int
	events                            []*corev1.Event
}

func baseStableHashCase() stableHashCase {
	return stableHashCase{
		namespace: "payments", podName: "payments-0", containerName: "app",
		stalePodStatus: "CrashLoopBackOff", issueType: store.IssueCrashLoop,
		restartCount: 1, ageDays: 3,
		containerState: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}
}

func buildStableHashCapture(t *testing.T, c stableHashCase) warRoomAICapture {
	t.Helper()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	container := corev1.Container{Name: c.containerName}
	if c.livenessProbe {
		container.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"}}}
	}
	status := corev1.ContainerStatus{
		Name: c.containerName, RestartCount: c.restartCount, Ready: c.ready,
		State: c.containerState, LastTerminationState: c.lastTerminationState,
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: c.podName, Namespace: c.namespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{status}},
	}

	fingerprint := store.WorkloadFingerprintForPod(c.namespace, c.podName, c.issueType)
	scan := &clusterScan{
		report: &models.CloudCostReport{Timestamp: now, ClusterName: displayName("prod")},
		wasteAudit: &analyzer.WasteAudit{ScannedAt: now, StalePods: []analyzer.StalePod{{
			Name: c.podName, Namespace: c.namespace, Kind: analyzer.StalePodZombie,
			Status: c.stalePodStatus, Severity: "critical", RestartCount: c.restartCount, AgeDays: c.ageDays,
		}}},
		PodWorkloads: map[string]models.WorkloadRef{c.namespace + "/" + c.podName: {Kind: "StatefulSet", Name: c.podName, Namespace: c.namespace}},
	}
	scan.aiPodEvidence = buildWarRoomAIPodEvidenceIndex([]*corev1.Pod{pod}, c.events, true, true, now)

	db := warRoomAITestStore{incidents: map[string][]store.IncidentSummary{
		"prod": {{
			Fingerprint: fingerprint, Namespace: c.namespace, Resource: c.podName,
			IssueType: c.issueType, Severity: "critical", Status: "active",
			FirstSeen: now.Add(-2 * time.Hour), LastSeen: now, ReopenCount: c.reopenCount,
		}},
	}}

	selections := collectWarRoomAISelections(scan, "prod", db)
	if len(selections) != 1 {
		t.Fatalf("expected exactly one selectable issue, got %d: %+v", len(selections), selections)
	}
	selection, err := findWarRoomAISelection(scan, "prod", db, selections[0].Selector)
	if err != nil {
		t.Fatalf("findWarRoomAISelection: %v", err)
	}
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatalf("captureWarRoomAIEvidence: %v", err)
	}
	return capture
}

// TestWarRoomAIStableHashIgnoresRestartCountAndAgeDuringSameIncident is
// required test 1+2: an enormous restart_count/resource_age_days jump
// (1 -> 2000, 1 -> 400), everything else identical (same cluster, incident
// episode, selector, issue type, pod, container, termination state,
// probes, and events), must produce the SAME stable hash — while the
// exact counts remain present and different in the live AnalysisRequest
// evidence sent to the provider.
func TestWarRoomAIStableHashIgnoresRestartCountAndAgeDuringSameIncident(t *testing.T) {
	low := baseStableHashCase()
	low.restartCount, low.ageDays = 1, 1
	high := baseStableHashCase()
	high.restartCount, high.ageDays = 2000, 400

	lowCapture := buildStableHashCapture(t, low)
	highCapture := buildStableHashCapture(t, high)

	if lowCapture.Hash != highCapture.Hash {
		t.Fatalf("stable hash changed from a restart_count/resource_age_days-only difference: %s vs %s", lowCapture.Hash, highCapture.Hash)
	}

	lowCounts := warRoomAITestFindEvidenceBySummary(t, lowCapture.Request.Evidence, "Observed pod counts")
	highCounts := warRoomAITestFindEvidenceBySummary(t, highCapture.Request.Evidence, "Observed pod counts")
	if lowCounts.Details == highCounts.Details {
		t.Fatal("expected exact restart_count/resource_age_days to differ in the live evidence")
	}
	if !strings.Contains(lowCounts.Details, "restart_count=1;") || !strings.Contains(highCounts.Details, "restart_count=2000;") {
		t.Fatalf("exact restart counts missing from live evidence: low=%q high=%q", lowCounts.Details, highCounts.Details)
	}

	lowTarget := warRoomAITestFindEvidenceBySummary(t, lowCapture.Request.Evidence, "Target container runtime observation")
	highTarget := warRoomAITestFindEvidenceBySummary(t, highCapture.Request.Evidence, "Target container runtime observation")
	if lowTarget.Details == highTarget.Details {
		t.Fatal("expected the exact container restart_count to differ in the live evidence")
	}
}

// TestWarRoomAIStableHashChangesOnMeaningfulEvidence is required test 3:
// a change to any of these must still change the stable hash.
func TestWarRoomAIStableHashChangesOnMeaningfulEvidence(t *testing.T) {
	cases := []struct {
		name string
		a, b stableHashCase
	}{
		{
			name: "waiting reason",
			a:    baseStableHashCase(),
			b: func() stableHashCase {
				c := baseStableHashCase()
				c.containerState = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "Error"}}
				return c
			}(),
		},
		{
			name: "termination exit code",
			a: func() stableHashCase {
				c := baseStableHashCase()
				c.lastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}
				return c
			}(),
			b: func() stableHashCase {
				c := baseStableHashCase()
				c.lastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 137}}
				return c
			}(),
		},
		{
			name: "focus container",
			a:    baseStableHashCase(),
			b: func() stableHashCase {
				c := baseStableHashCase()
				c.containerName = "worker"
				return c
			}(),
		},
		{
			name: "probe configuration",
			a:    baseStableHashCase(),
			b: func() stableHashCase {
				c := baseStableHashCase()
				c.livenessProbe = true
				return c
			}(),
		},
		{
			name: "issue type",
			a:    baseStableHashCase(),
			b: func() stableHashCase {
				c := baseStableHashCase()
				c.stalePodStatus = "oomkilled"
				c.issueType = store.IssueOOMKilled
				c.containerState = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
				c.lastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}
				c.ready = true
				return c
			}(),
		},
		{
			name: "termination reason",
			a: func() stableHashCase {
				c := baseStableHashCase()
				c.lastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}
				return c
			}(),
			b: func() stableHashCase {
				c := baseStableHashCase()
				c.lastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 1}}
				return c
			}(),
		},
		{
			name: "new event reason",
			a: func() stableHashCase {
				c := baseStableHashCase()
				c.events = []*corev1.Event{stableHashWarningEvent("payments", "payments-0", "BackOff", 5, stableHashEventTime)}
				return c
			}(),
			b: func() stableHashCase {
				c := baseStableHashCase()
				c.events = []*corev1.Event{
					stableHashWarningEvent("payments", "payments-0", "BackOff", 5, stableHashEventTime),
					stableHashWarningEvent("payments", "payments-0", "Unhealthy", 2, stableHashEventTime),
				}
				return c
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureA := buildStableHashCapture(t, tc.a)
			captureB := buildStableHashCapture(t, tc.b)
			if captureA.Hash == captureB.Hash {
				t.Fatalf("expected a %s change to change the stable analysis hash", tc.name)
			}
		})
	}
}

// TestWarRoomAIStableHashChangesOnNewIncidentEpisode is required test 4: a
// reopened incident (a new episode of the same fingerprint) must change
// the stable hash even when every other input is identical.
func TestWarRoomAIStableHashChangesOnNewIncidentEpisode(t *testing.T) {
	base := baseStableHashCase()
	reopened := baseStableHashCase()
	reopened.reopenCount = 1

	baseCapture := buildStableHashCapture(t, base)
	reopenedCapture := buildStableHashCapture(t, reopened)
	if baseCapture.Hash == reopenedCapture.Hash {
		t.Fatal("expected a new incident episode (reopen count change) to change the stable analysis hash")
	}
}

// TestWarRoomAILogSignalBucketsGovernRefinedStableHash is required test 5:
// refined log-signal counts within the same frozen bucket (0, 1, 2-5,
// 6-20, 21-100, >100) must hash the same, a category appearing at all
// (0 -> nonzero) must hash differently, and exact counts must remain
// visible and different in the live evidence regardless.
func TestWarRoomAILogSignalBucketsGovernRefinedStableHash(t *testing.T) {
	base := buildStableHashCapture(t, baseStableHashCase())

	lowPreview := logSignalsPreview{
		Source: aiLogSignalsSourcePreviousContainer, ContainerRole: aiLogSignalsContainerRoleTargetContainer,
		LinesRequested: investigationLogTailLines, ByteLimit: investigationLogMaxBytes,
		BytesReceived: 500, Signals: []logSignalCount{{Category: "dependency_timeout", Count: 3}},
	}
	highPreview := lowPreview
	highPreview.Signals = []logSignalCount{{Category: "dependency_timeout", Count: 5}}

	refinedLow, err := appendLogSignalsEvidence(base, lowPreview)
	if err != nil {
		t.Fatal(err)
	}
	refinedHigh, err := appendLogSignalsEvidence(base, highPreview)
	if err != nil {
		t.Fatal(err)
	}
	if refinedLow.Hash != refinedHigh.Hash {
		t.Fatalf("expected counts 3 and 5 (same 2-5 bucket) to hash the same: %s vs %s", refinedLow.Hash, refinedHigh.Hash)
	}

	zeroPreview := lowPreview
	zeroPreview.Signals = nil
	refinedZero, err := appendLogSignalsEvidence(base, zeroPreview)
	if err != nil {
		t.Fatal(err)
	}
	if refinedZero.Hash == refinedLow.Hash {
		t.Fatal("expected a category going from absent to present (bucket 0 -> 2-5) to change the stable analysis hash")
	}

	lastLow := refinedLow.Request.Evidence[len(refinedLow.Request.Evidence)-1]
	lastHigh := refinedHigh.Request.Evidence[len(refinedHigh.Request.Evidence)-1]
	if lastLow.Details == lastHigh.Details {
		t.Fatal("expected exact counts (3 vs 5) to remain visible and different in the live evidence")
	}
}

// stableHashEventTime is a fixed instant at or before buildStableHashCapture's
// internal capture timestamp (2026-09-15T12:00:00Z), so every synthetic
// event below has a known, computable age.
var stableHashEventTime = time.Date(2026, 9, 15, 11, 0, 0, 0, time.UTC)

// stableHashWarningEvent builds a minimal Warning Event for namespace/podName
// with the given reason, classic (non-Series) count, and last-observed time.
func stableHashWarningEvent(namespace, podName, reason string, count int32, observedAt time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: reason + "-event", Namespace: namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: namespace, Name: podName},
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Count:          count,
		LastTimestamp:  metav1.NewTime(observedAt),
	}
}

// TestWarRoomAIStableHashIgnoresEventCountAndAge is required test 4A: a
// BackOff event with count 1 at one observed time, versus the same reason
// with count 24018 at a different observed time (hence a different
// age_seconds at capture), must produce the SAME complete stable base
// hash — only the event's reason participates in stable identity.
func TestWarRoomAIStableHashIgnoresEventCountAndAge(t *testing.T) {
	low := baseStableHashCase()
	low.events = []*corev1.Event{stableHashWarningEvent("payments", "payments-0", "BackOff", 1, time.Date(2026, 9, 15, 11, 59, 0, 0, time.UTC))}
	high := baseStableHashCase()
	high.events = []*corev1.Event{stableHashWarningEvent("payments", "payments-0", "BackOff", 24018, time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC))}

	lowCapture := buildStableHashCapture(t, low)
	highCapture := buildStableHashCapture(t, high)
	if lowCapture.Hash != highCapture.Hash {
		t.Fatalf("stable hash changed from an event count/age-only difference: %s vs %s", lowCapture.Hash, highCapture.Hash)
	}

	lowEvent := warRoomAITestJoinEvidenceBySummary(lowCapture.Request.Evidence, "Warning Event observation")
	highEvent := warRoomAITestJoinEvidenceBySummary(highCapture.Request.Evidence, "Warning Event observation")
	if lowEvent == "" || highEvent == "" {
		t.Fatal("expected a Warning Event observation item in the live evidence")
	}
	if lowEvent == highEvent {
		t.Fatal("expected the exact event count/age to differ in the live evidence")
	}
}
