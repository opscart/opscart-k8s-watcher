package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// These tests guard against issue-type vocabulary drift: producers emitting
// one identifier (e.g. "oomkilled") while consumers switch on another
// (e.g. "oom_killed"), which silently disables scoring and grouping.
// See v1.11.0 bug: calcIncidentScore, formatGroupedIssue, humanizeWRType
// checked "oom_killed"/"image_pull" which no producer ever emits.

// TestZombieTypeEmitsCanonicalIssueTypes pins that the War Room producer
// only emits canonical identifiers. If a new status is added with a
// non-canonical identifier, this test fails.
func TestZombieTypeEmitsCanonicalIssueTypes(t *testing.T) {
	statuses := []string{
		"OOMKilled",
		"CrashLoopBackOff (OOMKilled)",
		"ImagePullBackOff",
		"ErrImagePull",
		"ProbeFailure",
		"CrashLoopBackOff (ProbeFailure)",
		"CrashLoopBackOff",
		"HighRestartCount",
		"anything-else-falls-back",
	}
	for _, status := range statuses {
		got := zombieTypeForStatus(status)
		if got != store.CanonicalIssueType(got) {
			t.Errorf("zombieTypeForStatus(%q) = %q, which is not canonical (canonical: %q)",
				status, got, store.CanonicalIssueType(got))
		}
	}
}

func TestCollectWarRoomIssuesPreservesSharedClassifierSeverity(t *testing.T) {
	scan := &clusterScan{wasteAudit: &analyzer.WasteAudit{StalePods: []analyzer.StalePod{
		{Name: "image-pod", Namespace: "apps", Kind: analyzer.StalePodZombie, Status: "ErrImagePull", Severity: "high"},
		{Name: "restart-pod", Namespace: "apps", Kind: analyzer.StalePodZombie, Status: "HighRestartCount", Severity: "medium"},
	}}}

	issues := collectWarRoomIssues(scan, 0)
	if len(issues) != 2 {
		t.Fatalf("collectWarRoomIssues returned %d issues, want 2: %+v", len(issues), issues)
	}
	got := map[string]string{}
	for _, issue := range issues {
		got[issue.Resource] = issue.Severity
	}
	if got["image-pod"] != "high" || got["restart-pod"] != "medium" {
		t.Fatalf("shared severities were not preserved: %+v", got)
	}
}

// TestHumanizeWRTypeAcceptsCanonicalAndAliases pins that the consumer
// recognizes both the canonical vocabulary and legacy aliases, so no
// producer output can silently fall through to the default branch.
func TestHumanizeWRTypeAcceptsCanonicalAndAliases(t *testing.T) {
	cases := map[string]string{
		// canonical (what producers emit today)
		store.IssueCrashLoop:            "Pod crash looping",
		store.IssueOOMKilled:            "Pod OOMKilled",
		store.IssueImagePullBackOff:     "Image pull failure",
		store.IssueUnprotectedNamespace: "Namespace",
		// legacy aliases (must normalize, not fall through)
		"oom_killed": "Pod OOMKilled",
		"image_pull": "Image pull failure",
	}
	for input, want := range cases {
		if got := humanizeWRType(input); got != want {
			t.Errorf("humanizeWRType(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestFormatGroupedIssueUsesCanonicalTitles pins that OOMKilled and
// ImagePullBackOff groups produce their dedicated titles instead of
// falling through to the generic default branch (the v1.11.0 bug).
func TestFormatGroupedIssueUsesCanonicalTitles(t *testing.T) {
	grp := []warRoomIssue{
		{Namespace: "payments", Resource: "api-7f9c4d5b6-abcde", Type: store.IssueOOMKilled, Severity: "critical"},
		{Namespace: "payments", Resource: "api-7f9c4d5b6-fghij", Type: store.IssueOOMKilled, Severity: "critical"},
	}
	title, _, countText, _ := formatGroupedIssue(store.IssueOOMKilled, "critical", grp)
	if title != "2 pods OOMKilled" {
		t.Errorf("OOMKilled group title = %q, want %q", title, "2 pods OOMKilled")
	}
	if countText != "2 pods" {
		t.Errorf("OOMKilled group countText = %q, want %q", countText, "2 pods")
	}

	grp2 := []warRoomIssue{
		{Namespace: "web", Resource: "frontend-abc123-xyzzy", Type: store.IssueImagePullBackOff, Severity: "high"},
	}
	title2, _, _, _ := formatGroupedIssue(store.IssueImagePullBackOff, "high", grp2)
	if title2 != "1 ImagePullBackOff failure" {
		t.Errorf("ImagePullBackOff group title = %q, want %q", title2, "1 ImagePullBackOff failure")
	}
}
