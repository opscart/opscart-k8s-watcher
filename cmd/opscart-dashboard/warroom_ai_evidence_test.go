package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

type warRoomAITestStore struct {
	store.NullStore
	incidents map[string][]store.IncidentSummary
	err       error
}

func (db warRoomAITestStore) QueryIncidents(filter store.IncidentFilter) ([]store.IncidentSummary, int, error) {
	if db.err != nil {
		return nil, 0, db.err
	}
	var matches []store.IncidentSummary
	for _, incident := range db.incidents[filter.Cluster] {
		if filter.Namespace != "" && incident.Namespace != filter.Namespace {
			continue
		}
		if filter.IssueType != "" && incident.IssueType != filter.IssueType {
			continue
		}
		if filter.Status != "" && incident.Status != filter.Status {
			continue
		}
		matches = append(matches, incident)
	}
	return matches, len(matches), nil
}

func warRoomAICrashFixture(cluster string, restarts int32) (*clusterScan, warRoomAITestStore) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	pod := "payments-0"
	fingerprint := store.WorkloadFingerprintForPod("payments", pod, store.IssueCrashLoop)
	scan := &clusterScan{
		report: &models.CloudCostReport{Timestamp: now, ClusterName: displayName(cluster)},
		wasteAudit: &analyzer.WasteAudit{ScannedAt: now, StalePods: []analyzer.StalePod{{
			Name: pod, Namespace: "payments", Kind: analyzer.StalePodZombie,
			Status: "CrashLoopBackOff", Severity: "critical", RestartCount: restarts, AgeDays: 3,
			ObservedEvidence: "raw-log-sentinel", Reason: "secret-value-sentinel",
		}}},
		PodWorkloads: map[string]models.WorkloadRef{
			"payments/" + pod: {Kind: "StatefulSet", Name: "payments", Namespace: "payments"},
		},
	}
	db := warRoomAITestStore{incidents: map[string][]store.IncidentSummary{
		cluster: {{
			Fingerprint: fingerprint, Namespace: "payments", Resource: pod,
			IssueType: store.IssueCrashLoop, Severity: "critical", Status: "active",
			FirstSeen: now.Add(-2 * time.Hour), LastSeen: now, ReopenCount: 2,
		}},
	}}
	return scan, db
}

func TestWarRoomAIEvidenceIsServerConstructedAndSanitized(t *testing.T) {
	scan, db := warRoomAICrashFixture("", 17)
	selections := collectWarRoomAISelections(scan, "", db)
	if len(selections) != 1 {
		t.Fatalf("expected one selectable issue, got %d", len(selections))
	}
	capture, err := captureWarRoomAIEvidence(scan, "", db, selections[0])
	if err != nil {
		t.Fatal(err)
	}
	if capture.Request.Cluster != "current-context" {
		t.Fatalf("empty internal cluster key mapped to %q", capture.Request.Cluster)
	}
	if capture.Request.ResourceKind != "StatefulSet" || capture.Request.ResourceName != "payments-0" {
		t.Fatalf("StatefulSet instance scope was not preserved: %+v", capture.Request)
	}
	if capture.Request.ReopenCount != 2 || capture.Request.FirstDetected.IsZero() {
		t.Fatalf("incident history not preserved: %+v", capture.Request)
	}
	encoded, err := json.Marshal(capture.Request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"raw-log-sentinel", "secret-value-sentinel", "kubectl", "--previous", "DetailsJSON"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("provider request contains excluded content %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "restart_count=17") || strings.Contains(strings.ToLower(text), "increased") {
		t.Fatalf("restart observation should be a count, not a trend claim: %s", text)
	}
	if len(encoded) > warRoomAIMaxRequestBytes || len(capture.Request.Evidence) > warRoomAIMaxEvidenceItems {
		t.Fatalf("request exceeded bounds: bytes=%d evidence=%d", len(encoded), len(capture.Request.Evidence))
	}
}

func TestWarRoomAISelectorsAreClusterAndFocusScoped(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 4)
	prod := collectWarRoomAISelections(scan, "prod", db)
	db.incidents["staging"] = db.incidents["prod"]
	staging := collectWarRoomAISelections(scan, "staging", db)
	if len(prod) != 1 || len(staging) != 1 || prod[0].Selector == staging[0].Selector {
		t.Fatalf("selectors did not preserve cluster isolation: prod=%+v staging=%+v", prod, staging)
	}
	if got := warRoomAISelector("prod", prod[0].Fingerprint, "payments-1", ""); got == prod[0].Selector {
		t.Fatal("focus pod identity was not included in selector")
	}
	if len(prod[0].Selector) != warRoomAIMaxSelectorBytes {
		t.Fatalf("selector length = %d, want %d", len(prod[0].Selector), warRoomAIMaxSelectorBytes)
	}
}

