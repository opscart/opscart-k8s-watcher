package analyzer

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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

	// pvcSnapshot is the PersistentVolumeClaim list most recently retrieved
	// by detectOrphanedPVCs under a cluster-wide (empty-namespace) AuditWaste
	// call. See PVCSnapshot.
	pvcSnapshot []corev1.PersistentVolumeClaim
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
	PVCNeverBound        PVCStatus = "Never bound"               // Legacy label for current Pending phase; not binding history.
	PVCReleased          PVCStatus = "Released (pod deleted)"    // Legacy label for current Lost phase; not Pod deletion evidence.
	PVCBoundNoPod        PVCStatus = "Bound but no pod using it" // Bound but no pod references it
	PVCUnrecognizedPhase PVCStatus = "Unrecognized phase"        // Phase other than Pending/Bound/Lost; not characterized as any of those.
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
	JobStatus          string // Plain-Job values ("Completed"/"Failed") require an authoritative Kubernetes terminal Condition; CronJob values do not.
	Schedule           string // Retained CronJob metadata; empty when unavailable.
	Suspended          *bool  // nil means unspecified; Kubernetes defaults this to false.
	AttemptCountsKnown bool
	SucceededPods      int32
	FailedPods         int32
	// AgeDays means completion age when JobStatus=="Completed" and
	// CompletionTimeKnown is true; otherwise it is resource/object creation
	// age (never failure age — Kubernetes records no such timestamp).
	AgeDays int
	// CompletionTimeKnown is only meaningful when JobStatus=="Completed". It
	// is true when Kubernetes reported Status.CompletionTime, and false when
	// Complete=True was observed without one (an honest fallback to creation
	// age, not a fabricated completion time).
	CompletionTimeKnown bool
	Reason              string
	Score               float64
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

// PVCSnapshot returns a copy of the cluster-wide PersistentVolumeClaim
// snapshot most recently retrieved by AuditWaste (via detectOrphanedPVCs),
// so other scan-pipeline consumers (e.g. Node Optimization storage evidence)
// can reuse it instead of listing PersistentVolumeClaims again. It is nil
// until AuditWaste has run with a cluster-wide (empty) namespace filter.
func (w *WasteAuditor) PVCSnapshot() []corev1.PersistentVolumeClaim {
	return append([]corev1.PersistentVolumeClaim(nil), w.pvcSnapshot...)
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
