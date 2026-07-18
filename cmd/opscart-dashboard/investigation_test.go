package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
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
						{Name: "test-ns", PodCount: 8, RiskLevel: "HIGH", RiskReason: "Production namespace with no network isolation"},
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
		})
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
		NamespaceScoped:  true,
		Namespace:        "test-ns",
		NamespaceFinding: "No NetworkPolicy present",
		IssueType:        "unprotected_namespace",
	}
	summary := buildOperationalSummary(data)

	if !strings.Contains(summary, "test-ns") || !strings.Contains(summary, "No NetworkPolicy present") {
		t.Errorf("expected summary to include namespace and finding, got %q", summary)
	}
	if strings.Contains(summary, "restarted") {
		t.Errorf("namespace-scoped summary should not include pod restart language, got %q", summary)
	}
	if !strings.Contains(summary, "not a runtime failure") {
		t.Errorf("expected closing sentence about configuration gap, got %q", summary)
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
}
