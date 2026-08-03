package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// rawIssue builds a raw scanner finding (pre-classification), mirroring
// what analyzePodForIssues (pkg/scanner/cluster.go) emits.
func rawIssue(ns, name, severity, reason, message string, restarts int) models.EmergencyIssue {
	return models.EmergencyIssue{
		Severity:  severity,
		Resource:  "pod",
		Namespace: ns,
		Name:      name,
		Reason:    reason,
		Message:   message,
		Restarts:  restarts,
	}
}

// Priority 1: OOMKilled + CrashLoopBackOff classifies as one CRITICAL
// entry naming the real container and the causal OOM message. Matches
// the exact stream-processor case from the corp-cluster report.
func TestClassifyPod_OOMKilledMergesWithCrashLoop(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("data-pipeline", "stream-processor-66c474d5fd-9zpwq", "critical", "CrashLoopBackOff",
			"Container stress is crash looping: back-off restarting failed container", 6301),
		rawIssue("data-pipeline", "stream-processor-66c474d5fd-9zpwq", "critical", "OOMKilled",
			"Container stress killed due to out of memory", 6301),
	}

	got, ok := classifyPod(podIssues, false)
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "CrashLoopBackOff (OOMKilled)" {
		t.Errorf("Reason = %q, want %q", got.Reason, "CrashLoopBackOff (OOMKilled)")
	}
	if got.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", got.Severity)
	}
	if got.Message != "Container termination state reports OOMKilled; the pod is currently in CrashLoopBackOff." {
		t.Errorf("Message = %q, want observational OOM wording", got.Message)
	}
}

// Priority 1 wins even when a probe-failure signature is ALSO present:
// an observed OOM is a more certain, direct cause than a correlated
// probe-failure event.
func TestClassifyPod_OOMKilledTakesPrecedenceOverProbeFailure(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("prod", "checkout-7d9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 42),
		rawIssue("prod", "checkout-7d9f8b6c5-x7z2m9", "critical", "OOMKilled",
			"Container app killed due to out of memory", 42),
	}

	got, ok := classifyPod(podIssues, true) // probe-failure signature also present
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "CrashLoopBackOff (OOMKilled)" {
		t.Errorf("Reason = %q, want OOMKilled to win even with a probe-failure signal present", got.Reason)
	}
}

// Priority 2: a probe-failure signature relabels a plain CrashLoopBackOff
// with no OOM cause.
func TestClassifyPod_ProbeFailureRelabelsCrashLoop(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("prod", "checkout-7d9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 42),
	}

	got, ok := classifyPod(podIssues, true)
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "CrashLoopBackOff (ProbeFailure)" {
		t.Errorf("Reason = %q, want %q", got.Reason, "CrashLoopBackOff (ProbeFailure)")
	}
	if !strings.Contains(got.Message, "Kubernetes events show repeated startup/liveness probe failures followed by container restarts") {
		t.Errorf("Message = %q, missing observational probe text", got.Message)
	}
}

// A pod in CrashLoopBackOff with NO probe-failure event (a genuine
// unrelated crash) must be left exactly as scanned: no relabeling.
func TestClassifyPod_NoProbeSignatureUnaffected(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("prod", "checkout-7d9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 42),
	}

	got, ok := classifyPod(podIssues, false)
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "CrashLoopBackOff" {
		t.Errorf("Reason = %q, want plain CrashLoopBackOff", got.Reason)
	}
	if strings.Contains(got.Message, "probe") {
		t.Errorf("should show no probe-related text when no signature was found, got Message=%q", got.Message)
	}
}

// Priority 3: plain CrashLoopBackOff, with neither an OOM cause nor a
// probe-failure signature, is left exactly as scanned.
func TestClassifyPod_PlainCrashLoopUnaffected(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("prod", "solo-crash-pod", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 12),
	}

	got, ok := classifyPod(podIssues, false)
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "CrashLoopBackOff" {
		t.Errorf("Reason = %q, want plain CrashLoopBackOff", got.Reason)
	}
	if got.Message != "Container app is crash looping: back-off restarting failed container" {
		t.Errorf("Message was rewritten, want unchanged: %q", got.Message)
	}
}

