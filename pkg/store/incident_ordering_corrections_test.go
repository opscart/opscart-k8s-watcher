package store

import (
	"testing"
	"time"
)

func TestQueryIncidentsDefaultPriorityOrdering(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	high := IncidentData{Fingerprint: "monitoring/Namespace/monitoring/unprotected_namespace", Namespace: "monitoring", Resource: "namespace", IssueType: "unprotected_namespace", Severity: "high"}
	criticalNew := IncidentData{Fingerprint: "apps/Deployment/api/crash_loop", Namespace: "apps", Resource: "api-new", IssueType: "crash_loop", Severity: "critical"}
	criticalOld := IncidentData{Fingerprint: "apps/Deployment/worker/crash_loop", Namespace: "apps", Resource: "api-old", IssueType: "crash_loop", Severity: "critical"}
	resolved := IncidentData{Fingerprint: "apps/Deployment/resolved/crash_loop", Namespace: "apps", Resource: "resolved", IssueType: "crash_loop", Severity: "critical"}
	insertRawIncident(t, s, "test-cluster", high, "active", now, now)
	insertRawIncident(t, s, "test-cluster", criticalOld, "active", now.Add(-2*time.Hour), now)
	insertRawIncident(t, s, "test-cluster", criticalNew, "active", now.Add(-time.Hour), now)
	insertRawIncident(t, s, "test-cluster", resolved, "resolved", now.Add(time.Hour), now)

	items, _, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", SortBy: "priority"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"api-new", "api-old", "namespace", "resolved"}
	for i, resource := range want {
		if items[i].Resource != resource {
			t.Fatalf("order[%d] = %q, want %q; items=%+v", i, items[i].Resource, resource, items)
		}
	}
}

func TestRestartTrendApplicability(t *testing.T) {
	for _, issueType := range []string{
		"crash_loop",
		"oomkilled",
		"oom_killed",
		"probe_failure",
	} {
		if !RestartTrendApplies(issueType) {
			t.Errorf("%s should support restart trend", issueType)
		}
	}

	for _, issueType := range []string{
		"image_pull_backoff",
		"unprotected_namespace",
		"idle_namespace",
		"privileged_container",
		"orphaned_pvc",
		"zero_replica",
	} {
		if RestartTrendApplies(issueType) {
			t.Errorf("%s must not support restart trend", issueType)
		}
	}
}

func TestQueryIncidentsStaticFindingHasNoTrend(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	finding := IncidentData{Fingerprint: "monitoring/Namespace/monitoring/unprotected_namespace", Namespace: "monitoring", Resource: "namespace", IssueType: "unprotected_namespace", Severity: "high", RestartCount: 10}
	id := insertRawIncident(t, s, "test-cluster", finding, "active", now.Add(-24*time.Hour), now)
	insertRawEvent(t, s, id, "DETECTED", "Detected", 1, now.Add(-2*time.Hour))
	insertRawEvent(t, s, id, "UPDATED", "RestartMilestone", 10, now.Add(-time.Hour))
	items, _, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", SortBy: "priority"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Trend != "" {
		t.Fatalf("static finding received restart trend: %+v", items)
	}
}
