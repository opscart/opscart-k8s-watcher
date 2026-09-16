package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

const (
	warRoomAIMaxSelectorBytes = 64
	warRoomAIMaxEvidenceItems = 8
	warRoomAIMaxScalarBytes   = 256
	warRoomAIMaxSummaryBytes  = 160
	warRoomAIMaxDetailsBytes  = 512
	warRoomAIMaxRequestBytes  = 8 << 10
)

var (
	errWarRoomAISelectionNotFound  = errors.New("selected issue is not active")
	errWarRoomAIHistoryUnavailable = errors.New("incident history is unavailable")
	errWarRoomAISourceUnavailable  = errors.New("current issue evidence is unavailable")
	errWarRoomAIEvidenceTooLarge   = errors.New("issue evidence exceeds analysis limits")
)

type warRoomAISelection struct {
	Selector    string
	Fingerprint string
	Identity    string
	Issue       warRoomIssue
}

type warRoomAICapture struct {
	Selection  warRoomAISelection
	Request    aianalysis.AnalysisRequest
	CapturedAt time.Time
	Hash       string
}

type warRoomAIEvidenceSet struct {
	items       []aianalysis.EvidenceItem
	stableItems []aianalysis.EvidenceItem
}

func (set *warRoomAIEvidenceSet) append(item aianalysis.EvidenceItem) {
	set.items = append(set.items, item)
	set.stableItems = append(set.stableItems, item)
}

func (set *warRoomAIEvidenceSet) appendStable(item aianalysis.EvidenceItem, stableDetails string) {
	set.items = append(set.items, item)
	item.Details = stableDetails
	set.stableItems = append(set.stableItems, item)
}

func collectWarRoomAISelections(scan *clusterScan, cluster string, db store.Store) []warRoomAISelection {
	issues := collectWarRoomIssuesWithStore(scan, 0, db, cluster)
	selections := make([]warRoomAISelection, 0, len(issues))
	seen := make(map[string]bool, len(issues))
	for i := range issues {
		enrichWarRoomIdentity(&issues[i], scan)
		if !warRoomAISupports(issues[i]) {
			continue
		}
		fingerprint := warRoomAIFingerprint(issues[i])
		selector := warRoomAISelector(cluster, fingerprint, issues[i].Resource, issues[i].Container)
		if seen[selector] {
			continue
		}
		seen[selector] = true
		selections = append(selections, warRoomAISelection{
			Selector: selector, Fingerprint: fingerprint,
			Identity: warRoomAIIdentity(issues[i]), Issue: issues[i],
		})
	}
	return selections
}

func findWarRoomAISelection(scan *clusterScan, cluster string, db store.Store, selector string) (warRoomAISelection, error) {
	if selector == "" || len(selector) > warRoomAIMaxSelectorBytes {
		return warRoomAISelection{}, errWarRoomAISelectionNotFound
	}
	for _, selection := range collectWarRoomAISelections(scan, cluster, db) {
		if selection.Selector == selector {
			return selection, nil
		}
	}
	return warRoomAISelection{}, errWarRoomAISelectionNotFound
}

func captureWarRoomAIEvidence(scan *clusterScan, cluster string, db store.Store, selection warRoomAISelection) (warRoomAICapture, error) {
	if scan == nil {
		return warRoomAICapture{}, errWarRoomAISourceUnavailable
	}
	capturedAt := warRoomAICapturedAt(scan)
	if capturedAt.IsZero() {
		return warRoomAICapture{}, errWarRoomAISourceUnavailable
	}
	incident, err := warRoomAIIncident(db, cluster, selection.Fingerprint, selection.Issue)
	if err != nil || incident.FirstSeen.IsZero() {
		return warRoomAICapture{}, errWarRoomAIHistoryUnavailable
	}

	evidence, err := warRoomAIEvidenceForIssue(scan, selection.Issue)
	if err != nil {
		return warRoomAICapture{}, err
	}
	request := aianalysis.AnalysisRequest{
		Cluster:       displayName(cluster),
		Namespace:     selection.Issue.Namespace,
		ResourceKind:  selection.Issue.WorkloadKind,
		ResourceName:  selection.Issue.WorkloadName,
		IssueType:     store.CanonicalIssueType(selection.Issue.Type),
		Severity:      selection.Issue.Severity,
		FirstDetected: incident.FirstSeen,
		ReopenCount:   incident.ReopenCount,
		Evidence:      evidence.items,
	}
	if _, err := validateAndEncodeWarRoomAIRequest(request); err != nil {
		return warRoomAICapture{}, err
	}
	stableRequest := request
	stableRequest.Evidence = evidence.stableItems
	stableEncoded, err := validateAndEncodeWarRoomAIRequest(stableRequest)
	if err != nil {
		return warRoomAICapture{}, err
	}
	digest := sha256.Sum256(stableEncoded)
	return warRoomAICapture{
		Selection: selection, Request: request, CapturedAt: capturedAt,
		Hash: hex.EncodeToString(digest[:]),
	}, nil
}

