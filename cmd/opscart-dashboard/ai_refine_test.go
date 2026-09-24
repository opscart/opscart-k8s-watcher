package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
	"k8s.io/client-go/kubernetes"
)

func TestHandleAIRefineRejectsChangedEpisodeAndEvidence(t *testing.T) {
	for _, scenario := range []string{"episode", "evidence", "expired_context"} {
		t.Run(scenario, func(t *testing.T) {
			scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
			provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
			srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
			selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
			previewID := requestPreview(t, srv, "prod", selector)
			switch scenario {
			case "episode":
				incident := db.incidents["prod"][0]
				incident.ReopenCount++
				db.incidents["prod"][0] = incident
			case "evidence":
				scan.aiPodEvidence = aiIntegrationEventEvidence(pod, "NewFailure", 1, time.Now(), time.Now())
			case "expired_context":
				srv.states["prod"].mu.Lock()
				srv.states["prod"].scan = &clusterScan{report: scan.report}
				srv.states["prod"].mu.Unlock()
				base := srv.aiRuntime.cache.now()
				srv.aiRuntime.cache.now = func() time.Time { return base.Add(warRoomAICacheTTL + time.Minute) }
			}
			result := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
			if result.Code == http.StatusOK || provider.callCount() != 0 {
				t.Fatalf("status=%d calls=%d", result.Code, provider.callCount())
			}
		})
	}
}

// requestPreview drives the real preview handler to obtain a genuine
// preview ID for use in refine tests, rather than constructing one by
// hand — this keeps the refine tests honest about what the preview
// endpoint actually returns.
func requestPreview(t *testing.T, srv *server, cluster, selector string) string {
	t.Helper()
	recorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster="+cluster,
		url.Values{"issue": {selector}}, "http://example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	return decodePreviewResponse(t, recorder).PreviewID
}

func refineForm(selector, previewID string) url.Values {
	return url.Values{"issue": {selector}, "preview_id": {previewID}}
}

func TestHandleAIRefineMakesZeroKubernetesCallsAndOneProviderCall(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)

	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		t.Fatal("refine must never call the Kubernetes client factory")
		return nil, nil
	}

	recorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("refine status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.callCount())
	}
}

func TestHandleAIRefineIncludesLogSignalsEvidenceInRequestAndHash(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	baseSelection, _ := findWarRoomAISelection(scan, "prod", db, selector)
	baseCapture, err := captureWarRoomAIEvidence(scan, "prod", db, baseSelection)
	if err != nil {
		t.Fatal(err)
	}

	previewID := requestPreview(t, srv, "prod", selector)
	recorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("refine status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	request := provider.lastRequest()
	foundLogSignals := false
	for _, item := range request.Evidence {
		if item.Type == aianalysis.EvidenceLogSignals {
			foundLogSignals = true
			if !containsAll(item.Details, "connection_refused=1", "source=previous_container", "container_role=target_container") {
				t.Fatalf("log_signals evidence details missing expected fields: %s", item.Details)
			}
			// Rejecting istio-proxy proves nothing about what the
			// remaining container actually runs — the provider request
			// must never claim an "application" role for it.
			if strings.Contains(item.Details, "application") {
				t.Fatalf("log_signals evidence must never claim an application-container role: %s", item.Details)
			}
		}
	}
	if !foundLogSignals {
		t.Fatalf("expected a log_signals evidence item in the refine request, got %+v", request.Evidence)
	}

	entry, ok := srv.aiRuntime.cache.latest("prod", selector)
	if !ok {
		t.Fatal("expected a cached entry after refine")
	}
	if !entry.Refined {
		t.Fatal("expected the cached entry to be marked Refined")
	}
	if entry.EvidenceHash == baseCapture.Hash {
		t.Fatal("refinement must change the evidence hash from the base (unrefined) capture")
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}

func TestHandleAIRefineRejectsExpiredPreview(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	recorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, "not-a-real-preview-id"), "http://example.com")
	if recorder.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 Gone; body=%s", recorder.Code, recorder.Body.String())
	}
	if provider.callCount() != 0 {
		t.Fatal("an expired/unknown preview must never reach the provider")
	}
	// A stale/unknown preview ID must carry the fixed preview_expired reason
	// code so the browser's state machine can restore itself through the
	// safe response contract (clear the preview ID, disable Refine, show an
	// inline error) rather than parsing the human-readable message.
	if !aiTestResponseHasReason(t, recorder, aiReasonPreviewExpired) {
		t.Fatalf("expected reason=%s, body=%s", aiReasonPreviewExpired, recorder.Body.String())
	}
}

