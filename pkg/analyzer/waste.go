package analyzer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/kube"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ================================================================
// Types
// ================================================================

type WasteAuditor struct {
	clientset  kubernetes.Interface
	ctx        context.Context
	minAgeDays int
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

	TotalWasteItems       int
	OrphanedPVCStorageGB  int     // total GB across all orphaned PVCs
	EstimatedMonthlyWaste float64 // 0 if monthly cost not provided
}

// ----------------------------------------------------------------
// Abandoned Namespaces
// ----------------------------------------------------------------

type AbandonedNamespace struct {
	Name        string
	AgeDays     int
	PodCount    int
	AllPodsIdle bool
	Reason      string // data-driven explanation
	Score       float64
}

// ----------------------------------------------------------------
// Stale Pods - two distinct categories
// ----------------------------------------------------------------

type StalePodKind string

const (
	StalePodIdle   StalePodKind = "IDLE"   // old + no recent activity
	StalePodZombie StalePodKind = "ZOMBIE" // CrashLoopBackOff / OOMKilled for days
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
	Status string // CrashLoopBackOff, OOMKilled, etc.
	Reason string // data-driven explanation
	Score  float64
}

// ----------------------------------------------------------------
// Orphaned PVCs
// ----------------------------------------------------------------

type PVCStatus string

const (
	PVCNeverBound PVCStatus = "Never bound"               // Available - created but never used
	PVCReleased   PVCStatus = "Released (pod deleted)"    // Released - pod gone
	PVCBoundNoPod PVCStatus = "Bound but no pod using it" // Bound but no pod references it
)

type OrphanedPVC struct {
	Name      string
	Namespace string
	SizeGB    int
	Status    PVCStatus
	AgeDays   int
	Reason    string
	Score     float64
}

// ----------------------------------------------------------------
// Stale Jobs / CronJobs
// ----------------------------------------------------------------

