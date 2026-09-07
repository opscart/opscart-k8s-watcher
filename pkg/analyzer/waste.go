package analyzer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/classify"
	"github.com/opscart/opscart-k8s-watcher/pkg/kube"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// ================================================================
// Types
// ================================================================

type WasteAuditor struct {
	clientset       kubernetes.Interface
	ctx             context.Context
	minAgeDays      int
	podSnapshot     []corev1.Pod
	podsByNamespace map[string][]corev1.Pod
}

type WasteAudit struct {
	ClusterContext string
	ScannedAt      time.Time

	AbandonedNamespaces  []AbandonedNamespace
	StalePods            []StalePod
	OrphanedPVCs         []OrphanedPVC
	StaleJobs            []StaleJob
	ZeroReplicaWorkloads []ZeroReplicaWorkload
	OldReplicaSets       []OldReplicaSet
	OrphanedServices     []OrphanedService
	BrokenIngresses      []BrokenIngress
	MisconfiguredHPAs    []MisconfiguredHPA
	DetectorWarnings     []WasteDetectorWarning

	TotalWasteItems       int     // Legacy: excludes ReplicaSets, includes failed Pods; use BuildWastePresentation counts.
	OrphanedPVCStorageGB  int     // Deprecated: whole GiB; presentation uses RequestedStorageBytes.
	RequestedStorageBytes int64   // Sum of candidate PVC requests, not billable or idle storage.
	EstimatedMonthlyWaste float64 // Legacy, unsupported estimate; never used by waste presentation.
}

type WasteDetectorWarning struct {
	Category string
	Error    string
}

// ----------------------------------------------------------------
// Abandoned Namespaces
// ----------------------------------------------------------------

type AbandonedNamespace struct {
	Name        string
	AgeDays     int
	PodCount    int
	AllPodsIdle bool
	Reason      string // Evidence prose also flows into future namespace incident messages; identities are unchanged.
	Score       float64
}

// ----------------------------------------------------------------
// Stale Pods - two distinct categories
// ----------------------------------------------------------------

type StalePodKind string

const (
	StalePodIdle   StalePodKind = "IDLE"   // legacy subtype: age-gated Pod without a recognized owner kind
	StalePodZombie StalePodKind = "ZOMBIE" // legacy subtype: current, historical, or inferred Pod failure evidence
)

type StalePod struct {
	Name      string
	Namespace string
	Kind      StalePodKind
	AgeDays   int
	// For IDLE pods:
	LastActivityDays int // days since last restart (or age if never restarted)
	RestartCount     int32
	// For ZOMBIE pods:
	ObservedEvidence string // Snapshot evidence, separate from classifier inference.
	Status           string // CrashLoopBackOff, OOMKilled, etc.
	Severity         string // shared pod classifier severity; empty means legacy critical
	Reason           string // data-driven explanation
	Score            float64
}

// ----------------------------------------------------------------
// Orphaned PVCs
// ----------------------------------------------------------------

type PVCStatus string

const (
	PVCNeverBound PVCStatus = "Never bound"               // Legacy label for current Pending phase; not binding history.
	PVCReleased   PVCStatus = "Released (pod deleted)"    // Legacy label for current Lost phase; not Pod deletion evidence.
	PVCBoundNoPod PVCStatus = "Bound but no pod using it" // Bound but no pod references it
)

type OrphanedPVC struct {
	Name           string
	Namespace      string
	SizeGB         int // Deprecated: whole GiB; do not use for presentation.
	RequestedBytes int64
	RequestKnown   bool
	Status         PVCStatus
	AgeDays        int
	Reason         string
	Score          float64
}

// ----------------------------------------------------------------
// Stale Jobs / CronJobs
// ----------------------------------------------------------------

type StaleJob struct {
	Name               string
	Namespace          string
	IsCronJob          bool
	JobStatus          string // Legacy detector subtype, not a terminal-state assertion.
	Schedule           string // Retained CronJob metadata; empty when unavailable.
	Suspended          *bool  // nil means unspecified; Kubernetes defaults this to false.
	AttemptCountsKnown bool
	SucceededPods      int32
	FailedPods         int32
	AgeDays            int
	Reason             string
	Score              float64
}

// ----------------------------------------------------------------
// Zero-Replica Workloads (Deployments + StatefulSets)
// ----------------------------------------------------------------

type ZeroReplicaWorkload struct {
	Name      string
	Namespace string
	Kind      string // Deployment, StatefulSet
	AgeDays   int
	Reason    string
	Score     float64
}

// ----------------------------------------------------------------
// Old ReplicaSets (retention review)
// ----------------------------------------------------------------

type OldReplicaSet struct {
	Name            string
	Namespace       string
	AgeDays         int
	OwnerDeployment string
	DesiredReplicas *int32 // Retained evidence; nil does not establish zero.
	Reason          string
	Score           float64
}

// ----------------------------------------------------------------
// Orphaned Services
// ----------------------------------------------------------------

type OrphanedService struct {
	Name      string
	Namespace string
	Type      string // ClusterIP, LoadBalancer, NodePort
	AgeDays   int
	IsLB      bool
	Selector  map[string]string // Retained selector evidence, not endpoint state.
	Reason    string
	Score     float64
}

// ----------------------------------------------------------------
// Broken Ingresses
// ----------------------------------------------------------------

type BrokenIngress struct {
	Name      string
	Namespace string
	AgeDays   int
	Hosts     []string
	Reason    string // which backend is missing
	Score     float64
	// IsActive is always true for BrokenIngress: it is only ever
	// produced from currently-observed missing endpoints, never age-gated.
	IsActive bool
}

// ----------------------------------------------------------------
// Misconfigured HPAs
// ----------------------------------------------------------------

type MisconfiguredHPA struct {
	Name        string
	Namespace   string
	TargetName  string
	MinReplicas int32
	MaxReplicas int32
	AgeDays     int
	Condition   string // ScalingDisabled, MetricNotAvailable, AlwaysAtMin
	Reason      string
	Score       float64
	// IsActive is true for a currently K8s-reported ScalingActive=False
	// condition, false for the age-gated AlwaysAtMin tuning candidate.
	IsActive bool
}

// ================================================================
// Constructor
// ================================================================

func NewWasteAuditor(clientset kubernetes.Interface, minAgeDays int) (*WasteAuditor, context.CancelFunc) {
	// 60-second timeout per cluster - prevents hanging on corporate networks
	// Caller should defer the cancel function for proper cleanup
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	return &WasteAuditor{
		clientset:  clientset,
		ctx:        ctx,
		minAgeDays: minAgeDays,
	}, cancel
}

