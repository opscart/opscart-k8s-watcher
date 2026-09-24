package main

import (
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	"k8s.io/client-go/kubernetes"
)

func warRoomAINamespaceFixture(cluster string) (*clusterScan, warRoomAITestStore) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	fingerprint := store.WorkloadFingerprintForPod("payments", "namespace", store.IssueUnprotectedNamespace)
	scan := &clusterScan{
		report: &models.CloudCostReport{Timestamp: now, ClusterName: displayName(cluster)},
		netAudit: &analyzer.NetworkPolicyAudit{UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{{
			Name: "payments", RiskLevel: "HIGH", PodCount: 5, PolicyCount: 0,
		}}},
	}
	db := warRoomAITestStore{incidents: map[string][]store.IncidentSummary{
		cluster: {{
			Fingerprint: fingerprint, Namespace: "payments", Resource: "namespace",
			IssueType: store.IssueUnprotectedNamespace, Status: "active",
			FirstSeen: now.Add(-time.Hour), LastSeen: now,
		}},
	}}
	return scan, db
}

func warRoomAINodeFixture(cluster string) (*clusterScan, warRoomAITestStore) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	fingerprint := store.Fingerprint("cluster", "Node", "worker-1", "DiskPressure")
	scan := &clusterScan{
		report: &models.CloudCostReport{Timestamp: now, ClusterName: displayName(cluster)},
		nodeHealth: []models.NodeConditionFinding{{
			NodeName: "worker-1", ConditionType: "DiskPressure", ConditionStatus: "True",
			Reason: "reason-sentinel", Message: "message-sentinel", LastTransitionTime: now,
		}},
	}
	db := warRoomAITestStore{incidents: map[string][]store.IncidentSummary{
		cluster: {{
			Fingerprint: fingerprint, Namespace: "", Resource: "worker-1",
			IssueType: "DiskPressure", Status: "active",
			FirstSeen: now.Add(-time.Hour), LastSeen: now,
		}},
	}}
	return scan, db
}

// TestInvestigationAIGetMakesNoKubernetesCalls proves the AI tab resolves
// entirely from the cached scan, incident store, and AI runtime/cache: no
// kubeClientFor call and no scan refresh.
func TestInvestigationAIGetMakesNoKubernetesCalls(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	kubeCalls := 0
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		kubeCalls++
		return nil, errors.New("kubeClientFor must not be called for the AI tab")
	}
	scanBefore := srv.states["prod"].scan

	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selector, nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("AI tab status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if kubeCalls != 0 {
		t.Fatalf("AI tab invoked kubeClientFor %d time(s)", kubeCalls)
	}
	if srv.states["prod"].scan != scanBefore {
		t.Fatal("AI tab refreshed the cluster scan")
	}
	if !strings.Contains(rec.Body.String(), "NOT_GENERATED") {
		t.Fatalf("expected a not-generated status before any generation: %s", rec.Body.String())
	}
}

// TestInvestigationAIRendersSafeStateWhenScanAbsent proves a cluster with no
// cached scan yet renders a safe not-generated state instead of triggering a
// refresh or live Kubernetes read.
func TestInvestigationAIRendersSafeStateWhenScanAbsent(t *testing.T) {
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": nil}, warRoomAITestStore{}, provider)
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		t.Fatal("kubeClientFor must not be called when the scan is absent")
		return nil, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue=deadbeef", nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "NOT_GENERATED") {
		t.Fatalf("expected NOT_GENERATED when no scan is cached: %s", rec.Body.String())
	}
	if srv.states["prod"].scan != nil {
		t.Fatal("AI tab triggered a scan")
	}
}

// TestInvestigationMissingTabStillOpensEvidence proves the routing dispatch
// at the top of handleInvestigationPage preserves the existing Evidence
// behavior when tab is missing or explicitly "evidence".
func TestInvestigationMissingTabStillOpensEvidence(t *testing.T) {
	for _, tab := range []string{"", "evidence"} {
		srv := newTestServer()
		target := "/investigate?pod=some-pod&ns=test-ns&type=crash_loop&cluster=" + bogusClusterCtx
		if tab != "" {
			target += "&tab=" + tab
		}
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		srv.handleInvestigationPage(rec, req)
		// bogusClusterCtx makes kubeClientFor fail deterministically — reaching
		// that failure proves the Evidence (pod-lookup) path ran.
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("tab=%q: expected the Evidence pod-lookup path, got %d: %s", tab, rec.Code, rec.Body.String())
		}
	}
}

