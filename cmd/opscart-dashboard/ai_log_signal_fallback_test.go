package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// aiTestResponseHasReason decodes recorder's body as a warRoomAIAPIResponse
// and reports whether its Reason field matches want.
func aiTestResponseHasReason(t *testing.T, recorder *httptest.ResponseRecorder, want string) bool {
	t.Helper()
	var response warRoomAIAPIResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode error response: %v (%s)", err, recorder.Body.String())
	}
	return response.Reason == want
}

// aiLogSignalsSeedGeneratedEntry seeds srv's AI cache with a GENERATED
// entry carrying the Resolved* fallback fields, exactly as
// handleWarRoomAIAnalysis would after a real initial generation — used to
// simulate "an analysis was already generated while the issue was active"
// without needing to drive the full generate endpoint in every test.
func aiLogSignalsSeedGeneratedEntry(t *testing.T, srv *server, cluster, selector string, scan *clusterScan, db warRoomAITestStore) {
	t.Helper()
	selection, err := findWarRoomAISelection(scan, cluster, db, selector)
	if err != nil {
		t.Fatalf("setup: findWarRoomAISelection: %v", err)
	}
	capture, err := captureWarRoomAIEvidence(scan, cluster, db, selection)
	if err != nil {
		t.Fatalf("setup: captureWarRoomAIEvidence: %v", err)
	}
	namespace, podName, containerName, issueType := aiLogSignalsResolvedTarget(scan, cluster, db, selector)
	if namespace == "" || podName == "" || containerName == "" {
		t.Fatalf("setup: expected a resolvable log-signals target for %q", selector)
	}
	srv.aiRuntime.cache.put(warRoomAICacheEntry{
		ClusterKey: cluster, Selector: selector, EvidenceHash: capture.Hash, BaseEvidenceHash: capture.Hash,
		RuntimeKey: srv.aiRuntime.key(), IssueIdentity: selection.Identity,
		Provider: "openai", Model: "synthetic-model",
		EvidenceCaptured: capture.CapturedAt, GeneratedAt: srv.aiRuntime.cache.now(), Response: *testWarRoomAIResponse(),
		Evidence:              capture.Request.Evidence,
		BaseCapture:           capture,
		ResolvedNamespace:     namespace,
		ResolvedPodName:       podName,
		ResolvedContainerName: containerName,
		ResolvedIssueType:     issueType,
	})
}

// TestHandleAILogSignalsPreviewFallsBackToCachedContextWhenSnapshotMissing
// is required scenario A: the issue transiently disappears from the
// current War Room snapshot (a realistic scan-cycle gap for a
// crash-looping pod), the cached GENERATED-analysis context is still
// valid, and the bound pod/container are still live with restarts — the
// preview must still succeed, using exactly one Pods.Get and one GetLogs
// call for the ORIGINAL resolved target, never a browser-supplied
// substitute.
func TestHandleAILogSignalsPreviewFallsBackToCachedContextWhenSnapshotMissing(t *testing.T) {
	fullScan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	selector := collectWarRoomAISelections(fullScan, "prod", db)[0].Selector

	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, clientset := newAILogSignalsTestServer("prod", fullScan, db, provider, pod)
	aiLogSignalsSeedGeneratedEntry(t, srv, "prod", selector, fullScan, db)

	// The current scan transiently lacks the issue entirely (e.g. a scan
	// cycle that didn't re-observe the StalePod), while the incident
	// itself remains tracked as active in db and the pod is still live.
	transientScan := &clusterScan{report: fullScan.report}
	srv.states["prod"].mu.Lock()
	srv.states["prod"].scan = transientScan
	srv.states["prod"].mu.Unlock()

	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if got := countPodGetActions(clientset); got != 1 {
		t.Fatalf("Pods.Get calls = %d, want 1", got)
	}
	if reader.callCount() != 1 {
		t.Fatalf("Pods.GetLogs calls = %d, want 1", reader.callCount())
	}
	if provider.callCount() != 0 {
		t.Fatal("preview must never call the AI provider")
	}
	response := decodePreviewResponse(t, recorder)
	if response.PreviewID == "" {
		t.Fatal("expected a preview ID from the fallback-resolved preview")
	}
}

