package main

import (
	"strings"
	"testing"
)

func TestStructuredLogSeverityIgnoresNamesInInfoAndDebugLines(t *testing.T) {
	lines := []string{
		"2026-09-15 12:00:00.000 INFO 1 --- [main] com.example.ExampleErrorSink : channel error-channel registered",
		"2026-09-15 12:00:00.001 DEBUG 1 --- [main] com.example.ErrorHandlerConfiguration : bean ExampleErrorSink configured",
		"level=INFO logger=com.example.ErrorHandlerConfiguration method=handleError channel=error-channel message=initialized",
		`{"level":"DEBUG","logger":"com.example.ExampleErrorSink","channel":"error-channel"}`,
		`{"level":"INFO","message":"literal level=ERROR in a bean description"}`,
		`level=DEBUG logger=ExampleErrorSink message="literal level=ERROR in a method description"`,
	}
	counts := classifyLogSignals([]byte(strings.Join(lines, "\n") + "\n"))
	if counts[logSignalUnknownErrorMarker] != 0 || counts[logSignalSeverityError] != 0 || counts[logSignalSeverityWarning] != 0 {
		t.Fatalf("benign names became error signals: %v", counts)
	}
}

func TestStructuredLogSeveritiesAndLifecycleCategories(t *testing.T) {
	lines := []string{
		"2026-09-15 12:00:00.000 INFO 1 --- [main] com.example.ServerLogger : Server started on port 18080",
		"2026-09-15 12:00:00.100 INFO 1 --- [main] com.example.ExampleApplication : Started ExampleApplication in 2.5 seconds",
		"2026-09-15 12:00:00.200 INFO 1 --- [main] com.example.ExampleApplication : ready to accept connections",
		"2026-09-15 12:00:00.300 WARN 1 --- [main] com.example.ExampleApplication : Graceful shutdown requested",
		`{"severity":"ERROR","message":"processing stopped"}`,
		"level=warning logger=ExampleLogger message=retrying",
	}
	counts := classifyLogSignals([]byte(strings.Join(lines, "\n") + "\n"))
	for category, want := range map[logSignalCategory]int{
		logSignalServerStartup: 1, logSignalApplicationStartup: 2,
		logSignalGracefulShutdown: 1, logSignalSeverityError: 1,
		logSignalSeverityWarning: 2,
	} {
		if got := counts[category]; got != want {
			t.Errorf("%s=%d, want %d", category, got, want)
		}
	}
	if counts[logSignalUnknownErrorMarker] != 0 {
		t.Fatalf("unexpected unknown marker: %v", counts)
	}
	preview := buildLogSignalsPreview(counts, 100)
	if !hasActionableLogSignal(preview.Signals) {
		t.Fatal("lifecycle signals must be eligible for refinement")
	}
}

func TestLogSignalCountsAreBounded(t *testing.T) {
	data := strings.Repeat("2026-09-15T12:00:00Z ERROR request failed\n", 250)
	counts := classifyLogSignals([]byte(data))
	if counts[logSignalSeverityError] != int(investigationLogTailLines) {
		t.Fatalf("severity count=%d, want cap %d", counts[logSignalSeverityError], investigationLogTailLines)
	}
}