// TestInvestigationUnknownTabRejected proves an unrecognized tab value is
// rejected before any parameter validation or Kubernetes access.
func TestInvestigationUnknownTabRejected(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/investigate?tab=bogus&cluster="+bogusClusterCtx, nil)
	rec := httptest.NewRecorder()
	srv.handleInvestigationPage(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown tab status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// TestInvestigateLinkNeverOpensAITab proves the War Room "Investigate →"
// deep link never carries a tab parameter, so it always opens Evidence.
func TestInvestigateLinkNeverOpensAITab(t *testing.T) {
	issue := warRoomIssue{Namespace: "payments", Resource: "payments-0", Type: "crash_loop", WorkloadKind: "StatefulSet", WorkloadName: "payments-0"}
	href := warRoomIssueURL(issue, "prod")
	parsed, err := url.Parse(href)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("tab") != "" {
		t.Fatalf("Investigate link carried a tab parameter: %s", href)
	}
	nodeIssue := warRoomIssue{IsNode: true, Resource: "worker-1", Type: "DiskPressure"}
	nodeHref := warRoomIssueURL(nodeIssue, "prod")
	if parsed, err = url.Parse(nodeHref); err != nil || parsed.Query().Get("tab") != "" {
		t.Fatalf("node Investigate link carried a tab parameter: %s", nodeHref)
	}
}

// TestInvestigationAIHrefCarriesOnlyAllowedParams pins the routing contract:
// an AI deep link carries only cluster, tab=ai, the opaque selector, and an
// optional navigation origin — never namespace/pod/container/node.
func TestInvestigationAIHrefCarriesOnlyAllowedParams(t *testing.T) {
	href := investigationAIHref("prod", "deadbeefcafe", "incidents")
	parsed, err := url.Parse(href)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	allowed := map[string]bool{"tab": true, "issue": true, "cluster": true, "from": true}
	for key := range q {
		if !allowed[key] {
			t.Fatalf("AI href leaked disallowed parameter %q: %s", key, href)
		}
	}
	if q.Get("tab") != "ai" || q.Get("issue") != "deadbeefcafe" || q.Get("cluster") != "prod" || q.Get("from") != "incidents" {
		t.Fatalf("AI href malformed: %s", href)
	}
}

// TestWarRoomAICardLinkIsOpaqueWhenDisabledOrEnabled proves supported
// analysis remains discoverable without exposing issue identity in either
// deployment state.
func TestWarRoomAICardLinkIsOpaqueWhenDisabledOrEnabled(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	const marker = "class=\"ai-chip\" href=\""
	for _, enabled := range []bool{false, true} {
		body := renderWarRoomPageWithAI(scan, "prod", []string{"prod"}, nil, db, enabled)
		idx := strings.Index(body, marker)
		if idx == -1 {
			t.Fatalf("AI chip missing when enabled=%t: %s", enabled, body)
		}
		start := idx + len(marker)
		end := strings.Index(body[start:], "\"")
		if end == -1 {
			t.Fatalf("AI chip href malformed: %s", body)
		}
		href := html.UnescapeString(body[start : start+end])
		parsed, err := url.Parse(href)
		if err != nil {
			t.Fatal(err)
		}
		q := parsed.Query()
		for _, forbidden := range []string{"ns", "pod", "container", "node"} {
			if q.Get(forbidden) != "" {
				t.Fatalf("AI card link exposed %q as an input: %s", forbidden, href)
			}
		}
		if q.Get("tab") != "ai" || q.Get("issue") == "" {
			t.Fatalf("AI card link missing tab/issue: %s", href)
		}
	}
}

// TestInvestigationTabsShowSetupWhenDisabled and
// TestInvestigationTabsHideAIForUnsupportedIssueType cover the Evidence
// page's AI discovery and issue-support gating.
func TestInvestigationTabsShowSetupWhenDisabled(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, nil)
	tabs := srv.buildInvestigationTabs("prod", scan, store.IssueCrashLoop, "payments", "payments-0", "", "")
	if !tabs.AIAvailable || tabs.AIConfigured || tabs.AITabHref == "" {
		t.Fatalf("AI setup tab missing while AI disabled: %+v", tabs)
	}
	if tabs.EvidenceTabHref == "" {
		t.Fatal("Evidence tab href missing")
	}
}

func TestInvestigationTabsHideAIForUnsupportedIssueType(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, &fakeWarRoomAIProvider{response: testWarRoomAIResponse()})
	tabs := srv.buildInvestigationTabs("prod", scan, "running_as_root", "payments", "payments-0", "", "")
	if tabs.AIAvailable || tabs.AITabHref != "" {
		t.Fatalf("unsupported issue type exposed an AI tab: %+v", tabs)
	}
}

