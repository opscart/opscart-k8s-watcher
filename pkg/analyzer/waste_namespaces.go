package analyzer

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file is the Abandoned Namespaces detector: the legacy live-client
// orchestration (detectAbandonedNamespaces) plus the two pure functions
// docs/08 Phase 4D.5 split it into — namespaceAbandonmentEligible (a
// pre-fetch eligibility gate: age/infra-pattern checks that must run BEFORE
// any pods are fetched, so a namespace that fails them never causes a live
// Pods LIST) and evaluateAbandonedNamespace (the post-fetch pure decision).
// See AnalyzeWaste (waste_analysis.go) for the snapshot-path caller of both.

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
		ageDays, eligible := namespaceAbandonmentEligible(ns, w.minAgeDays, now)
		if !eligible {
			continue // matches the original pre-fetch skip: never issue a Pods LIST for an ineligible namespace
		}

		// Count running pods. A non-nil map, including an empty map, is a
		// cluster-wide snapshot; otherwise retain the legacy namespace LIST.
		var namespacePods []corev1.Pod
		if w.podsByNamespace == nil {
			pods, err := w.clientset.CoreV1().Pods(ns.Name).List(w.ctx, metav1.ListOptions{TimeoutSeconds: int64Ptr(10)})
			if err != nil {
				return fmt.Errorf("list pods in namespace %q: %w", ns.Name, err)
			}
			namespacePods = pods.Items
		} else {
			namespacePods = w.podsByNamespace[ns.Name]
		}

		if finding, ok := evaluateAbandonedNamespace(ns, ageDays, namespacePods); ok {
			audit.AbandonedNamespaces = append(audit.AbandonedNamespaces, finding)
		}
	}

	sort.Slice(audit.AbandonedNamespaces, func(i, j int) bool {
		return audit.AbandonedNamespaces[i].Score > audit.AbandonedNamespaces[j].Score
	})

	return nil
}

// namespaceAbandonmentEligible is the pre-Pod-evidence gate for one
// Namespace: default/infrastructure namespaces and namespaces younger than
// minAgeDays never need their Pods examined at all. Splitting this out from
// evaluateAbandonedNamespace lets a caller (the legacy detector) skip
// fetching Pod evidence entirely for an ineligible namespace, exactly
// preserving the original "skip before fetch" call order — not just the
// original result.
func namespaceAbandonmentEligible(ns corev1.Namespace, minAgeDays int, now time.Time) (ageDays int, ok bool) {
	// default always exists even when empty — not a candidate for abandonment.
	if ns.Name == "default" {
		return 0, false
	}
	// Skip infrastructure namespaces (reuse same logic as network command)
	if isInfraPattern(ns.Name) {
		return 0, false
	}
	ageDays = int(now.Sub(ns.CreationTimestamp.Time).Hours() / 24)
	if ageDays < minAgeDays {
		return ageDays, false
	}
	return ageDays, true
}

// evaluateAbandonedNamespace decides whether one already-eligible Namespace
// (see namespaceAbandonmentEligible), given the Pods currently observed in
// it, is an abandoned-namespace finding. It performs no Kubernetes API
// calls and is deterministic for identical inputs.
func evaluateAbandonedNamespace(ns corev1.Namespace, ageDays int, namespacePods []corev1.Pod) (AbandonedNamespace, bool) {
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
		return AbandonedNamespace{}, false // has running pods, not abandoned
	}

	if score < 10 {
		return AbandonedNamespace{}, false
	}

	return AbandonedNamespace{
		Name:     ns.Name,
		AgeDays:  ageDays,
		PodCount: podCount,
		Reason:   reason,
		Score:    score,
	}, true
}
