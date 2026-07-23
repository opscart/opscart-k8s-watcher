package main

import (
	"bytes"
	"strings"
	"testing"
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

// A pod in CrashLoopBackOff with a matching probe-failure event in its
// recent history relabels to "CrashLoopBackOff (ProbeFailure)" with the
// probe-specific causal message.
func TestMergeCrashLoopOOM_ProbeFailureRelabel(t *testing.T) {
	crashLoop := issue("prod", "checkout-7d9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
		"Container app is crash looping: back-off restarting failed container", 42)
	crashLoop.probeFailure = true

	items := mergeCrashLoopOOM([]enrichedIssue{crashLoop})
	if len(items) != 1 {
		t.Fatalf("expected 1 critical item, got %d: %+v", len(items), items)
	}
	if items[0].oomCause != nil {
		t.Fatalf("expected no OOMKilled merge, got oomCause=%+v", items[0].oomCause)
	}
	if !items[0].probeFailure {
		t.Fatalf("expected probeFailure item, got %+v", items[0])
	}

	var buf bytes.Buffer
	printCriticalItem(&buf, items[0])
	out := buf.String()

	if !strings.Contains(out, "Status: CrashLoopBackOff (ProbeFailure)") {
		t.Errorf("missing relabeled status, got:\n%s", out)
	}
	if !strings.Contains(out, "Container app is being killed by its startup/liveness probe before it can stabilize") {
		t.Errorf("missing causal probe message, got:\n%s", out)
	}
}

// A pod in CrashLoopBackOff with NO probe-failure event (a genuine
// unrelated crash) must be left exactly as today: a plain
// CrashLoopBackOff entry, no relabeling.
func TestMergeCrashLoopOOM_NoProbeSignatureUnaffected(t *testing.T) {
	crashLoop := issue("prod", "checkout-7d9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
		"Container app is crash looping: back-off restarting failed container", 42)
	// probeFailure left false, as if annotateProbeFailures found no
	// matching event (or none at all).

	items := mergeCrashLoopOOM([]enrichedIssue{crashLoop})
	if len(items) != 1 || items[0].oomCause != nil || items[0].probeFailure {
		t.Fatalf("expected unmerged, unrelabeled entry, got %+v", items)
	}

	var buf bytes.Buffer
	printCriticalItem(&buf, items[0])
	out := buf.String()
	if !strings.Contains(out, "Status: CrashLoopBackOff | Restarts: 42") {
		t.Errorf("expected unchanged plain status line, got:\n%s", out)
	}
	if strings.Contains(out, "ProbeFailure") || strings.Contains(out, "probe") {
		t.Errorf("should show no probe-related text when no signature was found, got:\n%s", out)
	}
}

// A pod with BOTH an OOMKilled cause AND a probe-failure event signature
// must resolve to the OOMKilled merge — documented precedence: an
// observed OOM is the more certain, direct cause; the probe-failure
// signal is dropped rather than shown alongside it.
func TestMergeCrashLoopOOM_OOMKilledTakesPrecedenceOverProbeFailure(t *testing.T) {
	crashLoop := issue("prod", "checkout-7d9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
		"Container app is crash looping: back-off restarting failed container", 42)
	crashLoop.probeFailure = true
	oom := issue("prod", "checkout-7d9f8b6c5-x7z2m9", "critical", "OOMKilled",
		"Container app killed due to out of memory", 42)

	items := mergeCrashLoopOOM([]enrichedIssue{crashLoop, oom})
	if len(items) != 1 {
		t.Fatalf("expected 1 merged critical item, got %d: %+v", len(items), items)
	}
	if items[0].oomCause == nil {
		t.Fatalf("expected OOMKilled merge to win over probe-failure signal, got %+v", items[0])
	}
	if items[0].probeFailure {
		t.Errorf("probeFailure flag should not also be set once OOMKilled merge applies, got %+v", items[0])
	}

	var buf bytes.Buffer
	printCriticalItem(&buf, items[0])
	out := buf.String()
	if !strings.Contains(out, "Status: CrashLoopBackOff (OOMKilled)") {
		t.Errorf("expected OOMKilled label to take precedence, got:\n%s", out)
	}
	if strings.Contains(out, "ProbeFailure") {
		t.Errorf("expected no probe-failure text once OOMKilled wins, got:\n%s", out)
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

func TestAnnotateProbeFailures_NoCrashLoopIssuesSkipsClientsetEntirely(t *testing.T) {
	// getKubernetesClient would fail against an unresolvable context; if
	// annotateProbeFailures tried to build a clientset here it would log
	// an error. With no CrashLoopBackOff issues in the input there is
	// nothing to check, so it must return early without attempting to
	// connect at all — and therefore without mutating anything.
	issues := []enrichedIssue{
		issue("prod", "steady-pod", "high", "Pending", "Pod pending for extended period", 0),
	}
	got := annotateProbeFailures("does-not-exist-context", issues)
	if len(got) != 1 || got[0].probeFailure {
		t.Fatalf("expected input returned unmodified, got %+v", got)
	}
}
