package analyzer

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// WasteCounts partitions findings, not necessarily distinct resources. Operational
// includes detector-reported Pod failures and current Ingress/HPA conditions. It
// is not a count of persisted incidents or confirmed application outages.
type WasteCounts struct {
	Findings          int `json:"finding_count"`
	DistinctResources int `json:"distinct_resource_count"`
	Operational       int `json:"operational_findings"`
	Retention         int `json:"housekeeping_retention_findings"`
	Review            int `json:"other_review_findings"`
}

const (
	WasteReview      = "review"
	WasteOperational = "operational"
	WasteRetention   = "retention"
)

// WasteFinding separates snapshot evidence from interpretation. Priority is the
// original detector score, not severity, confidence, financial impact, or safety.
// Confidence refers only to Observed, and is not a probability of waste.
type WasteFinding struct {
	ID, Kind, Namespace, Name, Subtype, Category, Group string
	Observed, Inference, Limitations, Review            string
	Priority                                            float64
	Confidence, ConfidenceReason                        string
	AgeDays                                             int
	Command                                             string
	Storage                                             string
	// Section preserves existing presentation placement independently of counts.
	Section string
}

func (f WasteFinding) ResourceKey() string { return f.Kind + "\x00" + f.Namespace + "\x00" + f.Name }
func (f WasteFinding) EvidenceText() string {
	return "Observed: " + f.Observed + " Inference: " + f.Inference + " Limitations: " + f.Limitations
}

type WastePresentation struct {
	Available              bool
	ScannedAt              time.Time
	Counts                 WasteCounts
	Findings               []WasteFinding
	Warnings               []WasteDetectorWarning
	Coverage               string
	RequestedStorageBytes  int64
	UnknownStorageRequests int
}

func (p WastePresentation) ScanTimeLabel() string {
	if p.ScannedAt.IsZero() {
		return "Unknown"
	}
	return p.ScannedAt.Format("2006-01-02 15:04:05 MST")
}