func warRoomAIIncident(db store.Store, cluster, fingerprint string, issue warRoomIssue) (store.IncidentSummary, error) {
	if db == nil {
		return store.IncidentSummary{}, errWarRoomAIHistoryUnavailable
	}
	incidents, err := queryAllIncidentSummaries(db, store.IncidentFilter{
		Cluster: cluster, Namespace: issue.Namespace,
		IssueType: store.CanonicalIssueType(issue.Type), Status: "active",
	})
	if err != nil {
		return store.IncidentSummary{}, errWarRoomAIHistoryUnavailable
	}
	for _, incident := range incidents {
		if incident.Fingerprint == fingerprint && incident.Status == "active" {
			return incident, nil
		}
	}
	return store.IncidentSummary{}, errWarRoomAIHistoryUnavailable
}

func warRoomAIEvidenceItems(scan *clusterScan, issue warRoomIssue) ([]aianalysis.EvidenceItem, error) {
	evidence, err := warRoomAIEvidenceForIssue(scan, issue)
	return evidence.items, err
}

func warRoomAIEvidenceForIssue(scan *clusterScan, issue warRoomIssue) (warRoomAIEvidenceSet, error) {
	identityDetails := fmt.Sprintf("resource_kind=%s; resource_name=%s", issue.WorkloadKind, issue.WorkloadName)
	if !issue.IsNode && !isNamespaceFinding(issue) {
		identityDetails += "; focus_pod=" + issue.Resource
	}
	if issue.Container != "" {
		identityDetails += "; container=" + issue.Container
	}
	evidence := warRoomAIEvidenceSet{}
	evidence.append(aianalysis.EvidenceItem{
		Type: aianalysis.EvidenceObservation, Summary: "Selected issue identity", Details: identityDetails,
	})

	canonicalType := store.CanonicalIssueType(issue.Type)
	switch canonicalType {
	case store.IssueCrashLoop, store.IssueProbeFailure, store.IssueOOMKilled,
		store.IssueImagePullBackOff, store.IssueHighRestartCount:
		if scan.wasteAudit != nil {
			for _, pod := range scan.wasteAudit.StalePods {
				if pod.Kind == analyzer.StalePodZombie && pod.Namespace == issue.Namespace && pod.Name == issue.Resource && zombieTypeForStatus(pod.Status) == canonicalType {
					evidence.append(aianalysis.EvidenceItem{
						Type: aianalysis.EvidenceMetric, Summary: "Observed pod counts",
						Details: fmt.Sprintf("restart_count=%d; resource_age_days=%d", pod.RestartCount, pod.AgeDays),
					})
					appendWarRoomAIPodEvidence(&evidence, scan.aiPodEvidence, issue)
					return evidence, nil
				}
			}
		}
	case store.IssuePrivilegedContainer:
		if scan.secAudit != nil {
			for _, finding := range scan.secAudit.Issues {
				pod, container := splitWarRoomContainer(finding.Name)
				if finding.Type == store.IssuePrivilegedContainer && finding.Namespace == issue.Namespace && pod == issue.Resource && container == issue.Container {
					evidence.append(aianalysis.EvidenceItem{
						Type: aianalysis.EvidenceConfiguration, Summary: "Privileged container observed", Details: "privileged=true",
					})
					appendWarRoomAIPodEvidence(&evidence, scan.aiPodEvidence, issue)
					return evidence, nil
				}
			}
		}
	case store.IssueUnprotectedNamespace:
		if scan.netAudit != nil {
			for _, namespace := range scan.netAudit.UnprotectedNamespaces {
				if namespace.Name == issue.Namespace && namespace.RiskLevel == "HIGH" {
					evidence.append(aianalysis.EvidenceItem{
						Type: aianalysis.EvidenceConfiguration, Summary: "NetworkPolicy coverage observed",
						Details: fmt.Sprintf("pod_count=%d; policy_count=%d; coverage_gap_pod_count=%d; ingress_restricted=%t; egress_restricted=%t; default_deny_ingress=%t; default_deny_egress=%t",
							namespace.PodCount, namespace.PolicyCount, namespace.CoverageGapPodCount,
							namespace.HasIngressRestriction, namespace.HasEgressRestriction,
							namespace.HasDefaultDenyIngress, namespace.HasDefaultDenyEgress),
					})
					return evidence, nil
				}
			}
		}
	case store.IssueIdleNamespace:
		if scan.wasteAudit != nil {
			for _, namespace := range scan.wasteAudit.AbandonedNamespaces {
				if namespace.Name == issue.Namespace {
					evidence.append(aianalysis.EvidenceItem{
						Type: aianalysis.EvidenceMetric, Summary: "Namespace activity counts observed",
						Details: fmt.Sprintf("namespace_age_days=%d; pod_count=%d; all_pods_idle=%t", namespace.AgeDays, namespace.PodCount, namespace.AllPodsIdle),
					})
					return evidence, nil
				}
			}
		}
	default:
		if issue.IsNode {
			for _, finding := range scan.nodeHealth {
				if finding.NodeName != issue.Resource || finding.ConditionType != issue.Type {
					continue
				}
				pods := 0
				for _, workload := range finding.CorrelatedWorkloads {
					pods += workload.PodCount
				}
				details := fmt.Sprintf("condition=%s; status=%s; colocated_workload_count=%d; colocated_pod_count=%d",
					finding.ConditionType, finding.ConditionStatus, len(finding.CorrelatedWorkloads), pods)
				if !finding.LastTransitionTime.IsZero() {
					details += "; last_transition_time=" + finding.LastTransitionTime.UTC().Format(time.RFC3339)
				}
				evidence.append(aianalysis.EvidenceItem{
					Type: aianalysis.EvidenceObservation, Summary: "Node condition observed", Details: details,
				})
				return evidence, nil
			}
		}
	}
	return warRoomAIEvidenceSet{}, errWarRoomAISourceUnavailable
}

