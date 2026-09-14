package analyzer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/kube"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// This file is the two Waste detectors whose shared responsibility is
// judging whether a Service or Ingress's own networking configuration is
// stale or broken: Orphaned Services (no matching pods) and Broken Ingresses
// (a backend Service with zero ready endpoints — see kube.ServiceHasReadyEndpoints
// and kube.EndpointSlicesForService, pkg/kube/endpoints.go, for the
// live-vs-snapshot seam this second detector needs). Each pairs its legacy
// live-client orchestration (detectX) with the pure per-item evaluator
// docs/08 Phase 4D.5 extracted from it (evaluateX), which AnalyzeWaste
// (waste_analysis.go) calls directly from an already-observed WasteSnapshot.

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
		if finding, ok := evaluateOrphanedService(svc, podItems, w.minAgeDays, now); ok {
			audit.OrphanedServices = append(audit.OrphanedServices, finding)
		}
	}

	sort.Slice(audit.OrphanedServices, func(i, j int) bool {
		return audit.OrphanedServices[i].Score > audit.OrphanedServices[j].Score
	})

	return nil
}

// evaluateOrphanedService decides whether one Service, given the Pods
// currently observed cluster/namespace-wide, is an orphaned-service
// finding. It performs no Kubernetes API calls; now is passed in explicitly
// for the same determinism reason as evaluateAbandonedNamespace.
func evaluateOrphanedService(svc corev1.Service, podItems []corev1.Pod, minAgeDays int, now time.Time) (OrphanedService, bool) {
	if isInfraPattern(svc.Namespace) {
		return OrphanedService{}, false
	}

	// Skip kubernetes default service
	if svc.Name == "kubernetes" && svc.Namespace == "default" {
		return OrphanedService{}, false
	}

	// ExternalName services DNS-alias to external hosts — they have no endpoints by design.
	// Flagging them as "no endpoints" is always a false positive.
	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		return OrphanedService{}, false
	}

	// Services with no selector are intentional: manually-managed endpoints,
	// headless StatefulSet discovery, or external endpoint objects.
	// Only services WITH a selector that matches nothing are genuinely orphaned.
	if len(svc.Spec.Selector) == 0 {
		return OrphanedService{}, false
	}

	ageDays := int(now.Sub(svc.CreationTimestamp.Time).Hours() / 24)
	if ageDays < minAgeDays {
		return OrphanedService{}, false
	}

	selector := labels.SelectorFromSet(svc.Spec.Selector)
	matchedPods := 0
	for _, pod := range podItems {
		if pod.Namespace == svc.Namespace && selector.Matches(labels.Set(pod.Labels)) {
			matchedPods++
		}
	}

	if matchedPods != 0 {
		return OrphanedService{}, false
	}

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

	return OrphanedService{
		Name:      svc.Name,
		Namespace: svc.Namespace,
		Type:      string(svc.Spec.Type),
		AgeDays:   ageDays,
		IsLB:      isLB,
		Selector:  svc.Spec.Selector,
		Reason:    reason,
		Score:     score,
	}, true
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

	liveReadyCheck := func(namespace, svcName string) (bool, error) {
		return kube.ServiceHasReadyEndpoints(w.ctx, w.clientset, namespace, svcName)
	}

	for _, ing := range ingresses.Items {
		finding, ok, checkErrs := evaluateBrokenIngress(ing, now, liveReadyCheck)
		for _, checkErr := range checkErrs {
			// An API failure means endpoint status is UNKNOWN, not zero.
			// Surface it as a detector warning rather than fabricating an
			// outage finding.
			audit.addDetectorWarning("Broken ingresses", checkErr)
		}
		if ok {
			audit.BrokenIngresses = append(audit.BrokenIngresses, finding)
		}
	}

	sort.Slice(audit.BrokenIngresses, func(i, j int) bool {
		return audit.BrokenIngresses[i].Score > audit.BrokenIngresses[j].Score
	})

	return nil
}

// evaluateBrokenIngress decides whether one Ingress is a broken-ingress
// finding: it currently has at least one backend with no ready endpoints.
// hasReadyEndpoints abstracts "does this namespace/service currently have a
// ready endpoint" — the one seam between the legacy live-API path (a
// closure over kube.ServiceHasReadyEndpoints) and the snapshot path (a
// closure over an in-memory EndpointSlice index) — so the surrounding
// rule/host/scoring logic is not duplicated between them. A hasReadyEndpoints
// error is reported back via the returned error slice rather than treated
// as "not ready", matching the legacy detector's own contract. now is
// passed in explicitly for the same determinism reason as
// evaluateAbandonedNamespace; ageDays is carried on the finding for display
// only — see the "no age gate" note below for why it never gates eligibility.
func evaluateBrokenIngress(
	ing networkingv1.Ingress,
	now time.Time,
	hasReadyEndpoints func(namespace, svcName string) (bool, error),
) (finding BrokenIngress, ok bool, checkErrs []error) {
	if isInfraPattern(ing.Namespace) {
		return BrokenIngress{}, false, nil
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
			hasReady, err := hasReadyEndpoints(ing.Namespace, svcName)
			if err != nil {
				checkErrs = append(checkErrs, fmt.Errorf("checking endpoints for %s/%s (backend of ingress %s): %w", ing.Namespace, svcName, ing.Name, err))
				continue
			}
			if !hasReady {
				missingBackends = append(missingBackends, fmt.Sprintf("%s → %s (no ready endpoints)", rule.Host, svcName))
			}
		}
	}

	if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
		svcName := ing.Spec.DefaultBackend.Service.Name
		hasReady, err := hasReadyEndpoints(ing.Namespace, svcName)
		if err != nil {
			checkErrs = append(checkErrs, fmt.Errorf("checking endpoints for %s/%s (default backend of ingress %s): %w", ing.Namespace, svcName, ing.Name, err))
		} else if !hasReady {
			missingBackends = append(missingBackends, fmt.Sprintf("default-backend → %s (no ready endpoints)", svcName))
		}
	}

	if len(missingBackends) == 0 {
		return BrokenIngress{}, false, checkErrs
	}

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

	return BrokenIngress{
		Name:      ing.Name,
		Namespace: ing.Namespace,
		AgeDays:   ageDays,
		Hosts:     hosts,
		Reason:    reason,
		Score:     float64(len(missingBackends)) * 10,
		IsActive:  true,
	}, true, checkErrs
}