func TestWarRoomAIEvidenceHashIgnoresUnrelatedGenerationAndCaptureTime(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 4)
	selection := collectWarRoomAISelections(scan, "prod", db)[0]
	first, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	scan.generation++
	scan.report.Timestamp = scan.report.Timestamp.Add(time.Minute)
	second, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash {
		t.Fatal("unrelated cluster generation or capture-time change invalidated unchanged evidence")
	}
	if first.CapturedAt.Equal(second.CapturedAt) {
		t.Fatal("source capture timestamps were not tracked separately")
	}
}

func TestWarRoomAIEvidenceRequiresKnownHistoryAndSourceTime(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 4)
	selection := collectWarRoomAISelections(scan, "prod", db)[0]
	missingHistory := warRoomAITestStore{incidents: map[string][]store.IncidentSummary{}}
	if _, err := captureWarRoomAIEvidence(scan, "prod", missingHistory, selection); err != errWarRoomAIHistoryUnavailable {
		t.Fatalf("missing history error = %v", err)
	}
	scan.report.Timestamp = time.Time{}
	scan.wasteAudit.ScannedAt = time.Time{}
	if _, err := captureWarRoomAIEvidence(scan, "prod", db, selection); err != errWarRoomAISourceUnavailable {
		t.Fatalf("missing source time error = %v", err)
	}
}

func TestWarRoomAIEvidenceMappingsUseOnlyScalarObservations(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	scan := &clusterScan{
		secAudit: &models.SecurityAudit{Issues: []models.SecurityIssue{{
			Type: store.IssuePrivilegedContainer, Severity: "critical", Namespace: "apps", Name: "api-0/main",
			Description: "security-description-sentinel", Remediation: "security-remediation-sentinel",
		}}},
		netAudit: &analyzer.NetworkPolicyAudit{UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{{
			Name: "apps", RiskLevel: "HIGH", RiskReason: "network-risk-sentinel", PodCount: 4,
			PolicyCount: 1, CoverageGapPodCount: 2, HasIngressRestriction: true,
		}}},
		wasteAudit: &analyzer.WasteAudit{AbandonedNamespaces: []analyzer.AbandonedNamespace{{
			Name: "idle", AgeDays: 45, PodCount: 0, AllPodsIdle: true, Reason: "idle-reason-sentinel",
		}}},
		nodeHealth: []models.NodeConditionFinding{{
			NodeName: "worker-1", ConditionType: "DiskPressure", ConditionStatus: "True",
			Reason: "node-reason-sentinel", Message: "node-message-sentinel", LastTransitionTime: now,
			CorrelatedWorkloads: []models.CorrelatedWorkload{{Namespace: "apps", Kind: "Deployment", Name: "api", PodCount: 3}},
		}},
	}
	tests := []struct {
		name  string
		issue warRoomIssue
		want  string
	}{
		{name: "privileged", issue: warRoomIssue{Type: store.IssuePrivilegedContainer, Namespace: "apps", Resource: "api-0", Container: "main", WorkloadKind: "StatefulSet", WorkloadName: "api-0"}, want: "privileged=true"},
		{name: "network", issue: warRoomIssue{Type: store.IssueUnprotectedNamespace, Namespace: "apps", Resource: "namespace", WorkloadKind: "Namespace", WorkloadName: "apps"}, want: "pod_count=4; policy_count=1; coverage_gap_pod_count=2"},
		{name: "idle", issue: warRoomIssue{Type: store.IssueIdleNamespace, Namespace: "idle", Resource: "namespace", WorkloadKind: "Namespace", WorkloadName: "idle"}, want: "namespace_age_days=45; pod_count=0; all_pods_idle=true"},
		{name: "node", issue: warRoomIssue{Type: "DiskPressure", Resource: "worker-1", WorkloadKind: "Node", WorkloadName: "worker-1", IsNode: true}, want: "condition=DiskPressure; status=True; colocated_workload_count=1; colocated_pod_count=3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence, err := warRoomAIEvidenceItems(scan, test.issue)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			text := string(encoded)
			if !strings.Contains(text, test.want) {
				t.Fatalf("evidence %s missing %q", text, test.want)
			}
			for _, forbidden := range []string{"security-description-sentinel", "security-remediation-sentinel", "network-risk-sentinel", "idle-reason-sentinel", "node-reason-sentinel", "node-message-sentinel"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("evidence contains excluded analyzer prose %q: %s", forbidden, text)
				}
			}
		})
	}
}

