package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
)

const (
	warRoomAIMaxPodWarningEvents = 2
	warRoomAIMaxReasonBytes      = 128
)

// warRoomAIPodEvidenceIndex is the deliberately small War Room AI view of
// informer-backed Pods and filtered Warning Events. It contains only primitive
// allowlisted observations and never retains Kubernetes objects.
type warRoomAIPodEvidenceIndex struct {
	podsAvailable   bool
	eventsAvailable bool
	capturedAtNanos int64
	pods            map[string]warRoomAIPodEvidence
}

type warRoomAIPodEvidence struct {
	phase             string
	containers        []warRoomAIContainerEvidence
	warningEvents     []warRoomAIWarningEventEvidence
	warningEventCount int
}

type warRoomAIContainerEvidence struct {
	name                    string
	ready                   string
	currentState            string
	waitingReason           string
	lastTerminationReason   string
	lastTerminationExitCode string
	restartCount            string
	cpuRequest              string
	memoryRequest           string
	cpuLimit                string
	memoryLimit             string
	livenessProbe           string
	readinessProbe          string
	startupProbe            string
}

type warRoomAIWarningEventEvidence struct {
	reason          string
	count           string
	observedAtNanos int64
	observedAtKnown bool
}

func buildWarRoomAIPodEvidenceIndex(
	pods []*corev1.Pod,
	events []*corev1.Event,
	podsAvailable, eventsAvailable bool,
	capturedAt time.Time,
) *warRoomAIPodEvidenceIndex {
	index := &warRoomAIPodEvidenceIndex{
		podsAvailable:   podsAvailable,
		eventsAvailable: eventsAvailable,
		capturedAtNanos: capturedAt.UnixNano(),
		pods:            make(map[string]warRoomAIPodEvidence, len(pods)),
	}
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		index.pods[pod.Namespace+"/"+pod.Name] = warRoomAIPodEvidence{
			phase:      warRoomAIPodPhase(pod.Status.Phase),
			containers: buildWarRoomAIContainerEvidence(pod),
		}
	}

	eventsByPod := make(map[string][]warRoomAIWarningEventEvidence)
	for _, event := range events {
		if event == nil || event.Type != corev1.EventTypeWarning ||
			(event.InvolvedObject.Kind != "" && event.InvolvedObject.Kind != "Pod") ||
			event.InvolvedObject.Name == "" {
			continue
		}
		namespace := event.Namespace
		if namespace == "" {
			namespace = event.InvolvedObject.Namespace
		}
		observedAt, observedAtKnown := warRoomAIEventObservedAt(event)
		eventsByPod[namespace+"/"+event.InvolvedObject.Name] = append(
			eventsByPod[namespace+"/"+event.InvolvedObject.Name],
			warRoomAIWarningEventEvidence{
				reason:          warRoomAIReason(event.Reason),
				count:           warRoomAIEventCount(event),
				observedAtNanos: observedAt.UnixNano(),
				observedAtKnown: observedAtKnown,
			},
		)
	}
	for key, pod := range index.pods {
		warningEvents := eventsByPod[key]
		sort.Slice(warningEvents, func(i, j int) bool {
			if warningEvents[i].observedAtKnown != warningEvents[j].observedAtKnown {
				return warningEvents[i].observedAtKnown
			}
			if warningEvents[i].observedAtNanos != warningEvents[j].observedAtNanos {
				return warningEvents[i].observedAtNanos > warningEvents[j].observedAtNanos
			}
			if warningEvents[i].reason != warningEvents[j].reason {
				return warningEvents[i].reason < warningEvents[j].reason
			}
			return warningEvents[i].count < warningEvents[j].count
		})
		pod.warningEventCount = len(warningEvents)
		if len(warningEvents) > warRoomAIMaxPodWarningEvents {
			warningEvents = warningEvents[:warRoomAIMaxPodWarningEvents]
		}
		pod.warningEvents = append([]warRoomAIWarningEventEvidence(nil), warningEvents...)
		index.pods[key] = pod
	}
	return index
}