// WithPodSnapshot supplies Pods already retrieved by a preceding analyzer.
// Only a genuinely cluster-wide snapshot is retained; namespace-scoped input
// leaves detectors on their existing Kubernetes API retrieval paths.
func (w *WasteAuditor) WithPodSnapshot(pods []corev1.Pod, clusterWide bool) *WasteAuditor {
	if !clusterWide {
		w.podSnapshot = nil
		w.podsByNamespace = nil
		return w
	}
	w.podSnapshot = append(w.podSnapshot[:0], pods...)
	w.podsByNamespace = make(map[string][]corev1.Pod)
	for _, pod := range pods {
		w.podsByNamespace[pod.Namespace] = append(w.podsByNamespace[pod.Namespace], pod)
	}
	return w
}

func (w *WasteAuditor) sharedPods(filterNamespace string) ([]corev1.Pod, bool) {
	if w.podsByNamespace == nil {
		return nil, false
	}
	if filterNamespace != "" {
		return w.podsByNamespace[filterNamespace], true
	}
	return w.podSnapshot, true
}

// ================================================================
// Main Audit
// ================================================================

func (w *WasteAuditor) AuditWaste(filterNamespace string) (*WasteAudit, error) {
	audit := &WasteAudit{
		ScannedAt: time.Now(),
	}

	// Run all detectors
	if err := w.detectAbandonedNamespaces(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Namespace scan skipped: %v\n", err)
		audit.addDetectorWarning("Abandoned namespaces", err)
	}

	if err := w.detectStalePods(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Pod scan skipped: %v\n", err)
		audit.addDetectorWarning("Zombie and idle/unmanaged pods", err)
	}

	if err := w.detectOrphanedPVCs(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  PVC scan skipped: %v\n", err)
		audit.addDetectorWarning("Unattached PVC candidates", err)
	}

	if err := w.detectStaleJobs(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Job scan skipped: %v\n", err)
		audit.addDetectorWarning("Stale jobs", err)
	}

	if err := w.detectZeroReplicaWorkloads(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Deployment scan skipped: %v\n", err)
		audit.addDetectorWarning("Zero-replica workloads", err)
	}

	if err := w.detectOldReplicaSets(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  ReplicaSet scan skipped: %v\n", err)
		audit.addDetectorWarning("Old ReplicaSets (housekeeping)", err)
	}

	if err := w.detectOrphanedServices(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Service scan skipped: %v\n", err)
		audit.addDetectorWarning("Orphaned services", err)
	}

	if err := w.detectBrokenIngresses(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Ingress scan skipped: %v\n", err)
		audit.addDetectorWarning("Broken ingresses", err)
	}

	if err := w.detectMisconfiguredHPAs(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  HPA scan skipped: %v\n", err)
		audit.addDetectorWarning("Misconfigured HPAs", err)
	}

	// Count totals (excluding OldReplicaSets - they're low-severity housekeeping items)
	audit.TotalWasteItems = len(audit.AbandonedNamespaces) +
		len(audit.StalePods) +
		len(audit.OrphanedPVCs) +
		len(audit.StaleJobs) +
		len(audit.ZeroReplicaWorkloads) +
		len(audit.OrphanedServices) +
		len(audit.BrokenIngresses) +
		len(audit.MisconfiguredHPAs)

	return audit, nil
}

func (a *WasteAudit) addDetectorWarning(category string, err error) {
	a.DetectorWarnings = append(a.DetectorWarnings, WasteDetectorWarning{
		Category: category,
		Error:    err.Error(),
	})
}

// formatDurationSince renders the time elapsed since t in the coarsest
// meaningful unit, so a failure that started 2 hours ago reads "2h", not
// the misleading "0 day(s)" that integer-day truncation would produce.
func formatDurationSince(t time.Time) string {
	elapsed := time.Since(t)
	if elapsed < time.Hour {
		mins := int(elapsed.Minutes())
		if mins < 1 {
			mins = 1
		}
		return fmt.Sprintf("%dm", mins)
	}
	if elapsed < 24*time.Hour {
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	}
	return fmt.Sprintf("%dd", int(elapsed.Hours()/24))
}

// ================================================================
// 1. Abandoned Namespaces
// ================================================================

