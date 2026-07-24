package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func issue(ns, name, severity, reason, message string, restarts int) enrichedIssue {
	return enrichedIssue{
		EmergencyIssue: models.EmergencyIssue{
			Severity:  severity,
			Resource:  "pod",
			Namespace: ns,
			Name:      name,
			Reason:    reason,
			Message:   message,
			Restarts:  restarts,
		},
	}
}

// Pending + ImagePullBackOff on the same pod collapses to just the
// ImagePullBackOff entry.
func TestDedupe_PendingMergesIntoImagePullBackOff(t *testing.T) {
	high := []enrichedIssue{
		issue("data-pipeline", "batch-importer-5849fd86bb-q9r8m", "high", "Pending",
			"Pod pending for extended period", 0),
		issue("data-pipeline", "batch-importer-5849fd86bb-q9r8m", "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup company-registry.internal on 192.168.65.254:53: no such host", 0),
	}

	got := dedupeHighTier(high)
	if len(got) != 1 {
		t.Fatalf("expected 1 surviving entry, got %d: %+v", len(got), got)
	}
	if got[0].Reason != "ImagePullBackOff" {
		t.Errorf("expected the ImagePullBackOff entry to survive, got Reason=%q", got[0].Reason)
	}
}

// A Pending pod with no corresponding ImagePullBackOff on the same
// container is a distinct scenario and must survive untouched.
func TestDedupe_PendingWithoutImagePullSurvives(t *testing.T) {
	high := []enrichedIssue{
		issue("prod", "resource-hog-xyz", "high", "Pending",
			"Pod pending for extended period", 0),
	}
	got := dedupeHighTier(high)
	if len(got) != 1 || got[0].Reason != "Pending" {
		t.Fatalf("expected standalone Pending entry to survive, got %+v", got)
	}
}

// 3 distinct pods hitting the same issue type against the identical
// literal cause text collapse into a single grouped entry naming all 3
// pods.
func TestGroupIssues_CollapsesSameCause(t *testing.T) {
	cause := "dial tcp: lookup company-registry.internal on 192.168.65.254:53: no such host"
	high := []enrichedIssue{
		issue("data-pipeline", "batch-importer-5849fd86bb-q9r8m", "high", "ImagePullBackOff",
			"Cannot pull image for container app: "+cause, 0),
		issue("inventory", "catalog-indexer-64dcbcb975-qqdmd", "high", "ImagePullBackOff",
			"Cannot pull image for container app: "+cause, 0),
		issue("notifications", "email-dispatcher-574556c86c-j44l6", "high", "ImagePullBackOff",
			"Cannot pull image for container app: "+cause, 0),
	}

	groups := groupIssues(high)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d: %+v", len(groups), groups)
	}
	if len(groups[0].issues) != 3 {
		t.Fatalf("expected group of 3, got %d", len(groups[0].issues))
	}

	var buf bytes.Buffer
	printGroupedIssue(&buf, groups[0].issues)
	out := buf.String()

	if !strings.Contains(out, "3 pods cannot pull image from company-registry.internal") {
		t.Errorf("missing expected title, got:\n%s", out)
	}
	for _, name := range []string{"batch-importer-5849fd86bb-q9r8m", "catalog-indexer-64dcbcb975-qqdmd", "email-dispatcher-574556c86c-j44l6"} {
		if !strings.Contains(out, name) {
			t.Errorf("missing pod name %q in grouped output:\n%s", name, out)
		}
	}
	for _, ns := range []string{"data-pipeline", "inventory", "notifications"} {
		if !strings.Contains(out, ns) {
			t.Errorf("missing namespace %q in grouped output:\n%s", ns, out)
		}
	}
	if !strings.Contains(out, "Cause: "+cause) {
		t.Errorf("missing cause line, got:\n%s", out)
	}
}

