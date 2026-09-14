package analyzer

import (
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file groups the four Waste detectors whose shared responsibility is
// judging whether a controller-owned resource's own lifecycle/configuration
// has gone stale: Stale Jobs & CronJobs, Zero-Replica Deployments/
// StatefulSets, old ReplicaSets kept past their retention window, and
// Horizontal Pod Autoscalers stuck at floor/misconfigured. Each pairs its
// legacy live-client orchestration (detectX) with the pure per-item
// evaluator(s) docs/08 Phase 4D.5 extracted from it (evaluateX), which
// AnalyzeWaste (waste_analysis.go) calls directly from an already-observed
// WasteSnapshot.
//
// This file is in AGENTS.md's 500-650 review band. It is kept as one file
// rather than split further because the four detectors share one
// responsibility question ("is this controller-owned resource stale?") and
// no finer domain boundary exists between them — Jobs/CronJobs,
// Deployments/StatefulSets, and ReplicaSets are all workload-controller
// staleness checks, and HPA is the same question applied to the autoscaler
// controller. Splitting along resource-kind lines here (one file per
// detector) would be a mechanical line-count split, not a responsibility
// split — AGENTS.md explicitly disfavors that.

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
		if finding, ok := evaluateStaleJob(job, w.minAgeDays, now); ok {
			audit.StaleJobs = append(audit.StaleJobs, finding)
		}
	}

	// CronJobs - detect misconfigured ones
	cronJobs, err := w.clientset.BatchV1().CronJobs(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		// A CronJob LIST failure must not discard the plain-Job findings
		// already collected above, and must not be silently swallowed.
		audit.addDetectorWarning("Stale jobs", fmt.Errorf("list cronjobs: %w", err))
	} else {
		for _, cj := range cronJobs.Items {
			audit.StaleJobs = append(audit.StaleJobs, evaluateStaleCronJob(cj, w.minAgeDays, now)...)
		}
	}

	sort.Slice(audit.StaleJobs, func(i, j int) bool {
		return audit.StaleJobs[i].Score > audit.StaleJobs[j].Score
	})

	return nil
}

// evaluateStaleJob decides whether one plain (non-CronJob-owned) Job is a
// stale-job finding. It performs no Kubernetes API calls; now is passed in
// explicitly for the same determinism reason as evaluateAbandonedNamespace.
func evaluateStaleJob(job batchv1.Job, minAgeDays int, now time.Time) (StaleJob, bool) {
	if isInfraPattern(job.Namespace) {
		return StaleJob{}, false
	}

	// Skip jobs owned by a CronJob (handled separately, via evaluateStaleCronJob)
	for _, ref := range job.OwnerReferences {
		if ref.Kind == "CronJob" {
			return StaleJob{}, false
		}
	}

	createdAgeDays := int(now.Sub(job.CreationTimestamp.Time).Hours() / 24)

	// Terminal state is established only by the Kubernetes Job Conditions
	// Kubernetes itself uses to mean "done" (batchv1.JobComplete/JobFailed
	// with Status=True). Succeeded/Failed Pod-attempt counters are live
	// counters that can be nonzero on an active, retrying, or suspended
	// Job; they are never used to decide terminal state here.
	//
	// If a malformed/unusual Job reports both Complete=True and
	// Failed=True at once, Failed wins deterministically (checked first,
	// independent of Conditions slice order), so an ambiguous Job is
	// never treated as if it had finished cleanly, and never produces
	// more than one finding.
	hasFailed, hasComplete := false, false
	for _, cond := range job.Status.Conditions {
		switch {
		case cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue:
			hasFailed = true
		case cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue:
			hasComplete = true
		}
	}

	var jobStatus, reason string
	var ageDays int
	var score float64
	completionTimeKnown := false

	switch {
	case hasFailed:
		// Kubernetes does not record a failure timestamp on Job status;
		// creation age is the only available signal, and must be
		// identified as such, never as "failure age".
		jobStatus = "Failed"
		ageDays = createdAgeDays
		if ageDays < minAgeDays {
			return StaleJob{}, false
		}
		reason = fmt.Sprintf(
			"Job currently reports the Kubernetes Failed terminal condition. "+
				"Resource created %d days ago; this check does not establish when terminal failure occurred. "+
				"Review current Job status and failure evidence.",
			ageDays,
		)
		score = float64(ageDays)*0.5 + float64(job.Status.Failed)*5

	case hasComplete:
		jobStatus = "Completed"
		if job.Status.CompletionTime != nil {
			completionTimeKnown = true
			ageDays = int(now.Sub(job.Status.CompletionTime.Time).Hours() / 24)
		} else {
			// Complete=True without a completionTime is unexpected, but
			// must not invent a completion timestamp. Fall back to
			// creation age and say so explicitly.
			ageDays = createdAgeDays
		}
		if ageDays < minAgeDays {
			return StaleJob{}, false
		}
		if completionTimeKnown {
			reason = fmt.Sprintf("Job reached the Kubernetes Complete condition. Completed %d days ago.", ageDays)
		} else {
			reason = fmt.Sprintf(
				"Job reached the Kubernetes Complete condition, but no completionTime was reported. "+
					"Resource created %d days ago; completion age could not be established, so creation age is shown instead.",
				ageDays,
			)
		}
		score = float64(ageDays) * 0.6

	default:
		// Active, retrying, suspended, or Active=0-with-no-terminal-
		// condition Jobs are not retention-review candidates: nonzero
		// Succeeded/Failed counters alone never establish terminal state.
		return StaleJob{}, false
	}

	return StaleJob{
		Name:                job.Name,
		Namespace:           job.Namespace,
		JobStatus:           jobStatus,
		AttemptCountsKnown:  true,
		SucceededPods:       job.Status.Succeeded,
		FailedPods:          job.Status.Failed,
		AgeDays:             ageDays,
		CompletionTimeKnown: completionTimeKnown,
		Reason:              reason,
		Score:               score,
	}, true
}