func (w *WasteAuditor) detectAbandonedNamespaces(audit *WasteAudit, filterNamespace string) error {
	if filterNamespace != "" {
		return nil // namespace filter means we're already scoped
	}

	nsList, err := w.clientset.CoreV1().Namespaces().List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}

	now := time.Now()

	for _, ns := range nsList.Items {
		nsName := ns.Name

		// default always exists even when empty — not a candidate for abandonment.
		if nsName == "default" {
			continue
		}

		// Skip infrastructure namespaces (reuse same logic as network command)
		if isInfraPattern(nsName) {
			continue
		}

		ageDays := int(now.Sub(ns.CreationTimestamp.Time).Hours() / 24)
		if ageDays < w.minAgeDays {
			continue
		}

		// Count running pods. A non-nil map, including an empty map, is a
		// cluster-wide snapshot; otherwise retain the legacy namespace LIST.
		var namespacePods []corev1.Pod
		if w.podsByNamespace == nil {
			pods, err := w.clientset.CoreV1().Pods(nsName).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
			if err != nil {
				return fmt.Errorf("list pods in namespace %q: %w", nsName, err)
			}
			namespacePods = pods.Items
		} else {
			namespacePods = w.podsByNamespace[nsName]
		}

		podCount := len(namespacePods)
		runningCount := 0
		for _, p := range namespacePods {
			if p.Status.Phase == corev1.PodRunning {
				runningCount++
			}
		}

		// Build data-driven reason
		var reason string
		var score float64

		if podCount == 0 {
			reason = fmt.Sprintf("No pods found. Namespace resource age: %d days", ageDays)
			score = float64(ageDays) * 0.8
		} else if runningCount == 0 {
			reason = fmt.Sprintf("%d pod(s) exist but none are in Running phase. Namespace is %d days old", podCount, ageDays)
			score = float64(ageDays)*0.6 + float64(podCount)*2
		} else {
			continue // has running pods, not abandoned
		}

		if score < 10 {
			continue
		}

		audit.AbandonedNamespaces = append(audit.AbandonedNamespaces, AbandonedNamespace{
			Name:     nsName,
			AgeDays:  ageDays,
			PodCount: podCount,
			Reason:   reason,
			Score:    score,
		})
	}

	sort.Slice(audit.AbandonedNamespaces, func(i, j int) bool {
		return audit.AbandonedNamespaces[i].Score > audit.AbandonedNamespaces[j].Score
	})

	return nil
}

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

		// ── ACTIVE POD-FAILURE CLASSIFICATION ───────────────────────
		// The CLI and dashboard now reduce the same independent container
		// signals through pkg/classify. This deliberately inspects every
		// container before deciding; API container order cannot choose the
		// winning issue. Age is not a gate for an active malfunction.
		if stale, ok := classifyStalePodFailure(pod, ageDays, hasProbeFailureEvent); ok {
			audit.StalePods = append(audit.StalePods, stale)
			goto nextPod
		}

		// ── IDLE detection: old pod, no recent restart activity ──────
		// Age gate applies here: we only flag idle pods that have been
		// sitting around longer than minAgeDays.
		if ageDays < w.minAgeDays {
			goto nextPod
		}
		{
			// Key insight: creationTimestamp never resets on restart.
			// We check LAST restart time to exclude recently-active pods.
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

			// Only flag as idle if last activity was also old
			// A pod that restarted 2 days ago is NOT idle even if created 90 days ago
			if lastActivityDays < w.minAgeDays {
				goto nextPod
			}

			// IDLE signal = bare pod with no owning controller.
			// A pod managed by Deployment/DaemonSet/StatefulSet is actively maintained -
			// 0 restarts on those is healthy, NOT a waste signal.
			// Real waste = manually created pods (kubectl run, raw Pod manifest)
			// that someone forgot about.
			isBarePod := true
			for _, ref := range pod.OwnerReferences {
				if ref.Kind == "ReplicaSet" || ref.Kind == "DaemonSet" ||
					ref.Kind == "StatefulSet" || ref.Kind == "Job" ||
					ref.Kind == "CronJob" {
					isBarePod = false
					break
				}
			}

			if isBarePod && pod.Status.Phase == corev1.PodRunning {
				explanation := fmt.Sprintf(
					"Running Pod is %d days old and has no owner reference of kind ReplicaSet, DaemonSet, "+
						"StatefulSet, Job, or CronJob. Restart count: %d.",
					ageDays, totalRestarts,
				)

				audit.StalePods = append(audit.StalePods, StalePod{
					Name:             pod.Name,
					Namespace:        pod.Namespace,
					Kind:             StalePodIdle,
					AgeDays:          ageDays,
					LastActivityDays: lastActivityDays,
					RestartCount:     totalRestarts,
					Reason:           explanation,
					Score:            float64(ageDays) * 0.5,
				})
			}
		}

	nextPod:
	}

	sort.Slice(audit.StalePods, func(i, j int) bool {
		return audit.StalePods[i].Score > audit.StalePods[j].Score
	})

	return eventScanErr
}

// ================================================================
// 3. Orphaned PVCs
// ================================================================

func (w *WasteAuditor) detectOrphanedPVCs(audit *WasteAudit, filterNamespace string) error {
	pvcs, err := w.clientset.CoreV1().PersistentVolumeClaims(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}

	// Build set of PVCs actively used by pods.
	podItems, shared := w.sharedPods(filterNamespace)
	if !shared {
		pods, err := w.clientset.CoreV1().Pods(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
		if err != nil {
			return fmt.Errorf("list pods for PVC reference detection: %w", err)
		}
		podItems = pods.Items
	}

	usedPVCs := map[string]bool{}
	for _, pod := range podItems {
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				key := pod.Namespace + "/" + vol.PersistentVolumeClaim.ClaimName
				usedPVCs[key] = true
			}
		}
	}

	now := time.Now()

	for _, pvc := range pvcs.Items {
		if isInfraPattern(pvc.Namespace) {
			continue
		}

		ageDays := int(now.Sub(pvc.CreationTimestamp.Time).Hours() / 24)
		if ageDays < w.minAgeDays {
			continue
		}

		storage, requestKnown := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		requestedBytes := storage.Value()
		// Keep the historical ranking input separate from accurate quantities.
		sizeGB := legacyPVCScoreSize(requestedBytes)
		requestedStorage := "Unknown"
		if requestKnown {
			requestedStorage = FormatWasteBytes(requestedBytes)
		}

		pvcKey := pvc.Namespace + "/" + pvc.Name

		var status PVCStatus
		var reason string
		var score float64

		switch pvc.Status.Phase {
		case corev1.ClaimPending:
			// Resource age gates eligibility; current Pending phase does not establish binding history.
			status = PVCNeverBound
			reason = fmt.Sprintf(
				"PVC currently reports Pending. Resource age: %d days. "+
					"Binding history and provisioner state were not checked. "+
					"Storage may or may not be provisioned depending on the provisioner.",
				ageDays,
			)
			score = float64(ageDays)*0.6 + float64(sizeGB)*0.4

		case corev1.ClaimLost:
			status = PVCReleased
			reason = fmt.Sprintf(
				"PVC currently reports Lost. Resource age: %d days. PV existence was not independently checked. "+
					"Review the PVC and storage-system state before making changes.",
				ageDays,
			)
			score = float64(ageDays) * 0.7

		case corev1.ClaimBound:
			// Check if any pod is actually using it
			if !usedPVCs[pvcKey] {
				status = PVCBoundNoPod
				reason = fmt.Sprintf(
					"PVC is Bound to a PV, and no currently listed pod in namespace %q references it. "+
						"The PVC requests %s of storage and is %d days old. "+
						"Retained data, a scaled-down StatefulSet, or an intentionally stopped workload may explain this state.",
					pvc.Namespace, requestedStorage, ageDays,
				)
				score = float64(ageDays)*0.5 + float64(sizeGB)*0.3
			} else {
				continue // actively used
			}
		}

		audit.OrphanedPVCs = append(audit.OrphanedPVCs, OrphanedPVC{
			Name:           pvc.Name,
			Namespace:      pvc.Namespace,
			SizeGB:         int(requestedBytes / (1 << 30)),
			RequestedBytes: requestedBytes,
			RequestKnown:   requestKnown,
			Status:         status,
			AgeDays:        ageDays,
			Reason:         reason,
			Score:          score,
		})
		audit.RequestedStorageBytes += requestedBytes
		audit.OrphanedPVCStorageGB = int(audit.RequestedStorageBytes / (1 << 30))
	}

	sort.Slice(audit.OrphanedPVCs, func(i, j int) bool {
		return audit.OrphanedPVCs[i].Score > audit.OrphanedPVCs[j].Score
	})

	return nil
}

// ================================================================
// 4. Stale Jobs & CronJobs
// ================================================================