func TestInvestigationTabsShowAIWhenSupportedAndEnabled(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, &fakeWarRoomAIProvider{response: testWarRoomAIResponse()})
	tabs := srv.buildInvestigationTabs("prod", scan, store.IssueCrashLoop, "payments", "payments-0", "", "")
	if !tabs.AIAvailable || tabs.AITabHref == "" {
		t.Fatalf("expected AI tab available: %+v", tabs)
	}
	parsed, err := url.Parse(tabs.AITabHref)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("tab") != "ai" || q.Get("issue") == "" {
		t.Fatalf("AI tab href malformed: %s", tabs.AITabHref)
	}
	for _, forbidden := range []string{"ns", "pod", "node", "container"} {
		if q.Get(forbidden) != "" {
			t.Fatalf("AI tab href exposed %q: %s", forbidden, tabs.AITabHref)
		}
	}
}

// TestInvestigationAIRendersGeneratedForCurrentCache covers a current cache
// hit rendering GENERATED with its provider/model recorded.
func TestInvestigationAIRendersGeneratedForCurrentCache(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	if rec := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true); rec.Code != http.StatusOK {
		t.Fatalf("generation status=%d body=%s", rec.Code, rec.Body.String())
	}

	page := srv.buildInvestigationAIPageData(scan, "prod", selector, "")
	if page.Status != "GENERATED" {
		t.Fatalf("status = %q, want GENERATED", page.Status)
	}
	if page.Provider != "openai" || page.Model != "synthetic-model" {
		t.Fatalf("provider/model not recorded: %+v", page)
	}
	if page.Result == nil {
		t.Fatal("expected a rendered result")
	}
}

// TestInvestigationAIStaleShowsOriginalGenerationEvidence pins the
// mandatory provenance rule: a stale result keeps showing exactly the
// evidence and provider/model that produced it, never the newer scan.
func TestInvestigationAIStaleShowsOriginalGenerationEvidence(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	if rec := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true); rec.Code != http.StatusOK {
		t.Fatalf("generation status=%d body=%s", rec.Code, rec.Body.String())
	}
	original := srv.buildInvestigationAIPageData(scan, "prod", selector, "")

	updated, _ := warRoomAICrashFixture("prod", 99)
	updated.report.Timestamp = updated.report.Timestamp.Add(time.Minute)
	updated.wasteAudit.ScannedAt = updated.wasteAudit.ScannedAt.Add(time.Minute)
	// A restart-count bump alone (7 -> 99) must not, by itself, make the
	// cached analysis stale during the same incident — a new Warning Event
	// is the genuinely meaningful evidence change driving staleness here.
	updated.aiPodEvidence = warRoomAINewWarningEventEvidence("payments", "payments-0", updated.report.Timestamp)
	srv.states["prod"].mu.Lock()
	srv.states["prod"].scan = updated
	srv.states["prod"].mu.Unlock()

	stale := srv.buildInvestigationAIPageData(updated, "prod", selector, "")
	if stale.Status != "STALE" {
		t.Fatalf("status = %q, want STALE", stale.Status)
	}
	if stale.Provider != original.Provider || stale.Model != original.Model {
		t.Fatalf("provider/model changed on a stale result: got %s/%s want %s/%s", stale.Provider, stale.Model, original.Provider, original.Model)
	}
	if len(stale.GenerationEvidence) == 0 {
		t.Fatal("stale result lost its generation-time evidence")
	}
	foundOriginal := false
	for _, item := range stale.GenerationEvidence {
		if strings.Contains(item.Details, "restart_count=99") {
			t.Fatalf("stale result rendered the newer scan's evidence: %+v", item)
		}
		if strings.Contains(item.Details, "restart_count=7") {
			foundOriginal = true
		}
	}
	if !foundOriginal {
		t.Fatalf("stale result did not show its original generation-time evidence: %+v", stale.GenerationEvidence)
	}
}