// Fix 1 (this task): 3 pods with SLIGHTLY DIFFERENT per-pod error text
// (one is a DNS lookup failure, two are quoted-image pull failures with
// different tags/digests) but the SAME underlying registry hostname must
// still group into 1 entry. This is exactly the case that failed under
// literal cause-text comparison.
func TestGroupIssues_ImagePullFingerprintGroupsDespiteDifferentText(t *testing.T) {
	high := []enrichedIssue{
		issue("data-pipeline", "batch-importer-5849fd86bb-q9r8m", "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup company-registry.internal on 192.168.65.254:53: no such host", 0),
		issue("inventory", "catalog-indexer-64dcbcb975-qqdmd", "high", "ImagePullBackOff",
			`Cannot pull image for container worker: Back-off pulling image "company-registry.internal/catalog-indexer:v2.4.1"`, 0),
		issue("notifications", "email-dispatcher-574556c86c-j44l6", "high", "ImagePullBackOff",
			`Cannot pull image for container app: Back-off pulling image "company-registry.internal/email-dispatcher@sha256:abc123def456"`, 0),
	}

	groups := groupIssues(high)
	if len(groups) != 1 || len(groups[0].issues) != 3 {
		t.Fatalf("expected all 3 pods to group by registry hostname despite differing per-pod text, got %d groups: %+v", len(groups), groups)
	}

	var buf bytes.Buffer
	printGroupedIssue(&buf, groups[0].issues)
	out := buf.String()
	if !strings.Contains(out, "3 pods cannot pull image from company-registry.internal") {
		t.Errorf("missing expected title, got:\n%s", out)
	}
}

// 2 pods with the SAME issue type but genuinely DIFFERENT registry hosts
// must NOT be grouped together.
func TestGroupIssues_DoesNotMergeDifferentCauses(t *testing.T) {
	high := []enrichedIssue{
		issue("ns1", "pod-a", "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup registry-one.internal on 10.0.0.1:53: no such host", 0),
		issue("ns2", "pod-b", "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup registry-two.internal on 10.0.0.2:53: no such host", 0),
		issue("ns3", "pod-c", "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup registry-one.internal on 10.0.0.1:53: no such host", 0),
	}

	groups := groupIssues(high)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups (registry-one x2, registry-two x1), got %d: %+v", len(groups), groups)
	}

	sizes := map[int]bool{}
	for _, g := range groups {
		sizes[len(g.issues)] = true
	}
	if !sizes[2] || !sizes[1] {
		t.Errorf("expected group sizes {2,1}, got groups=%+v", groups)
	}
}

// A single pod matching an issue+cause combo (no others to group with)
// still prints individually, not as a "group of 1".
func TestGroupIssues_SingleIssuePrintsIndividually(t *testing.T) {
	high := []enrichedIssue{
		issue("prod", "lonely-pod-123", "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup only-registry.internal on 10.0.0.9:53: no such host", 0),
	}

	groups := groupIssues(high)
	if len(groups) != 1 || len(groups[0].issues) != 1 {
		t.Fatalf("expected a single ungrouped entry, got %+v", groups)
	}

	var buf bytes.Buffer
	printIssueGroups(&buf, groups)
	out := buf.String()
	if strings.Contains(out, "Namespaces:") || strings.Contains(out, "Pods:") {
		t.Errorf("a lone issue should print via printEnrichedIssue, not the grouped format:\n%s", out)
	}
	if !strings.Contains(out, "lonely-pod-123") {
		t.Errorf("expected pod name in output:\n%s", out)
	}
}

// Fix 1 (this task): 3 HighRestartCount pods with the SAME container
// name but DIFFERENT restart counts (26, 26, 88) must all group
// together — this is the exact node-exporter case that broke under the
// old cause-text-includes-the-count comparison (2 of 3 grouped, the 3rd
// with a different count stayed separate).
func TestGroupIssues_HighRestartCountGroupsAcrossDifferentCounts(t *testing.T) {
	medium := []enrichedIssue{
		issue("monitoring", "node-exporter-aaa", "medium", "HighRestartCount",
			"Container node-exporter has restarted 26 times", 26),
		issue("monitoring", "node-exporter-bbb", "medium", "HighRestartCount",
			"Container node-exporter has restarted 26 times", 26),
		issue("monitoring", "node-exporter-ccc", "medium", "HighRestartCount",
			"Container node-exporter has restarted 88 times", 88),
	}

	groups := groupIssues(medium)
	if len(groups) != 1 || len(groups[0].issues) != 3 {
		t.Fatalf("expected all 3 node-exporter pods to group despite different restart counts, got %d groups: %+v", len(groups), groups)
	}

	var buf bytes.Buffer
	printGroupedIssue(&buf, groups[0].issues)
	out := buf.String()
	if !strings.Contains(out, "3 pods restarting frequently (container node-exporter)") {
		t.Errorf("missing expected title, got:\n%s", out)
	}
	if !strings.Contains(out, "Restarts: 26-88 across 3 pods") {
		t.Errorf("missing restart range detail line, got:\n%s", out)
	}
}