func (w *WasteAuditor) detectStaleJobs(audit *WasteAudit, filterNamespace string) error {
	jobs, err := w.clientset.BatchV1().Jobs(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}

	now := time.Now()

	for _, job := range jobs.Items {
		if isInfraPattern(job.Namespace) {
			continue
		}

		ageDays := int(now.Sub(job.CreationTimestamp.Time).Hours() / 24)
		if ageDays < w.minAgeDays {
			continue
		}

		// Skip jobs owned by a CronJob (handled separately)
		ownedByCronJob := false
		for _, ref := range job.OwnerReferences {
			if ref.Kind == "CronJob" {
				ownedByCronJob = true
				break
			}
		}
		if ownedByCronJob {
			continue
		}

		var jobStatus string
		var reason string
		var score float64

		if job.Status.Succeeded > 0 {
			jobStatus = "Completed"
			reason = fmt.Sprintf(
				"Job reports successful Pod attempts. Resource age: %d days. "+
					"Successful attempts do not establish terminal Job completion. Review status and retention requirements.",
				ageDays,
			)
			score = float64(ageDays) * 0.6
		} else if job.Status.Failed > 0 {
			jobStatus = "Failed"
			reason = fmt.Sprintf(
				"Job has %d failed attempt(s) and is %d days old. "+
					"Failed attempts do not establish terminal failure or stopped retries. "+
					"Review current Job status and failure evidence.",
				job.Status.Failed, ageDays,
			)
			score = float64(ageDays)*0.5 + float64(job.Status.Failed)*5
		} else {
			continue
		}

		audit.StaleJobs = append(audit.StaleJobs, StaleJob{
			Name:               job.Name,
			Namespace:          job.Namespace,
			JobStatus:          jobStatus,
			AttemptCountsKnown: true,
			SucceededPods:      job.Status.Succeeded,
			FailedPods:         job.Status.Failed,
			AgeDays:            ageDays,
			Reason:             reason,
			Score:              score,
		})
	}

	// CronJobs - detect misconfigured ones
	cronJobs, err := w.clientset.BatchV1().CronJobs(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err == nil {
		for _, cj := range cronJobs.Items {
			if isInfraPattern(cj.Namespace) {
				continue
			}

			ageDays := int(now.Sub(cj.CreationTimestamp.Time).Hours() / 24)
			if ageDays < w.minAgeDays {
				continue
			}

			// No lastScheduleTime was retained in the current status.
			if cj.Status.LastScheduleTime == nil {
				reason := fmt.Sprintf(
					"CronJob is %d days old and has no lastScheduleTime in the observed status. "+
						"This does not establish execution history; review the schedule and suspension intent. "+
						"Schedule: '%s', Suspended: %v",
					ageDays, cj.Spec.Schedule, cj.Spec.Suspend != nil && *cj.Spec.Suspend,
				)
				audit.StaleJobs = append(audit.StaleJobs, StaleJob{
					Name:      cj.Name,
					Namespace: cj.Namespace,
					IsCronJob: true,
					Schedule:  cj.Spec.Schedule,
					Suspended: cj.Spec.Suspend,
					JobStatus: "NeverScheduled",
					AgeDays:   ageDays,
					Reason:    reason,
					Score:     float64(ageDays) * 0.4,
				})
			}

			// Explicitly omitted history limits; Kubernetes defaults still apply.
			if cj.Spec.SuccessfulJobsHistoryLimit == nil && cj.Spec.FailedJobsHistoryLimit == nil {
				reason := fmt.Sprintf(
					"CronJob has no successfulJobsHistoryLimit or failedJobsHistoryLimit set. " +
						"Omitted history limits use Kubernetes defaults (3 successful Jobs and 1 failed Job), not unlimited retention. " +
						"Review effective retention settings and owner requirements.",
				)
				audit.StaleJobs = append(audit.StaleJobs, StaleJob{
					Name:      cj.Name,
					Namespace: cj.Namespace,
					IsCronJob: true,
					Schedule:  cj.Spec.Schedule,
					Suspended: cj.Spec.Suspend,
					JobStatus: "NoHistoryLimit",
					AgeDays:   ageDays,
					Reason:    reason,
					Score:     float64(ageDays) * 0.2,
				})
			}
		}
	}

	sort.Slice(audit.StaleJobs, func(i, j int) bool {
		return audit.StaleJobs[i].Score > audit.StaleJobs[j].Score
	})

	return nil
}

// ================================================================
// 5. Zero-Replica Workloads
// ================================================================

func (w *WasteAuditor) detectZeroReplicaWorkloads(audit *WasteAudit, filterNamespace string) error {
	now := time.Now()

	// Deployments
	deployments, err := w.clientset.AppsV1().Deployments(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}

	for _, d := range deployments.Items {
		if isInfraPattern(d.Namespace) {
			continue
		}

		ageDays := int(now.Sub(d.CreationTimestamp.Time).Hours() / 24)
		if ageDays < w.minAgeDays {
			continue
		}

		if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
			reason := fmt.Sprintf(
				"Deployment currently requests 0 replicas. Resource age: %d days. "+
					"Scale-transition history and associated resources were not checked. Review workload intent.",
				ageDays,
			)
			audit.ZeroReplicaWorkloads = append(audit.ZeroReplicaWorkloads, ZeroReplicaWorkload{
				Name:      d.Name,
				Namespace: d.Namespace,
				Kind:      "Deployment",
				AgeDays:   ageDays,
				Reason:    reason,
				Score:     float64(ageDays) * 0.5,
			})
		}
	}

	// StatefulSets
	statefulsets, err := w.clientset.AppsV1().StatefulSets(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err == nil {
		for _, s := range statefulsets.Items {
			if isInfraPattern(s.Namespace) {
				continue
			}

			ageDays := int(now.Sub(s.CreationTimestamp.Time).Hours() / 24)
			if ageDays < w.minAgeDays {
				continue
			}

			if s.Spec.Replicas != nil && *s.Spec.Replicas == 0 {
				reason := fmt.Sprintf(
					"StatefulSet currently requests 0 replicas. Resource age: %d days. "+
						"Scale-transition history and associated PVCs were not checked. Review workload and data-retention intent.",
					ageDays,
				)
				audit.ZeroReplicaWorkloads = append(audit.ZeroReplicaWorkloads, ZeroReplicaWorkload{
					Name:      s.Name,
					Namespace: s.Namespace,
					Kind:      "StatefulSet",
					AgeDays:   ageDays,
					Reason:    reason,
					Score:     float64(ageDays)*0.5 + 10, // StatefulSet scores higher due to PVC risk
				})
			}
		}
	}

	sort.Slice(audit.ZeroReplicaWorkloads, func(i, j int) bool {
		return audit.ZeroReplicaWorkloads[i].Score > audit.ZeroReplicaWorkloads[j].Score
	})

	return nil
}

// ================================================================
// 6. Old ReplicaSets (retention review)
// ================================================================

func (w *WasteAuditor) detectOldReplicaSets(audit *WasteAudit, filterNamespace string) error {
	rsList, err := w.clientset.AppsV1().ReplicaSets(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}

	now := time.Now()

	for _, rs := range rsList.Items {
		if isInfraPattern(rs.Namespace) {
			continue
		}

		// Only care about old RSes with 0 desired replicas
		if rs.Spec.Replicas != nil && *rs.Spec.Replicas != 0 {
			continue
		}

		ageDays := int(now.Sub(rs.CreationTimestamp.Time).Hours() / 24)
		if ageDays < w.minAgeDays {
			continue
		}

		// Find owner deployment name
		ownerDeploy := ""
		for _, ref := range rs.OwnerReferences {
			if ref.Kind == "Deployment" {
				ownerDeploy = ref.Name
				break
			}
		}

		reason := fmt.Sprintf(
			"ReplicaSet resource age: %d days. Deployment owner reference: %q. "+
				"Review desired replicas and rollback retention; rollout revision was not checked.",
			ageDays, ownerDeploy,
		)

		audit.OldReplicaSets = append(audit.OldReplicaSets, OldReplicaSet{
			Name:            rs.Name,
			Namespace:       rs.Namespace,
			AgeDays:         ageDays,
			OwnerDeployment: ownerDeploy,
			DesiredReplicas: rs.Spec.Replicas,
			Reason:          reason,
			Score:           float64(ageDays) * 0.3,
		})
	}

	sort.Slice(audit.OldReplicaSets, func(i, j int) bool {
		return audit.OldReplicaSets[i].Score > audit.OldReplicaSets[j].Score
	})

	return nil
}

// ================================================================
// 7. Orphaned Services
// ================================================================

func (w *WasteAuditor) detectOrphanedServices(audit *WasteAudit, filterNamespace string) error {
	services, err := w.clientset.CoreV1().Services(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}
	podItems, shared := w.sharedPods(filterNamespace)
	if !shared {
		pods, err := w.clientset.CoreV1().Pods(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
		if err != nil {
			return fmt.Errorf("list pods for Service selector matching: %w", err)
		}
		podItems = pods.Items
	}

	now := time.Now()

	for _, svc := range services.Items {
		if isInfraPattern(svc.Namespace) {
			continue
		}

		// Skip kubernetes default service
		if svc.Name == "kubernetes" && svc.Namespace == "default" {
			continue
		}

		// ExternalName services DNS-alias to external hosts — they have no endpoints by design.
		// Flagging them as "no endpoints" is always a false positive.
		if svc.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}

		// Services with no selector are intentional: manually-managed endpoints,
		// headless StatefulSet discovery, or external endpoint objects.
		// Only services WITH a selector that matches nothing are genuinely orphaned.
		if len(svc.Spec.Selector) == 0 {
			continue
		}

		ageDays := int(now.Sub(svc.CreationTimestamp.Time).Hours() / 24)
		if ageDays < w.minAgeDays {
			continue
		}

		selector := labels.SelectorFromSet(svc.Spec.Selector)
		matchedPods := 0
		for _, pod := range podItems {
			if pod.Namespace == svc.Namespace && selector.Matches(labels.Set(pod.Labels)) {
				matchedPods++
			}
		}

		if matchedPods == 0 {
			isLB := svc.Spec.Type == corev1.ServiceTypeLoadBalancer

			var reason string
			if isLB {
				reason = fmt.Sprintf(
					"LoadBalancer Service is %d days old, and selector %v matches zero currently listed pods in namespace %q. "+
						"Review workload ownership and cloud billing before making changes.",
					ageDays, svc.Spec.Selector, svc.Namespace,
				)
			} else {
				reason = fmt.Sprintf(
					"Service (type: %s) is %d days old, and selector %v matches zero currently listed pods in namespace %q.",
					svc.Spec.Type, ageDays, svc.Spec.Selector, svc.Namespace,
				)
			}

			score := float64(ageDays) * 0.4
			if isLB {
				score += 30 // LoadBalancers score higher due to cloud cost
			}

			audit.OrphanedServices = append(audit.OrphanedServices, OrphanedService{
				Name:      svc.Name,
				Namespace: svc.Namespace,
				Type:      string(svc.Spec.Type),
				AgeDays:   ageDays,
				IsLB:      isLB,
				Selector:  svc.Spec.Selector,
				Reason:    reason,
				Score:     score,
			})
		}
	}

	sort.Slice(audit.OrphanedServices, func(i, j int) bool {
		return audit.OrphanedServices[i].Score > audit.OrphanedServices[j].Score
	})

	return nil
}

// ================================================================
// 8. Broken Ingresses
// ================================================================

func (w *WasteAuditor) detectBrokenIngresses(audit *WasteAudit, filterNamespace string) error {
	ingresses, err := w.clientset.NetworkingV1().Ingresses(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}

	now := time.Now()

	for _, ing := range ingresses.Items {
		if isInfraPattern(ing.Namespace) {
			continue
		}

		ageDays := int(now.Sub(ing.CreationTimestamp.Time).Hours() / 24)

		// No age gate: a backend with zero ready endpoints is a current
		// condition, not something that needs time to become meaningful.
		// Age-gating this finding hides a currently-broken route for the
		// entire gate window.

		missingBackends := []string{}

		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service == nil {
					continue
				}
				svcName := path.Backend.Service.Name
				hasReady, err := kube.ServiceHasReadyEndpoints(w.ctx, w.clientset, ing.Namespace, svcName)
				if err != nil {
					// An API failure means endpoint status is UNKNOWN, not
					// zero. Surface it as a detector warning rather than
					// fabricating an outage finding.
					audit.addDetectorWarning("Broken ingresses",
						fmt.Errorf("checking endpoints for %s/%s (backend of ingress %s): %w", ing.Namespace, svcName, ing.Name, err))
					continue
				}
				if !hasReady {
					missingBackends = append(missingBackends, fmt.Sprintf("%s → %s (no ready endpoints)", rule.Host, svcName))
				}
			}
		}

		if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
			svcName := ing.Spec.DefaultBackend.Service.Name
			hasReady, err := kube.ServiceHasReadyEndpoints(w.ctx, w.clientset, ing.Namespace, svcName)
			if err != nil {
				audit.addDetectorWarning("Broken ingresses",
					fmt.Errorf("checking endpoints for %s/%s (default backend of ingress %s): %w", ing.Namespace, svcName, ing.Name, err))
			} else if !hasReady {
				missingBackends = append(missingBackends, fmt.Sprintf("default-backend → %s (no ready endpoints)", svcName))
			}
		}

		if len(missingBackends) > 0 {
			hosts := []string{}
			for _, rule := range ing.Spec.Rules {
				if rule.Host != "" {
					hosts = append(hosts, rule.Host)
				}
			}

			// Evidence-only wording: we observed zero ready endpoints, not
			// actual request failures.
			reason := fmt.Sprintf(
				"Ingress backend currently has no ready endpoints for: %s.",
				strings.Join(missingBackends, "; "),
			)

			audit.BrokenIngresses = append(audit.BrokenIngresses, BrokenIngress{
				Name:      ing.Name,
				Namespace: ing.Namespace,
				AgeDays:   ageDays,
				Hosts:     hosts,
				Reason:    reason,
				Score:     float64(len(missingBackends)) * 10,
				IsActive:  true,
			})
		}
	}

	sort.Slice(audit.BrokenIngresses, func(i, j int) bool {
		return audit.BrokenIngresses[i].Score > audit.BrokenIngresses[j].Score
	})

	return nil
}