func TestWarRoomAIRequestBounds(t *testing.T) {
	base := aianalysis.AnalysisRequest{
		Cluster: "prod", IssueType: store.IssueCrashLoop,
		Evidence: []aianalysis.EvidenceItem{{Type: aianalysis.EvidenceMetric, Summary: "counts", Details: "restart_count=1"}},
	}
	tooMany := base
	tooMany.Evidence = make([]aianalysis.EvidenceItem, warRoomAIMaxEvidenceItems+1)
	if _, err := validateAndEncodeWarRoomAIRequest(tooMany); err != errWarRoomAIEvidenceTooLarge {
		t.Fatalf("too many evidence items error = %v", err)
	}
	tooLong := base
	tooLong.ResourceName = strings.Repeat("x", warRoomAIMaxScalarBytes+1)
	if _, err := validateAndEncodeWarRoomAIRequest(tooLong); err != errWarRoomAIEvidenceTooLarge {
		t.Fatalf("oversized scalar error = %v", err)
	}
	tooDetailed := base
	tooDetailed.Evidence[0].Details = strings.Repeat("x", warRoomAIMaxDetailsBytes+1)
	if _, err := validateAndEncodeWarRoomAIRequest(tooDetailed); err != errWarRoomAIEvidenceTooLarge {
		t.Fatalf("oversized details error = %v", err)
	}
}

func TestWarRoomAISupportedIssueTypes(t *testing.T) {
	for _, issueType := range []string{
		store.IssueCrashLoop, store.IssueProbeFailure, store.IssueOOMKilled,
		store.IssueImagePullBackOff, store.IssueHighRestartCount,
		store.IssuePrivilegedContainer, store.IssueUnprotectedNamespace, store.IssueIdleNamespace,
	} {
		if !warRoomAISupports(warRoomIssue{Type: issueType}) {
			t.Errorf("expected %q to be supported", issueType)
		}
	}
	for _, condition := range []string{"Ready", "DiskPressure", "MemoryPressure", "PIDPressure", "NetworkUnavailable"} {
		if !warRoomAISupports(warRoomIssue{Type: condition, IsNode: true}) {
			t.Errorf("expected node condition %q to be supported", condition)
		}
	}
	if warRoomAISupports(warRoomIssue{Type: "running_as_root"}) {
		t.Fatal("unsupported issue type became selectable")
	}
}