// TestInvestigationAIIdentityRendersConditionally covers all three generic
// identity shapes (workload/pod, namespace, node) and proves empty
// pod/container/namespace fields are never rendered for the wrong shape.
func TestInvestigationAIIdentityRendersConditionally(t *testing.T) {
	cases := []struct {
		name           string
		fixture        func() (*clusterScan, warRoomAITestStore)
		mustContain    []string
		mustNotContain []string
	}{
		{
			// A workload/pod issue shows the pod in the (conditionally
			// rendered) pod-details disclosure, but no container row since
			// this fixture's issue carries no container.
			name:           "pod",
			fixture:        func() (*clusterScan, warRoomAITestStore) { return warRoomAICrashFixture("prod", 7) },
			mustContain:    []string{"payments-0", `class="ai-pod-details"`, `<span>Pod</span>`, `class="ai-head-ns"`},
			mustNotContain: []string{`<span>Container</span>`},
		},
		{
			// A namespace-scoped issue has no pod/container at all — the
			// entire pod-details disclosure must be omitted, not merely
			// left with empty rows — and no separate namespace badge next
			// to the resource name, since the resource name already reads
			// "Namespace/payments".
			name:           "namespace",
			fixture:        func() (*clusterScan, warRoomAITestStore) { return warRoomAINamespaceFixture("prod") },
			mustContain:    []string{"Namespace/payments"},
			mustNotContain: []string{`class="ai-pod-details"`, `<span>Pod</span>`, `<span>Container</span>`, `class="ai-head-ns"`},
		},
		{
			// A node-scoped issue has no namespace, pod, or container.
			name:           "node",
			fixture:        func() (*clusterScan, warRoomAITestStore) { return warRoomAINodeFixture("prod") },
			mustContain:    []string{"Node/worker-1"},
			mustNotContain: []string{`class="ai-pod-details"`, `<span>Pod</span>`, `<span>Container</span>`, `class="ai-head-ns"`, "Namespace/"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan, db := tc.fixture()
			provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
			srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
			selections := collectWarRoomAISelections(scan, "prod", db)
			if len(selections) == 0 {
				t.Fatal("fixture produced no AI selection")
			}
			req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selections[0].Selector, nil)
			req.SetBasicAuth("operator", "test-password")
			rec := httptest.NewRecorder()
			srv.newMux().ServeHTTP(rec, req)
			body := rec.Body.String()
			for _, want := range tc.mustContain {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %q: %s", want, body)
				}
			}
			for _, forbidden := range tc.mustNotContain {
				if strings.Contains(body, forbidden) {
					t.Fatalf("unexpected %q: %s", forbidden, body)
				}
			}
		})
	}
}

// TestInvestigationAIRendersProviderTextEscaped proves provider-controlled
// model text is always HTML-escaped, and that the page's own error handling
// never injects content through innerHTML.
func TestInvestigationAIRendersProviderTextEscaped(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selection := collectWarRoomAISelections(scan, "prod", db)[0]
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	response := testWarRoomAIResponse()
	response.Summary = `<script>alert("provider")</script>`
	response.LikelyCauses[0].Title = `<img src=x onerror=alert(1)>`
	srv.aiRuntime.cache.put(warRoomAICacheEntry{
		ClusterKey: "prod", Selector: selection.Selector, EvidenceHash: capture.Hash, BaseEvidenceHash: capture.Hash,
		RuntimeKey: srv.aiRuntime.key(), IssueIdentity: selection.Identity,
		Provider: "openai", Model: "synthetic-model",
		EvidenceCaptured: capture.CapturedAt, GeneratedAt: srv.aiRuntime.cache.now(), Response: *response,
		Evidence: capture.Request.Evidence,
	})

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selection.Selector, nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	body := rec.Body.String()

	if strings.Contains(body, response.Summary) || strings.Contains(body, response.LikelyCauses[0].Title) {
		t.Fatal("provider-controlled model text rendered as trusted HTML")
	}
	if !strings.Contains(body, `&lt;script&gt;alert`) || !strings.Contains(body, `&lt;img src=x onerror=alert(1)&gt;`) {
		t.Fatalf("escaped model text missing from page: %s", body)
	}
	if !strings.Contains(body, "textContent") {
		t.Fatal("AI page must render its error state via textContent")
	}
	if strings.Contains(body, ".innerHTML") {
		t.Fatal("AI page must never inject content through innerHTML")
	}
}