// ================================================================
// 9. Misconfigured HPAs
// ================================================================

func (w *WasteAuditor) detectMisconfiguredHPAs(audit *WasteAudit, filterNamespace string) error {
	// Try v2 first (preferred, available in k8s 1.23+)
	hpasV2, err := w.clientset.AutoscalingV2().HorizontalPodAutoscalers(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})

	// If v2 fails, try v1 (older clusters)
	if err != nil {
		hpasV1, errV1 := w.clientset.AutoscalingV1().HorizontalPodAutoscalers(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
		if errV1 != nil {
			return fmt.Errorf("list HPAs using autoscaling/v2 (%v) and autoscaling/v1: %w", err, errV1)
		}

		// Convert v1 to v2 format for unified processing
		hpasV2 = &autoscalingv2.HorizontalPodAutoscalerList{Items: make([]autoscalingv2.HorizontalPodAutoscaler, len(hpasV1.Items))}
		for i, v1hpa := range hpasV1.Items {
			hpasV2.Items[i] = autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: v1hpa.ObjectMeta,
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind:       v1hpa.Spec.ScaleTargetRef.Kind,
						Name:       v1hpa.Spec.ScaleTargetRef.Name,
						APIVersion: v1hpa.Spec.ScaleTargetRef.APIVersion,
					},
					MinReplicas: v1hpa.Spec.MinReplicas,
					MaxReplicas: v1hpa.Spec.MaxReplicas,
				},
				Status: autoscalingv2.HorizontalPodAutoscalerStatus{
					CurrentReplicas: v1hpa.Status.CurrentReplicas,
					DesiredReplicas: v1hpa.Status.DesiredReplicas,
					Conditions:      []autoscalingv2.HorizontalPodAutoscalerCondition{}, // v1 API does not support conditions
				},
			}
		}
	}

	now := time.Now()

	for _, hpa := range hpasV2.Items {
		if isInfraPattern(hpa.Namespace) {
			continue
		}

		ageDays := int(now.Sub(hpa.CreationTimestamp.Time).Hours() / 24)

		minReplicas := int32(1)
		if hpa.Spec.MinReplicas != nil {
			minReplicas = *hpa.Spec.MinReplicas
		}

		// A currently K8s-reported ScalingActive=False is a live condition —
		// no age gate. Duration comes from the condition's own transition
		// time (rendered in the coarsest meaningful unit — minutes, hours,
		// or days), never substituted with the HPA object's age.
		scalingInactive := false
		for _, cond := range hpa.Status.Conditions {
			if cond.Type == "ScalingActive" && cond.Status == "False" {
				scalingInactive = true

				var durationClause string
				if !cond.LastTransitionTime.IsZero() {
					durationClause = fmt.Sprintf(" for %s", formatDurationSince(cond.LastTransitionTime.Time))
				}

				reason := fmt.Sprintf(
					"HPA targeting '%s' currently reports ScalingActive=False%s (reason: %s - %s). "+
						"Review the reported condition and target configuration; application impact was not measured.",
					hpa.Spec.ScaleTargetRef.Name, durationClause,
					cond.Reason, cond.Message,
				)
				audit.MisconfiguredHPAs = append(audit.MisconfiguredHPAs, MisconfiguredHPA{
					Name:        hpa.Name,
					Namespace:   hpa.Namespace,
					TargetName:  hpa.Spec.ScaleTargetRef.Name,
					MinReplicas: minReplicas,
					MaxReplicas: hpa.Spec.MaxReplicas,
					AgeDays:     ageDays,
					Condition:   string(cond.Reason),
					Reason:      reason,
					Score:       100, // active failure — ranks above tuning candidates
					IsActive:    true,
				})
				break
			}
		}

		// AlwaysAtMin is a point-in-time snapshot (current==desired==min at
		// THIS scan), not observed history — it must not claim the HPA
		// "never scaled up". Kept age-gated: this is a tuning-review
		// candidate, not a proven active failure. Suppressed when the same
		// HPA already produced a ScalingActive=False finding this scan, so
		// one broken HPA doesn't produce two overlapping findings.
		if !scalingInactive &&
			ageDays >= w.minAgeDays &&
			hpa.Status.CurrentReplicas == minReplicas &&
			hpa.Status.DesiredReplicas == minReplicas &&
			ageDays > 30 {
			reason := fmt.Sprintf(
				"HPA is %d days old and currently has both current and desired replicas "+
					"equal to minReplicas (%d) at this scan. Review scaling history and demand "+
					"before changing configuration.",
				ageDays, minReplicas,
			)
			audit.MisconfiguredHPAs = append(audit.MisconfiguredHPAs, MisconfiguredHPA{
				Name:        hpa.Name,
				Namespace:   hpa.Namespace,
				TargetName:  hpa.Spec.ScaleTargetRef.Name,
				MinReplicas: minReplicas,
				MaxReplicas: hpa.Spec.MaxReplicas,
				AgeDays:     ageDays,
				Condition:   "AlwaysAtMin",
				Reason:      reason,
				Score:       float64(ageDays) * 0.2,
				IsActive:    false,
			})
		}
	}

	return nil
}

