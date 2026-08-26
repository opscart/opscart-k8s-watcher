package classify

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func podIssue(reason, severity, container string, restarts int) models.EmergencyIssue {
	return models.EmergencyIssue{
		Resource: "pod", Namespace: "apps", Name: "worker-0",
		Reason: reason, Severity: severity, Container: container, Restarts: restarts,
		Message: "Container " + container + " is crash looping: back-off restarting failed container",
	}
}

func TestPodFailurePriority(t *testing.T) {
	cases := []struct {
		name  string
		input []models.EmergencyIssue
		probe bool
		want  string
	}{
		{"crash and oom", []models.EmergencyIssue{podIssue("CrashLoopBackOff", "critical", "app", 8), podIssue("OOMKilled", "critical", "sidecar", 3)}, false, "CrashLoopBackOff (OOMKilled)"},
		{"oom beats probe", []models.EmergencyIssue{podIssue("CrashLoopBackOff", "critical", "app", 8), podIssue("OOMKilled", "critical", "app", 8)}, true, "CrashLoopBackOff (OOMKilled)"},
		{"crash and probe", []models.EmergencyIssue{podIssue("CrashLoopBackOff", "critical", "app", 8)}, true, "CrashLoopBackOff (ProbeFailure)"},
		{"standalone probe", []models.EmergencyIssue{podIssue("ProbeFailure", "critical", "app", 8)}, false, "ProbeFailure"},
		{"image pull beats restarts", []models.EmergencyIssue{podIssue("HighRestartCount", "medium", "app", 20), podIssue("ErrImagePull", "high", "sidecar", 0)}, false, "ErrImagePull"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PodFailure(tc.input, tc.probe)
			if !ok || got.Reason != tc.want {
				t.Fatalf("PodFailure() = {%q, %v}, want {%q, true}", got.Reason, ok, tc.want)
			}
		})
	}
}

func TestPodFailureIsIndependentOfContainerOrder(t *testing.T) {
	a := podIssue("ImagePullBackOff", "high", "z-sidecar", 2)
	b := podIssue("OOMKilled", "critical", "a-worker", 1)

	forward, okForward := PodFailure([]models.EmergencyIssue{a, b}, false)
	reverse, okReverse := PodFailure([]models.EmergencyIssue{b, a}, false)
	if !okForward || !okReverse {
		t.Fatalf("both orders must classify: forward=%v reverse=%v", okForward, okReverse)
	}
	if forward.Reason != "OOMKilled" || reverse.Reason != forward.Reason {
		t.Fatalf("classification changed with container order: forward=%q reverse=%q", forward.Reason, reverse.Reason)
	}
}

func TestPodFailureSelectsRepresentativeDeterministically(t *testing.T) {
	a := podIssue("CrashLoopBackOff", "critical", "z-sidecar", 4)
	b := podIssue("CrashLoopBackOff", "critical", "a-worker", 9)

	forward, _ := PodFailure([]models.EmergencyIssue{a, b}, false)
	reverse, _ := PodFailure([]models.EmergencyIssue{b, a}, false)
	if forward.Container != "a-worker" || reverse.Container != forward.Container {
		t.Fatalf("representative changed with order: forward=%q reverse=%q", forward.Container, reverse.Container)
	}
}
