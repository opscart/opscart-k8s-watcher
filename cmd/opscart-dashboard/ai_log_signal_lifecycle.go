package main

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// These expressions only decide which fixed counter to increment. Captured
// text is never retained, returned, or added to provider evidence.
var (
	logApplicationStarted = regexp.MustCompile(`\bstarted\s+[a-z][a-z0-9.$_-]*application\s+in\s+[0-9]+(?:\.[0-9]+)?\s+seconds?\b|\bapplication\s+started\s+successfully\b`)
	logServerStarted      = regexp.MustCompile(`\b(?:server|tomcat|jetty|netty|undertow)\s+started\s+on\s+ports?\b`)
	logReady              = regexp.MustCompile(`\bready\s+to\s+accept\s+connections\b`)
	logGracefulShutdown   = regexp.MustCompile(`\bgraceful\s+shutdown\b|\bshutting\s+down\s+gracefully\b`)
	logSeverityField      = regexp.MustCompile(`(?:^|[\s,{])"?(?:level|severity|log_level|loglevel)"?\s*[=:]\s*"?(error|warn|warning|info|debug|trace|fatal|critical)"?(?:[\s,}]|$)`)
	logSeverityPrefix     = regexp.MustCompile(`^\s*(?:\d{4}-\d{2}-\d{2}(?:[t ]\S+)?\s+|\d{2}:\d{2}:\d{2}(?:[.,]\d+)?\s+)?(?:\[[^]]+\]\s+)?\[?(error|warn|warning|info|debug|trace|fatal|critical)\]?(?:\s|$)`)
)

func classifyLogLifecycle(line []byte) []logSignalCategory {
	var categories []logSignalCategory
	if logApplicationStarted.Match(line) || logReady.Match(line) {
		categories = append(categories, logSignalApplicationStartup)
	}
	if logServerStarted.Match(line) {
		categories = append(categories, logSignalServerStartup)
	}
	if logGracefulShutdown.Match(line) {
		categories = append(categories, logSignalGracefulShutdown)
	}
	return categories
}

type logSeverity uint8

const (
	logSeverityUnknown logSeverity = iota
	logSeverityInformational
	logSeverityWarning
	logSeverityError
)

func structuredLogSeverity(line []byte) logSeverity {
	if bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) {
		var record struct {
			Level    string `json:"level"`
			Severity string `json:"severity"`
			LogLevel string `json:"log_level"`
		}
		if json.Unmarshal(line, &record) != nil {
			return logSeverityUnknown
		}
		for _, level := range []string{record.Level, record.Severity, record.LogLevel} {
			if level != "" {
				return knownLogSeverity([]byte(level))
			}
		}
		return logSeverityUnknown
	}
	// Logfmt message values can quote text such as "level=ERROR". Only
	// metadata before the message field is eligible as a severity source.
	for _, marker := range [][]byte{[]byte(" message="), []byte(" msg=")} {
		if index := bytes.Index(line, marker); index >= 0 {
			line = line[:index]
		}
	}
	match := logSeverityField.FindSubmatch(line)
	if len(match) == 0 {
		match = logSeverityPrefix.FindSubmatch(line)
	}
	if len(match) < 2 {
		return logSeverityUnknown
	}
	return knownLogSeverity(match[1])
}

func knownLogSeverity(level []byte) logSeverity {
	switch string(bytes.ToLower(level)) {
	case "error", "fatal", "critical":
		return logSeverityError
	case "warn", "warning":
		return logSeverityWarning
	default:
		return logSeverityInformational
	}
}