// FormatWasteBytes uses binary units explicitly; it never relabels MiB as GB.
func FormatWasteBytes(n int64) string {
	for _, u := range []struct {
		size  int64
		label string
	}{{1 << 40, "TiB"}, {1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if n >= u.size {
			return strconv.FormatFloat(float64(n)/float64(u.size), 'f', -1, 64) + " " + u.label
		}
	}
	return fmt.Sprintf("%d B", n)
}

// legacyPVCScoreSize intentionally retains the historical GiB/MiB discontinuity
// ONLY for ranking. Do not use this value for quantity storage or presentation.
// A separate scoring review must explicitly approve changing resulting scores.
func legacyPVCScoreSize(bytes int64) int {
	size := int(bytes / (1 << 30))
	if size == 0 {
		size = int(bytes / (1 << 20))
	}
	return size
}

// BuildWastePresentation is pure: no API access, clock reads, mutation, sorting,
// or detector re-evaluation. It preserves slice order and every emitted finding.
// Legacy SizeGB fields are deliberately not used: their units are ambiguous.
func BuildWastePresentation(a *WasteAudit) WastePresentation {
	p := WastePresentation{Coverage: "Audit unavailable"}
	if a == nil {
		return p
	}
	p.Available, p.ScannedAt = true, a.ScannedAt
	p.Warnings = append([]WasteDetectorWarning(nil), a.DetectorWarnings...)
	p.Coverage = "No warnings reported"
	if len(p.Warnings) > 0 {
		p.Coverage = "Warnings reported"
	}
	resources := map[string]bool{}
	add := func(f WasteFinding) {
		f.ID = f.ResourceKey() + "\x00" + f.Subtype
		kind := strings.ToLower(f.Kind)
		switch f.Kind {
		case "PersistentVolumeClaim":
			kind = "pvc"
		case "HorizontalPodAutoscaler":
			kind = "hpa"
		}
		f.Command = "kubectl get " + kind + " " + f.Name
		if f.Namespace != "" {
			f.Command += " -n " + f.Namespace
		}
		f.Command += " -o yaml"
		if f.Confidence == "" {
			f.Confidence = "Not assessed"
			f.ConfidenceReason = "No calibrated confidence assessment was performed; review the retained observation and its limitations."
		}
		if f.Storage == "" {
			f.Storage = "—"
		}
		p.Findings = append(p.Findings, f)
		resources[f.ResourceKey()] = true
		p.Counts.Findings++
		switch f.Group {
		case WasteOperational:
			p.Counts.Operational++
		case WasteRetention:
			p.Counts.Retention++
		default:
			p.Counts.Review++
		}
	}
	for _, x := range a.AbandonedNamespaces {
		add(WasteFinding{Kind: "Namespace", Name: x.Name, Subtype: "no-running-pods", Category: "Namespace activity review", Group: WasteReview, Section: "resource", AgeDays: x.AgeDays, Priority: x.Score,
			Observed: fmt.Sprintf("%d Pods were listed; none were in Running phase.", x.PodCount), Inference: "Namespace intent may warrant review.", Limitations: "Other resources and workload intent were not checked. No Running Pods does not establish abandonment.", Review: "Review namespace resources and confirm intent with the owner."})
	}
	// Keep the existing dashboard category insertion order, while preserving each
	// detector's order. Terminal/HTML consumers can group by Subtype without sorting.
	for _, x := range a.StalePods {
		f := WasteFinding{Kind: "Pod", Name: x.Name, Namespace: x.Namespace, Subtype: string(x.Kind), Category: "Pod ownership review", Group: WasteReview, Section: "resource", AgeDays: x.AgeDays, Priority: x.Score,
			Observed: fmt.Sprintf("Pod is Running; no owner reference matches ReplicaSet, DaemonSet, StatefulSet, Job, or CronJob. %d restarts observed.", x.RestartCount), Inference: "Ownership may warrant review.", Limitations: "Other controllers and static Pods are not excluded. Age and restart counts do not establish inactivity or resource usage.", Review: "Inspect owner references and confirm the Pod's purpose with its owner."}
		if x.Kind == StalePodZombie {
			f.Category, f.Group, f.Section = "Pod failure evidence", WasteOperational, "incident"
			f.Observed = fmt.Sprintf("Detector classification: %s. %d restarts observed.", x.Status, x.RestartCount)
			if x.ObservedEvidence != "" {
				f.Observed = x.ObservedEvidence
			}
			f.Inference = "The observed state or restart evidence may warrant operational investigation."
			f.Limitations = "Last termination and Events may be historical. Event matching uses Pod name without UID or recency validation; probe text does not establish probe type or restart cause. This is not a persisted incident count."
			f.Review = "Inspect current Pod status and relevant logs before determining cause or impact."
			f.Confidence, f.ConfidenceReason = "Not assessed", "The legacy classification combines current states, historical evidence, and inferred causes."
		}
		add(f)
	}
	for _, x := range a.OrphanedPVCs {
		observed := "PVC detector emitted a finding without a recognized phase."
		confidence, confidenceReason := "", ""
		switch x.Status {
		case PVCNeverBound:
			observed = "PVC currently reports Pending."
		case PVCReleased:
			observed = "PVC currently reports Lost."
		case PVCBoundNoPod:
			observed = "PVC reports Bound; no currently listed Pod in its namespace references it."
		default:
			confidence, confidenceReason = "Not assessed", "No recognized phase evidence was retained."
		}
		storage := "Unknown"
		if x.RequestKnown {
			storage = FormatWasteBytes(x.RequestedBytes)
			p.RequestedStorageBytes += x.RequestedBytes
			observed += " Requested storage: " + storage + "."
		} else {
			p.UnknownStorageRequests++
		}
		add(WasteFinding{Kind: "PersistentVolumeClaim", Name: x.Name, Namespace: x.Namespace, Subtype: string(x.Status), Category: "PVC state / reference review", Group: WasteReview, Section: "resource", AgeDays: x.AgeDays, Priority: x.Score, Storage: storage, Confidence: confidence, ConfidenceReason: confidenceReason,
			Observed: observed, Inference: "Binding state or workload references may warrant review.", Limitations: "Resource age is not time in this state. PV existence, attachments, retained data, controller intent, and billing were not verified. Pending/Lost claims can still have Pod references.", Review: "Review PVC state, workload ownership, and data-retention requirements."})
	}
	for _, x := range a.OrphanedServices {
		selector := ""
		if len(x.Selector) > 0 {
			selector = fmt.Sprintf(" Selector: %v.", x.Selector)
		}
		add(WasteFinding{Kind: "Service", Name: x.Name, Namespace: x.Namespace, Subtype: "selector", Category: "Service selector review", Group: WasteReview, Section: "resource", AgeDays: x.AgeDays, Priority: x.Score,
			Observed: fmt.Sprintf("Service type %s has a nonempty selector matching zero currently listed Pods in its namespace.", x.Type) + selector, Inference: "Selector or workload intent may warrant review.", Limitations: "Endpoint state and cloud billing were not checked. Scale-to-zero and rollout gaps may explain this observation.", Review: "Inspect the selector and confirm workload intent; consult billing separately if relevant."})
	}
	for _, x := range a.StaleJobs {
		kind := "Job"
		var observed, limitations string
		if x.IsCronJob {
			kind = "CronJob"
			observed = "The detector reported Job history for review."
			switch x.JobStatus {
			case "NeverScheduled":
				observed = "CronJob status has no lastScheduleTime."
			case "NoHistoryLimit":
				observed = "Both CronJob history-limit fields were absent in the observed object."
			}
			if x.Schedule != "" {
				observed += fmt.Sprintf(" Schedule: %q.", x.Schedule)
				if x.Suspended == nil {
					observed += " Suspend is unspecified (defaults to false)."
				} else {
					observed += fmt.Sprintf(" Suspended: %t.", *x.Suspended)
				}
			}
			limitations = "Creation age is not completion age. Attempt counters do not establish terminal status. Missing schedule status is not full execution history. Omitted CronJob history limits default to 3 successful Jobs and 1 failed Job; they do not imply unlimited retention."
		} else {
			// Plain-Job subtypes now require an authoritative Kubernetes
			// terminal Condition (Complete=True/Failed=True), not attempt
			// counters, so "attempt counters do not establish terminal
			// status" no longer applies here — the check DOES establish it.
			switch {
			case x.JobStatus == "Completed" && x.CompletionTimeKnown:
				observed = fmt.Sprintf("Job reached the Kubernetes Complete condition. Completed %d days ago.", x.AgeDays)
				limitations = "Retention policy, downstream artifact cleanup, and rollback requirements were not checked."
			case x.JobStatus == "Completed":
				observed = fmt.Sprintf("Job reached the Kubernetes Complete condition, but no completionTime was reported. Resource created %d days ago; completion age could not be established, so creation age is shown instead.", x.AgeDays)
				limitations = "Age reflects when this Job was created, not when it completed. Retention policy and downstream artifact cleanup were not checked."
			case x.JobStatus == "Failed":
				observed = fmt.Sprintf("Job currently reports the Kubernetes Failed terminal condition. Resource created %d days ago; this check does not establish when terminal failure occurred.", x.AgeDays)
				limitations = "Age reflects when this Job was created, not when it failed. Retry/backoff history and failure root cause were not checked."
			default:
				observed = "The detector reported Job history for review."
			}
			if x.AttemptCountsKnown {
				observed += fmt.Sprintf(" %d successful and %d failed Pod attempts were also reported.", x.SucceededPods, x.FailedPods)
			}
		}
		add(WasteFinding{Kind: kind, Name: x.Name, Namespace: x.Namespace, Subtype: x.JobStatus, Category: "Job / CronJob retention review", Group: WasteRetention, Section: "drift", AgeDays: x.AgeDays, Priority: x.Score,
			Observed: observed, Inference: "Status, scheduling intent, or retention may warrant review.", Limitations: limitations, Review: "Inspect current status, schedule/suspension where applicable, and owner retention requirements."})
	}
	for _, x := range a.ZeroReplicaWorkloads {
		kind := x.Kind
		if kind == "" {
			kind = "Deployment"
		}
		add(WasteFinding{Kind: kind, Name: x.Name, Namespace: x.Namespace, Subtype: "zero-desired", Category: "Zero-replica workload", Group: WasteReview, Section: "drift", AgeDays: x.AgeDays, Priority: x.Score,
			Observed: "Desired replicas are currently set to zero. Resource age reflects when this workload was created, not how long it has been at zero replicas.", Inference: "Workload intent may warrant review.", Limitations: "Scale-transition history, actual Pod count, associated PVCs/ConfigMaps, quota, and costs were not established by this check.", Review: "Confirm whether scale-to-zero is intentional and review associated resources with the owner."})
	}
	for _, x := range a.BrokenIngresses {
		add(WasteFinding{Kind: "Ingress", Name: x.Name, Namespace: x.Namespace, Subtype: "endpoints", Category: "Ingress backend evidence", Group: WasteOperational, Section: "drift", AgeDays: x.AgeDays, Priority: x.Score,
			Observed: x.Reason, Inference: "Backend readiness may warrant investigation.", Limitations: "No ready EndpointSlice addresses does not establish actual request failures. Service type, backend ports, controller behavior, and traffic were not verified.", Review: "Inspect backend readiness and routing configuration before determining impact."})
	}
	for _, x := range a.MisconfiguredHPAs {
		f := WasteFinding{Kind: "HorizontalPodAutoscaler", Name: x.Name, Namespace: x.Namespace, Subtype: x.Condition, Category: "HPA configuration review", Group: WasteReview, Section: "drift", AgeDays: x.AgeDays, Priority: x.Score,
			Observed: fmt.Sprintf("Current and desired replicas equal minReplicas (%d) at this scan. Resource age reflects when this HPA was created, not how long it has remained at minReplicas.", x.MinReplicas), Inference: "Scaling configuration may warrant review.", Limitations: "One snapshot does not establish scaling history or demand. v1 fallback does not retain v2 condition coverage.", Review: "Review scaling history and demand before changing configuration."}
		if x.IsActive {
			f.Group = WasteOperational
			f.Observed = strings.TrimSuffix(x.Reason, " Review the reported condition and target configuration; application impact was not measured.")
			f.Inference = "The reported condition may warrant investigation."
			f.Review = "Inspect the reported condition and target configuration."
		}
		add(f)
	}
	for _, x := range a.OldReplicaSets {
		observed := "Desired replica count was not retained or was unspecified."
		if x.DesiredReplicas != nil {
			observed = fmt.Sprintf("Desired replicas: %d.", *x.DesiredReplicas)
		}
		if x.OwnerDeployment != "" {
			observed += " Deployment owner reference: " + x.OwnerDeployment + "."
		}
		add(WasteFinding{Kind: "ReplicaSet", Name: x.Name, Namespace: x.Namespace, Subtype: "history", Category: "ReplicaSet retention review", Group: WasteRetention, Section: "housekeeping", AgeDays: x.AgeDays, Priority: x.Score,
			Observed: observed, Inference: "Retention may warrant review.", Limitations: "Actual replicas, Deployment revision, rollback requirements, and retention policy were not checked. An old object is not necessarily obsolete.", Review: "Inspect desired state and confirm rollback/retention requirements with the owner."})
	}
	p.Counts.DistinctResources = len(resources)
	return p
}

// FindingsInCategory preserves detector order within an existing presentation category.
func (p WastePresentation) FindingsInCategory(category string) []WasteFinding {
	var findings []WasteFinding
	for _, f := range p.Findings {
		if f.Category == category {
			findings = append(findings, f)
		}
	}
	return findings
}