// TestInvestigationAINeverRendersBaseURL proves the provider base URL is
// never rendered, even when a full generation has been cached.
func TestInvestigationAINeverRendersBaseURL(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	if rec := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true); rec.Code != http.StatusOK {
		t.Fatalf("generation status=%d body=%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selector, nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, aianalysis.DefaultBaseURL) ||
		strings.Contains(strings.ToLower(body), "baseurl") ||
		strings.Contains(strings.ToLower(body), "base_url") {
		t.Fatalf("AI page rendered a provider base URL: %s", body)
	}
}

// TestInvestigationAIRequiresIssueAndKnownCluster covers the AI-only
// handler's own input validation.
func TestInvestigationAIRequiresIssueAndKnownCluster(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, &fakeWarRoomAIProvider{response: testWarRoomAIResponse()})

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai", nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing issue selector status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/investigate?cluster=attacker&tab=ai&issue=x", nil)
	req.SetBasicAuth("operator", "test-password")
	rec = httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown cluster status = %d", rec.Code)
	}
}

func TestInvestigationAIContainerLinks(t *testing.T) {
	for _, names := range [][]string{{"main", "sidecar"}, {"sidecar", "main"}} {
		scan := &clusterScan{secAudit: &models.SecurityAudit{}}
		for _, name := range names {
			scan.secAudit.Issues = append(scan.secAudit.Issues, models.SecurityIssue{
				Type: store.IssuePrivilegedContainer, Severity: "critical", Namespace: "apps", Name: "api-0/" + name,
			})
		}
		db := warRoomAITestStore{}
		srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, &fakeWarRoomAIProvider{})
		selections := collectWarRoomAISelections(scan, "prod", db)
		if len(selections) != 2 {
			t.Fatalf("expected two container selections, got %d", len(selections))
		}
		issues := []warRoomIssue{selections[0].Issue, selections[1].Issue}
		enrichWarRoomAIHrefs(issues, scan, "prod", db)
		if issues[0].AIHref == issues[1].AIHref {
			t.Fatal("different containers share an AI link")
		}
		for _, issue := range issues {
			aiURL, _ := url.Parse(issue.AIHref)
			selected, err := findWarRoomAISelection(scan, "prod", db, aiURL.Query().Get("issue"))
			if err != nil || selected.Issue.Container != issue.Container {
				t.Fatalf("card selected wrong container: %+v, %v", selected, err)
			}
			for _, href := range []string{warRoomIssueURL(issue, "prod"), investigationEvidenceHref(issue, "prod", "warroom")} {
				evidenceURL, err := url.Parse(href)
				if err != nil {
					t.Fatal(err)
				}
				q := evidenceURL.Query()
				if q.Get("pod") != "api-0/"+issue.Container || q.Get("tab") != "" {
					t.Fatalf("Evidence URL lost container or changed default tab: %s", href)
				}
				tabs := srv.buildInvestigationTabs("prod", scan, q.Get("type"), q.Get("ns"), q.Get("pod"), "", "warroom")
				if !tabs.AIAvailable || tabs.AITabHref != issue.AIHref {
					t.Fatalf("Evidence-to-AI round trip selected wrong container: %+v", tabs)
				}
			}
		}
		for _, pod := range []string{"api-0", "api-0/missing"} {
			tabs := srv.buildInvestigationTabs("prod", scan, store.IssuePrivilegedContainer, "apps", pod, "", "")
			if tabs.AIAvailable || tabs.AITabHref != "" {
				t.Fatalf("ambiguous or nonexistent container exposed AI link: %+v", tabs)
			}
		}
		scan.secAudit.Issues = scan.secAudit.Issues[:1]
		tabs := srv.buildInvestigationTabs("prod", scan, store.IssuePrivilegedContainer, "apps", "api-0", "", "")
		if !tabs.AIAvailable {
			t.Fatal("unambiguous legacy URL should retain AI link")
		}
	}
}