func validateAndEncodeWarRoomAIRequest(request aianalysis.AnalysisRequest) ([]byte, error) {
	for _, value := range []string{request.Cluster, request.Namespace, request.ResourceKind, request.ResourceName, request.IssueType, request.Severity} {
		if len(value) > warRoomAIMaxScalarBytes {
			return nil, errWarRoomAIEvidenceTooLarge
		}
	}
	if len(request.Evidence) == 0 || len(request.Evidence) > warRoomAIMaxEvidenceItems {
		return nil, errWarRoomAIEvidenceTooLarge
	}
	for _, item := range request.Evidence {
		if len(item.Summary) > warRoomAIMaxSummaryBytes || len(item.Details) > warRoomAIMaxDetailsBytes {
			return nil, errWarRoomAIEvidenceTooLarge
		}
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > warRoomAIMaxRequestBytes {
		return nil, errWarRoomAIEvidenceTooLarge
	}
	return encoded, nil
}

func warRoomAISupports(issue warRoomIssue) bool {
	if issue.IsNode {
		_, ok := nodeWarRoomSeverity(issue.Type)
		return ok
	}
	switch store.CanonicalIssueType(issue.Type) {
	case store.IssueCrashLoop, store.IssueProbeFailure, store.IssueOOMKilled,
		store.IssueImagePullBackOff, store.IssueHighRestartCount,
		store.IssuePrivilegedContainer, store.IssueUnprotectedNamespace, store.IssueIdleNamespace:
		return true
	default:
		return false
	}
}

func warRoomAIFingerprint(issue warRoomIssue) string {
	if issue.IsNode {
		return store.Fingerprint("cluster", "Node", issue.Resource, issue.Type)
	}
	return store.WorkloadFingerprintForPod(issue.Namespace, issue.Resource, issue.Type)
}

func warRoomAISelector(cluster, fingerprint, focusPod, container string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{cluster, fingerprint, focusPod, container}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func warRoomAIIdentity(issue warRoomIssue) string {
	if issue.IsNode {
		return "Node/" + issue.Resource + " · " + nodeConditionLabel(issue)
	}
	if isNamespaceFinding(issue) {
		return "Namespace/" + issue.Namespace + " · " + issue.Classification
	}
	identity := issue.WorkloadKind + "/" + issue.WorkloadName + " · Pod/" + issue.Resource
	if issue.Container != "" {
		identity += " · Container/" + issue.Container
	}
	return identity
}

func warRoomAICapturedAt(scan *clusterScan) time.Time {
	if scan != nil && scan.report != nil && !scan.report.Timestamp.IsZero() {
		return scan.report.Timestamp
	}
	if scan != nil && scan.wasteAudit != nil && !scan.wasteAudit.ScannedAt.IsZero() {
		return scan.wasteAudit.ScannedAt
	}
	return time.Time{}
}

func splitWarRoomContainer(name string) (string, string) {
	parts := strings.SplitN(name, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return name, ""
}
