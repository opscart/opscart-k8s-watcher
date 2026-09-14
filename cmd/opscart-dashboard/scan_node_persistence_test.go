package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// backdateAbsentSince establishes an incident's absence as having started
// age ago, without sleeping in tests. pkg/store measures resolution from
// the incidents.absent_since column (the first-observed-absence timestamp,
// distinct from last_seen), so setting it directly is equivalent to the
// incident having already been absent for age of real time. A separate raw
// connection is used because store.Store exposes no such seam by design —
// see pkg/store/incident_resolution.go.
func backdateAbsentSince(t *testing.T, dbPath, cluster, fingerprint string, age time.Duration) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(
		`UPDATE incidents SET absent_since = ? WHERE cluster=? AND fingerprint=?`,
		time.Now().Add(-age).Unix(), cluster, fingerprint,
	); err != nil {
		t.Fatalf("backdate absent_since: %v", err)
	}
}

type incidentPersistenceSpy struct {
	store.NullStore
	upsertCalls  int
	resolveCalls int
	incidents    []store.IncidentData
}

func (s *incidentPersistenceSpy) UpsertIncidents(_ string, _ string, incidents []store.IncidentData) error {
	s.upsertCalls++
	s.incidents = append([]store.IncidentData(nil), incidents...)
	return nil
}

func (s *incidentPersistenceSpy) ResolveMissing(_ string, _ string) (int, error) {
	s.resolveCalls++
	return 0, nil
}

func TestDashboardPersistsOneCompleteWorkloadAndNodeBatch(t *testing.T) {
	workloads := []store.IncidentData{{
		Fingerprint: "prod/Workload/api/crash_loop", Namespace: "prod", Resource: "api-pod",
		IssueType: "crash_loop", Severity: "critical",
	}}
	nodes := []models.NodeConditionFinding{{NodeName: "worker-21", ConditionType: "DiskPressure", ConditionStatus: "True"}}
	complete := completeIncidentBatch(workloads, nodes)
	spy := &incidentPersistenceSpy{}
	if _, err := persistCompleteIncidentBatch(spy, "cluster-a", "scan-1", complete); err != nil {
		t.Fatal(err)
	}
	if spy.upsertCalls != 1 || spy.resolveCalls != 1 {
		t.Fatalf("calls: upsert=%d resolve=%d, want one each", spy.upsertCalls, spy.resolveCalls)
	}
	if len(spy.incidents) != 2 || spy.incidents[0].Fingerprint != workloads[0].Fingerprint || spy.incidents[1].Fingerprint != "cluster/Node/worker-21/DiskPressure" {
		t.Fatalf("complete incident batch = %+v", spy.incidents)
	}
}

func TestDashboardNoNodeConditionsLeavesWorkloadBatchUnchanged(t *testing.T) {
	workloads := []store.IncidentData{{Fingerprint: "prod/Workload/api/crash_loop"}}
	complete := completeIncidentBatch(workloads, nil)
	if len(complete) != 1 || complete[0] != workloads[0] {
		t.Fatalf("workload-only batch changed: %+v", complete)
	}
}

func TestCompleteScanBatchCategoriesDoNotAgeEachOther(t *testing.T) {
	workload := store.IncidentData{
		Fingerprint: "prod/Workload/api/crash_loop", Namespace: "prod", Resource: "api-pod",
		IssueType: "crash_loop", Severity: "critical",
	}
	nodeFinding := models.NodeConditionFinding{NodeName: "worker-21", ConditionType: "DiskPressure", ConditionStatus: "True"}
	node := scanner.NodeConditionIncidents([]models.NodeConditionFinding{nodeFinding})[0]

	for _, tc := range []struct {
		name          string
		present       []store.IncidentData
		resolvedFP    string
		stillActiveFP string
	}{
		{"healthy node scan preserves workload", []store.IncidentData{workload}, node.Fingerprint, workload.Fingerprint},
		{"workload absence preserves node", []store.IncidentData{node}, workload.Fingerprint, node.Fingerprint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "complete-batch.db")
			db, err := store.OpenSQLite(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := persistCompleteIncidentBatch(db, "cluster-a", "scan-initial", []store.IncidentData{workload, node}); err != nil {
				t.Fatal(err)
			}
			// Long enough for resolution regardless of pkg/store's exact
			// resolveAfter value; this test only cares that one category's
			// absence duration never affects the other's.
			backdateAbsentSince(t, dbPath, "cluster-a", tc.resolvedFP, 5*time.Minute)
			if _, err := persistCompleteIncidentBatch(db, "cluster-a", "scan-resolve", tc.present); err != nil {
				t.Fatal(err)
			}
			resolved, _ := db.GetIncidentHistory("cluster-a", tc.resolvedFP)
			active, _ := db.GetIncidentHistory("cluster-a", tc.stillActiveFP)
			if resolved == nil || resolved.Status != "resolved" || active == nil || active.Status != "active" {
				t.Fatalf("complete batch cross-aged categories: resolved=%+v active=%+v", resolved, active)
			}
		})
	}
}
