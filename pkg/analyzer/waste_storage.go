package analyzer

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file is the Orphaned PVCs detector: the legacy live-client
// orchestration (detectOrphanedPVCs) plus the pure functions docs/08 Phase
// 4D.5 split it into — pvcsUsedByPods (builds the used-PVC-name set a PVC's
// orphan status is checked against) and evaluateOrphanedPVC (the pure
// per-PVC decision). See AnalyzeWaste (waste_analysis.go) for the
// snapshot-path caller of both.

// ================================================================
// 3. Orphaned PVCs
// ================================================================

func (w *WasteAuditor) detectOrphanedPVCs(audit *WasteAudit, filterNamespace string) error {
	pvcs, err := w.clientset.CoreV1().PersistentVolumeClaims(filterNamespace).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
	if err != nil {
		return err
	}
	if filterNamespace == "" {
		w.pvcSnapshot = pvcs.Items
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

	usedPVCs := pvcsUsedByPods(podItems)
	now := time.Now()

	for _, pvc := range pvcs.Items {
		if finding, ok := evaluateOrphanedPVC(pvc, usedPVCs, w.minAgeDays, now); ok {
			audit.OrphanedPVCs = append(audit.OrphanedPVCs, finding)
			audit.RequestedStorageBytes += finding.RequestedBytes
			audit.OrphanedPVCStorageGB = int(audit.RequestedStorageBytes / (1 << 30))
		}
	}

	sort.Slice(audit.OrphanedPVCs, func(i, j int) bool {
		return audit.OrphanedPVCs[i].Score > audit.OrphanedPVCs[j].Score
	})

	return nil
}

// pvcsUsedByPods indexes "namespace/claimName" for every PersistentVolumeClaim
// referenced by any of pods' volumes.
func pvcsUsedByPods(pods []corev1.Pod) map[string]bool {
	used := map[string]bool{}
	for _, pod := range pods {
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				used[pod.Namespace+"/"+vol.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	return used
}

// evaluateOrphanedPVC decides whether one PersistentVolumeClaim, given the
// set of PVCs actively referenced by observed Pods, is an orphaned-PVC
// finding. It performs no Kubernetes API calls; now is passed in explicitly
// for the same determinism reason as evaluateAbandonedNamespace.
func evaluateOrphanedPVC(pvc corev1.PersistentVolumeClaim, usedPVCs map[string]bool, minAgeDays int, now time.Time) (OrphanedPVC, bool) {
	if isInfraPattern(pvc.Namespace) {
		return OrphanedPVC{}, false
	}

	ageDays := int(now.Sub(pvc.CreationTimestamp.Time).Hours() / 24)
	if ageDays < minAgeDays {
		return OrphanedPVC{}, false
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
			return OrphanedPVC{}, false // actively used
		}

	default:
		// An unrecognized phase must never be silently treated as Pending,
		// Lost, or Bound. Report it honestly instead of leaving an empty
		// Status/Reason on the emitted finding.
		status = PVCUnrecognizedPhase
		reason = fmt.Sprintf(
			"PVC reports phase %q, which this detector does not classify as Pending, Bound, or Lost. "+
				"Resource age: %d days. Binding state and workload references were not evaluated for this phase.",
			pvc.Status.Phase, ageDays,
		)
		score = float64(ageDays) * 0.3
	}

	return OrphanedPVC{
		Name:           pvc.Name,
		Namespace:      pvc.Namespace,
		SizeGB:         int(requestedBytes / (1 << 30)),
		RequestedBytes: requestedBytes,
		RequestKnown:   requestKnown,
		Status:         status,
		AgeDays:        ageDays,
		Reason:         reason,
		Score:          score,
	}, true
}