// evaluateStaleCronJob evaluates one CronJob for the two independent
// retention-review conditions the legacy detector checks — never-scheduled
// and no-history-limit — returning 0, 1, or 2 findings. It performs no
// Kubernetes API calls.
func evaluateStaleCronJob(cj batchv1.CronJob, minAgeDays int, now time.Time) []StaleJob {
	if isInfraPattern(cj.Namespace) {
		return nil
	}

	ageDays := int(now.Sub(cj.CreationTimestamp.Time).Hours() / 24)
	if ageDays < minAgeDays {
		return nil
	}

	var findings []StaleJob

	// No lastScheduleTime was retained in the current status.
	if cj.Status.LastScheduleTime == nil {
		reason := fmt.Sprintf(
			"CronJob is %d days old and has no lastScheduleTime in the observed status. "+
				"This does not establish execution history; review the schedule and suspension intent. "+
				"Schedule: '%s', Suspended: %v",
			ageDays, cj.Spec.Schedule, cj.Spec.Suspend != nil && *cj.Spec.Suspend,
		)
		findings = append(findings, StaleJob{
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
		reason := "CronJob has no successfulJobsHistoryLimit or failedJobsHistoryLimit set. " +
			"Omitted history limits use Kubernetes defaults (3 successful Jobs and 1 failed Job), not unlimited retention. " +
			"Review effective retention settings and owner requirements."
		findings = append(findings, StaleJob{
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

	return findings
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
		if finding, ok := evaluateZeroReplicaDeployment(d, w.minAgeDays, now); ok {
			audit.ZeroReplicaWorkloads = append(audit.ZeroReplicaWorkloads, finding)
		}
	}

	// StatefulSets
	statefulsets, err := w.clientset.AppsV1().StatefulSets(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		// A StatefulSet LIST failure must not discard the Deployment findings
		// already collected above, and must not be silently swallowed.
		audit.addDetectorWarning("Zero-replica workloads", fmt.Errorf("list statefulsets: %w", err))
	} else {
		for _, s := range statefulsets.Items {
			if finding, ok := evaluateZeroReplicaStatefulSet(s, w.minAgeDays, now); ok {
				audit.ZeroReplicaWorkloads = append(audit.ZeroReplicaWorkloads, finding)
			}
		}
	}

	sort.Slice(audit.ZeroReplicaWorkloads, func(i, j int) bool {
		return audit.ZeroReplicaWorkloads[i].Score > audit.ZeroReplicaWorkloads[j].Score
	})

	return nil
}

// evaluateZeroReplicaDeployment and evaluateZeroReplicaStatefulSet decide
// whether one workload is a zero-replica finding. They perform no
// Kubernetes API calls; now is passed in explicitly for the same
// determinism reason as evaluateAbandonedNamespace.
func evaluateZeroReplicaDeployment(d appsv1.Deployment, minAgeDays int, now time.Time) (ZeroReplicaWorkload, bool) {
	if isInfraPattern(d.Namespace) {
		return ZeroReplicaWorkload{}, false
	}

	ageDays := int(now.Sub(d.CreationTimestamp.Time).Hours() / 24)
	if ageDays < minAgeDays {
		return ZeroReplicaWorkload{}, false
	}

	if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
		return ZeroReplicaWorkload{}, false
	}

	reason := fmt.Sprintf(
		"Deployment currently requests 0 replicas. This Deployment was created %d days ago; "+
			"how long it has been at zero replicas was not established by this check. "+
			"Scale-transition history and associated resources were not checked. Review workload intent.",
		ageDays,
	)
	return ZeroReplicaWorkload{
		Name:      d.Name,
		Namespace: d.Namespace,
		Kind:      "Deployment",
		AgeDays:   ageDays,
		Reason:    reason,
		Score:     float64(ageDays) * 0.5,
	}, true
}

func evaluateZeroReplicaStatefulSet(s appsv1.StatefulSet, minAgeDays int, now time.Time) (ZeroReplicaWorkload, bool) {
	if isInfraPattern(s.Namespace) {
		return ZeroReplicaWorkload{}, false
	}

	ageDays := int(now.Sub(s.CreationTimestamp.Time).Hours() / 24)
	if ageDays < minAgeDays {
		return ZeroReplicaWorkload{}, false
	}

	if s.Spec.Replicas == nil || *s.Spec.Replicas != 0 {
		return ZeroReplicaWorkload{}, false
	}

	reason := fmt.Sprintf(
		"StatefulSet currently requests 0 replicas. This StatefulSet was created %d days ago; "+
			"how long it has been at zero replicas was not established by this check. "+
			"Scale-transition history and associated PVCs were not checked. Review workload and data-retention intent.",
		ageDays,
	)
	return ZeroReplicaWorkload{
		Name:      s.Name,
		Namespace: s.Namespace,
		Kind:      "StatefulSet",
		AgeDays:   ageDays,
		Reason:    reason,
		Score:     float64(ageDays)*0.5 + 10, // StatefulSet scores higher due to PVC risk
	}, true
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
		if finding, ok := evaluateOldReplicaSet(rs, w.minAgeDays, now); ok {
			audit.OldReplicaSets = append(audit.OldReplicaSets, finding)
		}
	}

	sort.Slice(audit.OldReplicaSets, func(i, j int) bool {
		return audit.OldReplicaSets[i].Score > audit.OldReplicaSets[j].Score
	})

	return nil
}

// evaluateOldReplicaSet decides whether one ReplicaSet with 0 desired
// replicas is old enough to be a retention-review finding. It performs no
// Kubernetes API calls; now is passed in explicitly for the same
// determinism reason as evaluateAbandonedNamespace.
func evaluateOldReplicaSet(rs appsv1.ReplicaSet, minAgeDays int, now time.Time) (OldReplicaSet, bool) {
	if isInfraPattern(rs.Namespace) {
		return OldReplicaSet{}, false
	}

	// Only care about old RSes with 0 desired replicas
	if rs.Spec.Replicas != nil && *rs.Spec.Replicas != 0 {
		return OldReplicaSet{}, false
	}

	ageDays := int(now.Sub(rs.CreationTimestamp.Time).Hours() / 24)
	if ageDays < minAgeDays {
		return OldReplicaSet{}, false
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

	return OldReplicaSet{
		Name:            rs.Name,
		Namespace:       rs.Namespace,
		AgeDays:         ageDays,
		OwnerDeployment: ownerDeploy,
		DesiredReplicas: rs.Spec.Replicas,
		Reason:          reason,
		Score:           float64(ageDays) * 0.3,
	}, true
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
		if finding, ok := evaluateMisconfiguredHPA(hpa, w.minAgeDays, now); ok {
			audit.MisconfiguredHPAs = append(audit.MisconfiguredHPAs, finding)
		}
	}

	return nil
}

// evaluateMisconfiguredHPA decides whether one autoscaling/v2-shaped HPA is
// a misconfigured-HPA finding: either a currently-reported ScalingActive=False
// condition (takes priority) or, failing that, an age-gated AlwaysAtMin
// tuning candidate. It performs no Kubernetes API calls; now is passed in
// explicitly for the same determinism reason as evaluateAbandonedNamespace.
func evaluateMisconfiguredHPA(hpa autoscalingv2.HorizontalPodAutoscaler, minAgeDays int, now time.Time) (MisconfiguredHPA, bool) {
	if isInfraPattern(hpa.Namespace) {
		return MisconfiguredHPA{}, false
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
	for _, cond := range hpa.Status.Conditions {
		if cond.Type == "ScalingActive" && cond.Status == "False" {
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
			return MisconfiguredHPA{
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
			}, true
		}
	}

	// AlwaysAtMin is a point-in-time snapshot (current==desired==min at
	// THIS scan), not observed history — it must not claim the HPA
	// "never scaled up". Kept age-gated: this is a tuning-review
	// candidate, not a proven active failure. Only reached when no
	// ScalingActive=False finding was already returned above, so one broken
	// HPA doesn't produce two overlapping findings.
	if ageDays >= minAgeDays &&
		hpa.Status.CurrentReplicas == minReplicas &&
		hpa.Status.DesiredReplicas == minReplicas &&
		ageDays > 30 {
		reason := fmt.Sprintf(
			"This HPA was created %d days ago. At this single scan, current and desired replicas "+
				"both equal minReplicas (%d); how long it has remained at this state was not "+
				"established by this check. Review scaling history and demand before changing configuration.",
			ageDays, minReplicas,
		)
		return MisconfiguredHPA{
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
		}, true
	}

	return MisconfiguredHPA{}, false
}

// isInfraPattern returns true for Kubernetes system/infrastructure namespaces
// whose pods and resources should be excluded from waste and zombie detection.
// Note: "default" is intentionally NOT excluded here — users regularly run
// workloads there and crash-looping pods must be visible. The abandoned-namespace
// detector handles "default" separately.
