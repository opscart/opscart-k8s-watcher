package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

func TestIncidentSummaryCountsMixedSeverities(t *testing.T) {
	rec := httptest.NewRecorder()
	renderIncidents(rec, incidentsPageData{
		ClusterName: "prod", ClusterParam: "prod", Filter: store.IncidentFilter{SortBy: "priority"},
		ActiveCritical: 17, ActiveHigh: 1, ActiveMedium: 2, ResolvedCount: 2,
		Incidents: []store.IncidentSummary{{
			Fingerprint: "apps/Deployment/api/crash_loop", Namespace: "apps", Resource: "api-pod",
			IssueType: "crash_loop", Severity: "critical", Status: "active", FirstSeen: time.Now(), LastSeen: time.Now(),
		}},
		Total: 1, Page: 1, PerPage: 50, TotalPages: 1,
	})
	body := rec.Body.String()
	for _, want := range []string{"17</b> active critical", "1</b> active high", "2</b> active medium", "2</b> resolved"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary missing %q", want)
		}
	}
	if !strings.Contains(body, "active first, then severity, newest first") {
		t.Fatal("default ordering footer is inaccurate")
	}
}

func TestIncidentTrendApplicabilityRendering(t *testing.T) {
	rec := httptest.NewRecorder()
	now := time.Now()
	renderIncidents(rec, incidentsPageData{
		ClusterName: "prod", ClusterParam: "prod", Filter: store.IncidentFilter{SortBy: "priority"},
		Incidents: []store.IncidentSummary{
			{Fingerprint: "apps/Deployment/api/crash_loop", Namespace: "apps", Resource: "api-pod", IssueType: "crash_loop", Severity: "critical", Status: "active", Trend: "stable", FirstSeen: now, LastSeen: now},
			{Fingerprint: "monitoring/Namespace/monitoring/unprotected_namespace", Namespace: "monitoring", Resource: "namespace", IssueType: "unprotected_namespace", Severity: "high", Status: "active", Trend: "stable", FirstSeen: now, LastSeen: now},
		},
		Total: 2, Page: 1, PerPage: 50, TotalPages: 1,
	})
	body := rec.Body.String()
	if strings.Count(body, "→ stable") != 1 {
		t.Fatalf("stable restart trend rendered for a non-applicable finding")
	}
	if !strings.Contains(body, "Restart trend does not apply to this finding.") {
		t.Fatal("non-applicable trend lacks accessible explanation")
	}
}

func TestWarRoomCanonicalFilterQuery(t *testing.T) {
	got, changed := canonicalWarRoomQuery(map[string][]string{
		"cluster": {"real-context"}, "q": {""}, "severity": {""}, "type": {""}, "limit": {"12"},
	})
	if !changed || got.Encode() != "cluster=real-context" {
		t.Fatalf("canonical query = %q, changed=%v", got.Encode(), changed)
	}
}

func TestNamespaceInvestigationUsesEvaluateWording(t *testing.T) {
	hints := investigationHints("unprotected_namespace", "", 0, nil, "monitoring")
	if len(hints) == 0 || hints[0].Title != "Evaluate a default-deny NetworkPolicy" {
		t.Fatalf("unexpected first hint: %+v", hints)
	}
	if hints[0].Command != "kubectl get networkpolicies -n monitoring" {
		t.Fatalf("read-only command changed: %q", hints[0].Command)
	}
}
