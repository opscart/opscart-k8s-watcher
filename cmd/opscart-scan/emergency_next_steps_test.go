package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func TestEmergencyNextStepsFlagAbsentPreservesOutput(t *testing.T) {
	issues := []enrichedIssue{{EmergencyIssue: models.EmergencyIssue{Severity: "critical", Resource: "pod", Namespace: "payments", Name: "api-abc", Reason: "CrashLoopBackOff", Container: "app"}}}
	var oldPath, explicitOff bytes.Buffer
	printEmergencyIssuesEnriched(&oldPath, issues)
	printEmergencyIssuesEnrichedWithNextSteps(&explicitOff, issues, "prod", false)
	if oldPath.String() != explicitOff.String() || strings.Contains(oldPath.String(), "Next inspection") {
		t.Fatalf("flag-absent output changed:\n%s", explicitOff.String())
	}
}

func TestEmergencyNextStepsCommandsAndGroupedPods(t *testing.T) {
	issues := []enrichedIssue{
		{EmergencyIssue: models.EmergencyIssue{Severity: "medium", Resource: "pod", Namespace: "payments", Name: "api-a", Reason: "HighRestartCount", Container: "app", Message: "Container app has restarted 20 times", Restarts: 20}},
		{EmergencyIssue: models.EmergencyIssue{Severity: "medium", Resource: "pod", Namespace: "payments", Name: "api-b", Reason: "HighRestartCount", Container: "app", Message: "Container app has restarted 30 times", Restarts: 30}},
	}
	var buf bytes.Buffer
	printEmergencyIssuesEnrichedWithNextSteps(&buf, issues, "prod-cluster", true)
	out := buf.String()
	for _, want := range []string{
		"kubectl --context prod-cluster logs api-a -n payments -c app --previous",
		"kubectl --context prod-cluster describe pod api-a -n payments",
		"kubectl --context prod-cluster logs api-b -n payments -c app --previous",
		"kubectl --context prod-cluster describe pod api-b -n payments",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("command count for %q = %d; output:\n%s", want, strings.Count(out, want), out)
		}
	}
}

func TestEmergencyNextStepsMappingsSafetyAndUnsupported(t *testing.T) {
	tests := []struct {
		reason string
		want   []string
	}{
		{"OOMKilled", []string{"describe pod pod-a", "get pod pod-a"}},
		{"CrashLoopBackOff (ProbeFailure)", []string{"describe pod pod-a", "get events"}},
		{"ImagePullBackOff", []string{"describe pod pod-a", "get events"}},
		{"Pending", []string{"describe pod pod-a", "get events"}},
		{"PodFailed", nil},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			issue := enrichedIssue{EmergencyIssue: models.EmergencyIssue{Resource: "pod", Namespace: "ns", Name: "pod-a", Reason: tt.reason, Container: "app"}}
			commands := inspectionCommands("ctx", issue)
			joined := strings.Join(commands, "\n")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in %q", want, joined)
				}
			}
			if tt.want == nil && len(commands) != 0 {
				t.Fatalf("unsupported finding invented commands: %v", commands)
			}
			for _, forbidden := range []string{" apply ", " patch ", " delete ", " rollout ", " scale ", " exec ", " debug ", " port-forward "} {
				if strings.Contains(" "+joined+" ", forbidden) {
					t.Errorf("unsafe command %q", joined)
				}
			}
		})
	}
}

func TestInspectionCommandsDeduplicatesAtRender(t *testing.T) {
	issue := enrichedIssue{EmergencyIssue: models.EmergencyIssue{Severity: "high", Resource: "pod", Namespace: "ns", Name: "pod-a", Reason: "Pending"}}
	var buf bytes.Buffer
	printNextInspection(&buf, "ctx", []enrichedIssue{issue, issue})
	if strings.Count(buf.String(), "describe pod pod-a") != 1 || strings.Count(buf.String(), "get events") != 1 {
		t.Fatalf("duplicate commands not removed:\n%s", buf.String())
	}
}
