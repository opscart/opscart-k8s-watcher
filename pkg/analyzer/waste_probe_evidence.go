package analyzer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/classify"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ================================================================
// 2. Stale Pods
// ================================================================

// probeFailurePodsByNamespace fetches Pod events for a namespace ONCE and
// returns the set of pod names that have at least one probe-failure event
// (kubelet killing the container because a liveness/startup probe failed).
//
// This is the discriminator between "the probe is killing an otherwise-viable
// container" (ProbeFailure) and "the process is crashing on its own"
// (CrashLoopBackOff/OOMKilled/etc.) — instantaneous pod state alone cannot
// tell these apart, since a probe-killed pod alternates between Waiting and
// Running from one scan to the next.
//
// An event-list failure returns an empty map for classification and an error
// so the audit can preserve that the pod detector was incomplete.
func (w *WasteAuditor) probeFailurePodsByNamespace(namespace string) (map[string]bool, error) {
	events, err := w.clientset.CoreV1().Events(namespace).List(w.ctx, metav1.ListOptions{
		FieldSelector:  "involvedObject.kind=Pod",
		TimeoutSeconds: int64Ptr(10),
	})
	if err != nil {
		fmt.Printf("⚠️  Could not list events in namespace %q for probe-failure detection: %v\n", namespace, err)
		return map[string]bool{}, err
	}
	return probeFailurePodsFromEvents(events.Items), nil
}

func probeFailurePodsFromEvents(events []corev1.Event) map[string]bool {
	result := make(map[string]bool)
	for _, ev := range events {
		if ev.InvolvedObject.Kind != "" && ev.InvolvedObject.Kind != "Pod" {
			continue
		}
		if ev.Type != corev1.EventTypeWarning {
			continue
		}
		lower := strings.ToLower(ev.Message)
		if strings.Contains(lower, "probe failed") || strings.Contains(lower, "probe, will be restarted") {
			result[ev.InvolvedObject.Name] = true
		}
	}
	return result
}