func TestInvestigationAIDisabledDeepLink(t *testing.T) {
	for _, missing := range []string{"provider", "runtime", "cache"} {
		t.Run(missing, func(t *testing.T) {
			scan, db := warRoomAICrashFixture("prod", 7)
			provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
			srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
			selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
			switch missing {
			case "provider":
				srv.aiProvider = nil
			case "runtime":
				srv.aiRuntime = nil
			case "cache":
				srv.aiRuntime.cache = nil
			}
			srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
				t.Fatal("disabled AI must not read Kubernetes")
				return nil, errors.New("unexpected Kubernetes read")
			}
			request := httptest.NewRequest(http.MethodGet, investigationAIHref("prod", selector, "warroom"), nil)
			request.SetBasicAuth("operator", "test-password")
			recorder := httptest.NewRecorder()
			srv.newMux().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("disabled AI deep link returned %d: %s", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			for _, want := range []string{"NOT_CONFIGURED", "Configure AI analysis", "ai.enabled", "existing Kubernetes Secret"} {
				if !strings.Contains(body, want) {
					t.Errorf("disabled AI page missing %q", want)
				}
			}
			if strings.Contains(body, "id=\"ai-generate\"") {
				t.Fatal("disabled AI page exposed generation control")
			}
			data := srv.buildInvestigationAIPageData(scan, "prod", selector, "warroom")
			if !data.AIAvailable || data.AIConfigured || data.AITabHref == "" || data.CanGenerate {
				t.Fatalf("disabled AI builder state is unsafe: %+v", data.investigationTabs)
			}
			if provider.callCount() != 0 {
				t.Fatal("disabled AI invoked provider")
			}
		})
	}
}

func TestSettingsShowsSafeAIConfigurationStatus(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	srv.aiRuntime.providerName = "internal<script>"
	srv.aiRuntime.model = "granite\"model"

	request := httptest.NewRequest(http.MethodGet, "/settings?cluster=prod", nil)
	recorder := httptest.NewRecorder()
	srv.handleSettingsPage(recorder, request)
	body := recorder.Body.String()
	for _, want := range []string{"AI analysis", "Enabled", "internal&lt;script&gt;", "granite&#34;model", "sanitized evidence"} {
		if !strings.Contains(body, want) {
			t.Errorf("configured Settings page missing %q", want)
		}
	}
	for _, forbidden := range []string{"OPSCART_AI_API_KEY", "OPSCART_AI_BASE_URL", "existingSecret"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Settings page exposed deployment detail %q", forbidden)
		}
	}

	srv.aiProvider = nil
	recorder = httptest.NewRecorder()
	srv.handleSettingsPage(recorder, request)
	body = recorder.Body.String()
	for _, want := range []string{"Disabled", "ai.enabled=true", "existing Kubernetes Secret"} {
		if !strings.Contains(body, want) {
			t.Errorf("disabled Settings page missing %q", want)
		}
	}
}

// TestInvestigationAIRendersGeneratingState covers the GENERATING status: an
// in-flight generation (cache.begin, not yet finished) must render
// GENERATING with no generate/regenerate control, and rendering it must
// never itself invoke the provider.
func TestInvestigationAIRendersGeneratingState(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	if !srv.aiRuntime.cache.begin("prod", selector) {
		t.Fatal("setup: expected begin to succeed")
	}
	defer srv.aiRuntime.cache.finish("prod", selector)

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selector, nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, `class="ai-status generating"`) || !strings.Contains(body, ">GENERATING<") {
		t.Fatalf("expected a GENERATING status: %s", body)
	}
	if strings.Contains(body, `id="ai-generate"`) {
		t.Fatal("GENERATING state must not expose a generate/regenerate control")
	}
	if provider.callCount() != 0 {
		t.Fatal("rendering the GENERATING state invoked the provider")
	}
}

