package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func TestEmergencyOMABoundaryWording(t *testing.T) {
	tests := []struct {
		name  string
		issue enrichedIssue
		want  string
	}{
		{
			name: "boundary finding",
			issue: enrichedIssue{EmergencyIssue: models.EmergencyIssue{
				Namespace: "payments", Name: "api-a", Reason: "CrashLoopBackOff",
			}, FirstDetected: "16d", atHistoryBoundary: true},
			want: "OMA history: present for at least 16d",
		},
		{
			name: "non-boundary finding",
			issue: enrichedIssue{EmergencyIssue: models.EmergencyIssue{
				Namespace: "payments", Name: "api-b", Reason: "CrashLoopBackOff",
			}, FirstDetected: "3d"},
			want: "First observed by this OMA: 3d ago",
		},
		{
			name: "no OMA history",
			issue: enrichedIssue{EmergencyIssue: models.EmergencyIssue{
				Namespace: "payments", Name: "api-c", Reason: "CrashLoopBackOff",
			}},
			want: "First observed by this OMA: —",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			printEnrichedIssue(&buf, tt.issue)
			out := buf.String()
			if !strings.Contains(out, tt.want) {
				t.Fatalf("output missing %q:\n%s", tt.want, out)
			}
			if tt.issue.atHistoryBoundary && (strings.Contains(out, "First observed by this OMA:") || strings.Contains(out, "Present when this OMA history began")) {
				t.Fatalf("boundary output retained verbose wording:\n%s", out)
			}
		})
	}
}

func TestEmergencyGroupedOMABoundaryWording(t *testing.T) {
	issues := []enrichedIssue{
		{EmergencyIssue: models.EmergencyIssue{Resource: "pod", Namespace: "payments", Name: "api-a", Reason: "HighRestartCount", Message: "Container app has restarted 20 times", Restarts: 20}, FirstDetected: "16d", atHistoryBoundary: true},
		{EmergencyIssue: models.EmergencyIssue{Resource: "pod", Namespace: "payments", Name: "api-b", Reason: "HighRestartCount", Message: "Container app has restarted 30 times", Restarts: 30}, FirstDetected: "2d"},
	}
	var buf bytes.Buffer
	printGroupedIssue(&buf, issues)
	out := buf.String()
	if !strings.Contains(out, "OMA history: present for at least 16d") {
		t.Fatalf("grouped boundary wording missing:\n%s", out)
	}
	if strings.Contains(out, "Present when this OMA history began") || strings.Contains(out, "First observed by this OMA: 16d") {
		t.Fatalf("grouped output retained verbose boundary wording:\n%s", out)
	}
}