func buildWarRoomAIContainerEvidence(pod *corev1.Pod) []warRoomAIContainerEvidence {
	specs := make(map[string]corev1.Container, len(pod.Spec.Containers))
	statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
	names := make(map[string]struct{}, len(pod.Spec.Containers)+len(pod.Status.ContainerStatuses))
	for _, container := range pod.Spec.Containers {
		specs[container.Name] = container
		names[container.Name] = struct{}{}
	}
	for _, status := range pod.Status.ContainerStatuses {
		statuses[status.Name] = status
		names[status.Name] = struct{}{}
	}

	orderedNames := make([]string, 0, len(names))
	for name := range names {
		orderedNames = append(orderedNames, name)
	}
	sort.Strings(orderedNames)
	result := make([]warRoomAIContainerEvidence, 0, len(orderedNames))
	for _, name := range orderedNames {
		evidence := warRoomAIContainerEvidence{
			name: name, ready: "unknown", currentState: "unknown", waitingReason: "unknown",
			lastTerminationReason: "unknown", lastTerminationExitCode: "unknown", restartCount: "unknown",
			cpuRequest: "unknown", memoryRequest: "unknown", cpuLimit: "unknown", memoryLimit: "unknown",
			livenessProbe: "unknown", readinessProbe: "unknown", startupProbe: "unknown",
		}
		if container, ok := specs[name]; ok {
			evidence.cpuRequest = warRoomAIResourceValue(container.Resources.Requests, corev1.ResourceCPU)
			evidence.memoryRequest = warRoomAIResourceValue(container.Resources.Requests, corev1.ResourceMemory)
			evidence.cpuLimit = warRoomAIResourceValue(container.Resources.Limits, corev1.ResourceCPU)
			evidence.memoryLimit = warRoomAIResourceValue(container.Resources.Limits, corev1.ResourceMemory)
			evidence.livenessProbe = warRoomAIProbeType(container.LivenessProbe)
			evidence.readinessProbe = warRoomAIProbeType(container.ReadinessProbe)
			evidence.startupProbe = warRoomAIProbeType(container.StartupProbe)
		}
		if status, ok := statuses[name]; ok {
			evidence.ready = strconv.FormatBool(status.Ready)
			evidence.currentState = warRoomAIContainerState(status.State)
			evidence.restartCount = strconv.FormatInt(int64(status.RestartCount), 10)
			if status.State.Waiting != nil {
				evidence.waitingReason = warRoomAIReason(status.State.Waiting.Reason)
			} else {
				evidence.waitingReason = "not_applicable"
			}
			if terminated := status.LastTerminationState.Terminated; terminated != nil {
				evidence.lastTerminationReason = warRoomAIReason(terminated.Reason)
				evidence.lastTerminationExitCode = strconv.FormatInt(int64(terminated.ExitCode), 10)
			} else {
				evidence.lastTerminationReason = "none_observed"
				evidence.lastTerminationExitCode = "none_observed"
			}
		}
		result = append(result, evidence)
	}
	return result
}