// Not one of the 6 enumerated priority steps, but required to preserve
// pre-existing behavior: cs.LastTerminationState is sticky and keeps
// reporting "OOMKilled" until the container's next restart, long after
// the container is back to stable Running, so a pod can show OOMKilled
// with no CrashLoopBackOff at all. That's already CRITICAL and no less
// real for lacking a current crash loop.
func TestClassifyPod_StandaloneOOMKilled(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("prod", "steady-now-pod", "critical", "OOMKilled",
			"Container app killed due to out of memory", 3),
	}

	got, ok := classifyPod(podIssues, false)
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "OOMKilled" {
		t.Errorf("Reason = %q, want OOMKilled", got.Reason)
	}
	if got.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", got.Severity)
	}
}

// Priority 4: ImagePullBackOff/ErrImagePull is unaffected by
// classification — same reason, same HIGH severity the scanner already
// assigns (per this task's explicit "existing logic, unchanged").
func TestClassifyPod_ImagePullBackOffUnaffected(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("data-pipeline", "batch-importer-5849fd86bb-q9r8m", "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup company-registry.internal on 192.168.65.254:53: no such host", 0),
	}

	got, ok := classifyPod(podIssues, false)
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "ImagePullBackOff" || got.Severity != "high" {
		t.Errorf("got Reason=%q Severity=%q, want ImagePullBackOff/high unchanged", got.Reason, got.Severity)
	}
}

// Priority 4 gates priority 5: ImagePullBackOff beats a coexisting
// HighRestartCount for the same pod (e.g. a container that was already
// restarting a lot also can't pull a newly rolled image).
func TestClassifyPod_ImagePullBackOffGatesHighRestartCount(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("prod", "rollout-pod", "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup company-registry.internal on 192.168.65.254:53: no such host", 0),
		rawIssue("prod", "rollout-pod", "medium", "HighRestartCount",
			"Container app has restarted 15 times", 15),
	}

	got, ok := classifyPod(podIssues, false)
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "ImagePullBackOff" {
		t.Errorf("Reason = %q, want ImagePullBackOff to gate HighRestartCount", got.Reason)
	}
}

// Priority 5: HighRestartCount only wins when nothing higher-priority
// matched.
func TestClassifyPod_HighRestartCountOnlyWhenNothingElseMatches(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("prod", "worker-abc123", "medium", "HighRestartCount",
			"Container app has restarted 15 times", 15),
	}

	got, ok := classifyPod(podIssues, false)
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "HighRestartCount" || got.Severity != "medium" {
		t.Errorf("got Reason=%q Severity=%q, want HighRestartCount/medium", got.Reason, got.Severity)
	}
}

// This task's core fixture: a single pod whose raw issues match MULTIPLE
// categories at once (a live CrashLoopBackOff and a high restart count —
// exactly what analyzePodForIssues' independent per-container `if`s can
// both produce for the same container in the same scan) now yields
// EXACTLY ONE classification, in the highest-priority category, never
// two coexisting entries.
func TestClassifyPod_MultiCategoryPod_ExactlyOneHighestPriorityResult(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("prod", "payment-processor-6c9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 6412),
		rawIssue("prod", "payment-processor-6c9f8b6c5-x7z2m9", "medium", "HighRestartCount",
			"Container app has restarted 6412 times", 6412),
	}

	got, ok := classifyPod(podIssues, false)
	if !ok {
		t.Fatalf("expected a classification")
	}
	if got.Reason != "CrashLoopBackOff" {
		t.Errorf("Reason = %q, want CrashLoopBackOff (higher priority than HighRestartCount)", got.Reason)
	}
	if got.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", got.Severity)
	}
}

// Step 6 of the priority list: a pod group holding nothing in
// classifiablePodReasons yields no classification. classifyIssues never
// actually constructs such a group in practice (every reason it groups by
// is one classifyPod explicitly handles), but classifyPod's own contract
// is tested directly here.
func TestClassifyPod_NoClassifiableIssues_ReturnsNotOK(t *testing.T) {
	_, ok := classifyPod(nil, false)
	if ok {
		t.Fatalf("expected ok=false for a pod with no classifiable issues")
	}
}

