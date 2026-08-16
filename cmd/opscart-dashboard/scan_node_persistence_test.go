package main

import (
	"testing"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

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
