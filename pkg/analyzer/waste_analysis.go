package analyzer

import (
	"sort"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/kube"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// This file is the pure Kubernetes-free analysis boundary docs/08 Phase
// 4D.5 introduced: WasteSnapshot (the Waste-domain input) and AnalyzeWaste
// (the pure function over it). See waste.go's AuditWaste for the live-client
// counterpart these detectors both delegate to the same evaluateX functions
// from (waste_namespaces.go, waste_storage.go, waste_workloads.go,
// waste_network.go, waste_probe_evidence.go).

// WasteSnapshot is the Waste-domain view over one ClusterSnapshot
// generation's already-observed resources — a deliberately Waste-specific
// input struct, not a clone of clusterstate.ClusterResources: it exists so
// AnalyzeWaste's signature stays readable across the many resource kinds
// Waste actually reads (docs/08 Phase 4D.5's dependency table), not to
// duplicate the coordinator's own snapshot type or become a second general
// cluster-resources shape. Every field is a value slice, matching every
// other pure function this package added during the Phase 4C/4D migration
// (AnalyzeResources, AnalyzeNodeHealth, AnalyzeNetworkPolicies, AnalyzeSecurity).
//
// PersistentVolumes is deliberately absent: AuditWaste never reads it (only
// PersistentVolumeClaims). NetworkPolicies is deliberately absent for the
// same reason.
type WasteSnapshot struct {
	Namespaces               []corev1.Namespace
	Pods                     []corev1.Pod
	PersistentVolumeClaims   []corev1.PersistentVolumeClaim
	Jobs                     []batchv1.Job
	CronJobs                 []batchv1.CronJob
	Deployments              []appsv1.Deployment
	StatefulSets             []appsv1.StatefulSet
	ReplicaSets              []appsv1.ReplicaSet
	Services                 []corev1.Service
	Ingresses                []networkingv1.Ingress
	HorizontalPodAutoscalers []autoscalingv2.HorizontalPodAutoscaler
	EndpointSlices           []discoveryv1.EndpointSlice
	// PodWarningEvents is the exact filtered evidence the shared informer
	// set already provides (involvedObject.kind=Pod,type=Warning — see
	// pkg/acquisition/event_filter.go), the same selector
	// detectStalePodsWithClusterEvents' own cluster-wide Events LIST uses.
	PodWarningEvents []corev1.Event
}

// AnalyzeWaste is AuditWaste's Kubernetes-free counterpart (docs/08 Phase
// 4D.5): the same 9 detectors, from an already-observed WasteSnapshot
// instead of the auditor's own LIST calls. It performs no Kubernetes API
// calls, no persistence, and no presentation work, and is deterministic:
// the same input and now always produce the same *WasteAudit.
//
// now is an explicit parameter, not an internal time.Now() call, because
// most Waste rules are clock-driven (age-gated against object timestamps —
// see docs/08 Phase 4D.5's time-semantics audit): this keeps every age
// computation traceable to one caller-supplied clock reading instead of a
// hidden one, and makes the function fully deterministic for tests.
//
// Every input here is assumed to be a single trustworthy, already-successful
// snapshot (the coordinator refuses to call this at all against an
// untrustworthy ClusterSnapshot — see waste_runtime.go), so there is no
// analogous failure mode to AuditWaste's per-detector API errors: the
// returned audit's DetectorWarnings is always empty.
func AnalyzeWaste(input WasteSnapshot, minAgeDays int, now time.Time) *WasteAudit {
	audit := &WasteAudit{ScannedAt: now}

	podsByNamespace := make(map[string][]corev1.Pod, len(input.Namespaces))
	for _, pod := range input.Pods {
		podsByNamespace[pod.Namespace] = append(podsByNamespace[pod.Namespace], pod)
	}

	for _, ns := range input.Namespaces {
		ageDays, eligible := namespaceAbandonmentEligible(ns, minAgeDays, now)
		if !eligible {
			continue
		}
		if finding, ok := evaluateAbandonedNamespace(ns, ageDays, podsByNamespace[ns.Name]); ok {
			audit.AbandonedNamespaces = append(audit.AbandonedNamespaces, finding)
		}
	}
	sort.Slice(audit.AbandonedNamespaces, func(i, j int) bool {
		return audit.AbandonedNamespaces[i].Score > audit.AbandonedNamespaces[j].Score
	})

	audit.StalePods = analyzeStalePods(input.Pods, input.PodWarningEvents, minAgeDays, now)

	usedPVCs := pvcsUsedByPods(input.Pods)
	for _, pvc := range input.PersistentVolumeClaims {
		if finding, ok := evaluateOrphanedPVC(pvc, usedPVCs, minAgeDays, now); ok {
			audit.OrphanedPVCs = append(audit.OrphanedPVCs, finding)
			audit.RequestedStorageBytes += finding.RequestedBytes
			audit.OrphanedPVCStorageGB = int(audit.RequestedStorageBytes / (1 << 30))
		}
	}
	sort.Slice(audit.OrphanedPVCs, func(i, j int) bool {
		return audit.OrphanedPVCs[i].Score > audit.OrphanedPVCs[j].Score
	})

	for _, job := range input.Jobs {
		if finding, ok := evaluateStaleJob(job, minAgeDays, now); ok {
			audit.StaleJobs = append(audit.StaleJobs, finding)
		}
	}
	for _, cj := range input.CronJobs {
		audit.StaleJobs = append(audit.StaleJobs, evaluateStaleCronJob(cj, minAgeDays, now)...)
	}
	sort.Slice(audit.StaleJobs, func(i, j int) bool {
		return audit.StaleJobs[i].Score > audit.StaleJobs[j].Score
	})

	for _, d := range input.Deployments {
		if finding, ok := evaluateZeroReplicaDeployment(d, minAgeDays, now); ok {
			audit.ZeroReplicaWorkloads = append(audit.ZeroReplicaWorkloads, finding)
		}
	}
	for _, s := range input.StatefulSets {
		if finding, ok := evaluateZeroReplicaStatefulSet(s, minAgeDays, now); ok {
			audit.ZeroReplicaWorkloads = append(audit.ZeroReplicaWorkloads, finding)
		}
	}
	sort.Slice(audit.ZeroReplicaWorkloads, func(i, j int) bool {
		return audit.ZeroReplicaWorkloads[i].Score > audit.ZeroReplicaWorkloads[j].Score
	})

	for _, rs := range input.ReplicaSets {
		if finding, ok := evaluateOldReplicaSet(rs, minAgeDays, now); ok {
			audit.OldReplicaSets = append(audit.OldReplicaSets, finding)
		}
	}
	sort.Slice(audit.OldReplicaSets, func(i, j int) bool {
		return audit.OldReplicaSets[i].Score > audit.OldReplicaSets[j].Score
	})

	for _, svc := range input.Services {
		if finding, ok := evaluateOrphanedService(svc, input.Pods, minAgeDays, now); ok {
			audit.OrphanedServices = append(audit.OrphanedServices, finding)
		}
	}
	sort.Slice(audit.OrphanedServices, func(i, j int) bool {
		return audit.OrphanedServices[i].Score > audit.OrphanedServices[j].Score
	})

	// The one live/snapshot seam evaluateBrokenIngress abstracts: readiness
	// comes from an in-memory EndpointSlice index instead of
	// kube.ServiceHasReadyEndpoints' live LIST. Never errors, since the
	// input is already a successful snapshot.
	snapshotReadyCheck := func(namespace, svcName string) (bool, error) {
		matching := kube.EndpointSlicesForService(input.EndpointSlices, namespace, svcName)
		return kube.ReadyAddressCount(matching) > 0, nil
	}
	for _, ing := range input.Ingresses {
		if finding, ok, _ := evaluateBrokenIngress(ing, now, snapshotReadyCheck); ok {
			audit.BrokenIngresses = append(audit.BrokenIngresses, finding)
		}
	}
	sort.Slice(audit.BrokenIngresses, func(i, j int) bool {
		return audit.BrokenIngresses[i].Score > audit.BrokenIngresses[j].Score
	})

	for _, hpa := range input.HorizontalPodAutoscalers {
		if finding, ok := evaluateMisconfiguredHPA(hpa, minAgeDays, now); ok {
			audit.MisconfiguredHPAs = append(audit.MisconfiguredHPAs, finding)
		}
	}
	// AuditWaste's own detectMisconfiguredHPAs does not sort MisconfiguredHPAs
	// either — preserved exactly, not an omission.

	// Count totals (excluding OldReplicaSets - they're low-severity housekeeping items)
	audit.TotalWasteItems = len(audit.AbandonedNamespaces) +
		len(audit.StalePods) +
		len(audit.OrphanedPVCs) +
		len(audit.StaleJobs) +
		len(audit.ZeroReplicaWorkloads) +
		len(audit.OrphanedServices) +
		len(audit.BrokenIngresses) +
		len(audit.MisconfiguredHPAs)

	return audit
}