// Reproduces tonight's real bug at the classification layer: the same
// underlying pod state — where a container's Waiting state and its high
// restart count both fire in the very same read, which is what a scan
// straddling the crash loop's brief Running window produces — must
// classify identically no matter how many times classification runs, and
// regardless of the raw issues' order. Before this fix, a pod like this
// could resolve to a DIFFERENT winning reason (and therefore a different
// fingerprint) run to run, which is exactly what silently "resolved" the
// CRITICAL incident in the DB.
func TestClassifyPod_FlickerBetweenRunningAndWaiting_SameFingerprintEveryTime(t *testing.T) {
	orderA := []models.EmergencyIssue{
		rawIssue("prod", "payment-processor-6c9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 6412),
		rawIssue("prod", "payment-processor-6c9f8b6c5-x7z2m9", "medium", "HighRestartCount",
			"Container app has restarted 6412 times", 6412),
	}
	orderB := []models.EmergencyIssue{orderA[1], orderA[0]} // same pod, reversed scan order

	run1, ok1 := classifyPod(orderA, false)
	run2, ok2 := classifyPod(orderB, false)
	if !ok1 || !ok2 {
		t.Fatalf("expected both runs to classify: ok1=%v ok2=%v", ok1, ok2)
	}

	fp1 := incidentFingerprint(run1)
	fp2 := incidentFingerprint(run2)
	if fp1 != fp2 {
		t.Fatalf("fingerprint changed across runs of the identical pod state: run1=%q run2=%q", fp1, fp2)
	}
	if run1.Reason != "CrashLoopBackOff" || run2.Reason != "CrashLoopBackOff" {
		t.Fatalf("expected both runs to classify as CrashLoopBackOff, got run1=%q run2=%q", run1.Reason, run2.Reason)
	}
}

// End-to-end: a classified OOMKilled-caused crash loop prints with the
// merged status label and observational message.
func TestClassifyPod_OOMKilledMerge_PrintsCorrectly(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("data-pipeline", "stream-processor-66c474d5fd-9zpwq", "critical", "CrashLoopBackOff",
			"Container stress is crash looping: back-off restarting failed container", 6301),
		rawIssue("data-pipeline", "stream-processor-66c474d5fd-9zpwq", "critical", "OOMKilled",
			"Container stress killed due to out of memory", 6301),
	}
	classified, ok := classifyPod(podIssues, false)
	if !ok {
		t.Fatalf("expected a classification")
	}
	classified.Age = 23 * 24 * time.Hour

	var buf bytes.Buffer
	printEnrichedIssue(&buf, enrichedIssue{EmergencyIssue: classified, FirstDetected: "14h"})
	out := buf.String()

	if strings.Count(out, "stream-processor-66c474d5fd-9zpwq") != 1 {
		t.Errorf("expected exactly 1 printed block, got:\n%s", out)
	}
	if !strings.Contains(out, "data-pipeline/stream-processor-66c474d5fd-9zpwq") {
		t.Errorf("missing pod identity, got:\n%s", out)
	}
	if !strings.Contains(out, "Status: CrashLoopBackOff (OOMKilled) | Restarts: 6301 | Pod Age: 23d") {
		t.Errorf("missing combined status line, got:\n%s", out)
	}
	if !strings.Contains(out, "Container termination state reports OOMKilled; the pod is currently in CrashLoopBackOff.") {
		t.Errorf("missing observational message, got:\n%s", out)
	}
	if !strings.Contains(out, "First observed by this OMA: 14h ago") {
		t.Errorf("missing enrichment line, got:\n%s", out)
	}
}