func appendWarRoomAIPodEvidence(set *warRoomAIEvidenceSet, index *warRoomAIPodEvidenceIndex, issue warRoomIssue) {
	if index == nil || !index.podsAvailable {
		set.append(aianalysis.EvidenceItem{
			Type: aianalysis.EvidenceObservation, Summary: "Pod snapshot evidence availability",
			Details: "pod_snapshot=unavailable; warning_events=unavailable",
		})
		return
	}
	pod, ok := index.pods[issue.Namespace+"/"+issue.Resource]
	if !ok {
		set.append(aianalysis.EvidenceItem{
			Type: aianalysis.EvidenceObservation, Summary: "Pod snapshot evidence availability",
			Details: "pod_snapshot=not_observed",
		})
		return
	}

	target, targetKnown, matching := warRoomAITargetContainer(pod.containers, issue)
	podDetails := fmt.Sprintf("pod_phase=%s; observed_container_count=%d", pod.phase, len(pod.containers))
	if targetKnown {
		podDetails += "; target_container=" + target.name
	} else {
		podDetails += fmt.Sprintf("; target_container=unknown; matching_container_count=%d", matching)
	}
	if index.eventsAvailable {
		podDetails += fmt.Sprintf("; warning_events_observed=%d; warning_events_included=%d", pod.warningEventCount, len(pod.warningEvents))
	} else {
		podDetails += "; warning_events=unavailable"
	}
	set.append(aianalysis.EvidenceItem{
		Type: aianalysis.EvidenceObservation, Summary: "Pod snapshot observation", Details: podDetails,
	})

	if targetKnown {
		// restart_count is excluded from the stable hash for the same
		// reason as "Observed pod counts" above (warroom_ai_evidence.go):
		// it climbs continuously during an active incident without
		// indicating a change in what is wrong. Every other field here
		// (waiting/termination reason, exit code) remains exact in both
		// representations — those are meaningful failure evidence.
		set.appendStable(aianalysis.EvidenceItem{
			Type: aianalysis.EvidenceObservation, Summary: "Target container runtime observation",
			Details: fmt.Sprintf("container=%s; ready=%s; current_state=%s; waiting_reason=%s; last_termination_reason=%s; last_termination_exit_code=%s; restart_count=%s",
				target.name, target.ready, target.currentState, target.waitingReason,
				target.lastTerminationReason, target.lastTerminationExitCode, target.restartCount),
		}, fmt.Sprintf("container=%s; ready=%s; current_state=%s; waiting_reason=%s; last_termination_reason=%s; last_termination_exit_code=%s; restart_count=stable",
			target.name, target.ready, target.currentState, target.waitingReason,
			target.lastTerminationReason, target.lastTerminationExitCode))
		set.append(aianalysis.EvidenceItem{
			Type: aianalysis.EvidenceConfiguration, Summary: "Target container resources and probes",
			Details: fmt.Sprintf("container=%s; cpu_request=%s; memory_request=%s; cpu_limit=%s; memory_limit=%s; liveness_probe=%s; readiness_probe=%s; startup_probe=%s",
				target.name, target.cpuRequest, target.memoryRequest, target.cpuLimit, target.memoryLimit,
				target.livenessProbe, target.readinessProbe, target.startupProbe),
		})
	}

	if !index.eventsAvailable {
		return
	}
	for _, event := range pod.warningEvents {
		age := "unknown"
		if event.observedAtKnown && index.capturedAtNanos >= event.observedAtNanos {
			age = strconv.FormatInt((index.capturedAtNanos-event.observedAtNanos)/int64(time.Second), 10)
		}
		// Stable identity is the event's reason alone: a repeat of the same
		// warning (count climbing) or the passage of time (age_seconds, or
		// even the absolute observed-at instant of the latest repeat)
		// updates this same Event object without being a change in what is
		// wrong. Only a genuinely new/different reason appearing among the
		// tracked warning events may change the stable hash.
		set.appendStable(aianalysis.EvidenceItem{
			Type: aianalysis.EvidenceEvent, Summary: "Warning Event observation",
			Details: fmt.Sprintf("reason=%s; count=%s; age_seconds=%s", event.reason, event.count, age),
		}, fmt.Sprintf("reason=%s", event.reason))
	}
}

