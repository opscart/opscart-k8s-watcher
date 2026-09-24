package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// aiIntegrationEventEvidence builds a pod evidence index for pod carrying
// exactly one Warning Event of the given reason/count/observed time — used
// by TestInvestigationAIPageRemainsGeneratedAfterRefine to distinguish a
// volatile change (same reason, different count/age) from a meaningful one
// (a different reason set).
func aiIntegrationEventEvidence(pod *corev1.Pod, reason string, count int32, observedAt, capturedAt time.Time) *warRoomAIPodEvidenceIndex {
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: reason + "-event", Namespace: pod.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name},
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Count:          count,
		LastTimestamp:  metav1.NewTime(observedAt),
	}
	return buildWarRoomAIPodEvidenceIndex([]*corev1.Pod{pod}, []*corev1.Event{event}, true, true, capturedAt)
}

// TestInvestigationAIShowsInvestigateDeeperOnlyWhenEligible proves the
// Investigate-deeper control appears only for a current GENERATED analysis
// whose server-resolved container is known and restart-eligible, is never
// computed from a Kubernetes call, and disappears when log preview is
// disabled — without disturbing the underlying GENERATED status/hash.
func TestInvestigationAIShowsInvestigateDeeperOnlyWhenEligible(t *testing.T) {
	scan, db, _ := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	srv.logsEnabled = true
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		t.Fatal("rendering the AI tab must never call Kubernetes")
		return nil, nil
	}
	selection := collectWarRoomAISelections(scan, "prod", db)[0]
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	srv.aiRuntime.cache.put(warRoomAICacheEntry{
		ClusterKey: "prod", Selector: selection.Selector, EvidenceHash: capture.Hash, BaseEvidenceHash: capture.Hash,
		RuntimeKey: srv.aiRuntime.key(), IssueIdentity: selection.Identity,
		Provider: "openai", Model: "synthetic-model",
		EvidenceCaptured: capture.CapturedAt, GeneratedAt: srv.aiRuntime.cache.now(), Response: *testWarRoomAIResponse(),
		Evidence: capture.Request.Evidence,
	})

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selection.Selector, nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="ai-investigate-deeper"`) {
		t.Fatalf("expected the Investigate-deeper control for an eligible restarted container: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "GENERATED") {
		t.Fatal("expected the cached entry to still read as GENERATED")
	}

	srv.logsEnabled = false
	rec = httptest.NewRecorder()
	req.SetBasicAuth("operator", "test-password")
	srv.newMux().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `id="ai-investigate-deeper"`) {
		t.Fatal("Investigate-deeper control must not render when log preview is disabled")
	}
	if !strings.Contains(rec.Body.String(), "GENERATED") {
		t.Fatal("disabling log preview must not affect the underlying GENERATED status")
	}
}

// TestInvestigationAIPreviewDialogUsesAccurateContainerRoleWording proves
// the rendered page describes the log-signals target as "the selected
// failing container" — never "application container" — since rejecting
// istio-proxy proves nothing about what role the remaining container
// actually plays.
func TestInvestigationAIPreviewDialogUsesAccurateContainerRoleWording(t *testing.T) {
	scan, db, _ := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	srv.logsEnabled = true
	selection := collectWarRoomAISelections(scan, "prod", db)[0]
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	srv.aiRuntime.cache.put(warRoomAICacheEntry{
		ClusterKey: "prod", Selector: selection.Selector, EvidenceHash: capture.Hash, BaseEvidenceHash: capture.Hash,
		RuntimeKey: srv.aiRuntime.key(), IssueIdentity: selection.Identity,
		Provider: "openai", Model: "synthetic-model",
		EvidenceCaptured: capture.CapturedAt, GeneratedAt: srv.aiRuntime.cache.now(), Response: *testWarRoomAIResponse(),
		Evidence: capture.Request.Evidence,
	})

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selection.Selector, nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Previous logs from the selected failing container") {
		t.Fatalf("page missing accurate target-container wording: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "application container") {
		t.Fatalf("page must never claim an application-container role: %s", body)
	}
}

// TestInvestigationAIPreviewDialogStructuralFixes is a template-level
// assertion (not a browser test — see the task report for separate manual
// browser verification of centering, focus, and Escape behavior) proving
// the rendered markup and script contain the pieces those browser-observed
// fixes depend on: a viewport-fixed centered overlay (not native <dialog>
// auto-margins, which this page's global reset would cancel out), a
// detected-signals list the script actually populates, and unknown-marker-
// aware refine gating.
func TestInvestigationAIPreviewDialogStructuralFixes(t *testing.T) {
	scan, db, _ := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv := newWarRoomAITestServer([]string{"prod"}, map[string]*clusterScan{"prod": scan}, db, provider)
	srv.logsEnabled = true
	selection := collectWarRoomAISelections(scan, "prod", db)[0]
	capture, err := captureWarRoomAIEvidence(scan, "prod", db, selection)
	if err != nil {
		t.Fatal(err)
	}
	srv.aiRuntime.cache.put(warRoomAICacheEntry{
		ClusterKey: "prod", Selector: selection.Selector, EvidenceHash: capture.Hash, BaseEvidenceHash: capture.Hash,
		RuntimeKey: srv.aiRuntime.key(), IssueIdentity: selection.Identity,
		Provider: "openai", Model: "synthetic-model",
		EvidenceCaptured: capture.CapturedAt, GeneratedAt: srv.aiRuntime.cache.now(), Response: *testWarRoomAIResponse(),
		Evidence: capture.Request.Evidence,
	})

	req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selection.Selector, nil)
	req.SetBasicAuth("operator", "test-password")
	rec := httptest.NewRecorder()
	srv.newMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="ai-secondary-btn ai-investigate-btn" id="ai-investigate-deeper"`,
		`.ai-investigate-btn {`,
		`background: rgba(99, 102, 241, 0.12);`,
		`border-color: rgba(129, 140, 248, 0.55);`,
		`.ai-investigate-btn:hover {`,
		`background: rgba(99, 102, 241, 0.22);`,
		`.ai-investigate-btn:focus-visible {`,
		`outline: 2px solid var(--primary-light);`,
		`outline-offset: 2px;`,
		`class="ai-secondary-btn" id="ai-preview-cancel"`,
		`class="ai-secondary-btn" id="ai-preview-return"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing dedicated investigate styling %q", want)
		}
	}

	// Issue 1: viewport-fixed centered overlay, not native dialog margins.
	for _, want := range []string{
		".ai-preview-dialog{position:fixed",
		".ai-preview-dialog[open]{display:grid;place-items:center}",
		"ai-preview-panel",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing centered-overlay CSS %q: %s", want, body)
		}
	}

	// Issue 2: separate actionable/observed-only detected-signals lists the
	// script populates, distinct from the fixed six-line summary list, with
	// humanized labels rendered separately from their counts (never
	// concatenated, e.g. never "unknown_error_marker1").
	for _, want := range []string{
		`id="ai-preview-actionable-section"`,
		`id="ai-preview-actionable-list"`,
		`id="ai-preview-observed-section"`,
		`id="ai-preview-observed-list"`,
		"function humanizeCategory(category){",
		"name.textContent=humanizeCategory(signal.category)",
		"count.textContent=String(signal.count)",
		"li.appendChild(name)",
		"li.appendChild(count)",
		"function pluralizeSignalCount(n){",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing detected-signals list scaffolding %q: %s", want, body)
		}
	}

	// Explicit singular/plural wording: "N diagnostic signal category
	// detected" (singular) for n===1 and "N diagnostic signal categories
	// detected" (plural) otherwise — the ternary must key off n===1, and
	// the singular/plural noun forms must both be present and distinct.
	if !strings.Contains(body, "n===1?(n+' diagnostic signal category detected')") {
		t.Fatalf("missing exact singular wording keyed off n===1: %s", body)
	}
	if !strings.Contains(body, "(n+' diagnostic signal categories detected')") {
		t.Fatalf("missing exact plural wording: %s", body)
	}

	// The server's can_refine/actionable_signal_count are authoritative for
	// the refine-eligibility DECISION; the client only uses the category
	// name to decide which of the two display groups a signal belongs to.
	if strings.Contains(body, "if(!actionable)") || strings.Contains(body, "signals.some(isActionable)") {
		t.Fatal("client must not independently re-derive the actionable-category refine decision")
	}
	for _, want := range []string{
		"payload.can_refine",
		"No additional evidence was found in the previous container logs. The existing analysis was not changed, and no AI request was made.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing server-authoritative actionable gating %q: %s", want, body)
		}
	}

	// Required client state machine (issue 3 of the follow-up amendment):
	// explicit states, AbortController timeouts, safe reason->message
	// mapping, Return to War Room, and re-entrancy/duplicate-submission
	// guards keyed off the state itself.
	for _, want := range []string{
		"STATE_IDLE='idle'", "STATE_LOADING='loading_preview'",
		"STATE_ACTIONABLE='preview_actionable'",
		"STATE_REFINING='refining'", "STATE_ERROR='error'",
		"new AbortController()", "PREVIEW_TIMEOUT_MS=8000",
		"REFINE_TIMEOUT_MS=", "data-refine-timeout-ms",
		"REASON_MESSAGES=", "issue_not_active:", "analysis_context_expired:",
		"RETURN_TO_WARROOM_REASONS=",
		"if(state===STATE_LOADING||state===STATE_REFINING)return", // duplicate-submission guard
		"if(state!==STATE_ACTIONABLE||!previewID)return",          // refine re-entrancy guard
		"id=\"ai-preview-return\"", "Return to War Room",
		".finally(function(){",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing client state-machine scaffolding %q: %s", want, body)
		}
	}
	if strings.Index(body, "fetch(openBtn.dataset.previewEndpoint") < strings.Index(body, "openBtn.addEventListener('click'") {
		t.Fatal("preview request must be inside the explicit Investigate deeper click handler")
	}
	if strings.Count(body, "fetch(openBtn.dataset.previewEndpoint") != 1 ||
		!strings.Contains(body, "fetch(endpoint,{") || !strings.Contains(body, `id="ai-generate"`) ||
		!strings.Contains(body, `id="ai-investigate-deeper"`) {
		t.Fatal("Generate and Investigate deeper must remain separate actions and endpoints")
	}
	if strings.Index(body, "dialog.showModal()") < strings.Index(body, "if(payload.can_refine&&payload.preview_id)") {
		t.Fatal("dialog must open only for actionable previews")
	}
	for _, want := range []string{"generation!==previewGeneration", "generation===previewGeneration", "generation!==refineGeneration", "generation===refineGeneration", "var controller=hasAbort?new AbortController():null"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing request-generation guard %q", want)
		}
	}

	// Issue 1: Escape and Cancel both close through the same path, and
	// focus returns to Investigate deeper.
	for _, want := range []string{
		"dialog.addEventListener('close',function(){",
		"openBtn.focus()",
		"cancelBtn.addEventListener('click',function(){\n    abortInFlight();\n    dialog.close();\n  });",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing dialog close/focus wiring %q: %s", want, body)
		}
	}
}

// TestInvestigationAIPageRemainsGeneratedAfterRefine is the required
// integration test for issue 4: generate an initial analysis, preview
// meaningful log signals, refine successfully, then immediately render the
// AI page and confirm it reads as current (GENERATED, not STALE) with the
// refined disclosure visible and zero Kubernetes/provider calls during
// rendering. It then proves a purely volatile Kubernetes change (the same
// Warning Event reason, a much higher count, a different age) still leaves
// the page current, while a genuinely meaningful base-evidence change (a
// new event reason appearing) correctly marks it stale.
func TestInvestigationAIPageRemainsGeneratedAfterRefine(t *testing.T) {
	captureTime := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	scan.aiPodEvidence = aiIntegrationEventEvidence(pod, "BackOff", 3, captureTime.Add(-time.Hour), captureTime)

	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	reader.body = []byte("request to billing-service timed out\n")
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	// 1. Generate initial analysis.
	genRecorder := warRoomAIPost(t, srv.newMux(), "/api/warroom/ai-analysis?cluster=prod", selector, "http://example.com", true)
	if genRecorder.Code != http.StatusOK {
		t.Fatalf("initial generate status=%d body=%s", genRecorder.Code, genRecorder.Body.String())
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls after initial generate = %d, want 1", provider.callCount())
	}

	// 2. Preview meaningful log signals.
	previewID := requestPreview(t, srv, "prod", selector)

	// 3. Refine successfully.
	refineRecorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod", refineForm(selector, previewID), "http://example.com")
	if refineRecorder.Code != http.StatusOK {
		t.Fatalf("refine status=%d body=%s", refineRecorder.Code, refineRecorder.Body.String())
	}
	if provider.callCount() != 2 {
		t.Fatalf("provider calls after refine = %d, want 2", provider.callCount())
	}

	// 4. Immediately render the AI page — no Kubernetes call may occur.
	srv.kubeClientFor = func(string, *apiCounters) (kubernetes.Interface, error) {
		t.Fatal("rendering the AI tab must never call Kubernetes")
		return nil, nil
	}
	render := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/investigate?cluster=prod&tab=ai&issue="+selector, nil)
		req.SetBasicAuth("operator", "test-password")
		rec := httptest.NewRecorder()
		srv.newMux().ServeHTTP(rec, req)
		return rec
	}

	rec := render()
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="ai-status">GENERATED<`) {
		t.Fatalf("expected GENERATED immediately after refine: %s", body)
	}
	if strings.Contains(body, "STALE") {
		t.Fatalf("refined result must not immediately read as STALE: %s", body)
	}
	if !strings.Contains(body, warRoomAIRefinedDisclosure) {
		t.Fatalf("expected the refined disclosure to be visible: %s", body)
	}
	if strings.Count(body, `id="ai-message"`) != 1 || strings.Count(body, warRoomAIRefinedDisclosure) != 1 ||
		!strings.Contains(body, `id="ai-message"`+"\n      "+`class="ai-refined-note"`) {
		t.Fatal("refined GENERATED must render one message with the refined-note style")
	}
	if provider.callCount() != 2 {
		t.Fatalf("rendering the page must never call the provider, got %d calls", provider.callCount())
	}

	// 5. A volatile event-count increase (same reason, much higher count,
	// different age) still leaves the result current.
	scan.aiPodEvidence = aiIntegrationEventEvidence(pod, "BackOff", 50000, captureTime.Add(-6*time.Hour), captureTime)
	rec = render()
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, `id="ai-status">GENERATED<`) {
		t.Fatalf("a volatile event-count/age change must not make the result stale: %s", body)
	}
	if provider.callCount() != 2 {
		t.Fatalf("rendering the page must never call the provider, got %d calls", provider.callCount())
	}

	// 6. A meaningful base-evidence change (a new, different event reason
	// appearing) correctly marks the result stale.
	event2 := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "unhealthy-event", Namespace: pod.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name},
		Type:           corev1.EventTypeWarning,
		Reason:         "Unhealthy",
		Count:          1,
		LastTimestamp:  metav1.NewTime(captureTime.Add(-time.Minute)),
	}
	backoffEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "BackOff-event", Namespace: pod.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackOff",
		Count:          50000,
		LastTimestamp:  metav1.NewTime(captureTime.Add(-6 * time.Hour)),
	}
	scan.aiPodEvidence = buildWarRoomAIPodEvidenceIndex([]*corev1.Pod{pod}, []*corev1.Event{backoffEvent, event2}, true, true, captureTime)
	rec = render()
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, `id="ai-status">STALE<`) {
		t.Fatalf("a new event reason (meaningful base-evidence change) must mark the result stale: %s", body)
	}
	if strings.Count(body, `id="ai-message"`) != 1 || strings.Contains(body, warRoomAIRefinedDisclosure) ||
		!strings.Contains(body, `class="ai-message">`+"\n      New evidence is available.") {
		t.Fatal("STALE must render only its stale reason in the single message")
	}
	if provider.callCount() != 2 {
		t.Fatalf("rendering the page must never call the provider, got %d calls", provider.callCount())
	}
}