func TestCachedContextPreviewRefineLifecycle(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, clientset := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	generated := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	if generated.Code != http.StatusOK || provider.callCount() != 1 {
		t.Fatalf("generate status=%d calls=%d", generated.Code, provider.callCount())
	}
	srv.states["prod"].mu.Lock()
	srv.states["prod"].scan = &clusterScan{report: scan.report}
	srv.states["prod"].mu.Unlock()
	previewID := requestPreview(t, srv, "prod", selector)
	if previewID == "" || countPodGetActions(clientset) != 1 || reader.callCount() != 1 || provider.callCount() != 1 {
		t.Fatalf("preview id=%q pod gets=%d log reads=%d provider=%d", previewID, countPodGetActions(clientset), reader.callCount(), provider.callCount())
	}
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		t.Fatal("refine must make zero Kubernetes calls")
		return nil, nil
	}
	refined := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
	if refined.Code != http.StatusOK || provider.callCount() != 2 {
		t.Fatalf("refine status=%d calls=%d body=%s", refined.Code, provider.callCount(), refined.Body.String())
	}
	entry, ok := srv.aiRuntime.cache.latest("prod", selector)
	if !ok || !entry.Refined {
		t.Fatal("refined result not cached")
	}
	page := srv.buildInvestigationAIPageData(srv.states["prod"].scan, "prod", selector, "")
	if page.Status != "GENERATED" || !page.Refined {
		t.Fatalf("page status=%q refined=%t", page.Status, page.Refined)
	}
}

func TestPreviewIDEntropyFailureIsSafe(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	srv.aiLogSignalsPreviewCache.random = failingPreviewRandom{}
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	response := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod", url.Values{"issue": {selector}}, "http://example.com")
	if response.Code != http.StatusInternalServerError || srv.aiLogSignalsPreviewCache.size() != 0 || provider.callCount() != 0 {
		t.Fatalf("status=%d size=%d calls=%d", response.Code, srv.aiLogSignalsPreviewCache.size(), provider.callCount())
	}
	if strings.Contains(response.Body.String(), "entropy") {
		t.Fatal("entropy error escaped")
	}
}

// TestHandleAILogSignalsPreviewExpiredContextRejected is required scenario
// B: the issue is absent from the current scan AND the cached context has
// aged past the AI cache's own TTL — the preview must reject with
// analysis_context_expired, making zero Kubernetes and zero provider
// calls.
func TestHandleAILogSignalsPreviewExpiredContextRejected(t *testing.T) {
	fullScan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	selector := collectWarRoomAISelections(fullScan, "prod", db)[0].Selector

	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, clientset := newAILogSignalsTestServer("prod", fullScan, db, provider, pod)
	aiLogSignalsSeedGeneratedEntry(t, srv, "prod", selector, fullScan, db)

	transientScan := &clusterScan{report: fullScan.report}
	srv.states["prod"].mu.Lock()
	srv.states["prod"].scan = transientScan
	srv.states["prod"].mu.Unlock()

	// Advance the cache's own clock past its TTL so the seeded entry is now
	// indistinguishable from one that never existed.
	base := srv.aiRuntime.cache.now()
	srv.aiRuntime.cache.now = func() time.Time { return base.Add(warRoomAICacheTTL + time.Minute) }

	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if recorder.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 Gone; body=%s", recorder.Code, recorder.Body.String())
	}
	if !aiTestResponseHasReason(t, recorder, aiReasonAnalysisContextExpired) {
		t.Fatalf("expected reason=%s, body=%s", aiReasonAnalysisContextExpired, recorder.Body.String())
	}
	if got := countPodGetActions(clientset); got != 0 {
		t.Fatalf("Pods.Get calls = %d, want 0", got)
	}
	if reader.callCount() != 0 {
		t.Fatalf("Pods.GetLogs calls = %d, want 0", reader.callCount())
	}
	if provider.callCount() != 0 {
		t.Fatal("an expired context must never reach the provider")
	}
}