// ================================================================
// Print Functions
// ================================================================

func PrintWasteAudit(audit *WasteAudit, minAgeDays int) {
	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║           CLUSTER WASTE & DRIFT ANALYSIS                  ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Age-gated checks: %2d days  │  Suggestions only - no changes made  ║\n", minAgeDays)
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	if audit == nil {
		printExecutiveSummary(nil)
		return
	}

	// Executive summary — high-level picture before the detail
	printExecutiveSummary(audit)

	// Scorecard
	printWasteScorecard(audit)

	// Details by category
	printAbandonedNamespaces(audit)
	printStalePods(audit)
	printOrphanedPVCs(audit)
	printStaleJobs(audit)
	printZeroReplicaWorkloads(audit)
	printOldReplicaSets(audit)
	printOrphanedServices(audit)
	printBrokenIngresses(audit)
	printMisconfiguredHPAs(audit)

	// Summary actions
	printWasteSummary(audit)
}

// splitMisconfiguredHPAs separates currently-active failures
// (ScalingActive=False) from age-gated tuning-review candidates
// (AlwaysAtMin), so callers classify waste findings without conflating a
// live break with a review suggestion.
func splitMisconfiguredHPAs(hpas []MisconfiguredHPA) (active, tuning int) {
	for _, hpa := range hpas {
		if hpa.IsActive {
			active++
		} else {
			tuning++
		}
	}
	return active, tuning
}