// classifyStalePodFailure translates one Kubernetes Pod snapshot into the
// same raw issue vocabulary produced by pkg/scanner, then delegates the
// priority decision to the shared classifier. Evidence collection remains in
// the analyzer for now; sharing that snapshot collector is a separate change.
func classifyStalePodFailure(pod corev1.Pod, ageDays int, hasProbeFailureEvent bool) (StalePod, bool) {
	issues := make([]models.EmergencyIssue, 0, len(pod.Status.ContainerStatuses)*2)
	var totalRestarts int32
	var notReady int
	hasDirectFailure := false

	newIssue := func(cs corev1.ContainerStatus, severity, reason, message string) models.EmergencyIssue {
		return models.EmergencyIssue{
			Severity:  severity,
			Resource:  "pod",
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Reason:    reason,
			Message:   message,
			Container: cs.Name,
			Restarts:  int(cs.RestartCount),
		}
	}

	for _, cs := range pod.Status.ContainerStatuses {
		totalRestarts += cs.RestartCount
		if !cs.Ready {
			notReady++
		}

		if waiting := cs.State.Waiting; waiting != nil {
			switch waiting.Reason {
			case "CrashLoopBackOff":
				hasDirectFailure = true
				issues = append(issues, newIssue(cs, "critical", waiting.Reason,
					fmt.Sprintf("Container %s is crash looping: %s", cs.Name, waiting.Message)))
			case "OOMKilled":
				hasDirectFailure = true
				issues = append(issues, newIssue(cs, "critical", waiting.Reason,
					fmt.Sprintf("Container %s killed due to out of memory", cs.Name)))
			case "Error":
				hasDirectFailure = true
				issues = append(issues, newIssue(cs, "critical", waiting.Reason,
					fmt.Sprintf("Container %s is waiting in Error state: %s", cs.Name, waiting.Message)))
			case "ImagePullBackOff", "ErrImagePull":
				hasDirectFailure = true
				issues = append(issues, newIssue(cs, "high", waiting.Reason,
					fmt.Sprintf("Cannot pull image for container %s: %s", cs.Name, waiting.Message)))
			}
		}

		// OOM evidence is reported by Kubernetes on the last termination
		// state, not normally as the current Waiting reason. The old dashboard
		// path missed this evidence even though the CLI scanner already used it.
		if terminated := cs.LastTerminationState.Terminated; terminated != nil && terminated.Reason == "OOMKilled" {
			hasDirectFailure = true
			issues = append(issues, newIssue(cs, "critical", "OOMKilled",
				fmt.Sprintf("Container %s last termination reports OOMKilled; current failure is not established by this historical state alone", cs.Name)))
		}

		if cs.RestartCount > 10 && pod.Status.Phase == corev1.PodRunning {
			issues = append(issues, newIssue(cs, "medium", "HighRestartCount",
				fmt.Sprintf("Container %s has restarted %d times", cs.Name, cs.RestartCount)))
		}
	}

	if hasProbeFailureEvent && notReady > 0 {
		// Event evidence is direct enough to classify immediately; no restart
		// threshold is required. The explicit issue also lets a Running pod
		// classify as standalone ProbeFailure while a Waiting pod merges it
		// with CrashLoopBackOff through the same shared priority function.
		issues = append(issues, models.EmergencyIssue{
			Severity: "critical", Resource: "pod", Namespace: pod.Namespace, Name: pod.Name,
			Reason: "ProbeFailure", Restarts: int(totalRestarts),
			Message: fmt.Sprintf(
				"%d/%d containers are not ready. A warning Event matched probe-failure text for this Pod name (%d restarts observed).",
				notReady, len(pod.Status.ContainerStatuses), totalRestarts,
			),
		})
	} else if !hasDirectFailure && notReady > 0 && totalRestarts > 10 && pod.Status.Phase == corev1.PodRunning {
		// Preserve the existing dashboard fallback when Event access is absent
		// or the event has expired. Direct Crash/OOM/ImagePull/Error evidence
		// wins over this inference; ProbeFailure still wins over a mere high
		// restart count.
		issues = append(issues, models.EmergencyIssue{
			Severity: "critical", Resource: "pod", Namespace: pod.Namespace, Name: pod.Name,
			Reason: "ProbeFailure", Restarts: int(totalRestarts),
			Message: fmt.Sprintf(
				"%d/%d containers not ready; %d restarts observed. Probe failure is a detector inference, not confirmed by Events.",
				notReady, len(pod.Status.ContainerStatuses), totalRestarts,
			),
		})
	}

	classified, ok := classify.PodFailure(issues, false)
	if !ok {
		return StalePod{}, false
	}

	observations := []string{fmt.Sprintf("Pod phase: %s; %d containers not ready; %d restarts observed.", pod.Status.Phase, notReady, totalRestarts)}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			observations = append(observations, fmt.Sprintf("Container %s waiting reason: %s.", cs.Name, cs.State.Waiting.Reason))
		}
		if cs.LastTerminationState.Terminated != nil {
			observations = append(observations, fmt.Sprintf("Container %s last termination reason: %s.", cs.Name, cs.LastTerminationState.Terminated.Reason))
		}
	}
	if hasProbeFailureEvent {
		observations = append(observations, "A warning Event matched probe-failure text for this Pod name.")
	}
	return StalePod{
		ObservedEvidence: strings.Join(observations, " "),
		Name:             pod.Name,
		Namespace:        pod.Namespace,
		Kind:             StalePodZombie,
		AgeDays:          ageDays,
		RestartCount:     totalRestarts,
		Status:           dashboardPodStatus(classified.Reason),
		Severity:         classified.Severity,
		Reason:           classified.Message,
		Score:            float64(ageDays)*0.4 + float64(totalRestarts)*0.3,
	}, true
}

// dashboardPodStatus keeps the durable dashboard category stable across the
// brief Running/Waiting phase flicker of a crash loop. The shared classifier's
// richer merged reason remains in its message and in CLI presentation.
func dashboardPodStatus(classifiedReason string) string {
	switch classifiedReason {
	case "CrashLoopBackOff (OOMKilled)":
		return "OOMKilled"
	case "CrashLoopBackOff (ProbeFailure)":
		return "ProbeFailure"
	default:
		return classifiedReason
	}
}

