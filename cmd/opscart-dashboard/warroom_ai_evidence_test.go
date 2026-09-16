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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// TestWarRoomAINamespaceEvidenceIgnoresRiskLevel pins the fix for a second,
// independent copy of the War Room list bug: collectWarRoomIssues used to
// gate unprotected_namespace existence on RiskLevel == "HIGH"; this file's
// evidence-capture switch had its own copy of the same gate. RiskLevel is a
// namespace-name/pod-count heuristic (analyzeRisk), not reliable existence
// evidence -- a namespace with the exact same coverage gap must produce
// identical NetworkPolicy evidence regardless of what RiskLevel happens to
// compute to, at both PolicyCount extremes (zero policies at all, and a
// partial-coverage gap).
func TestWarRoomAINamespaceEvidenceIgnoresRiskLevel(t *testing.T) {
	issue := warRoomIssue{Type: store.IssueUnprotectedNamespace, Namespace: "payments", Resource: "namespace", WorkloadKind: "Namespace", WorkloadName: "payments"}
	tests := []struct {
		name        string
		policyCount int
		coverageGap int
		want        string
	}{
		{name: "zero policies", policyCount: 0, coverageGap: 5, want: "pod_count=5; policy_count=0; coverage_gap_pod_count=5"},
		{name: "partial coverage", policyCount: 1, coverageGap: 2, want: "pod_count=5; policy_count=1; coverage_gap_pod_count=2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, riskLevel := range []string{"HIGH", "MEDIUM", "LOW"} {
				scan := &clusterScan{netAudit: &analyzer.NetworkPolicyAudit{UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{{
					Name: "payments", RiskLevel: riskLevel, RiskReason: "network-risk-sentinel",
					PodCount: 5, PolicyCount: test.policyCount, CoverageGapPodCount: test.coverageGap,
					HasIngressRestriction: true,
				}}}}
				evidence, err := warRoomAIEvidenceItems(scan, issue)
				if err != nil {
					t.Fatalf("RiskLevel %s: %v", riskLevel, err)
				}
				encoded, err := json.Marshal(evidence)
				if err != nil {
					t.Fatal(err)
				}
				text := string(encoded)
				if !strings.Contains(text, test.want) {
					t.Fatalf("RiskLevel %s: evidence %s missing %q", riskLevel, text, test.want)
				}
				if strings.Contains(text, "network-risk-sentinel") {
					t.Fatalf("RiskLevel %s: evidence contains excluded RiskReason prose: %s", riskLevel, text)
				}
			}
		})
	}
}

// warRoomAIProbeFailurePodEvidenceFixture builds a real corev1.Pod (one
// container, HTTP liveness probe, currently waiting on CrashLoopBackOff
// after a prior OOM termination) and a real corev1.Event (a Warning
// carrying the kubelet's actual probe-failure message text, which must
// never reach the AI request) for a probe_failure issue, and wires both
// into scan.aiPodEvidence exactly as buildClusterScan does in production
// (analysis.go) -- via buildWarRoomAIPodEvidenceIndex, not by hand.
func warRoomAIProbeFailurePodEvidenceFixture(namespace, podName, container string, capturedAt time.Time) *warRoomAIPodEvidenceIndex {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: podName},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: container,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
			LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt(8080),
			}}},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: container, Ready: false, RestartCount: 27779,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
			}},
		},
	}
	event := &corev1.Event{
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: namespace, Name: podName},
		Type:           corev1.EventTypeWarning,
		Reason:         "Unhealthy",
		Message:        "Liveness probe failed: HTTP probe failed with statuscode: 404 at http://10.1.2.3:8080/healthz",
		LastTimestamp:  metav1.NewTime(capturedAt.Add(-90 * time.Second)),
	}
	return buildWarRoomAIPodEvidenceIndex([]*corev1.Pod{pod}, []*corev1.Event{event}, true, true, capturedAt)
}

// TestWarRoomAIProbeFailureRequestContent is the decisive, full-path test:
// it drives the actual production entry point, captureWarRoomAIEvidence,
// for a real probe_failure selection with scan.aiPodEvidence populated the
// same way buildClusterScan populates it, and inspects the exact
// AnalysisRequest.Evidence a provider would receive. This settles, against
// real code rather than a stale snapshot, what evidence a probe-failure
// request actually contains: pod phase, container runtime state, resource
// requests/limits, probe TYPE (not path/port), and Warning Event reason/
// count/age (not message text). Container logs and the raw event message
// -- which does contain the probe path and HTTP status in this fixture --
// must never appear.
func TestWarRoomAIProbeFailureRequestContent(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	pod, container := "checkout-api-0", "api"
	fingerprint := store.WorkloadFingerprintForPod("payments", pod, store.IssueProbeFailure)
	scan := &clusterScan{
		report: &models.CloudCostReport{Timestamp: now, ClusterName: displayName("prod")},
		wasteAudit: &analyzer.WasteAudit{ScannedAt: now, StalePods: []analyzer.StalePod{{
			Name: pod, Namespace: "payments", Kind: analyzer.StalePodZombie,
			Status: "ProbeFailure", Severity: "critical", RestartCount: 27779, AgeDays: 8,
			ObservedEvidence: "raw-log-sentinel", Reason: "secret-value-sentinel",
		}}},
		PodWorkloads: map[string]models.WorkloadRef{
			"payments/" + pod: {Kind: "Deployment", Name: "checkout-api", Namespace: "payments"},
		},
		aiPodEvidence: warRoomAIProbeFailurePodEvidenceFixture("payments", pod, container, now),
	}
	db := warRoomAITestStore{incidents: map[string][]store.IncidentSummary{
		"prod": {{
			Fingerprint: fingerprint, Namespace: "payments", Resource: pod,
			IssueType: store.IssueProbeFailure, Severity: "critical", Status: "active",
			FirstSeen: now.Add(-8 * 24 * time.Hour), LastSeen: now, ReopenCount: 0,
		}},
	}}

	selections := collectWarRoomAISelections(scan, "prod", db)
	if len(selections) != 1 {
		t.Fatalf("got %d selections, want 1: %+v", len(selections), selections)
	}
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, selections[0])
	if err != nil {
		t.Fatalf("captureWarRoomAIEvidence: %v", err)
	}
	encoded, err := json.Marshal(capture.Request.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)

	for _, want := range []string{
		"restart_count=27779", "resource_age_days=8", // base metric evidence
		"pod_phase=Running", "target_container=" + container, // pod snapshot observation
		"ready=false", "current_state=waiting", "waiting_reason=CrashLoopBackOff", // runtime state
		"last_termination_reason=OOMKilled", "last_termination_exit_code=137",
		"cpu_request=250m", "memory_request=256Mi", "liveness_probe=http_get", // resources + probe TYPE only
		"reason=Unhealthy", // Warning Event reason
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("evidence missing %q; full evidence: %s", want, text)
		}
	}
	for _, forbidden := range []string{
		"raw-log-sentinel", "secret-value-sentinel", // analyzer-internal fields (ObservedEvidence, Reason)
		"404", "healthz", "10.1.2.3", // the Event's Message text -- must never appear
		"Liveness probe failed", // Event message, verbatim
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("evidence contains excluded content %q; full evidence: %s", forbidden, text)
		}
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
