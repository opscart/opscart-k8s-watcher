package analyzer

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func probeWarningEvent(namespace, podName, message string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: podName, Namespace: namespace},
		Type:           corev1.EventTypeWarning,
		Message:        message,
	}
}

func notReadyPod(namespace, name string, age time.Duration, now time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: metav1.NewTime(now.Add(-age))},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: false, RestartCount: 1},
			},
		},
	}
}

// TestAnalyzeStalePodsUsesSuppliedWarningEventsNotLiveEvents proves
// probe-failure evidence comes entirely from the supplied filtered Warning
// Events (the exact involvedObject.kind=Pod,type=Warning evidence the
// shared informer set provides) — no Event API read is reachable from
// analyzeStalePods at all, since it is a plain function over value slices.
func TestAnalyzeStalePodsUsesSuppliedWarningEventsNotLiveEvents(t *testing.T) {
	now := time.Now()
	pod := notReadyPod("payments", "api-1", 24*time.Hour, now)
	events := []corev1.Event{probeWarningEvent("payments", "api-1", "Liveness probe failed: timeout")}

	got := analyzeStalePods([]corev1.Pod{pod}, events, 7, now)

	if len(got) != 1 || got[0].Kind != StalePodZombie {
		t.Fatalf("analyzeStalePods = %+v, want one ZOMBIE finding from the supplied probe-failure Event", got)
	}
	if got[0].Severity != "critical" {
		t.Fatalf("got severity %q, want critical — probe-failure evidence must classify immediately, no restart threshold required", got[0].Severity)
	}
}

// TestAnalyzeStalePodsWithoutMatchingEventFallsBackToRestartInference proves
// the existing dashboard fallback (high restart count, no confirmed probe
// Event) survives unchanged when PodWarningEvents simply doesn't mention
// this pod — the same "detector inference, not confirmed by Events" path
// AuditWaste's own live-Events branch produces.
func TestAnalyzeStalePodsWithoutMatchingEventFallsBackToRestartInference(t *testing.T) {
	now := time.Now()
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "payments", CreationTimestamp: metav1.NewTime(now.Add(-24 * time.Hour))},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: false, RestartCount: 15}},
		},
	}

	got := analyzeStalePods([]corev1.Pod{pod}, nil, 7, now) // no PodWarningEvents supplied at all

	if len(got) != 1 || got[0].Kind != StalePodZombie {
		t.Fatalf("analyzeStalePods = %+v, want one ZOMBIE finding via restart-count inference", got)
	}
	if got[0].Reason == "" {
		t.Fatal("expected a non-empty classifier reason")
	}
}

func TestAnalyzeStalePodsHealthyPodProducesNoFinding(t *testing.T) {
	now := time.Now()
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "payments", CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour))},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true, RestartCount: 0}},
		},
	}

	got := analyzeStalePods([]corev1.Pod{pod}, nil, 7, now)

	if len(got) != 0 {
		t.Fatalf("analyzeStalePods = %+v, want no findings for a healthy pod", got)
	}
}

// TestAnalyzeStalePodsEventFromDifferentNamespaceDoesNotMatch proves the
// per-namespace Event cache (probeFailurePodsFromEvents applied per
// namespace) stays namespace-scoped in the snapshot path, exactly like the
// live path's per-namespace event grouping.
func TestAnalyzeStalePodsEventFromDifferentNamespaceDoesNotMatch(t *testing.T) {
	now := time.Now()
	pod := notReadyPod("payments", "api-1", 24*time.Hour, now)
	events := []corev1.Event{probeWarningEvent("checkout", "api-1", "Liveness probe failed: timeout")} // different namespace, same pod name

	got := analyzeStalePods([]corev1.Pod{pod}, events, 7, now)

	// No probe-failure Event matched in "payments"; falls through to the
	// restart-threshold inference path — 1 restart is below the >10
	// threshold, so no finding at all.
	if len(got) != 0 {
		t.Fatalf("analyzeStalePods = %+v, want no finding — the Event belongs to a different namespace", got)
	}
}
