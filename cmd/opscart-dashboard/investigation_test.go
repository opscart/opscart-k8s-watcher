package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// bogusClusterCtx is a kubeconfig context name that will never exist on the
// machine running these tests, so kubeClient() deterministically fails
// without touching the network. Pod-scoped issue types are expected to hit
// this failure (they still attempt a kube client); namespace-scoped issue
// types must never reach kubeClient() at all and should render normally.
const bogusClusterCtx = "opscart-test-nonexistent-context-zzz"

func newTestServer() *server {
	return newServer([]string{bogusClusterCtx}, store.NullStore{}, 0, false)
}

func TestHandleInvestigationPage_NamespaceScopedSkipsPodLookup(t *testing.T) {
	cases := []struct {
		issueType string
		scan      *clusterScan
	}{
		{
			issueType: "unprotected_namespace",
			scan: &clusterScan{
				netAudit: &analyzer.NetworkPolicyAudit{
					UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{
						{Name: "test-ns", PodCount: 8, RiskLevel: "HIGH", RiskReason: "critical infrastructure exposed"},
					},
				},
			},
		},
		{
			issueType: "idle_namespace",
			scan: &clusterScan{
				wasteAudit: &analyzer.WasteAudit{
					AbandonedNamespaces: []analyzer.AbandonedNamespace{
						{Name: "test-ns", PodCount: 3, Reason: "No activity in 30 days"},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.issueType, func(t *testing.T) {
			srv := newTestServer()
			state := srv.getState(bogusClusterCtx)
			state.scan = tc.scan

			req := httptest.NewRequest(http.MethodGet, "/investigate?pod=namespace&ns=test-ns&type="+tc.issueType+"&cluster="+bogusClusterCtx, nil)
			rec := httptest.NewRecorder()
			srv.handleInvestigationPage(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 OK (namespace-scoped path never calls kubeClient), got %d: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, "Namespace Finding") {
				t.Errorf("expected rendered page to contain the Namespace Finding section")
			}
			if strings.Contains(body, "Blast Radius") {
				t.Errorf("namespace-scoped page should not render the Blast Radius section")
			}
			if strings.Contains(body, "critical infrastructure exposed") {
				t.Errorf("namespace-scoped page rendered unsupported source text")
			}
		})
	}
}

func TestHandleInvestigationPage_NamespaceScopedWithoutPodParameter(t *testing.T) {
	srv := newTestServer()
	state := srv.getState(bogusClusterCtx)
	state.scan = &clusterScan{
		netAudit: &analyzer.NetworkPolicyAudit{
			UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{{
				Name: "test-ns", PodCount: 8, RiskLevel: "HIGH", RiskReason: "critical infrastructure exposed",
			}},
		},
	}

	req := httptest.NewRequest(http.MethodGet,
		"/investigate?ns=test-ns&type=unprotected_namespace&cluster="+bogusClusterCtx, nil)
	rec := httptest.NewRecorder()
	srv.handleInvestigationPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("namespace-scoped URL without pod should render, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "critical infrastructure exposed") {
		t.Errorf("complete rendered namespace page contained unsupported finding text")
	}
	if !strings.Contains(rec.Body.String(), "No default-deny NetworkPolicy detected in a system namespace") {
		t.Errorf("rendered namespace page missing mapped finding")
	}
}

func TestHandleInvestigationPage_PodScopedTypesStillAttemptPodLookup(t *testing.T) {
	podScopedTypes := []string{"crash_loop", "image_pull_backoff", "oomkilled", "privileged_container", "probe_failure"}

	for _, issueType := range podScopedTypes {
		t.Run(issueType, func(t *testing.T) {
			srv := newTestServer()

			req := httptest.NewRequest(http.MethodGet, "/investigate?pod=some-pod-abc123&ns=test-ns&type="+issueType+"&cluster="+bogusClusterCtx, nil)
			rec := httptest.NewRecorder()
			srv.handleInvestigationPage(rec, req)

			// A nonexistent kube context means kubeClient() fails — proving this
			// issue type took the pod-lookup path rather than the namespace-scoped
			// shortcut, which never reaches kubeClient() at all.
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("expected 502 (pod-scoped path attempts kubeClient), got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPopulateNamespaceFinding(t *testing.T) {
	scan := &clusterScan{
		netAudit: &analyzer.NetworkPolicyAudit{
			UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{
				{Name: "exposed-ns", PodCount: 8, RiskReason: "Production namespace with no network isolation"},
			},
		},
		wasteAudit: &analyzer.WasteAudit{
			AbandonedNamespaces: []analyzer.AbandonedNamespace{
				{Name: "idle-ns", PodCount: 3, Reason: "No activity in 30 days"},
			},
		},
	}

	t.Run("unprotected_namespace match", func(t *testing.T) {
		data := &investigationPageData{}
		populateNamespaceFinding(data, scan, "unprotected_namespace", "exposed-ns")
		if data.NamespacePodCount != 8 || data.NamespaceFinding == "" {
			t.Errorf("expected pod count 8 and non-empty finding, got %d / %q", data.NamespacePodCount, data.NamespaceFinding)
		}
		if data.NamespaceFinding != "No default-deny NetworkPolicy detected in a system namespace" {
			t.Errorf("unexpected mapped finding: %q", data.NamespaceFinding)
		}
	})

	t.Run("idle_namespace match", func(t *testing.T) {
		data := &investigationPageData{}
		populateNamespaceFinding(data, scan, "idle_namespace", "idle-ns")
		if data.NamespacePodCount != 3 || data.NamespaceFinding == "" {
			t.Errorf("expected pod count 3 and non-empty finding, got %d / %q", data.NamespacePodCount, data.NamespaceFinding)
		}
	})

	t.Run("no match does not panic", func(t *testing.T) {
		data := &investigationPageData{}
		populateNamespaceFinding(data, scan, "unprotected_namespace", "unknown-ns")
		if data.NamespacePodCount != 0 || data.NamespaceFinding != "" {
			t.Errorf("expected zero values for unmatched namespace, got %d / %q", data.NamespacePodCount, data.NamespaceFinding)
		}
	})

	t.Run("nil scan does not panic", func(t *testing.T) {
		data := &investigationPageData{}
		populateNamespaceFinding(data, nil, "unprotected_namespace", "exposed-ns")
	})
}

func TestInvestigationHintsNamespaceScopedNilPod(t *testing.T) {
	for _, issueType := range []string{"unprotected_namespace", "idle_namespace"} {
		t.Run(issueType, func(t *testing.T) {
			hints := investigationHints(issueType, "", 0, nil, "test-ns")
			if len(hints) == 0 {
				t.Fatalf("expected at least one hint for %s", issueType)
			}
			for _, h := range hints {
				if h.Command != "" && !strings.Contains(h.Command, "test-ns") {
					t.Errorf("expected command to reference namespace, got %q", h.Command)
				}
			}
		})
	}
}

func TestBuildOperationalSummaryNamespaceScoped(t *testing.T) {
	data := &investigationPageData{
		NamespaceScoped:   true,
		Namespace:         "test-ns",
		NamespaceFinding:  "No NetworkPolicy present",
		IssueType:         "unprotected_namespace",
		NamespacePodCount: 8,
		Severity:          "critical",
	}
	summary := buildOperationalSummary(data)

	for _, want := range []string{"No NetworkPolicy was detected", "test-ns", "8 pods", "classified as critical"} {
		if !strings.Contains(summary, want) {
			t.Errorf("expected summary to include %q, got %q", want, summary)
		}
	}
	for _, unsupported := range []string{"restarted", "critical infrastructure exposed", "every pod"} {
		if strings.Contains(summary, unsupported) {
			t.Errorf("namespace-scoped summary contains unsupported language %q: %q", unsupported, summary)
		}
	}
}

func TestRenderInvestigationTemplate_NamespaceScoped(t *testing.T) {
	data := investigationPageData{
		Namespace:         "test-ns",
		PodName:           "namespace",
		IssueType:         "unprotected_namespace",
		NamespaceScoped:   true,
		NamespacePodCount: 8,
		NamespaceFinding:  "No NetworkPolicy present",
		Hints:             investigationHints("unprotected_namespace", "", 0, nil, "test-ns"),
	}
	data.OperationalSummary = buildOperationalSummary(&data)

	rec := httptest.NewRecorder()
	renderInvestigation(rec, data)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK rendering namespace-scoped template, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Namespace Finding") {
		t.Errorf("expected rendered page to contain the Namespace Finding section")
	}
	if !strings.Contains(rec.Body.String(), "apiVersion: networking.k8s.io/v1") {
		t.Errorf("expected rendered page to preserve the NetworkPolicy YAML")
	}
}

func TestRenderInvestigationTemplateUsesFocusPodLabel(t *testing.T) {
	data := investigationPageData{
		Namespace:     "default",
		PodName:       "fraud-detection-7cddf79d98-jxmtx",
		IssueType:     "crash_loop",
		Severity:      "critical",
		WorkloadLabel: "Deployment/fraud-detection",
		PodAge:        "5m",
	}

	rec := httptest.NewRecorder()
	renderInvestigation(rec, data)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, body)
	}
	if !strings.Contains(body, "Focus Pod") {
		t.Errorf("expected Focus Pod label, got %q", body)
	}
	if strings.Contains(body, "Current Pod") {
		t.Errorf("rendered page retained Current Pod label")
	}
}

func TestInvestigationClassificationIndependentFromCurrentStatus(t *testing.T) {
	data := investigationPageData{
		Namespace:     "default",
		PodName:       "fraud-detection-7cddf79d98-jxmtx",
		IssueType:     "probe_failure",
		Severity:      "critical",
		WorkloadLabel: "Deployment/fraud-detection",
		TrackingLabel: "Deployment scoped",
		Phase:         "Running",
		StateReason:   "CrashLoopBackOff",
		PodAge:        "3 hours",
		Restarts:      47,
		FirstDetected: "Jul 29 · today",
		OperationalSummary: buildOperationalSummary(&investigationPageData{
			PodName: "fraud-detection-7cddf79d98-jxmtx", IssueType: "probe_failure",
			WorkloadLabel: "Deployment/fraud-detection", StateReason: "CrashLoopBackOff",
			PodAge: "3 hours", Restarts: 47, FirstDetected: "Jul 29 · today",
		}),
	}

	body := renderInvestigationBody(t, data)
	assertInOrder(t, body,
		"Classification", "Probe Failure",
		"Pod Phase", "Running",
		"Container State", "CrashLoopBackOff",
	)
	for _, want := range []string{
		"A probe failure incident is active for Deployment/fraud-detection.",
		"The Focus Pod fraud-detection-7cddf79d98-jxmtx is currently in CrashLoopBackOff",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workload page missing %q", want)
		}
	}
	if strings.Contains(body, "currently in Running") {
		t.Errorf("briefing contradicted the effective container state: %s", body)
	}
}

func TestInvestigationHintsUseObservationalRestartLanguage(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}}
	hints := investigationHints("crash_loop", "CrashLoopBackOff", 101, pod, "default")
	var reasons string
	for _, hint := range hints {
		reasons += hint.Reason
	}
	want := "The pod has restarted 101 times. Review previous container logs and restart timing to identify the recurring condition."
	if !strings.Contains(reasons, want) {
		t.Errorf("restart guidance missing observational wording: %q", reasons)
	}
	if strings.Contains(reasons, "failure reproduces consistently on every start") {
		t.Errorf("restart guidance retained unsupported deterministic claim")
	}
}

func TestEffectiveContainerState(t *testing.T) {
	tests := []struct {
		name   string
		status corev1.ContainerStatus
		want   string
	}{
		{
			name: "waiting",
			status: corev1.ContainerStatus{State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			}},
			want: "CrashLoopBackOff",
		},
		{
			name: "terminated error",
			status: corev1.ContainerStatus{State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
			}},
			want: "Error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveContainerState(tc.status); got != tc.want {
				t.Errorf("effectiveContainerState() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInvestigationWorkloadSectionsAndLabels(t *testing.T) {
	data := investigationPageData{
		Namespace:          "default",
		PodName:            "fraud-detection-7cddf79d98-jxmtx",
		IssueType:          "crash_loop",
		Severity:           "critical",
		WorkloadLabel:      "Deployment/fraud-detection",
		TrackingLabel:      "Deployment scoped",
		StateReason:        "CrashLoopBackOff",
		PodAge:             "3 hours",
		Restarts:           47,
		FirstDetected:      "Jul 10 · 19 days",
		OperationalSummary: "A CrashLoopBackOff incident is active. The Focus Pod is observed from live data.",
		Hints:              []investigationHint{{Confidence: "high", Title: "Check logs"}},
		Timeline: []store.IncidentEvent{{
			OccurredAt: time.Now(), EventType: "DETECTED", Message: "crash_loop first detected",
		}},
		BlastSiblings: []blastRadiusPod{{Name: "fraud-detection-a", Phase: "Running", Healthy: true}},
		BlastHealthy:  1,
		BlastTotal:    2,
	}

	body := renderInvestigationBody(t, data)
	if strings.Contains(body, "<h2>Evidence</h2>") || strings.Contains(body, "<div class=\"inv-evidence-label\">State</div>") {
		t.Errorf("workload page rendered legacy Evidence content")
	}
	for _, want := range []string{"Replica Health", "Jul 10 · 19 days"} {
		if !strings.Contains(body, want) {
			t.Errorf("workload page missing %q", want)
		}
	}
	assertInOrder(t, body,
		"Situation Briefing",
		"Operational Identity",
		"Current Observation",
		"Recommended Investigation",
		"Incident Timeline",
		"Blast Radius",
		"Related Resources",
	)
}

func TestInvestigationNamespaceSectionsAndEvidence(t *testing.T) {
	command := "kubectl get networkpolicies -n monitoring"
	data := investigationPageData{
		Namespace:          "monitoring",
		PodName:            "namespace",
		IssueType:          "unprotected_namespace",
		Severity:           "critical",
		NamespaceScoped:    true,
		NamespacePodCount:  8,
		NamespaceFinding:   "No NetworkPolicy present",
		OperationalSummary: "No NetworkPolicy was detected in the monitoring namespace. The namespace currently contains 8 pods.",
		Hints:              []investigationHint{{Confidence: "high", Title: "Apply a policy", Command: command}},
		Commands:           []string{command},
		Timeline: []store.IncidentEvent{{
			OccurredAt: time.Now(), EventType: "DETECTED", Message: "unprotected_namespace first detected",
		}},
	}

	body := renderInvestigationBody(t, data)
	for _, want := range []string{
		"Namespace Finding", "Namespace", "Pods affected", "Finding",
		"apiVersion: networking.k8s.io/v1", "kind: NetworkPolicy",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("namespace page missing %q", want)
		}
	}
	if strings.Contains(body, "<h2>Investigation Commands</h2>") {
		t.Errorf("namespace page duplicated a command already represented by Recommended Investigation")
	}
	assertInOrder(t, body,
		"Situation Briefing",
		"Namespace Finding",
		"Recommended Investigation",
		"Incident Timeline",
	)
}

func TestInvestigationRecentEventsHiddenWhenEmpty(t *testing.T) {
	empty := renderInvestigationBody(t, investigationPageData{Namespace: "default", PodName: "pod"})
	if strings.Contains(empty, "<h2>Recent Events</h2>") {
		t.Fatal("Recent Events section rendered without events")
	}
	withEvent := renderInvestigationBody(t, investigationPageData{
		Namespace: "default", PodName: "pod",
		Events: []investigationEvent{{Type: "Warning", Reason: "BackOff", Age: "1m", Count: 1, Message: "retrying"}},
	})
	if !strings.Contains(withEvent, "<h2>Recent Events</h2>") {
		t.Fatal("Recent Events section missing when events exist")
	}
}

func TestFirstDetectedLabelIncludesAbsoluteAndRelativeDate(t *testing.T) {
	location := time.FixedZone("test", -4*60*60)
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, location)
	tests := []struct {
		firstSeen time.Time
		want      string
	}{
		{time.Date(2026, time.July, 29, 1, 0, 0, 0, location), "Jul 29 · today"},
		{time.Date(2026, time.July, 10, 23, 0, 0, 0, location), "Jul 10 · 19 days"},
	}
	for _, tc := range tests {
		if got := firstDetectedLabelAt(tc.firstSeen, now); got != tc.want {
			t.Errorf("firstDetectedLabelAt(%v) = %q, want %q", tc.firstSeen, got, tc.want)
		}
	}
}

func renderInvestigationBody(t *testing.T, data investigationPageData) string {
	t.Helper()
	rec := httptest.NewRecorder()
	renderInvestigation(rec, data)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func assertInOrder(t *testing.T, body string, labels ...string) {
	t.Helper()
	position := -1
	for _, label := range labels {
		next := strings.Index(body[position+1:], label)
		if next < 0 {
			t.Fatalf("rendered page missing %q", label)
		}
		position += next + 1
	}
}
