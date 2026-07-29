package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

func TestRenderIncidentsUsesWorkloadIdentity(t *testing.T) {
	data := incidentsPageData{
		ClusterName:  "opscart",
		ClusterParam: "opscart",
		Page:         1,
		PerPage:      50,
		Total:        2,
		TotalPages:   1,
		Incidents: []store.IncidentSummary{
			{
				Namespace: "payments", Resource: "fraud-detection-7cddf79d98-jxmtx",
				IssueType: "crash_loop", Severity: "critical", Status: "active",
				ReopenCount: 1095, FirstSeen: time.Now().Add(-26 * 24 * time.Hour),
			},
			{
				Namespace: "monitoring", Resource: "namespace",
				IssueType: "idle_namespace", Severity: "high", Status: "active",
			},
		},
	}
	rec := httptest.NewRecorder()
	renderIncidents(rec, data)
	body := rec.Body.String()

	for _, want := range []string{
		"<th>Workload</th>",
		"Workload/fraud-detection",
		"currently: fraud-detection-7cddf79d98-jxmtx",
		"crash_loop",
		"Namespace/monitoring",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered incidents page missing %q", want)
		}
	}
	if strings.Contains(body, "×1095") {
		t.Error("rendered incidents page exposed an untrusted recurrence count")
	}
	if strings.Contains(body, "cluster=current-context") {
		t.Error("rendered incidents page leaked current-context into a URL")
	}
	if strings.Contains(body, "/investigate?pod=namespace") {
		t.Error("namespace incident link included a synthetic pod parameter")
	}
	if !strings.Contains(body, "/investigate?ns=monitoring") {
		t.Error("namespace incident link omitted namespace")
	}
	if !strings.Contains(body, "/investigate?pod=fraud-detection-7cddf79d98-jxmtx") {
		t.Error("workload incident link omitted the Focus Pod")
	}
}
