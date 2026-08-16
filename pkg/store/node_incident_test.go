package store

import (
	"fmt"
	"testing"
	"time"
)

func nodeStoreIncident(cluster, node, condition, details string) IncidentData {
	return IncidentData{
		Fingerprint: Fingerprint("cluster", "Node", node, condition),
		Resource:    node,
		IssueType:   condition,
		Severity:    "low",
		DetailsJSON: details,
	}
}

func TestNodeIncidentDetectionAndMutableEvidence(t *testing.T) {
	s := openTestStore(t)
	cluster := "cluster-a"
	inc := nodeStoreIncident(cluster, "worker-21", "DiskPressure", `{"node_pool":"system","correlated_workloads":[{"name":"checkout-api","pod_count":3}]}`)
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(detected): %v", err)
	}
	first, err := s.GetIncidentHistory(cluster, inc.Fingerprint)
	if err != nil || first == nil || first.Status != "active" {
		t.Fatalf("detected incident = %+v, %v", first, err)
	}
	events, err := s.GetIncidentTimeline(cluster, inc.Fingerprint)
	if err != nil || len(events) != 1 || events[0].EventType != "DETECTED" {
		t.Fatalf("initial events = %+v, %v", events, err)
	}

	inc.DetailsJSON = `{"node_pool":"replacement","reason":"updated","correlated_workloads":[{"name":"checkout-api","pod_count":2},{"name":"exporter","pod_count":1}]}`
	if err := s.UpsertIncidents(cluster, "scan-2", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(update): %v", err)
	}
	second, err := s.GetIncidentHistory(cluster, inc.Fingerprint)
	if err != nil || second == nil {
		t.Fatalf("updated incident = %+v, %v", second, err)
	}
	if first.ID != second.ID || first.Fingerprint != second.Fingerprint || second.DetailsJSON != inc.DetailsJSON {
		t.Fatalf("evidence update changed identity or was not stored: first=%+v second=%+v", first, second)
	}
	var rowCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM incidents WHERE cluster=? AND fingerprint=?`, cluster, inc.Fingerprint).Scan(&rowCount); err != nil || rowCount != 1 {
		t.Fatalf("evidence update created duplicate rows: count=%d err=%v", rowCount, err)
	}
	events, _ = s.GetIncidentTimeline(cluster, inc.Fingerprint)
	if len(events) != 1 || events[0].EventType != "DETECTED" {
		t.Fatalf("evidence churn emitted lifecycle events: %+v", events)
	}
}

func TestNodeIncidentConditionIsolationAndDebouncedResolution(t *testing.T) {
	s := openTestStore(t)
	cluster := "cluster-a"
	disk := nodeStoreIncident(cluster, "worker-21", "DiskPressure", `{}`)
	memory := nodeStoreIncident(cluster, "worker-21", "MemoryPressure", `{}`)
	if disk.Fingerprint == memory.Fingerprint {
		t.Fatalf("conditions share fingerprint %q", disk.Fingerprint)
	}
	if err := s.UpsertIncidents(cluster, "scan-0", []IncidentData{disk, memory}); err != nil {
		t.Fatalf("UpsertIncidents(initial): %v", err)
	}

	for miss := 1; miss < resolveThreshold; miss++ {
		scanID := fmt.Sprintf("scan-miss-%d", miss)
		if err := s.UpsertIncidents(cluster, scanID, []IncidentData{memory}); err != nil {
			t.Fatalf("UpsertIncidents(%s): %v", scanID, err)
		}
		resolved, err := s.ResolveMissing(cluster, scanID)
		if err != nil || resolved != 0 {
			t.Fatalf("before threshold miss %d resolved=%d err=%v", miss, resolved, err)
		}
		rec, _ := s.GetIncidentHistory(cluster, disk.Fingerprint)
		if rec.Status != "active" {
			t.Fatalf("DiskPressure resolved before threshold at miss %d", miss)
		}
	}

	scanID := fmt.Sprintf("scan-miss-%d", resolveThreshold)
	if err := s.UpsertIncidents(cluster, scanID, []IncidentData{memory}); err != nil {
		t.Fatalf("UpsertIncidents(threshold): %v", err)
	}
	resolved, err := s.ResolveMissing(cluster, scanID)
	if err != nil || resolved != 1 {
		t.Fatalf("at threshold resolved=%d err=%v", resolved, err)
	}
	diskRec, _ := s.GetIncidentHistory(cluster, disk.Fingerprint)
	memoryRec, _ := s.GetIncidentHistory(cluster, memory.Fingerprint)
	if diskRec.Status != "resolved" || memoryRec.Status != "active" {
		t.Fatalf("condition isolation failed: disk=%s memory=%s", diskRec.Status, memoryRec.Status)
	}
}

func TestNodeIncidentReappearsBeforeThresholdAbsorbsFlap(t *testing.T) {
	s := openTestStore(t)
	cluster := "cluster-a"
	inc := nodeStoreIncident(cluster, "worker-21", "DiskPressure", `{}`)
	if err := s.UpsertIncidents(cluster, "scan-0", []IncidentData{inc}); err != nil {
		t.Fatal(err)
	}
	for miss := 1; miss < resolveThreshold; miss++ {
		scanID := fmt.Sprintf("scan-miss-%d", miss)
		if err := s.UpsertIncidents(cluster, scanID, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveMissing(cluster, scanID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertIncidents(cluster, "scan-return", []IncidentData{inc}); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetIncidentHistory(cluster, inc.Fingerprint)
	events, _ := s.GetIncidentTimeline(cluster, inc.Fingerprint)
	if rec.Status != "active" || len(events) != 1 || events[0].EventType != "DETECTED" {
		t.Fatalf("pre-threshold flap was not absorbed: rec=%+v events=%+v", rec, events)
	}
}

func TestNodeIncidentResolvedFlapAndTrueReopenUseExistingLifecycle(t *testing.T) {
	t.Run("recent resolution is absorbed", func(t *testing.T) {
		s := openTestStore(t)
		cluster := "cluster-a"
		inc := nodeStoreIncident(cluster, "worker-21", "DiskPressure", `{}`)
		if err := s.UpsertIncidents(cluster, "scan-0", []IncidentData{inc}); err != nil {
			t.Fatal(err)
		}
		driveToResolved(t, s, cluster, nil, "scan-miss")
		if err := s.UpsertIncidents(cluster, "scan-return", []IncidentData{inc}); err != nil {
			t.Fatal(err)
		}
		events, _ := s.GetIncidentTimeline(cluster, inc.Fingerprint)
		if len(events) != 1 || events[0].EventType != "DETECTED" {
			t.Fatalf("recent resolved flap emitted RESOLVED/REOPENED: %+v", events)
		}
	})

	t.Run("later recurrence reopens same incident", func(t *testing.T) {
		s := openTestStore(t)
		cluster := "cluster-a"
		inc := nodeStoreIncident(cluster, "worker-21", "DiskPressure", `{}`)
		if err := s.UpsertIncidents(cluster, "scan-0", []IncidentData{inc}); err != nil {
			t.Fatal(err)
		}
		driveToResolved(t, s, cluster, nil, "scan-miss")
		backdateIncidentEvents(t, s, cluster, inc.Fingerprint, flapAbsorptionWindow+time.Minute)
		if err := s.UpsertIncidents(cluster, "scan-return", []IncidentData{inc}); err != nil {
			t.Fatal(err)
		}
		rec, _ := s.GetIncidentHistory(cluster, inc.Fingerprint)
		events, _ := s.GetIncidentTimeline(cluster, inc.Fingerprint)
		if rec.Status != "active" || len(events) != 3 || events[0].EventType != "DETECTED" || events[1].EventType != "RESOLVED" || events[2].EventType != "REOPENED" {
			t.Fatalf("true reopen lifecycle mismatch: rec=%+v events=%+v", rec, events)
		}
	})
}

func TestNodeIncidentMultiNodeIsolation(t *testing.T) {
	s := openTestStore(t)
	cluster := "cluster-a"
	a := nodeStoreIncident(cluster, "worker-21", "DiskPressure", `{}`)
	b := nodeStoreIncident(cluster, "worker-22", "DiskPressure", `{}`)
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{a, b}); err != nil {
		t.Fatal(err)
	}
	items, total, err := s.QueryIncidents(IncidentFilter{Cluster: cluster})
	if err != nil || total != 2 || len(items) != 2 || a.Fingerprint == b.Fingerprint {
		t.Fatalf("multi-node incidents not isolated: total=%d items=%+v err=%v", total, items, err)
	}
}