// HighRestartCount's grouping key intentionally includes namespace (see
// groupKeyFor's doc comment): the same container name restarting a lot
// in two unrelated namespaces is a coincidence, not one incident, so
// they must NOT be grouped together.
func TestGroupIssues_HighRestartCountDifferentNamespaceNotGrouped(t *testing.T) {
	medium := []enrichedIssue{
		issue("monitoring", "node-exporter-aaa", "medium", "HighRestartCount",
			"Container node-exporter has restarted 26 times", 26),
		issue("logging", "node-exporter-zzz", "medium", "HighRestartCount",
			"Container node-exporter has restarted 30 times", 30),
	}

	groups := groupIssues(medium)
	if len(groups) != 2 {
		t.Fatalf("expected 2 separate groups (different namespaces), got %d: %+v", len(groups), groups)
	}
}

// The severity header must count PRINTED entries (an N-pod group counts
// as 1), not underlying pod counts. A same-pod CRITICAL+HIGH pairing is
// no longer possible by the time issues reach the printer (classifyIssues
// already collapsed it upstream — see emergency_classify_test.go), so
// this exercises grouping across DIFFERENT pods only.
func TestHeaderCount_MatchesPrintedEntries(t *testing.T) {
	var all []enrichedIssue

	all = append(all,
		issue("data-pipeline", "stream-processor-66c474d5fd-9zpwq", "critical", "CrashLoopBackOff",
			"Container stress is being OOM-killed, causing the crash loop — check resources.limits.memory", 6301),
		issue("prod", "solo-crash-pod", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 12),
	)

	// HIGH: a group of 3 (1 printed entry) + 1 standalone (1 printed
	// entry) -> 2 printed HIGH entries, despite 4 underlying raw issues.
	for _, name := range []string{"batch-importer-a", "batch-importer-b", "batch-importer-c"} {
		all = append(all, issue("data-pipeline", name, "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup company-registry.internal on 192.168.65.254:53: no such host", 0))
	}
	all = append(all, issue("prod", "resource-hog-xyz", "high", "Pending",
		"Pod pending for extended period", 0))

	var buf bytes.Buffer
	printEmergencyIssuesEnriched(&buf, all)
	out := buf.String()

	if !strings.Contains(out, "🔴 CRITICAL: 2") {
		t.Errorf("expected CRITICAL header to count printed entries (2), got:\n%s", out)
	}
	if !strings.Contains(out, "🟡 HIGH: 2") {
		t.Errorf("expected HIGH header to count printed entries (2: group of 3 + solo), got:\n%s", out)
	}
	// The header must never regress to reporting the underlying pod
	// count (4 HIGH pods across 2 entries).
	if strings.Contains(out, "🟡 HIGH: 4") {
		t.Errorf("header appears to count underlying pods instead of printed entries, got:\n%s", out)
	}
}