func TestClassifyPod_ProbeFailureMerge_PrintsCorrectly(t *testing.T) {
	podIssues := []models.EmergencyIssue{
		rawIssue("prod", "checkout-7d9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 42),
	}
	classified, ok := classifyPod(podIssues, true)
	if !ok {
		t.Fatalf("expected a classification")
	}

	var buf bytes.Buffer
	printEnrichedIssue(&buf, enrichedIssue{EmergencyIssue: classified})
	out := buf.String()

	if !strings.Contains(out, "Status: CrashLoopBackOff (ProbeFailure)") {
		t.Errorf("missing relabeled status, got:\n%s", out)
	}
	if !strings.Contains(out, "Kubernetes events show repeated startup/liveness probe failures followed by container restarts") {
		t.Errorf("missing observational probe message, got:\n%s", out)
	}
}

// classifyIssues collapses a pod's classifiable issues to one, while
// leaving a non-classifiable pod issue (Pending) and a non-pod issue
// (PVC) for a DIFFERENT resource untouched.
func TestClassifyIssues_PassesThroughNonClassifiableAndNonPod(t *testing.T) {
	raw := []models.EmergencyIssue{
		rawIssue("prod", "payment-processor-6c9f8b6c5-x7z2m9", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 6412),
		rawIssue("prod", "payment-processor-6c9f8b6c5-x7z2m9", "medium", "HighRestartCount",
			"Container app has restarted 6412 times", 6412),
		rawIssue("prod", "steady-worker", "high", "Pending", "Pod pending for extended period", 0),
		{Severity: "critical", Resource: "pvc", Namespace: "prod", Name: "data-vol", Reason: "PVCLost", Message: "PersistentVolumeClaim in Lost state"},
	}

	got := classifyIssues(raw, nil)
	if len(got) != 3 {
		t.Fatalf("expected 3 issues (1 classified pod + Pending + PVC), got %d: %+v", len(got), got)
	}

	var sawCrashLoop, sawPending, sawPVC bool
	for _, iss := range got {
		switch {
		case iss.Reason == "CrashLoopBackOff" && iss.Name == "payment-processor-6c9f8b6c5-x7z2m9":
			sawCrashLoop = true
		case iss.Reason == "HighRestartCount":
			t.Errorf("HighRestartCount should have been collapsed away, got %+v", iss)
		case iss.Reason == "Pending":
			sawPending = true
		case iss.Reason == "PVCLost":
			sawPVC = true
		}
	}
	if !sawCrashLoop || !sawPending || !sawPVC {
		t.Errorf("missing expected entries, got %+v", got)
	}
}

// The full corp-cluster shape: 8 pods with a mix of raw signal
// combinations. classifyIssues must leave exactly 8 issues — one per
// pod — never 16.
func TestClassifyIssues_EightPodsNotSixteen(t *testing.T) {
	var raw []models.EmergencyIssue
	raw = append(raw,
		rawIssue("prod", "pod1-abc", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 100),
		rawIssue("prod", "pod2-abc", "critical", "OOMKilled",
			"Container app killed due to out of memory", 200),
		rawIssue("prod", "pod2-abc", "medium", "HighRestartCount",
			"Container app has restarted 200 times", 200),
		rawIssue("prod", "pod3-abc", "medium", "HighRestartCount",
			"Container app has restarted 300 times", 300),
		rawIssue("prod", "pod4-abc", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 400),
		rawIssue("prod", "pod4-abc", "critical", "OOMKilled",
			"Container app killed due to out of memory", 400),
		rawIssue("prod", "pod5-abc", "medium", "HighRestartCount",
			"Container app has restarted 500 times", 500),
		rawIssue("prod", "pod6-abc", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 600),
		rawIssue("prod", "pod6-abc", "medium", "HighRestartCount",
			"Container app has restarted 600 times", 600),
		rawIssue("prod", "pod7-abc", "critical", "OOMKilled",
			"Container app killed due to out of memory", 700),
		rawIssue("prod", "pod8-abc", "medium", "HighRestartCount",
			"Container app has restarted 800 times", 800),
	)

	got := classifyIssues(raw, nil)
	if len(got) != 8 {
		t.Fatalf("expected exactly 8 classified issues (one per pod), got %d: %+v", len(got), got)
	}
	seen := make(map[string]int)
	for _, iss := range got {
		seen[podKey(iss.Namespace, iss.Name)]++
	}
	for i := 1; i <= 8; i++ {
		key := podKey("prod", fmt.Sprintf("pod%d-abc", i))
		if seen[key] != 1 {
			t.Errorf("expected exactly 1 classified issue for %s, got %d", key, seen[key])
		}
	}
}