// TestHandleAILogSignalsPreviewDeletedPodReturnsTargetUnavailable is
// required scenario C: the cached context is valid, but the live pod no
// longer exists — the preview must return target_unavailable and make
// zero provider calls.
func TestHandleAILogSignalsPreviewDeletedPodReturnsTargetUnavailable(t *testing.T) {
	fullScan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	selector := collectWarRoomAISelections(fullScan, "prod", db)[0].Selector

	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	// No pods registered in the fake clientset — the bound pod has been
	// deleted from the live cluster.
	srv, _, _ := newAILogSignalsTestServer("prod", fullScan, db, provider)
	aiLogSignalsSeedGeneratedEntry(t, srv, "prod", selector, fullScan, db)

	transientScan := &clusterScan{report: fullScan.report}
	srv.states["prod"].mu.Lock()
	srv.states["prod"].scan = transientScan
	srv.states["prod"].mu.Unlock()

	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", recorder.Code, recorder.Body.String())
	}
	if !aiTestResponseHasReason(t, recorder, aiReasonTargetUnavailable) {
		t.Fatalf("expected reason=%s, body=%s", aiReasonTargetUnavailable, recorder.Body.String())
	}
	if provider.callCount() != 0 {
		t.Fatal("a deleted pod must never reach the provider")
	}
	refine := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, ""), "http://example.com")
	if refine.Code != http.StatusGone || provider.callCount() != 0 {
		t.Fatalf("deleted pod must not yield a refinable preview: status=%d calls=%d", refine.Code, provider.callCount())
	}
	_ = pod // fixture-constructed pod deliberately not registered with the fake clientset
}

// TestHandleAILogSignalsPreviewFallbackRejectsArbitraryContainerInput is
// required scenario D: even while resolving via the cached-context
// fallback, the browser cannot smuggle a pod/container selection — no
// unauthorized GetLogs call results.
func TestHandleAILogSignalsPreviewFallbackRejectsArbitraryContainerInput(t *testing.T) {
	fullScan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	selector := collectWarRoomAISelections(fullScan, "prod", db)[0].Selector

	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, _ := newAILogSignalsTestServer("prod", fullScan, db, provider, pod)
	aiLogSignalsSeedGeneratedEntry(t, srv, "prod", selector, fullScan, db)

	transientScan := &clusterScan{report: fullScan.report}
	srv.states["prod"].mu.Lock()
	srv.states["prod"].scan = transientScan
	srv.states["prod"].mu.Unlock()

	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}, "container": {"istio-proxy"}, "pod": {"someone-elses-pod"}}, "http://example.com")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if reader.callCount() != 0 {
		t.Fatal("a smuggled container field must never result in a GetLogs call")
	}
}

// TestResolveAILogSignalsSelectorResolutionIsConsistentAcrossRepeatedCalls
// is a regression test proving no incidental state mutation causes a
// selector that resolves once to fail on a later, otherwise-identical
// call against the same unchanged scan — investigated per the report that
// a later preview unexpectedly saw "selected issue is not active".
// Neither the preview handler nor the resolver mutate scan/db, so this is
// expected to always pass; it exists to pin that invariant.
func TestResolveAILogSignalsSelectorResolutionIsConsistentAcrossRepeatedCalls(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	first := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if first.Code != http.StatusOK {
		t.Fatalf("first preview status = %d, body=%s", first.Code, first.Body.String())
	}
	srv.aiOperationCooldown = newAIOperationCooldown(0, srv.aiRuntime.cache.now) // isolate resolution from the cooldown, covered separately

	second := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if second.Code != http.StatusOK {
		t.Fatalf("second preview status = %d, body=%s (selector resolution must remain consistent against an unchanged scan)", second.Code, second.Body.String())
	}
	if reader.callCount() != 2 {
		t.Fatalf("GetLogs calls = %d, want 2 (one per successful preview)", reader.callCount())
	}
}

var _ = corev1.Pod{}
var _ = store.IssueCrashLoop
