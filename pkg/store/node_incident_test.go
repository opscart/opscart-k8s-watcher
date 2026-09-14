package store

import (
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

func TestNodeIncidentHealthyActiveResolvedBoundary(t *testing.T) {
	s := openTestStore(t)
	cluster := "cluster-a"
	if err := s.UpsertIncidents(cluster, "scan-healthy", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveMissing(cluster, "scan-healthy"); err != nil {
		t.Fatal(err)
	}
	if _, total, err := s.QueryIncidents(IncidentFilter{Cluster: cluster}); err != nil || total != 0 {
		t.Fatalf("healthy baseline created incidents: total=%d err=%v", total, err)
	}

	inc := nodeStoreIncident(cluster, "worker-21", "DiskPressure", `{}`)
	if err := s.UpsertIncidents(cluster, "scan-active", []IncidentData{inc}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpsertIncidents(cluster, "scan-absent", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveMissing(cluster, "scan-absent"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.GetIncidentHistory(cluster, inc.Fingerprint); rec == nil || rec.Status != "active" {
		t.Fatalf("resolved before resolveAfter elapsed: %+v", rec)
	}

	backdateAbsentSince(t, s, cluster, inc.Fingerprint, resolveAfter)
	if _, err := s.ResolveMissing(cluster, "scan-absent"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.GetIncidentHistory(cluster, inc.Fingerprint); rec == nil || rec.Status != "resolved" {
		t.Fatalf("expected resolved at resolveAfter, got %+v", rec)
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

	// disk stays present every scan; memory goes absent.
	if err := s.UpsertIncidents(cluster, "scan-miss", []IncidentData{disk}); err != nil {
		t.Fatalf("UpsertIncidents(miss): %v", err)
	}
	if resolved, err := s.ResolveMissing(cluster, "scan-miss"); err != nil || resolved != 0 {
		t.Fatalf("resolved before resolveAfter elapsed: resolved=%d err=%v", resolved, err)
	}
	if rec, _ := s.GetIncidentHistory(cluster, disk.Fingerprint); rec.Status != "active" {
		t.Fatalf("DiskPressure resolved prematurely: %+v", rec)
	}
	if rec, _ := s.GetIncidentHistory(cluster, memory.Fingerprint); rec.Status != "active" {
		t.Fatalf("MemoryPressure resolved before resolveAfter elapsed: %+v", rec)
	}

	backdateAbsentSince(t, s, cluster, memory.Fingerprint, resolveAfter)
	resolved, err := s.ResolveMissing(cluster, "scan-miss")
	if err != nil || resolved != 1 {
		t.Fatalf("at resolveAfter resolved=%d err=%v", resolved, err)
	}
	diskRec, _ := s.GetIncidentHistory(cluster, disk.Fingerprint)
	memoryRec, _ := s.GetIncidentHistory(cluster, memory.Fingerprint)
	if diskRec.Status != "active" || memoryRec.Status != "resolved" {
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
	if err := s.UpsertIncidents(cluster, "scan-miss", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveMissing(cluster, "scan-miss"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertIncidents(cluster, "scan-return", []IncidentData{inc}); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetIncidentHistory(cluster, inc.Fingerprint)
	events, _ := s.GetIncidentTimeline(cluster, inc.Fingerprint)
	if rec.Status != "active" || len(events) != 1 || events[0].EventType != "DETECTED" {
		t.Fatalf("pre-duration flap was not absorbed: rec=%+v events=%+v", rec, events)
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