func printExecutiveSummary(audit *WasteAudit) {
	p := BuildWastePresentation(audit)
	fmt.Println("EXECUTIVE SUMMARY")
	fmt.Printf("Scanned: %s\n", p.ScanTimeLabel())
	fmt.Printf("Finding count: %d | Distinct resource count: %d\n", p.Counts.Findings, p.Counts.DistinctResources)
	fmt.Printf("Operational findings: %d | Housekeeping/retention findings: %d | Other review findings: %d\n",
		p.Counts.Operational, p.Counts.Retention, p.Counts.Review)
	fmt.Println("Operational counts are audit findings, not incident-store counts; historical and inferred evidence may be included.")
	fmt.Printf("Candidate PVC requests: %s (%d quantities unknown)\n", FormatWasteBytes(p.RequestedStorageBytes), p.UnknownStorageRequests)
	fmt.Printf("Coverage: %s\n", p.Coverage)
	for _, warning := range p.Warnings {
		fmt.Printf("Check warning: %s: %s\n", warning.Category, warning.Error)
	}
	if p.Counts.Findings == 0 {
		fmt.Println("No findings were reported by the available checks; this does not establish a clean cluster.")
	}
}

func printWasteScorecard(audit *WasteAudit) {
	fmt.Println("WASTE SCORECARD")
	fmt.Println("═══════════════════════════════════════════════════════════")

	printScoreRow("Namespace Activity Review", len(audit.AbandonedNamespaces), "🔴")
	zombieCount := 0
	bareCount := 0
	for _, p := range audit.StalePods {
		if p.Kind == StalePodZombie {
			zombieCount++
		} else {
			bareCount++
		}
	}
	printScoreRow("Pod Failure Evidence", zombieCount, "🔴")
	printScoreRow("Pod Ownership Review", bareCount, "🔴")
	printScoreRow("PVC State / Reference Review", len(audit.OrphanedPVCs), "🔴")
	printScoreRow("Service Selector Review", len(audit.OrphanedServices), "🟡")
	printScoreRow("Ingress Backend Evidence", len(audit.BrokenIngresses), "🔴")
	printScoreRow("Job / CronJob Retention Review", len(audit.StaleJobs), "🟡")
	printScoreRow("Zero-Replica Workloads", len(audit.ZeroReplicaWorkloads), "🟡")
	printScoreRow("ReplicaSet Retention Review", len(audit.OldReplicaSets), "🟢")
	hpaEmoji := "🟢" // tuning candidates only (AlwaysAtMin)
	if active, _ := splitMisconfiguredHPAs(audit.MisconfiguredHPAs); active > 0 {
		hpaEmoji = "🔴" // at least one HPA currently reporting ScalingActive=False
	}
	printScoreRow("HPA Configuration Review", len(audit.MisconfiguredHPAs), hpaEmoji)

	fmt.Println("───────────────────────────────────────────────────────────")
	fmt.Printf("  Finding count:  %d\n", BuildWastePresentation(audit).Counts.Findings)
	fmt.Println()
}

func printScoreRow(label string, count int, emoji string) {
	if count == 0 {
		fmt.Printf("  ✅ %-28s 0\n", label+":")
	} else {
		fmt.Printf("  %s %-28s %d\n", emoji, label+":", count)
	}
}