type StaleJob struct {
	Name      string
	Namespace string
	IsCronJob bool
	JobStatus string // Completed, Failed, NeverRan, NoHistoryLimit
	AgeDays   int
	Reason    string
	Score     float64
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
// Old ReplicaSets (leftover from rollouts)
// ----------------------------------------------------------------

type OldReplicaSet struct {
	Name            string
	Namespace       string
	AgeDays         int
	OwnerDeployment string
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
	}

	if err := w.detectStalePods(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Pod scan skipped: %v\n", err)
	}

	if err := w.detectOrphanedPVCs(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  PVC scan skipped: %v\n", err)
	}

	if err := w.detectStaleJobs(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Job scan skipped: %v\n", err)
	}

	if err := w.detectZeroReplicaWorkloads(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Deployment scan skipped: %v\n", err)
	}

	if err := w.detectOldReplicaSets(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  ReplicaSet scan skipped: %v\n", err)
	}

	if err := w.detectOrphanedServices(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Service scan skipped: %v\n", err)
	}

	if err := w.detectBrokenIngresses(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  Ingress scan skipped: %v\n", err)
	}

	if err := w.detectMisconfiguredHPAs(audit, filterNamespace); err != nil {
		fmt.Printf("⚠️  HPA scan skipped: %v\n", err)
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

		// Count running pods
		pods, err := w.clientset.CoreV1().Pods(nsName).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
		if err != nil {
			continue
		}

		podCount := len(pods.Items)
		runningCount := 0
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning {
				runningCount++
			}
		}

		// Build data-driven reason
		var reason string
		var score float64

		if podCount == 0 {
			reason = fmt.Sprintf("No pods found. Namespace is %d days old with zero workloads", ageDays)
			score = float64(ageDays) * 0.8
		} else if runningCount == 0 {
			reason = fmt.Sprintf("%d pod(s) exist but none are Running (all Pending/Failed/Completed). Namespace is %d days old", podCount, ageDays)
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

func (w *WasteAuditor) detectStalePods(audit *WasteAudit, filterNamespace string) error {
	pods, err := w.clientset.CoreV1().Pods(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}

	now := time.Now()

	for _, pod := range pods.Items {
		// Skip system namespaces
		if isInfraPattern(pod.Namespace) {
			continue
		}

		ageDays := int(now.Sub(pod.CreationTimestamp.Time).Hours() / 24)

		// ── ZOMBIE detection: CrashLoopBackOff / OOMKilled ──────────
		// Age gate is deliberately skipped — a crashing pod is a problem
		// on day 0. The minAgeDays filter only applies to idle pod detection.
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				reason := cs.State.Waiting.Reason
				if reason == "CrashLoopBackOff" || reason == "OOMKilled" ||
					reason == "Error" || reason == "ImagePullBackOff" {

					explanation := fmt.Sprintf(
						"Container '%s' has been in %s state. Pod is %d days old with %d restarts. "+
							"This workload is not functioning and consuming scheduling resources.",
						cs.Name, reason, ageDays, cs.RestartCount,
					)

					audit.StalePods = append(audit.StalePods, StalePod{
						Name:         pod.Name,
						Namespace:    pod.Namespace,
						Kind:         StalePodZombie,
						AgeDays:      ageDays,
						RestartCount: cs.RestartCount,
						Status:       reason,
						Reason:       explanation,
						Score:        float64(ageDays)*0.4 + float64(cs.RestartCount)*0.3,
					})
					goto nextPod
				}
			}
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
					"Pod is %d days old and has no owning controller - it is not managed by any "+
						"Deployment, DaemonSet, or StatefulSet. Manually-created pods are often "+
						"used for debugging or testing and left running. Restart count: %d.",
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

	return nil
}

// ================================================================
// 3. Orphaned PVCs
// ================================================================

func (w *WasteAuditor) detectOrphanedPVCs(audit *WasteAudit, filterNamespace string) error {
	pvcs, err := w.clientset.CoreV1().PersistentVolumeClaims(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}

	// Build set of PVCs actively used by pods
	pods, err := w.clientset.CoreV1().Pods(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		fmt.Printf("⚠️  Could not list pods for PVC orphan detection: %v. Continuing with incomplete data.\n", err)
		pods = &corev1.PodList{} // Continue with empty list
	}

	usedPVCs := map[string]bool{}
	for _, pod := range pods.Items {
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

		// Get size
		sizeGB := 0
		if storage, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			sizeGB = int(storage.Value() / (1024 * 1024 * 1024))
			if sizeGB == 0 {
				sizeGB = int(storage.Value() / (1024 * 1024)) // check MB
			}
		}

		pvcKey := pvc.Namespace + "/" + pvc.Name

		var status PVCStatus
		var reason string
		var score float64

		switch pvc.Status.Phase {
		case corev1.ClaimPending:
			// PVC pending for longer than minAgeDays = never successfully bound
			// (normal PVCs bind within seconds/minutes)
			status = PVCNeverBound
			reason = fmt.Sprintf(
				"PVC has been in 'Pending' state for %d days - it was never successfully bound to a PV. "+
					"This usually means no matching PersistentVolume exists or the StorageClass cannot provision one. "+
					"Storage may or may not be provisioned depending on the provisioner.",
				ageDays,
			)
			score = float64(ageDays)*0.6 + float64(sizeGB)*0.4

		case corev1.ClaimLost:
			status = PVCReleased
			reason = fmt.Sprintf(
				"PVC is in 'Lost' state for %d days - the underlying PV no longer exists. "+
					"Data may already be gone. Safe to delete the PVC object.",
				ageDays,
			)
			score = float64(ageDays) * 0.7

		case corev1.ClaimBound:
			// Check if any pod is actually using it
			if !usedPVCs[pvcKey] {
				status = PVCBoundNoPod
				reason = fmt.Sprintf(
					"PVC is Bound to a PV but no running pod is mounting it (checked %d pods in namespace). "+
						"%dGB of storage is allocated but idle for %d days. "+
						"Common cause: StatefulSet scaled down or pod deleted without removing PVC.",
					len(pods.Items), sizeGB, ageDays,
				)
				score = float64(ageDays)*0.5 + float64(sizeGB)*0.3
			} else {
				continue // actively used
			}
		}

		audit.OrphanedPVCs = append(audit.OrphanedPVCs, OrphanedPVC{
			Name:      pvc.Name,
			Namespace: pvc.Namespace,
			SizeGB:    sizeGB,
			Status:    status,
			AgeDays:   ageDays,
			Reason:    reason,
			Score:     score,
		})
		audit.OrphanedPVCStorageGB += sizeGB
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
				"Job completed successfully %d days ago and has not been cleaned up. "+
					"Completed jobs consume namespace quota and clutter `kubectl get jobs` output. "+
					"Consider setting ttlSecondsAfterFinished on future jobs.",
				ageDays,
			)
			score = float64(ageDays) * 0.6
		} else if job.Status.Failed > 0 {
			jobStatus = "Failed"
			reason = fmt.Sprintf(
				"Job has %d failed attempt(s) and is %d days old. "+
					"It is not retrying and will not succeed without intervention. "+
					"Investigate the failure reason then remove.",
				job.Status.Failed, ageDays,
			)
			score = float64(ageDays)*0.5 + float64(job.Status.Failed)*5
		} else {
			continue
		}

		audit.StaleJobs = append(audit.StaleJobs, StaleJob{
			Name:      job.Name,
			Namespace: job.Namespace,
			JobStatus: jobStatus,
			AgeDays:   ageDays,
			Reason:    reason,
			Score:     score,
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

			// CronJob that has never run
			if cj.Status.LastScheduleTime == nil {
				reason := fmt.Sprintf(
					"CronJob is %d days old but has never been scheduled. "+
						"This may indicate a misconfigured schedule, suspended state, or the CronJob controller cannot reach it. "+
						"Schedule: '%s', Suspended: %v",
					ageDays, cj.Spec.Schedule, cj.Spec.Suspend != nil && *cj.Spec.Suspend,
				)
				audit.StaleJobs = append(audit.StaleJobs, StaleJob{
					Name:      cj.Name,
					Namespace: cj.Namespace,
					IsCronJob: true,
					JobStatus: "NeverScheduled",
					AgeDays:   ageDays,
					Reason:    reason,
					Score:     float64(ageDays) * 0.4,
				})
			}

			// CronJob with no history limits (will pile up jobs forever)
			if cj.Spec.SuccessfulJobsHistoryLimit == nil && cj.Spec.FailedJobsHistoryLimit == nil {
				reason := fmt.Sprintf(
					"CronJob has no successfulJobsHistoryLimit or failedJobsHistoryLimit set. " +
						"This means completed/failed jobs will accumulate indefinitely. " +
						"Recommend setting successfulJobsHistoryLimit: 3 and failedJobsHistoryLimit: 1.",
				)
				audit.StaleJobs = append(audit.StaleJobs, StaleJob{
					Name:      cj.Name,
					Namespace: cj.Namespace,
					IsCronJob: true,
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
				"Deployment has been set to 0 replicas for %d days. "+
					"It is not running any pods but still consumes a Deployment object, "+
					"ConfigMaps, and namespace quota. "+
					"If intentionally disabled, consider archiving it instead.",
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
					"StatefulSet has been set to 0 replicas for %d days. "+
						"Its PVCs may still exist and incur storage costs even with no running pods. "+
						"Check associated PVCs before removing.",
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
// 6. Old ReplicaSets (leftover from rollouts)
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
			"ReplicaSet has 0 desired replicas and is %d days old. "+
				"This is a leftover from a previous deployment rollout of '%s'. "+
				"Kubernetes retains old ReplicaSets for rollback history but they accumulate over time.",
			ageDays, ownerDeploy,
		)

		audit.OldReplicaSets = append(audit.OldReplicaSets, OldReplicaSet{
			Name:            rs.Name,
			Namespace:       rs.Namespace,
			AgeDays:         ageDays,
			OwnerDeployment: ownerDeploy,
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

		// Check endpoints
		hasEndpoints, _ := kube.ServiceHasReadyEndpoints(w.ctx, w.clientset, svc.Namespace, svc.Name)

		if !hasEndpoints {
			isLB := svc.Spec.Type == corev1.ServiceTypeLoadBalancer

			var reason string
			if isLB {
				reason = fmt.Sprintf(
					"LoadBalancer service has no active endpoints for %d days. "+
						"Cloud load balancers incur hourly cost even with zero traffic. "+
						"Selector '%v' does not match any running pod.",
					ageDays, svc.Spec.Selector,
				)
			} else {
				reason = fmt.Sprintf(
					"Service (type: %s) has no active endpoints for %d days. "+
						"Selector '%v' does not match any running pod. "+
						"This service cannot route traffic to anything.",
					svc.Spec.Type, ageDays, svc.Spec.Selector,
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
		if ageDays < w.minAgeDays {
			continue
		}

		missingBackends := []string{}

		// Check each rule's backend service exists and has endpoints
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service == nil {
					continue
				}
				svcName := path.Backend.Service.Name
				hasReady, _ := kube.ServiceHasReadyEndpoints(w.ctx, w.clientset, ing.Namespace, svcName)
				if !hasReady {
					missingBackends = append(missingBackends, fmt.Sprintf("%s → %s (no endpoints)", rule.Host, svcName))
				}
			}
		}

		// Also check default backend
		if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
			svcName := ing.Spec.DefaultBackend.Service.Name
			hasReady, _ := kube.ServiceHasReadyEndpoints(w.ctx, w.clientset, ing.Namespace, svcName)
			if !hasReady {
				missingBackends = append(missingBackends, fmt.Sprintf("default-backend → %s (no endpoints)", svcName))
			}
		}

		if len(missingBackends) > 0 {
			hosts := []string{}
			for _, rule := range ing.Spec.Rules {
				if rule.Host != "" {
					hosts = append(hosts, rule.Host)
				}
			}

			reason := fmt.Sprintf(
				"Ingress is %d days old and has %d backend(s) with no active endpoints: %s. "+
					"Traffic arriving at %v will return errors.",
				ageDays, len(missingBackends),
				strings.Join(missingBackends, "; "),
				hosts,
			)

			audit.BrokenIngresses = append(audit.BrokenIngresses, BrokenIngress{
				Name:      ing.Name,
				Namespace: ing.Namespace,
				AgeDays:   ageDays,
				Hosts:     hosts,
				Reason:    reason,
				Score:     float64(ageDays)*0.4 + float64(len(missingBackends))*10,
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
			// Both v2 and v1 failed - skip HPA detection
			return nil
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
		if ageDays < w.minAgeDays {
			continue
		}

		minReplicas := int32(1)
		if hpa.Spec.MinReplicas != nil {
			minReplicas = *hpa.Spec.MinReplicas
		}

		// Check HPA conditions for issues
		for _, cond := range hpa.Status.Conditions {
			if cond.Type == "ScalingActive" && cond.Status == "False" {
				reason := fmt.Sprintf(
					"HPA targeting '%s' has been inactive for %d days. "+
						"Condition: %s - %s. "+
						"The HPA exists but cannot function, so autoscaling is disabled effectively.",
					hpa.Spec.ScaleTargetRef.Name, ageDays,
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
					Score:       float64(ageDays) * 0.3,
				})
				break
			}
		}

		// HPA where current == min for a very long time (not scaling up)
		if hpa.Status.CurrentReplicas == minReplicas &&
			hpa.Status.DesiredReplicas == minReplicas &&
			ageDays > 30 {
			reason := fmt.Sprintf(
				"HPA targeting '%s' has been at minReplicas (%d) for %d days and never scaled up. "+
					"Either the workload has no traffic/load, the metric source is wrong, "+
					"or the HPA thresholds are set too high.",
				hpa.Spec.ScaleTargetRef.Name, minReplicas, ageDays,
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
	fmt.Printf("║  Resources older than %2d days  │  Suggestions only - no changes made  ║\n", minAgeDays)
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	if audit.TotalWasteItems == 0 {
		fmt.Println("✅ No waste detected! Cluster looks clean.")
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

func printExecutiveSummary(audit *WasteAudit) {
	zombieCount := 0
	for _, p := range audit.StalePods {
		if p.Kind == StalePodZombie {
			zombieCount++
		}
	}

	criticalCount := len(audit.AbandonedNamespaces) + zombieCount + len(audit.OrphanedPVCs)
	warningCount := len(audit.StaleJobs) + len(audit.ZeroReplicaWorkloads) +
		len(audit.OrphanedServices) + len(audit.BrokenIngresses) + len(audit.MisconfiguredHPAs)

	fmt.Println("EXECUTIVE SUMMARY")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("  Total Waste Items:  %d   (%d critical  /  %d warning)\n",
		audit.TotalWasteItems, criticalCount, warningCount)

	if audit.OrphanedPVCStorageGB > 0 {
		fmt.Printf("  Idle Storage:       %dGB across %d orphaned PVCs\n",
			audit.OrphanedPVCStorageGB, len(audit.OrphanedPVCs))
	}

	fmt.Println()
	fmt.Println("  🔴 Critical findings requiring action:")
	if zombieCount > 0 {
		// Find the worst zombie for context
		maxRestarts := int32(0)
		for _, p := range audit.StalePods {
			if p.Kind == StalePodZombie && p.RestartCount > maxRestarts {
				maxRestarts = p.RestartCount
			}
		}
		fmt.Printf("     • %d zombie pod(s) — up to %d restarts, consuming scheduling resources\n",
			zombieCount, maxRestarts)
	}
	if len(audit.OrphanedPVCs) > 0 {
		storageStr := ""
		if audit.OrphanedPVCStorageGB > 0 {
			storageStr = fmt.Sprintf(" (%dGB idle storage)", audit.OrphanedPVCStorageGB)
		}
		fmt.Printf("     • %d orphaned PVC(s)%s\n", len(audit.OrphanedPVCs), storageStr)
	}
	if len(audit.AbandonedNamespaces) > 0 {
		fmt.Printf("     • %d abandoned namespace(s)\n", len(audit.AbandonedNamespaces))
	}
	if criticalCount == 0 {
		fmt.Println("     • None")
	}

	fmt.Println()
	fmt.Println("  🟡 Warning findings to review:")
	if len(audit.StaleJobs) > 0 {
		fmt.Printf("     • %d stale job(s)/cronjob(s) not cleaned up\n", len(audit.StaleJobs))
	}
	if len(audit.OrphanedServices) > 0 {
		fmt.Printf("     • %d service(s) with no endpoints\n", len(audit.OrphanedServices))
	}
	if len(audit.ZeroReplicaWorkloads) > 0 {
		fmt.Printf("     • %d zero-replica workload(s)\n", len(audit.ZeroReplicaWorkloads))
	}
	if len(audit.BrokenIngresses) > 0 {
		fmt.Printf("     • %d broken ingress(es)\n", len(audit.BrokenIngresses))
	}
	if len(audit.MisconfiguredHPAs) > 0 {
		fmt.Printf("     • %d misconfigured HPA(s)\n", len(audit.MisconfiguredHPAs))
	}
	if warningCount == 0 {
		fmt.Println("     • None")
	}

	fmt.Println()
	fmt.Println("  ℹ️  Housekeeping (not counted in total):")
	if len(audit.OldReplicaSets) > 0 {
		fmt.Printf("     • %d old ReplicaSet(s) from past rollouts\n", len(audit.OldReplicaSets))
	} else {
		fmt.Println("     • None")
	}
	fmt.Println()
}

func printWasteScorecard(audit *WasteAudit) {
	fmt.Println("WASTE SCORECARD")
	fmt.Println("═══════════════════════════════════════════════════════════")

	printScoreRow("Abandoned Namespaces", len(audit.AbandonedNamespaces), "🔴")
	zombieCount := 0
	bareCount := 0
	for _, p := range audit.StalePods {
		if p.Kind == StalePodZombie {
			zombieCount++
		} else {
			bareCount++
		}
	}
	printScoreRow("Zombie Pods (CrashLoop/OOM)", zombieCount, "🔴")
	printScoreRow("Unmanaged Pods (no controller)", bareCount, "🔴")
	printScoreRow("Orphaned PVCs", len(audit.OrphanedPVCs), "🔴")
	printScoreRow("Orphaned Services", len(audit.OrphanedServices), "🟡")
	printScoreRow("Broken Ingresses", len(audit.BrokenIngresses), "🟡")
	printScoreRow("Stale Jobs/CronJobs", len(audit.StaleJobs), "🟡")
	printScoreRow("Zero-Replica Workloads", len(audit.ZeroReplicaWorkloads), "🟡")
	printScoreRow("Old ReplicaSets", len(audit.OldReplicaSets), "🟢")
	printScoreRow("Misconfigured HPAs", len(audit.MisconfiguredHPAs), "🟢")

	fmt.Println("───────────────────────────────────────────────────────────")
	fmt.Printf("  Total waste items found:  %d\n", audit.TotalWasteItems)
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
	if len(audit.AbandonedNamespaces) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🔴 ABANDONED NAMESPACES (%d)\n", len(audit.AbandonedNamespaces))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for _, ns := range audit.AbandonedNamespaces {
		fmt.Printf("\n  📁 %s\n", ns.Name)
		fmt.Printf("     Age:      %d days\n", ns.AgeDays)
		fmt.Printf("     Pods:     %s\n", podLabel(ns.PodCount))
		fmt.Printf("     Finding:  %s\n", ns.Reason)
		fmt.Printf("     Suggest:  Confirm with team if namespace is still needed.\n")
		fmt.Printf("               kubectl get all -n %s\n", ns.Name)
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
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Printf("🔴 ZOMBIE PODS - not functioning (%d)\n", len(zombies))
		fmt.Println("═══════════════════════════════════════════════════════════")
		for _, p := range zombies {
			fmt.Printf("\n  💀 %s (namespace: %s)\n", p.Name, p.Namespace)
			fmt.Printf("     Status:   %s\n", p.Status)
			fmt.Printf("     Age:      %d days\n", p.AgeDays)
			fmt.Printf("     Restarts: %d\n", p.RestartCount)
			fmt.Printf("     Finding:  %s\n", p.Reason)
			fmt.Printf("     Suggest:  Check logs: kubectl logs %s -n %s --previous\n", p.Name, p.Namespace)
		}
		fmt.Println()
	}

	if len(idle) > 0 {
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Printf("🔴 UNMANAGED PODS - no owning controller (%d)\n", len(idle))
		fmt.Println("═══════════════════════════════════════════════════════════")
		for _, p := range idle {
			fmt.Printf("\n  [BARE] %s (namespace: %s)\n", p.Name, p.Namespace)
			fmt.Printf("     Age:           %d days\n", p.AgeDays)
			fmt.Printf("     Restarts:      %d (total)\n", p.RestartCount)
			fmt.Printf("     Finding:       %s\n", p.Reason)
			fmt.Printf("     Suggest:       Verify with owner: kubectl describe pod %s -n %s\n", p.Name, p.Namespace)
		}
		fmt.Println()
	}
}

func printOrphanedPVCs(audit *WasteAudit) {
	if len(audit.OrphanedPVCs) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	if audit.OrphanedPVCStorageGB > 0 {
		fmt.Printf("🔴 ORPHANED PVCs - storage waste (%d)  |  Total idle: %dGB\n",
			len(audit.OrphanedPVCs), audit.OrphanedPVCStorageGB)
	} else {
		fmt.Printf("🔴 ORPHANED PVCs - storage waste (%d)\n", len(audit.OrphanedPVCs))
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	for _, pvc := range audit.OrphanedPVCs {
		sizeStr := "unknown size"
		if pvc.SizeGB > 0 {
			sizeStr = fmt.Sprintf("%dGB", pvc.SizeGB)
		}
		fmt.Printf("\n  💾 %s (namespace: %s)\n", pvc.Name, pvc.Namespace)
		fmt.Printf("     Status:  %s\n", pvc.Status)
		fmt.Printf("     Size:    %s\n", sizeStr)
		fmt.Printf("     Age:     %d days\n", pvc.AgeDays)
		fmt.Printf("     Finding: %s\n", pvc.Reason)
		fmt.Printf("     Suggest: Verify data is backed up before removing.\n")
		fmt.Printf("              kubectl get pvc %s -n %s -o yaml\n", pvc.Name, pvc.Namespace)
	}
	fmt.Println()
}

func printStaleJobs(audit *WasteAudit) {
	if len(audit.StaleJobs) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 STALE JOBS & CRONJOBS (%d)\n", len(audit.StaleJobs))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for _, job := range audit.StaleJobs {
		kind := "Job"
		if job.IsCronJob {
			kind = "CronJob"
		}
		fmt.Printf("\n  ⏰ %s [%s] (namespace: %s)\n", job.Name, kind, job.Namespace)
		fmt.Printf("     Status:  %s\n", job.JobStatus)
		fmt.Printf("     Age:     %d days\n", job.AgeDays)
		fmt.Printf("     Finding: %s\n", job.Reason)
		if job.JobStatus == "Completed" {
			fmt.Printf("     Suggest: Safe to delete: kubectl delete job %s -n %s\n", job.Name, job.Namespace)
		} else {
			fmt.Printf("     Suggest: kubectl describe job %s -n %s\n", job.Name, job.Namespace)
		}
	}
	fmt.Println()
}

func printZeroReplicaWorkloads(audit *WasteAudit) {
	if len(audit.ZeroReplicaWorkloads) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 ZERO-REPLICA WORKLOADS (%d)\n", len(audit.ZeroReplicaWorkloads))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for _, w := range audit.ZeroReplicaWorkloads {
		fmt.Printf("\n  📦 %s [%s] (namespace: %s)\n", w.Name, w.Kind, w.Namespace)
		fmt.Printf("     Age:     %d days\n", w.AgeDays)
		fmt.Printf("     Finding: %s\n", w.Reason)
		fmt.Printf("     Suggest: kubectl get %s %s -n %s -o yaml\n",
			strings.ToLower(w.Kind), w.Name, w.Namespace)
	}
	fmt.Println()
}

func printOldReplicaSets(audit *WasteAudit) {
	if len(audit.OldReplicaSets) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟢 OLD REPLICASETS - rollout leftovers (%d)\n", len(audit.OldReplicaSets))
	fmt.Println("═══════════════════════════════════════════════════════════")
	// Show top 10 only - these can be numerous
	shown := audit.OldReplicaSets
	remaining := 0
	if len(shown) > 10 {
		remaining = len(shown) - 10
		shown = shown[:10]
	}
	for _, rs := range shown {
		fmt.Printf("  📋 %s (owner: %s, age: %d days)\n", rs.Name, rs.OwnerDeployment, rs.AgeDays)
	}
	if remaining > 0 {
		fmt.Printf("  ... and %d more old ReplicaSets\n", remaining)
		fmt.Printf("  Suggest: kubectl get rs -A | awk '$3==0 && $4==0'\n")
	}
	fmt.Println()
}

func printOrphanedServices(audit *WasteAudit) {
	if len(audit.OrphanedServices) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 SERVICES WITH NO ENDPOINTS (%d)\n", len(audit.OrphanedServices))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for _, svc := range audit.OrphanedServices {
		lbNote := ""
		if svc.IsLB {
			lbNote = " ⚠️  LoadBalancer - incurring cloud cost!"
		}
		fmt.Printf("\n  🔌 %s [%s]%s (namespace: %s)\n", svc.Name, svc.Type, lbNote, svc.Namespace)
		fmt.Printf("     Age:     %d days\n", svc.AgeDays)
		fmt.Printf("     Finding: %s\n", svc.Reason)
		fmt.Printf("     Suggest: kubectl get endpoints %s -n %s\n", svc.Name, svc.Namespace)
	}
	fmt.Println()
}

func printBrokenIngresses(audit *WasteAudit) {
	if len(audit.BrokenIngresses) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 INGRESSES WITH MISSING BACKENDS (%d)\n", len(audit.BrokenIngresses))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for _, ing := range audit.BrokenIngresses {
		fmt.Printf("\n  🌐 %s (namespace: %s)\n", ing.Name, ing.Namespace)
		fmt.Printf("     Hosts:   %s\n", strings.Join(ing.Hosts, ", "))
		fmt.Printf("     Age:     %d days\n", ing.AgeDays)
		fmt.Printf("     Finding: %s\n", ing.Reason)
		fmt.Printf("     Suggest: kubectl describe ingress %s -n %s\n", ing.Name, ing.Namespace)
	}
	fmt.Println()
}

func printMisconfiguredHPAs(audit *WasteAudit) {
	if len(audit.MisconfiguredHPAs) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟢 MISCONFIGURED HPAs (%d)\n", len(audit.MisconfiguredHPAs))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for _, hpa := range audit.MisconfiguredHPAs {
		fmt.Printf("\n  📈 %s → %s (namespace: %s)\n", hpa.Name, hpa.TargetName, hpa.Namespace)
		fmt.Printf("     Replicas: min=%d max=%d  Condition: %s\n", hpa.MinReplicas, hpa.MaxReplicas, hpa.Condition)
		fmt.Printf("     Age:      %d days\n", hpa.AgeDays)
		fmt.Printf("     Finding:  %s\n", hpa.Reason)
		fmt.Printf("     Suggest:  kubectl describe hpa %s -n %s\n", hpa.Name, hpa.Namespace)
	}
	fmt.Println()
}

func printWasteSummary(audit *WasteAudit) {
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("📋 NEXT STEPS")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println()
	fmt.Println("  ⚠️  This report shows SUGGESTIONS based on observed data.")
	fmt.Println("  Always verify with the owning team before removing resources.")
	fmt.Println()

	if len(audit.StaleJobs) > 0 {
		// Count only completed jobs (safest to delete)
		completed := 0
		for _, j := range audit.StaleJobs {
			if j.JobStatus == "Completed" {
				completed++
			}
		}
		if completed > 0 {
			fmt.Printf("  ✅ SAFE TO DELETE: %d completed Jobs (no data loss risk)\n", completed)
		}
	}

	if len(audit.OldReplicaSets) > 0 {
		fmt.Printf("  ✅ SAFE TO DELETE: %d old ReplicaSets (Kubernetes manages these)\n", len(audit.OldReplicaSets))
	}

	if len(audit.AbandonedNamespaces) > 0 || len(audit.OrphanedPVCs) > 0 || len(audit.StalePods) > 0 {
		fmt.Println("  🔍 VERIFY FIRST:   Namespaces, PVCs, and Pods - confirm with owners")
	}

	fmt.Println()
	fmt.Println("  💡 Re-run with --min-age-days to adjust the age threshold.")
	fmt.Println("  💡 Use --namespace to focus on a specific namespace.")
	fmt.Println()
}

// ================================================================
// Helpers
// ================================================================

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
