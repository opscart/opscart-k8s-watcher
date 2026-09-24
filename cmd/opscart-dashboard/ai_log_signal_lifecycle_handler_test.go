package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestLifecyclePreviewAndRefineKeepFixtureTextLocal(t *testing.T) {
	lines := []string{
		"2026-09-15 12:00:00.000 INFO 1 --- [main] com.example.ExampleErrorSink : Server started on port 18080",
		"2026-09-15 12:00:00.100 INFO 1 --- [main] com.example.ErrorHandlerConfiguration : Started ExampleApplication in 2.5 seconds",
		"2026-09-15 12:00:00.200 DEBUG 1 --- [main] com.example.ExampleApplication : error-channel /readyz initialized",
		"2026-09-15 12:00:00.300 INFO 1 --- [main] com.example.ExampleApplication : ready to accept connections at https://service.example.invalid/api identifier=trace-7f42",
		"2026-09-15 12:00:00.400 WARN 1 --- [main] com.example.ExampleApplication : Graceful shutdown requested",
	}
	scan, db, pod := aiLogSignalsCrashFixture("prod", 4, true, false, 0)
	provider := &fakeWarRoomAIProvider{response: testWarRoomAIResponse()}
	srv, reader, clientset := newAILogSignalsTestServer("prod", scan, db, provider, pod)
	reader.body = []byte(strings.Join(lines, "\n") + "\n")
	selector := collectWarRoomAISelections(scan, "prod", db)[0].Selector

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	previewResponse := aiFormPost(t, srv.handleAILogSignalsPreview, "/api/investigation/ai/log-signals/preview?cluster=prod",
		url.Values{"issue": {selector}}, "http://example.com")
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d", previewResponse.Code)
	}
	preview := decodePreviewResponse(t, previewResponse)
	if !preview.CanRefine || preview.PreviewID == "" {
		t.Fatal("lifecycle preview must be refinable")
	}
	if countPodGetActions(clientset) != 1 || reader.callCount() != 1 || provider.callCount() != 0 {
		t.Fatal("preview call budget changed")
	}
	if len(preview.Signals) != 4 || preview.ActionableSignalCount != 4 {
		t.Fatalf("unexpected fixed categories: %+v", preview.Signals)
	}
	refineResponse := aiFormPost(t, srv.handleAIRefine, "/api/investigation/ai/refine?cluster=prod",
		refineForm(selector, preview.PreviewID), "http://example.com")
	if refineResponse.Code != http.StatusOK || provider.callCount() != 1 || countPodGetActions(clientset) != 1 || reader.callCount() != 1 {
		t.Fatalf("refine status=%d provider=%d pod_get=%d log_read=%d", refineResponse.Code, provider.callCount(), countPodGetActions(clientset), reader.callCount())
	}
	previewEntry, ok := srv.aiLogSignalsPreviewCache.get(preview.PreviewID)
	if !ok {
		t.Fatal("missing safe preview entry")
	}
	analysisEntry, ok := srv.aiRuntime.cache.latest("prod", selector)
	if !ok {
		t.Fatal("missing refined analysis entry")
	}
	materials := map[string]any{
		"provider request": provider.lastRequest(),
		"preview cache":    previewEntry,
		"analysis cache":   analysisEntry,
	}
	for name, value := range materials {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		assertNoLifecycleFixtureText(t, name, string(encoded), lines)
	}
	assertNoLifecycleFixtureText(t, "preview response", previewResponse.Body.String(), lines)
	assertNoLifecycleFixtureText(t, "refine response", refineResponse.Body.String(), lines)
	assertNoLifecycleFixtureText(t, "operational logs", logs.String(), lines)
	providerEvidence := provider.lastRequest().Evidence
	if len(providerEvidence) == 0 || providerEvidence[len(providerEvidence)-1].Type != "log_signals" {
		t.Fatalf("missing controlled provider evidence: %+v", providerEvidence)
	}
	details := providerEvidence[len(providerEvidence)-1].Details
	for _, category := range []string{
		"application_startup_complete=2", "server_startup=1",
		"graceful_shutdown=1", "severity_warning=1",
	} {
		if !strings.Contains(details, category) {
			t.Errorf("provider evidence missing fixed count %q", category)
		}
	}
	if strings.Contains(details, "unknown_error_marker") || strings.Contains(details, "severity_error") {
		t.Fatal("provider evidence included an unsupported error signal")
	}
}

func assertNoLifecycleFixtureText(t *testing.T, name, content string, lines []string) {
	t.Helper()
	for _, forbidden := range append(append([]string(nil), lines...),
		"ExampleErrorSink", "ErrorHandlerConfiguration", "ExampleApplication",
		"error-channel", "/readyz", "18080", "https://service.example.invalid/api", "trace-7f42") {
		if strings.Contains(content, forbidden) {
			t.Errorf("%s leaked fixture fragment %q", name, forbidden)
		}
	}
}