// Reproduces the representative mixed-workload scenario used by this test,
// run through the real pipeline order (classifyIssues first, exactly as
// runEmergencyScan does): dozens of raw candidate entries describing a
// handful of distinct workloads collapse via classification (same-pod)
// and fingerprint-based grouping (cross-pod). Covers: the
// stream-processor CrashLoopBackOff+OOMKilled pair classifying to one
// CRITICAL entry, the 3 image-pull pods grouping despite differing
// per-pod text, and all 3 node-exporter pods grouping despite differing
// restart counts.
func TestPrintEmergencyIssuesEnriched_MixedWorkloadShape(t *testing.T) {
	var raw []models.EmergencyIssue

	// CRITICAL: stream-processor's CrashLoopBackOff+OOMKilled pair
	// (classifies to 1), token-service CrashLoopBackOff (+ a coexisting
	// raw MEDIUM HighRestartCount candidate, collapsed away by
	// classifyIssues), and 3 more distinct, unrelated CrashLoopBackOff
	// pods.
	raw = append(raw,
		rawIssue("data-pipeline", "stream-processor-66c474d5fd-9zpwq", "critical", "CrashLoopBackOff",
			"Container stress is crash looping: back-off restarting failed container", 6301),
		rawIssue("data-pipeline", "stream-processor-66c474d5fd-9zpwq", "critical", "OOMKilled",
			"Container stress killed due to out of memory", 6301),
		rawIssue("prod", "token-service-786498c5c-phg2g", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 6228),
		rawIssue("prod", "token-service-786498c5c-phg2g", "medium", "HighRestartCount",
			"Container app has restarted 6228 times", 6228),
	)
	for _, name := range []string{"payments-worker-1", "payments-worker-2", "payments-worker-3"} {
		raw = append(raw, rawIssue("prod", name, "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 50))
	}

	// HIGH: batch-importer's Pending+ImagePullBackOff duplicate, plus 2
	// more distinct pods hitting the SAME registry host with DIFFERENT
	// per-pod message text, and a standalone Pending pod unrelated to
	// image pulls.
	raw = append(raw,
		rawIssue("data-pipeline", "batch-importer-5849fd86bb-q9r8m", "high", "Pending",
			"Pod pending for extended period", 0),
		rawIssue("data-pipeline", "batch-importer-5849fd86bb-q9r8m", "high", "ImagePullBackOff",
			"Cannot pull image for container app: dial tcp: lookup company-registry.internal on 192.168.65.254:53: no such host", 0),
		rawIssue("inventory", "catalog-indexer-64dcbcb975-qqdmd", "high", "ImagePullBackOff",
			`Cannot pull image for container worker: Back-off pulling image "company-registry.internal/catalog-indexer:v2.4.1"`, 0),
		rawIssue("notifications", "email-dispatcher-574556c86c-j44l6", "high", "ImagePullBackOff",
			`Cannot pull image for container app: Back-off pulling image "company-registry.internal/email-dispatcher@sha256:abc123def456"`, 0),
		rawIssue("prod", "resource-hog-xyz", "high", "Pending",
			"Pod pending for extended period", 0),
	)

	// MEDIUM: 3 node-exporter pods with different restart counts, plus 1
	// unrelated standalone HighRestartCount pod.
	raw = append(raw,
		rawIssue("monitoring", "node-exporter-aaa", "medium", "HighRestartCount",
			"Container node-exporter has restarted 26 times", 26),
		rawIssue("monitoring", "node-exporter-bbb", "medium", "HighRestartCount",
			"Container node-exporter has restarted 26 times", 26),
		rawIssue("monitoring", "node-exporter-ccc", "medium", "HighRestartCount",
			"Container node-exporter has restarted 88 times", 88),
		rawIssue("staging", "noisy-solo", "medium", "HighRestartCount",
			"Container sidecar has restarted 40 times", 40),
	)

	classified := classifyIssues(raw, nil)
	var all []enrichedIssue
	for _, iss := range classified {
		all = append(all, enrichedIssue{EmergencyIssue: iss})
	}

	var buf bytes.Buffer
	printEmergencyIssuesEnriched(&buf, all)
	out := buf.String()

	// 16 raw issues in, but the printed entries should be: CRITICAL 5
	// (stream-processor merge + token-service + 3 payments-workers), HIGH
	// 2 (grouped image-pull trio + solo Pending), MEDIUM 2 (grouped
	// node-exporter trio + solo). The header must match exactly.
	if !strings.Contains(out, "🔴 CRITICAL: 5    🟡 HIGH: 2    🟠 MEDIUM: 2") {
		t.Errorf("expected header CRITICAL:5 HIGH:2 MEDIUM:2, got:\n%s", out)
	}
	if !strings.Contains(out, "Status: CrashLoopBackOff (OOMKilled)") {
		t.Errorf("expected stream-processor pair to merge, got:\n%s", out)
	}
	if !strings.Contains(out, "3 pods cannot pull image from company-registry.internal") {
		t.Errorf("expected the 3 image-pull pods to group despite differing text, got:\n%s", out)
	}
	if !strings.Contains(out, "3 pods restarting frequently (container node-exporter)") ||
		!strings.Contains(out, "Restarts: 26-88 across 3 pods") {
		t.Errorf("expected all 3 node-exporter pods to group despite differing restart counts, got:\n%s", out)
	}
	if strings.Count(out, "token-service-786498c5c-phg2g") != 1 {
		t.Errorf("expected token-service's duplicate MEDIUM entry to stay collapsed away, got:\n%s", out)
	}

	lineCount := len(strings.Split(strings.TrimRight(out, "\n"), "\n"))
	if lineCount >= 60 {
		t.Errorf("expected a materially shorter output, got %d lines:\n%s", lineCount, out)
	}
}