func warRoomAITargetContainer(containers []warRoomAIContainerEvidence, issue warRoomIssue) (warRoomAIContainerEvidence, bool, int) {
	if issue.Container != "" {
		for _, container := range containers {
			if container.name == issue.Container {
				return container, true, 1
			}
		}
		return warRoomAIContainerEvidence{}, false, 0
	}

	canonicalType := store.CanonicalIssueType(issue.Type)
	if canonicalType == store.IssueHighRestartCount {
		var matches []warRoomAIContainerEvidence
		var highest int64
		for _, container := range containers {
			restarts, err := strconv.ParseInt(container.restartCount, 10, 32)
			if err != nil || restarts <= 10 || restarts < highest {
				continue
			}
			if restarts > highest {
				highest = restarts
				matches = matches[:0]
			}
			matches = append(matches, container)
		}
		if len(matches) == 1 {
			return matches[0], true, 1
		}
		return warRoomAIContainerEvidence{}, false, len(matches)
	}

	matches := make([]warRoomAIContainerEvidence, 0, len(containers))
	for _, container := range containers {
		matched := false
		switch canonicalType {
		case store.IssueCrashLoop:
			matched = container.currentState == "waiting" && (container.waitingReason == "CrashLoopBackOff" || container.waitingReason == "Error")
		case store.IssueOOMKilled:
			matched = container.waitingReason == "OOMKilled" || container.lastTerminationReason == "OOMKilled"
		case store.IssueImagePullBackOff:
			matched = container.waitingReason == "ImagePullBackOff" || container.waitingReason == "ErrImagePull"
		case store.IssueProbeFailure:
			matched = container.ready == "false"
		}
		if matched {
			matches = append(matches, container)
		}
	}
	if len(matches) == 1 {
		return matches[0], true, 1
	}
	return warRoomAIContainerEvidence{}, false, len(matches)
}

func warRoomAIPodPhase(phase corev1.PodPhase) string {
	switch phase {
	case corev1.PodPending, corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
		return string(phase)
	default:
		return "unknown"
	}
}

func warRoomAIContainerState(state corev1.ContainerState) string {
	present := 0
	value := "unknown"
	if state.Waiting != nil {
		present++
		value = "waiting"
	}
	if state.Running != nil {
		present++
		value = "running"
	}
	if state.Terminated != nil {
		present++
		value = "terminated"
	}
	if present != 1 {
		return "unknown"
	}
	return value
}

func warRoomAIResourceValue(resources corev1.ResourceList, name corev1.ResourceName) string {
	quantity, ok := resources[name]
	if !ok {
		return "unspecified"
	}
	return quantity.String()
}

func warRoomAIProbeType(probe *corev1.Probe) string {
	if probe == nil {
		return "absent"
	}
	types := make([]string, 0, 4)
	if probe.Exec != nil {
		types = append(types, "exec")
	}
	if probe.GRPC != nil {
		types = append(types, "grpc")
	}
	if probe.HTTPGet != nil {
		types = append(types, "http_get")
	}
	if probe.TCPSocket != nil {
		types = append(types, "tcp_socket")
	}
	if len(types) == 0 {
		return "unknown"
	}
	return strings.Join(types, "+")
}

func warRoomAIReason(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if len(value) > warRoomAIMaxReasonBytes {
		return "unavailable"
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("-_.:/", char) {
			continue
		}
		return "unavailable"
	}
	return value
}

func warRoomAIEventCount(event *corev1.Event) string {
	if event.Series != nil && event.Series.Count > 0 {
		return strconv.FormatInt(int64(event.Series.Count), 10)
	}
	if event.Count > 0 {
		return strconv.FormatInt(int64(event.Count), 10)
	}
	return "unknown"
}

func warRoomAIEventObservedAt(event *corev1.Event) (time.Time, bool) {
	if event.Series != nil && !event.Series.LastObservedTime.IsZero() {
		return event.Series.LastObservedTime.Time, true
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time, true
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time, true
	}
	if !event.FirstTimestamp.IsZero() {
		return event.FirstTimestamp.Time, true
	}
	if !event.CreationTimestamp.IsZero() {
		return event.CreationTimestamp.Time, true
	}
	return time.Time{}, false
}