func printAbandonedNamespaces(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Namespace activity review")
	if len(audit.AbandonedNamespaces) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🔴 NAMESPACE ACTIVITY REVIEW (%d)\n", len(audit.AbandonedNamespaces))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, ns := range audit.AbandonedNamespaces {
		fmt.Printf("\n  📁 %s\n", ns.Name)
		fmt.Printf("     Age:      %d days\n", ns.AgeDays)
		fmt.Printf("     Pods:     %s\n", podLabel(ns.PodCount))
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printStalePods(audit *WasteAudit) {
	if len(audit.StalePods) == 0 {
		return
	}

	// Separate zombies from idle
	zombies := []StalePod{}
	idle := []StalePod{}
	for _, p := range audit.StalePods {
		if p.Kind == StalePodZombie {
			zombies = append(zombies, p)
		} else {
			idle = append(idle, p)
		}
	}

	if len(zombies) > 0 {
		evidence := BuildWastePresentation(audit).FindingsInCategory("Pod failure evidence")
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Printf("🔴 POD FAILURE EVIDENCE (%d)\n", len(zombies))
		fmt.Println("═══════════════════════════════════════════════════════════")
		for i, p := range zombies {
			fmt.Printf("\n  💀 %s (namespace: %s)\n", p.Name, p.Namespace)
			fmt.Printf("     Classification: %s\n", p.Status)
			fmt.Printf("     Age:      %d days\n", p.AgeDays)
			fmt.Printf("     Restarts: %d\n", p.RestartCount)
			printWasteEvidence(evidence[i])
		}
		fmt.Println()
	}

	if len(idle) > 0 {
		evidence := BuildWastePresentation(audit).FindingsInCategory("Pod ownership review")
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Printf("🔴 POD OWNERSHIP REVIEW (%d)\n", len(idle))
		fmt.Println("═══════════════════════════════════════════════════════════")
		for i, p := range idle {
			fmt.Printf("\n  [BARE] %s (namespace: %s)\n", p.Name, p.Namespace)
			fmt.Printf("     Age:           %d days\n", p.AgeDays)
			fmt.Printf("     Restarts:      %d (total)\n", p.RestartCount)
			printWasteEvidence(evidence[i])
		}
		fmt.Println()
	}
}

func printOrphanedPVCs(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("PVC state / reference review")
	if len(audit.OrphanedPVCs) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🔴 PVC STATE / REFERENCE REVIEW (%d)\n", len(audit.OrphanedPVCs))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, pvc := range audit.OrphanedPVCs {
		sizeStr := evidence[i].Storage
		fmt.Printf("\n  💾 %s (namespace: %s)\n", pvc.Name, pvc.Namespace)
		fmt.Printf("     State evidence: %s\n", evidence[i].Observed)
		fmt.Printf("     Size:    %s\n", sizeStr)
		fmt.Printf("     Age:     %d days\n", pvc.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printStaleJobs(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Job / CronJob retention review")
	if len(audit.StaleJobs) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 JOB / CRONJOB RETENTION REVIEW (%d)\n", len(audit.StaleJobs))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, job := range audit.StaleJobs {
		kind := "Job"
		if job.IsCronJob {
			kind = "CronJob"
		}
		fmt.Printf("\n  ⏰ %s [%s] (namespace: %s)\n", job.Name, kind, job.Namespace)
		fmt.Printf("     Review category: %s\n", evidence[i].Category)
		fmt.Printf("     Age:     %d days\n", job.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printZeroReplicaWorkloads(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Zero-replica workload")
	if len(audit.ZeroReplicaWorkloads) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 ZERO-REPLICA WORKLOADS (%d)\n", len(audit.ZeroReplicaWorkloads))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, w := range audit.ZeroReplicaWorkloads {
		fmt.Printf("\n  📦 %s [%s] (namespace: %s)\n", w.Name, w.Kind, w.Namespace)
		fmt.Printf("     Age:     %d days\n", w.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printOldReplicaSets(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("ReplicaSet retention review")
	if len(audit.OldReplicaSets) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟢 REPLICASET RETENTION REVIEW (%d)\n", len(audit.OldReplicaSets))
	fmt.Println("═══════════════════════════════════════════════════════════")
	// Show top 10 only - these can be numerous
	shown := audit.OldReplicaSets
	remaining := 0
	if len(shown) > 10 {
		remaining = len(shown) - 10
		shown = shown[:10]
	}
	for i, rs := range shown {
		fmt.Printf("  📋 %s (owner: %s, age: %d days)\n", rs.Name, rs.OwnerDeployment, rs.AgeDays)
		printWasteEvidence(evidence[i])
	}
	if remaining > 0 {
		fmt.Printf("  ... and %d more old ReplicaSets\n", remaining)
		fmt.Println("  Inspect: kubectl get rs -A -o yaml; confirm rollback and retention requirements with owners.")
	}
	fmt.Println()
}

func printOrphanedServices(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Service selector review")
	if len(audit.OrphanedServices) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 SERVICE SELECTOR REVIEW (%d)\n", len(audit.OrphanedServices))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, svc := range audit.OrphanedServices {
		lbNote := ""
		if svc.IsLB {
			lbNote = " LoadBalancer — billing not checked"
		}
		fmt.Printf("\n  🔌 %s [%s]%s (namespace: %s)\n", svc.Name, svc.Type, lbNote, svc.Namespace)
		fmt.Printf("     Age:     %d days\n", svc.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printBrokenIngresses(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Ingress backend evidence")
	if len(audit.BrokenIngresses) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 INGRESS BACKEND EVIDENCE (%d)\n", len(audit.BrokenIngresses))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, ing := range audit.BrokenIngresses {
		activeNote := ""
		if ing.IsActive {
			activeNote = " condition reported"
		}
		fmt.Printf("\n  🌐 %s (namespace: %s)%s\n", ing.Name, ing.Namespace, activeNote)
		fmt.Printf("     Hosts:   %s\n", strings.Join(ing.Hosts, ", "))
		fmt.Printf("     Age:     %d days\n", ing.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printMisconfiguredHPAs(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("HPA configuration review")
	if len(audit.MisconfiguredHPAs) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟢 HPA CONFIGURATION REVIEW (%d)\n", len(audit.MisconfiguredHPAs))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, hpa := range audit.MisconfiguredHPAs {
		activeNote := ""
		if hpa.IsActive {
			activeNote = " condition reported"
		}
		fmt.Printf("\n  📈 %s → %s (namespace: %s)%s\n", hpa.Name, hpa.TargetName, hpa.Namespace, activeNote)
		fmt.Printf("     Replicas: min=%d max=%d  Condition: %s\n", hpa.MinReplicas, hpa.MaxReplicas, hpa.Condition)
		fmt.Printf("     Age:      %d days\n", hpa.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printWasteSummary(audit *WasteAudit) {
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("📋 NEXT STEPS")
	fmt.Println("Review observations, workload intent, and retention requirements with the owning team.")
	fmt.Println("Priority is the legacy heuristic, not confidence, financial impact, or cleanup safety.")
	fmt.Println("Use --min-age-days to adjust age-gated checks and --namespace to focus the scan.")
}

func printWasteEvidence(f WasteFinding) {
	fmt.Printf("     Observed: %s\n     Inference: %s\n     Limitations: %s\n     Review: %s\n", f.Observed, f.Inference, f.Limitations, f.Review)
	fmt.Printf("     Priority score: %g (legacy heuristic) | Evidence confidence: %s — %s\n     %s\n", f.Priority, f.Confidence, f.ConfidenceReason, f.Command)
}

// isInfraPattern returns true for Kubernetes system/infrastructure namespaces
// whose pods and resources should be excluded from waste and zombie detection.
// Note: "default" is intentionally NOT excluded here — users regularly run
// workloads there and crash-looping pods must be visible. The abandoned-namespace
// detector handles "default" separately.
func isInfraPattern(name string) bool {
	// aks-command is an AKS internal namespace used by "az aks command invoke".
	// It creates ephemeral pods that look like zombies but are system-managed.
	if name == "aks-command" {
		return true
	}

	infraPatterns := []string{
		"kube-", "istio-", "calico-", "tigera-", "cert-manager",
		"ingress-nginx", "flux-system", "argocd", "velero",
		"longhorn-", "cattle-", "openshift-", "gke-", "azure-",
		"karpenter", "crossplane-",
	}
	for _, pattern := range infraPatterns {
		if strings.HasPrefix(name, pattern) {
			return true
		}
	}
	return false
}

// int64Ptr returns a pointer to an int64 value.
// Used for Kubernetes API ListOptions.TimeoutSeconds.
func int64Ptr(i int64) *int64 {
	return &i
}
