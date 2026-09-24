package main

import (
	"bufio"
	"bytes"
	"regexp"
)

const (
	aiLogSignalsSourcePreviousContainer      = "previous_container"
	aiLogSignalsContainerRoleTargetContainer = "target_container"
)

// logSignalCategory is one of the fixed, OpsCart-controlled diagnostic
// signal categories. This vocabulary is exhaustive and frozen: the
// classifier below must never emit a category outside this list, and
// nothing outside this list may be added without revisiting the frozen
// design this file implements.
type logSignalCategory string

const (
	logSignalPanic                 logSignalCategory = "panic"
	logSignalFatalError            logSignalCategory = "fatal_error"
	logSignalUncaughtException     logSignalCategory = "uncaught_exception"
	logSignalDependencyTimeout     logSignalCategory = "dependency_timeout"
	logSignalDNSFailure            logSignalCategory = "dns_failure"
	logSignalConnectionRefused     logSignalCategory = "connection_refused"
	logSignalConnectionReset       logSignalCategory = "connection_reset"
	logSignalTLSFailure            logSignalCategory = "tls_failure"
	logSignalAuthenticationFailure logSignalCategory = "authentication_failure"
	logSignalAuthorizationFailure  logSignalCategory = "authorization_failure"
	logSignalConfigurationError    logSignalCategory = "configuration_error"
	logSignalOutOfMemorySignal     logSignalCategory = "out_of_memory_signal"
	logSignalDiskFull              logSignalCategory = "disk_full"
	logSignalProcessTermination    logSignalCategory = "process_termination"
	logSignalApplicationStartup    logSignalCategory = "application_startup_complete"
	logSignalServerStartup         logSignalCategory = "server_startup"
	logSignalGracefulShutdown      logSignalCategory = "graceful_shutdown"
	logSignalSeverityError         logSignalCategory = "severity_error"
	logSignalSeverityWarning       logSignalCategory = "severity_warning"
	logSignalUnknownErrorMarker    logSignalCategory = "unknown_error_marker"
)

// logSignalCategoryOrder is the fixed, deterministic output order for every
// safe preview and evidence representation this file produces.
var logSignalCategoryOrder = []logSignalCategory{
	logSignalPanic, logSignalFatalError, logSignalUncaughtException,
	logSignalDependencyTimeout, logSignalDNSFailure, logSignalConnectionRefused,
	logSignalConnectionReset, logSignalTLSFailure, logSignalAuthenticationFailure,
	logSignalAuthorizationFailure, logSignalConfigurationError, logSignalOutOfMemorySignal,
	logSignalDiskFull, logSignalProcessTermination,
	logSignalApplicationStartup, logSignalServerStartup, logSignalGracefulShutdown,
	logSignalSeverityError, logSignalSeverityWarning, logSignalUnknownErrorMarker,
}

// logSignalKeywords is the fixed, lowercase keyword vocabulary a line is
// tested against. A line may match more than one category. This is a
// deliberately simple substring classifier, not a log-parsing framework:
// it exists to produce fixed integer counters, never to extract or
// reproduce any part of the matched line.
var logSignalKeywords = map[logSignalCategory][]string{
	logSignalPanic:                 {"panic"},
	logSignalFatalError:            {"fatal error", "fatal:"},
	logSignalUncaughtException:     {"uncaught exception", "unhandled exception"},
	logSignalDependencyTimeout:     {"timeout", "timed out"},
	logSignalDNSFailure:            {"no such host", "dns resolution failed", "nxdomain", "could not resolve host"},
	logSignalConnectionRefused:     {"connection refused"},
	logSignalConnectionReset:       {"connection reset"},
	logSignalTLSFailure:            {"tls handshake", "ssl handshake", "certificate verify failed", "x509:"},
	logSignalAuthenticationFailure: {"authentication failed", "invalid credentials", "401 unauthorized", "login failed"},
	logSignalAuthorizationFailure:  {"permission denied", "403 forbidden", "access denied", "not authorized"},
	logSignalConfigurationError:    {"configuration error", "invalid configuration", "missing required config", "config validation failed"},
	logSignalOutOfMemorySignal:     {"out of memory", "cannot allocate memory", "oomkilled", "oom-killer"},
	logSignalDiskFull:              {"no space left on device", "disk full", "disk quota exceeded"},
	logSignalProcessTermination:    {"sigkill", "sigterm", "signal: killed", "signal: terminated"},
}

// A generic marker must be a standalone message word. This deliberately
// excludes class, bean, method, and channel names such as ExampleErrorSink
// and error-channel.
var logSignalGenericErrorMarker = regexp.MustCompile(`(?:^|[\s:([{])(?:error|exception|failure|failed)(?:$|[\s:,;!?)}\]])`)