// TestInvestigationAIRendersCompleteGenerationEvidence proves every
// generation-time evidence item is rendered in "Evidence supplied by
// OpsCart" — none dropped or truncated — by counting rendered evidence rows
// against the cached entry's own Evidence slice and the model's own
// EvidenceUsed slice (rendered in the separate "Evidence cited by AI"
// section).
func TestInvestigationAIRendersCompleteGenerationEvidence(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selection := collectWarRoomAISelections(scan, "prod", db)[0]
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(capture.Request.Evidence) < 3 {
		t.Fatalf("setup: fixture produced too few evidence items to prove completeness: %d", len(capture.Request.Evidence))
	}
	response := testWarRoomAIResponse()
	srv.aiRuntime.cache.put(warRoomAICacheEntry{
		ClusterKey: "prod", Selector: selection.Selector, EvidenceHash: capture.Hash, BaseEvidenceHash: capture.Hash,
		RuntimeKey: srv.aiRuntime.key(), IssueIdentity: selection.Identity,
		Provider: "openai", Model: "synthetic-model",
		EvidenceCaptured: capture.CapturedAt, GeneratedAt: srv.aiRuntime.cache.now(),
		Response: *response, Evidence: capture.Request.Evidence,
	})

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selection.Selector, nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	body := rec.Body.String()

	got := strings.Count(body, `class="ai-ev-row"`)
	want := len(capture.Request.Evidence) + len(response.EvidenceUsed)
	if got != want {
		t.Fatalf("rendered %d evidence rows, want %d (evidence supplied %d + evidence cited %d): %s", got, want, len(capture.Request.Evidence), len(response.EvidenceUsed), body)
	}
}

// TestInvestigationAIDisclosuresAreNativeOnly proves the pod-details and
// evidence sections are plain native <details>/<summary> with no JS wiring:
// expanding them can never trigger a network call, since nothing listens
// for it. Combined with generating an analysis beforehand and resetting the
// provider's call counter, a zero count after the page GET also confirms
// page rendering itself never calls the provider.
func TestInvestigationAIDisclosuresAreNativeOnly(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	if rec := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true); rec.Code != http.StatusOK {
		t.Fatalf("generation status=%d body=%s", rec.Code, rec.Body.String())
	}
	provider.mu.Lock()
	provider.calls = 0 // reset after the setup generation above
	provider.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selector, nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	body := rec.Body.String()

	// base.html contributes its own earlier <script> block, so the LAST
	// <script>...</script> pair (this page's own) must be isolated, not
	// everything from the first <script> tag onward — that range would
	// wrongly include this page's own subsequent HTML (e.g. the literal
	// class name text "ai-pod-details").
	scriptStart := strings.LastIndex(body, "<script>")
	scriptEnd := strings.LastIndex(body, "</script>")
	if scriptStart == -1 || scriptEnd == -1 || scriptEnd < scriptStart {
		t.Fatal("expected a script block")
	}
	script := body[scriptStart:scriptEnd]
	for _, forbidden := range []string{"ai-pod-details", "ai-evidence-toggle", "ontoggle", "'toggle'"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script wires JS behavior to a disclosure element (%q) — expanding it must never trigger a request", forbidden)
		}
	}
	if provider.callCount() != 0 {
		t.Fatal("rendering the page invoked the provider")
	}
}

// TestInvestigationAICacheVersionBumpInvalidatesOldResults proves the
// warRoomAIContractVersion mechanism actually works: a cache entry produced
// under an older contract version — matching provider/model, and even
// matching the current evidence hash — must never be presented as
// GENERATED, only STALE, so tightened generation instructions can never be
// silently bypassed by a result produced under the old instructions.
func TestInvestigationAICacheVersionBumpInvalidatesOldResults(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selection := collectWarRoomAISelections(scan, "prod", db)[0]
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}

	const oldRuntimeKey = "openai\x00synthetic-model\x00warroom-ai-v2"
	if oldRuntimeKey == srv.aiRuntime.key() {
		t.Fatal("setup: old and current runtime keys must differ for this test to be meaningful")
	}
	srv.aiRuntime.cache.put(warRoomAICacheEntry{
		ClusterKey: "prod", Selector: selection.Selector, EvidenceHash: capture.Hash, BaseEvidenceHash: capture.Hash,
		RuntimeKey: oldRuntimeKey, IssueIdentity: selection.Identity,
		Provider: "openai", Model: "synthetic-model",
		EvidenceCaptured: capture.CapturedAt, GeneratedAt: srv.aiRuntime.cache.now(),
		Response: *testWarRoomAIResponse(), Evidence: capture.Request.Evidence,
	})

	page := srv.buildInvestigationAIPageData(scan, "prod", selection.Selector, "")
	if page.Status != "STALE" {
		t.Fatalf("status = %q, want STALE — a result generated under an old contract version must not be shown as current even though its evidence hash matches", page.Status)
	}
	if !page.CanGenerate || !page.Regenerate {
		t.Fatalf("expected regeneration to be offered: %+v", page)
	}
}
