package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func TestHasProbeFailureSignature_Matches(t *testing.T) {
	tests := []struct {
		name string
		msgs []string
		want bool
	}{
		{
			name: "startup probe failed with status code",
			msgs: []string{"Startup probe failed: HTTP probe failed with statuscode: 500"},
			want: true,
		},
		{
			name: "will be restarted phrasing",
			msgs: []string{"Container app failed startup probe, will be restarted"},
			want: true,
		},
		{
			name: "liveness probe wording, mixed case",
			msgs: []string{"Liveness Probe Failed: connection refused"},
			want: true,
		},
		{
			name: "unrelated events only",
			msgs: []string{"Pulled image successfully", "Created container app", "Started container app"},
			want: false,
		},
		{
			name: "no events at all",
			msgs: nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasProbeFailureSignature(tt.msgs); got != tt.want {
				t.Errorf("hasProbeFailureSignature(%+v) = %v, want %v", tt.msgs, got, tt.want)
			}
		})
	}
}

// Reproduces the representative sample-worker event text from
// this session: "Startup probe failed: HTTP probe failed with
// statuscode: 500" followed by "failed startup probe, will be
// restarted", ending in CrashLoopBackOff. Confirms the exact real-world
// message format is detected.
func TestHasProbeFailureSignature_RepresentativeProbeEvents(t *testing.T) {
	events := []string{
		"Startup probe failed: HTTP probe failed with statuscode: 500",
		"Container sample-worker failed startup probe, will be restarted",
	}
	if !hasProbeFailureSignature(events) {
		t.Errorf("expected the real sample-worker event text to match the probe-failure signature")
	}
}

// detectProbeFailures must not attempt a clientset connection at all when
// there are no CrashLoopBackOff issues to check — getKubernetesClient
// would fail against an unresolvable context, which would otherwise show
// up as a logged error with nothing to check it against.
func TestDetectProbeFailures_NoCrashLoopIssuesSkipsClientsetEntirely(t *testing.T) {
	issues := []models.EmergencyIssue{
		rawIssue("prod", "steady-pod", "high", "Pending", "Pod pending for extended period", 0),
	}
	got := detectProbeFailures("does-not-exist-context", issues)
	if got != nil {
		t.Fatalf("expected a nil map (no clientset attempt) when there are no CrashLoopBackOff issues, got %+v", got)
	}
}