// classifyLogSignals inspects data locally, line by line, and returns only
// fixed category counters — never a line, a captured substring, or any
// other value derived from the content itself. The caller must discard
// data immediately after this call returns (see handleAILogSignalsPreview,
// ai_log_signal_preview.go).
func classifyLogSignals(data []byte) map[logSignalCategory]int {
	counts := make(map[logSignalCategory]int, len(logSignalCategoryOrder))
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	for scanner.Scan() {
		line := bytes.ToLower(scanner.Bytes())
		matched := false
		for _, category := range classifyLogLifecycle(line) {
			incrementLogSignal(counts, category)
			matched = true
		}
		switch structuredLogSeverity(line) {
		case logSeverityError:
			incrementLogSignal(counts, logSignalSeverityError)
			matched = true
		case logSeverityWarning:
			incrementLogSignal(counts, logSignalSeverityWarning)
			matched = true
		}
		for category, keywords := range logSignalKeywords {
			for _, keyword := range keywords {
				if bytes.Contains(line, []byte(keyword)) {
					incrementLogSignal(counts, category)
					matched = true
					break
				}
			}
		}
		if matched {
			continue
		}
		if logSignalGenericErrorMarker.Match(line) {
			incrementLogSignal(counts, logSignalUnknownErrorMarker)
		}
	}
	return counts
}

func incrementLogSignal(counts map[logSignalCategory]int, category logSignalCategory) {
	if counts[category] < int(investigationLogTailLines) {
		counts[category]++
	}
}

// logSignalCountBucket maps an exact, potentially volatile count to one of
// a small number of fixed buckets, per the frozen design's derived-signal
// hashing rule. Exact counts remain safe to show in a preview; only the
// stable cache/hash identity uses buckets.
func logSignalCountBucket(count int) string {
	switch {
	case count <= 0:
		return "0"
	case count == 1:
		return "1"
	case count <= 5:
		return "2-5"
	case count <= 20:
		return "6-20"
	case count <= 100:
		return "21-100"
	default:
		return ">100"
	}
}

// logSignalCount is one non-zero category's exact count, for the operator
// preview only.
type logSignalCount struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

// logSignalsPreview is the entire safe, exact-preview structure: every
// field is a fixed label or an integer. It is constructible only from
// fixed constants and the classifier's integer counters — see this file's
// package doc and the frozen design's privacy boundary.
type logSignalsPreview struct {
	Source           string           `json:"source"`
	ContainerRole    string           `json:"container_role"`
	LinesRequested   int64            `json:"lines_requested"`
	ByteLimit        int64            `json:"byte_limit"`
	BytesReceived    int              `json:"bytes_received"`
	RawLinesIncluded int              `json:"raw_lines_included"`
	Signals          []logSignalCount `json:"signals"`
}

func buildLogSignalsPreview(counts map[logSignalCategory]int, bytesReceived int) logSignalsPreview {
	preview := logSignalsPreview{
		Source:           aiLogSignalsSourcePreviousContainer,
		ContainerRole:    aiLogSignalsContainerRoleTargetContainer,
		LinesRequested:   investigationLogTailLines,
		ByteLimit:        investigationLogMaxBytes,
		BytesReceived:    bytesReceived,
		RawLinesIncluded: 0,
		Signals:          make([]logSignalCount, 0, len(logSignalCategoryOrder)),
	}
	for _, category := range logSignalCategoryOrder {
		if count := counts[category]; count > 0 {
			preview.Signals = append(preview.Signals, logSignalCount{Category: string(category), Count: count})
		}
	}
	return preview
}

// logSignalIsActionable reports whether category is specific enough to
// justify a paid refinement call and to be sent to the AI provider as new
// evidence. unknown_error_marker is the one exception: it is a generic
// catch-all for lines that merely look error-like, not a diagnosis — the
// preview may still safely report its local count, but it must never, by
// itself, drive a refinement or appear in provider evidence.
func logSignalIsActionable(category string) bool {
	return category != string(logSignalUnknownErrorMarker)
}

// hasActionableLogSignal reports whether signals contains at least one
// category other than unknown_error_marker.
func hasActionableLogSignal(signals []logSignalCount) bool {
	return actionableLogSignalCount(signals) > 0
}

// actionableLogSignalCount counts how many detected categories are
// actionable (i.e. not unknown_error_marker). The server computes this
// once and publishes it in the preview response (actionable_signal_count,
// can_refine) as the sole authoritative source — the browser must never
// duplicate this policy by re-deriving it from the category name itself.
func actionableLogSignalCount(signals []logSignalCount) int {
	count := 0
	for _, signal := range signals {
		if logSignalIsActionable(signal.Category) {
			count++
		}
	}
	return count
}