func (w *WasteAuditor) detectStalePods(audit *WasteAudit, filterNamespace string) error {
	return w.detectStalePodsWithClusterEvents(audit, filterNamespace, true)
}

func (w *WasteAuditor) detectStalePodsWithClusterEvents(audit *WasteAudit, filterNamespace string, useClusterEvents bool) error {
	podItems, shared := w.sharedPods(filterNamespace)
	if !shared {
		pods, err := w.clientset.CoreV1().Pods(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
		if err != nil {
			return err
		}
		podItems = pods.Items
	}

	now := time.Now()

	var eventsByNamespace map[string][]corev1.Event
	if useClusterEvents {
		events, listErr := w.clientset.CoreV1().Events("").List(w.ctx, metav1.ListOptions{
			FieldSelector:  "involvedObject.kind=Pod,type=Warning",
			TimeoutSeconds: int64Ptr(10),
		})
		if listErr == nil {
			eventsByNamespace = make(map[string][]corev1.Event)
			for _, event := range events.Items {
				eventsByNamespace[event.Namespace] = append(eventsByNamespace[event.Namespace], event)
			}
		}
	}

	// Namespace event cache: fetched at most once per namespace encountered,
	// not once per pod (a 350-pod cluster must not turn into hundreds of
	// extra API calls).
	probeFailurePods := make(map[string]map[string]bool)
	var eventScanErr error

	for _, pod := range podItems {
		// Skip system namespaces
		if isInfraPattern(pod.Namespace) {
			continue
		}

		ageDays := int(now.Sub(pod.CreationTimestamp.Time).Hours() / 24)

		if _, ok := probeFailurePods[pod.Namespace]; !ok {
			if eventsByNamespace != nil {
				probeFailurePods[pod.Namespace] = probeFailurePodsFromEvents(eventsByNamespace[pod.Namespace])
			} else {
				var err error
				probeFailurePods[pod.Namespace], err = w.probeFailurePodsByNamespace(pod.Namespace)
				if err != nil && eventScanErr == nil {
					eventScanErr = fmt.Errorf("list pod events in namespace %q: %w", pod.Namespace, err)
				}
			}
		}
		hasProbeFailureEvent := probeFailurePods[pod.Namespace][pod.Name]

		if stale, ok := evaluateStalePod(pod, ageDays, hasProbeFailureEvent, w.minAgeDays, now); ok {
			audit.StalePods = append(audit.StalePods, stale)
		}
	}

	sort.Slice(audit.StalePods, func(i, j int) bool {
		return audit.StalePods[i].Score > audit.StalePods[j].Score
	})

	return eventScanErr
}

// evaluateStalePod decides whether one Pod, given its age and whether a
// probe-failure Warning Event was observed for it, is a stale-pod finding —
// either a ZOMBIE (active failure, classifyStalePodFailure; no age gate) or
// an IDLE bare pod (age- and last-activity-gated). It performs no
// Kubernetes API calls; now is passed in explicitly so age-derived fields
// come from the caller's clock reading, keeping this deterministic for
// identical inputs. Callers are responsible for the isInfraPattern skip and
// for computing ageDays themselves (docs/08 Phase 4D.5: the infra skip must
// happen before any per-namespace Event evidence is gathered, not after).
func evaluateStalePod(pod corev1.Pod, ageDays int, hasProbeFailureEvent bool, minAgeDays int, now time.Time) (StalePod, bool) {
	// ── ACTIVE POD-FAILURE CLASSIFICATION ───────────────────────
	// The CLI and dashboard reduce the same independent container signals
	// through pkg/classify. classifyStalePodFailure deliberately inspects
	// every container before deciding; API container order cannot choose
	// the winning issue. Age is not a gate for an active malfunction.
	if stale, ok := classifyStalePodFailure(pod, ageDays, hasProbeFailureEvent); ok {
		return stale, true
	}

	// ── IDLE detection: old pod, no recent restart activity ──────
	// Age gate applies here: we only flag idle pods that have been sitting
	// around longer than minAgeDays.
	if ageDays < minAgeDays {
		return StalePod{}, false
	}

	// Key insight: creationTimestamp never resets on restart. We check LAST
	// restart time to exclude recently-active pods.
	lastActivityDays := ageDays // assume as old as pod if never restarted
	totalRestarts := int32(0)
	for _, cs := range pod.Status.ContainerStatuses {
		totalRestarts += cs.RestartCount

		// If it restarted recently, it IS active - skip idle check
		if cs.LastTerminationState.Terminated != nil {
			lastRestart := cs.LastTerminationState.Terminated.FinishedAt.Time
			daysSinceRestart := int(now.Sub(lastRestart).Hours() / 24)
			if daysSinceRestart < lastActivityDays {
				lastActivityDays = daysSinceRestart
			}
		}
	}

	// Only flag as idle if last activity was also old.
	// A pod that restarted 2 days ago is NOT idle even if created 90 days ago.
	if lastActivityDays < minAgeDays {
		return StalePod{}, false
	}

	// IDLE signal = bare pod with no owning controller. A pod managed by
	// Deployment/DaemonSet/StatefulSet is actively maintained - 0 restarts
	// on those is healthy, NOT a waste signal. Real waste = manually
	// created pods (kubectl run, raw Pod manifest) that someone forgot about.
	isBarePod := true
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "ReplicaSet" || ref.Kind == "DaemonSet" ||
			ref.Kind == "StatefulSet" || ref.Kind == "Job" ||
			ref.Kind == "CronJob" {
			isBarePod = false
			break
		}
	}

	if !isBarePod || pod.Status.Phase != corev1.PodRunning {
		return StalePod{}, false
	}

	explanation := fmt.Sprintf(
		"Running Pod is %d days old and has no owner reference of kind ReplicaSet, DaemonSet, "+
			"StatefulSet, Job, or CronJob. Restart count: %d.",
		ageDays, totalRestarts,
	)

	return StalePod{
		Name:             pod.Name,
		Namespace:        pod.Namespace,
		Kind:             StalePodIdle,
		AgeDays:          ageDays,
		LastActivityDays: lastActivityDays,
		RestartCount:     totalRestarts,
		Reason:           explanation,
		Score:            float64(ageDays) * 0.5,
	}, true
}