func TestHandleAIRefineRejectsSelectorMismatch(t *testing.T) {
	scanA, dbA, podA := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scanA, dbA, provider, podA)
	selectorA := collectWarRoomAISelections(scanA, "prod", dbA)[0].Selector
	previewID := requestPreview(t, srv, "prod", selectorA)

	// A different (structurally valid but unrelated) selector must not be
	// able to redeem a preview captured for selectorA.
	otherSelector := warRoomAISelector("prod", "different-fingerprint", "other-pod", "app")
	recorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(otherSelector, previewID), "http://example.com")
	if recorder.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 Gone; body=%s", recorder.Code, recorder.Body.String())
	}
	if provider.callCount() != 0 {
		t.Fatal("a selector/preview mismatch must never reach the provider")
	}
}

func TestHandleAIRefineRejectsNoSignalsPreview(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	reader.body = []byte("application started\nready to serve traffic\n")
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)
	if previewID != "" || srv.aiLogSignalsPreviewCache.size() != 0 {
		t.Fatal("no-signal preview must not be cached")
	}

	recorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
	if recorder.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410; body=%s", recorder.Code, recorder.Body.String())
	}
	if provider.callCount() != 0 {
		t.Fatal("a no-signals preview must never reach the provider")
	}
}

// TestHandleAIRefineRejectsUnknownMarkerAlone proves unknown_error_marker
// is not sufficient new evidence for a paid refinement call: the preview
// still safely reports the local count, but refine is rejected and the
// provider is never called.
func TestHandleAIRefineRejectsUnknownMarkerAlone(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	reader.body = []byte("an unexpected error occurred during processing\n")
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	previewRecorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if previewRecorder.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", previewRecorder.Code, previewRecorder.Body.String())
	}
	preview := decodePreviewResponse(t, previewRecorder)
	if preview.PreviewID != "" || preview.CanRefine || srv.aiLogSignalsPreviewCache.size() != 0 {
		t.Fatal("unknown-only preview must not be cached or refinable")
	}
	if len(preview.Signals) != 1 || preview.Signals[0].Category != "unknown_error_marker" {
		t.Fatalf("expected the preview to safely report the local unknown_error_marker count, got %+v", preview.Signals)
	}

	recorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, preview.PreviewID), "http://example.com")
	if recorder.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410; body=%s", recorder.Code, recorder.Body.String())
	}
	if provider.callCount() != 0 {
		t.Fatal("unknown_error_marker alone must never reach the provider")
	}
}

// TestHandleAIRefineWithUnknownMarkerAndActionableSignalSendsOnlyActionable
// proves that when unknown_error_marker appears alongside a genuinely
// actionable category, refinement proceeds but the provider receives only
// the actionable category.
func TestHandleAIRefineWithUnknownMarkerAndActionableSignalSendsOnlyActionable(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	reader.body = []byte("an unexpected error occurred during processing\nrequest to billing-service timed out\n")
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)

	recorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("refine status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.callCount())
	}

	request := provider.lastRequest()
	found := false
	for _, item := range request.Evidence {
		if item.Type != aianalysis.EvidenceLogSignals {
			continue
		}
		found = true
		if !strings.Contains(item.Details, "dependency_timeout=1") {
			t.Fatalf("expected the actionable category in provider evidence: %s", item.Details)
		}
		if strings.Contains(item.Details, "unknown_error_marker") {
			t.Fatalf("unknown_error_marker must never appear in provider evidence, even alongside an actionable category: %s", item.Details)
		}
	}
	if !found {
		t.Fatal("expected a log_signals evidence item in the refine request")
	}
}

