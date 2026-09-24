package main

import (
	"bytes"
	"errors"
	"log"
	"net/url"
	"strings"
	"testing"
)

// TestAILogSignalsAndRefineLoggingIsSafeUnderAdversarialInput drives a full
// preview-then-refine cycle with deliberately adversarial content in every
// place raw content could leak from — the container's previous-log bytes
// and the AI provider's failure text — and asserts that the resulting log
// output contains the required lifecycle lines (operation/reason only)
// while never containing any raw fragment, the opaque preview ID, the
// namespace, or the pod name. See ai_log_signal_preview.go and
// ai_refine.go's log.Printf call sites for the safe-logging convention
// this pins.
func TestAILogSignalsAndRefineLoggingIsSafeUnderAdversarialInput(t *testing.T) {
	const adversarialLogFragment = "sekret-log-fragment-do-not-log-zzz"
	const adversarialProviderBody = "adversarial-provider-body-marker-should-never-log"

	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector
	provider := &fakeWarRoomAIProvider{err: errors.New(adversarialProviderBody)}
	srv, reader, _ := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	reader.body = []byte("2026-09-15T00:00:00Z connection refused: " + adversarialLogFragment + "\n")

	var buf bytes.Buffer
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	}()

	previewRecorder := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if previewRecorder.Code != 200 {
		t.Fatalf("preview status = %d, body=%s", previewRecorder.Code, previewRecorder.Body.String())
	}
	previewID := decodePreviewResponse(t, previewRecorder).PreviewID

	refineRecorder := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod",
		refineForm(selector, previewID), "http://example.com")
	if refineRecorder.Code != 502 {
		t.Fatalf("refine status = %d, want 502; body=%s", refineRecorder.Code, refineRecorder.Body.String())
	}

	output := buf.String()

	for _, want := range []string{
		"ai log signals: resolved source=current_snapshot",
		"ai log signals: completed categories=1 actionable=1 bytes=",
		"ai refinement: started",
		"ai refinement: failed reason=provider_error",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("log output missing required lifecycle line %q; full output:\n%s", want, output)
		}
	}

	for _, forbidden := range []string{
		adversarialLogFragment,
		adversarialProviderBody,
		previewID,
		selector,
		"payments-0",
		"payments/",
	} {
		if strings.Contains(output, forbidden) {
			t.Errorf("log output must never contain %q; full output:\n%s", forbidden, output)
		}
	}
}