// analyzeStalePods is detectStalePodsWithClusterEvents' Kubernetes-free
// counterpart (docs/08 Phase 4D.5): the same probe-failure/idle
// classification, from an already-observed Pods snapshot and the exact
// filtered Warning Event evidence the shared informer set already provides
// (involvedObject.kind=Pod,type=Warning — see pkg/acquisition/event_filter.go)
// instead of the detector's own cluster-wide Events LIST. It performs no
// Kubernetes API calls and is deterministic for identical inputs.
func analyzeStalePods(pods []corev1.Pod, warningEvents []corev1.Event, minAgeDays int, now time.Time) []StalePod {
	eventsByNamespace := make(map[string][]corev1.Event)
	for _, event := range warningEvents {
		eventsByNamespace[event.Namespace] = append(eventsByNamespace[event.Namespace], event)
	}

	probeFailurePods := make(map[string]map[string]bool)
	var stalePods []StalePod

	for _, pod := range pods {
		if isInfraPattern(pod.Namespace) {
			continue
		}

		ageDays := int(now.Sub(pod.CreationTimestamp.Time).Hours() / 24)

		if _, ok := probeFailurePods[pod.Namespace]; !ok {
			probeFailurePods[pod.Namespace] = probeFailurePodsFromEvents(eventsByNamespace[pod.Namespace])
		}
		hasProbeFailureEvent := probeFailurePods[pod.Namespace][pod.Name]

		if stale, ok := evaluateStalePod(pod, ageDays, hasProbeFailureEvent, minAgeDays, now); ok {
			stalePods = append(stalePods, stale)
		}
	}

	sort.Slice(stalePods, func(i, j int) bool {
		return stalePods[i].Score > stalePods[j].Score
	})

	return stalePods
}