// TestHandleAIRefineCountBucketingStabilizesCacheIdentity proves that two
// previews whose exact category counts differ, but fall in the same fixed
// bucket, produce the SAME evidence hash — the frozen design's
// derived-signal hashing rule.
func TestHandleAIRefineCountBucketingStabilizesCacheIdentity(t *testing.T) {
	scan, db, _ := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	baseSelection, _ := findWarRoomAISelection(scan, "prod", db, collectWarRoomAISelections(scan, "prod", db)[0].Selector)
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, baseSelection)
	if err != nil {
		t.Fatal(err)
	}

	previewLow := logSignalsPreview{
		Source: aiLogSignalsSourcePreviousContainer, ContainerRole: aiLogSignalsContainerRoleTargetContainer,
		LinesRequested: investigationLogTailLines, ByteLimit: investigationLogMaxBytes,
		BytesReceived: 1000, Signals: []logSignalCount{{Category: "dependency_timeout", Count: 7}},
	}
	previewHigh := previewLow
	previewHigh.Signals = []logSignalCount{{Category: "dependency_timeout", Count: 19}}
	previewHigh.BytesReceived = 1200

	refinedLow, err := appendLogSignalsEvidence(capture, previewLow)
	if err != nil {
		t.Fatal(err)
	}
	refinedHigh, err := appendLogSignalsEvidence(capture, previewHigh)
	if err != nil {
		t.Fatal(err)
	}
	if refinedLow.Hash != refinedHigh.Hash {
		t.Fatalf("expected bucketed hash to stay stable across 7 vs 19 (same 6-20 bucket): %s vs %s", refinedLow.Hash, refinedHigh.Hash)
	}
	// Exact display counts must still differ.
	if refinedLow.Request.Evidence[len(refinedLow.Request.Evidence)-1].Details == refinedHigh.Request.Evidence[len(refinedHigh.Request.Evidence)-1].Details {
		t.Fatal("expected the exact (non-hashed) evidence details to differ between 7 and 19")
	}

	previewCrossedBucket := previewLow
	previewCrossedBucket.Signals = []logSignalCount{{Category: "dependency_timeout", Count: 25}}
	refinedCrossed, err := appendLogSignalsEvidence(capture, previewCrossedBucket)
	if err != nil {
		t.Fatal(err)
	}
	if refinedCrossed.Hash == refinedLow.Hash {
		t.Fatal("expected a bucket-crossing count change (7 -> 25) to change the hash")
	}
}

func TestHandleAIRefineBrowserRetryOfSameEvidenceHashDoesNotCallProviderAgain(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)

	first := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
	if first.Code != http.StatusOK {
		t.Fatalf("first refine status = %d, body=%s", first.Code, first.Body.String())
	}
	// Bypass the operation cooldown to isolate the evidence-hash dedup
	// behavior under test (the cooldown is covered separately).
	srv.aiOperationCooldown = newAIOperationCooldown(0, srv.aiRuntime.cache.now)

	second := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
	if second.Code != http.StatusOK {
		t.Fatalf("retried refine status = %d, body=%s", second.Code, second.Body.String())
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (retry of the same evidence hash must not call again)", provider.callCount())
	}
}

func TestHandleAIRefineZeroKubernetesCallsEvenOnCacheHit(t *testing.T) {
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, _, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	previewID := requestPreview(t, srv, "prod", selector)

	kubeCalls := 0
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		kubeCalls++
		return nil, context.DeadlineExceeded
	}
	recorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
	if recorder.Code != http.StatusOK {
		t.Fatalf("refine status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if kubeCalls != 0 {
		t.Fatalf("kubeClientFor invoked %d times during refine, want 0", kubeCalls)
	}
}

// TestExistingInitialAnalysisFlowUnchangedByRefineFeature proves the plain
// (non-refined) generate flow still behaves exactly as before: unaffected
// by the log-signals feature's new cache field, contract version bump, or
// routes.
func TestExistingInitialAnalysisFlowUnchangedByRefineFeature(t *testing.T) {
	scan, db := warRoomAICrashFixture("prod", 7)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	recorder := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	if recorder.Code != http.StatusOK || provider.callCount() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, provider.callCount(), recorder.Body.String())
	}
	entry, ok := srv.aiRuntime.cache.latest("prod", selector)
	if !ok {
		t.Fatal("expected a cached entry")
	}
	if entry.Refined {
		t.Fatal("a plain generation must never be marked Refined")
	}
}
