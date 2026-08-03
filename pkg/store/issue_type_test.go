package store

import (
	"testing"
	"time"
)

func TestCanonicalIssueTypeAliasesAndFingerprint(t *testing.T) {
	tests := map[string]string{
		"CrashLoopBackOff": "crash_loop", "ProbeFailure": "probe_failure",
		"CrashLoopBackOff (ProbeFailure)": "probe_failure", "OOMKilled": "oomkilled",
		"oom_killed": "oomkilled", "CrashLoopBackOff (OOMKilled)": "oomkilled",
		"ImagePullBackOff": "image_pull_backoff", "ErrImagePull": "image_pull_backoff",
		"image_pull": "image_pull_backoff", "HighRestartCount": "high_restart_count",
		"custom_type": "custom_type",
	}
	for input, want := range tests {
		if got := CanonicalIssueType(input); got != want {
			t.Errorf("CanonicalIssueType(%q) = %q, want %q", input, got, want)
		}
		if got := Fingerprint("ns", "Workload", "api", input); got != "ns/Workload/api/"+want {
			t.Errorf("Fingerprint(%q) = %q", input, got)
		}
	}
}

func TestLegacyAliasReuseChoosesEarliestAndPreservesTimeline(t *testing.T) {
	s := openTestStore(t)
	cluster := "legacy-cluster"
	now := time.Now().Unix()
	legacyFP := "prod/Workload/api/CrashLoopBackOff"
	newerFP := "prod/Workload/api/crash_loop"
	res, err := s.db.Exec(`INSERT INTO incidents
		(fingerprint,cluster,namespace,resource,issue_type,severity,first_seen,last_seen,status,last_scan_id,current_restart_count,missing_scans)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, legacyFP, cluster, "prod", "api-old", "CrashLoopBackOff", "critical", now-86400, now-100, "active", "old", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	legacyID, _ := res.LastInsertId()
	if err := insertIncidentEventTxForTest(s, legacyID, "legacy", now-86400); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO incidents
		(fingerprint,cluster,namespace,resource,issue_type,severity,first_seen,last_seen,status,last_scan_id,current_restart_count,missing_scans)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, newerFP, cluster, "prod", "api-new", "crash_loop", "critical", now-3600, now-100, "active", "old", 20, 0); err != nil {
		t.Fatal(err)
	}

	inc := IncidentData{Fingerprint: legacyFP, Namespace: "prod", Resource: "api-live", IssueType: "CrashLoopBackOff", Severity: "critical", RestartCount: 11}
	if err := s.UpsertIncidents(cluster, "scan-new", []IncidentData{inc}); err != nil {
		t.Fatal(err)
	}

	rec, err := s.GetIncidentHistory(cluster, newerFP)
	if err != nil || rec == nil {
		t.Fatalf("history: %+v, %v", rec, err)
	}
	if rec.FirstSeen.Unix() != now-86400 {
		t.Fatalf("first_seen = %v", rec.FirstSeen)
	}
	timeline, err := s.GetIncidentTimeline(cluster, newerFP)
	if err != nil || len(timeline) != 1 || timeline[0].EventType != "DETECTED" {
		t.Fatalf("timeline was not preserved or another DETECTED was emitted: %+v, %v", timeline, err)
	}
	var legacyScan, duplicateScan string
	if err := s.db.QueryRow(`SELECT last_scan_id FROM incidents WHERE fingerprint=?`, legacyFP).Scan(&legacyScan); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT last_scan_id FROM incidents WHERE fingerprint=?`, newerFP).Scan(&duplicateScan); err != nil {
		t.Fatal(err)
	}
	if legacyScan != "scan-new" || duplicateScan != "old" {
		t.Fatalf("selected=%q duplicate=%q", legacyScan, duplicateScan)
	}

	items, total, err := s.QueryIncidents(IncidentFilter{Cluster: cluster, IssueType: "crash_loop"})
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("alias filter: total=%d items=%d err=%v", total, len(items), err)
	}
	_, total, err = s.QueryIncidents(IncidentFilter{Cluster: cluster, Text: "crash_loop"})
	if err != nil || total != 2 {
		t.Fatalf("alias text filter: total=%d err=%v", total, err)
	}
}

func TestSemanticAliasSelectionPrefersActiveThenEarliest(t *testing.T) {
	tests := []struct {
		name            string
		legacyStatus    string
		canonicalStatus string
		wantFingerprint string
		wantFirstAge    int64
	}{
		{"newer active canonical beats older resolved legacy", "resolved", "active", "prod/Workload/api/crash_loop", 3600},
		{"older active legacy beats newer active canonical", "active", "active", "prod/Workload/api/CrashLoopBackOff", 86400},
		{"earliest resolved alias wins when none active", "resolved", "resolved", "prod/Workload/api/CrashLoopBackOff", 86400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			cluster := "selection"
			now := time.Now().Unix()
			legacyFP := "prod/Workload/api/CrashLoopBackOff"
			canonicalFP := "prod/Workload/api/crash_loop"
			ids := make(map[string]int64)
			for _, row := range []struct {
				fp, issueType, status string
				firstAge              int64
			}{{legacyFP, "CrashLoopBackOff", tt.legacyStatus, 86400}, {canonicalFP, "crash_loop", tt.canonicalStatus, 3600}} {
				res, err := s.db.Exec(`INSERT INTO incidents
					(fingerprint,cluster,namespace,resource,issue_type,severity,first_seen,last_seen,status,last_scan_id,current_restart_count,missing_scans)
					VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, row.fp, cluster, "prod", "api", row.issueType, "critical", now-row.firstAge, now, row.status, "old", 10, 0)
				if err != nil {
					t.Fatal(err)
				}
				ids[row.fp], _ = res.LastInsertId()
				if _, err := s.db.Exec(`INSERT INTO incident_events
					(incident_id,scan_id,occurred_at,event_type,event_reason,message)
					VALUES (?,?,?,?,?,?)`, ids[row.fp], "seed", now-row.firstAge, "DETECTED", "Detected", row.fp); err != nil {
					t.Fatal(err)
				}
			}

			rec, err := s.GetIncidentHistory(cluster, canonicalFP)
			if err != nil || rec == nil || rec.Status != map[string]string{legacyFP: tt.legacyStatus, canonicalFP: tt.canonicalStatus}[tt.wantFingerprint] || rec.FirstSeen.Unix() != now-tt.wantFirstAge {
				t.Fatalf("history=%+v err=%v", rec, err)
			}
			batch, err := s.BatchGetIncidentHistory(cluster, []string{canonicalFP})
			if err != nil || batch[canonicalFP] == nil || batch[canonicalFP].ID != ids[tt.wantFingerprint] {
				t.Fatalf("batch=%+v err=%v", batch, err)
			}
			timeline, err := s.GetIncidentTimeline(cluster, canonicalFP)
			if err != nil || len(timeline) != 1 || timeline[0].Message != tt.wantFingerprint {
				t.Fatalf("timeline=%+v err=%v", timeline, err)
			}

			if err := s.UpsertIncidents(cluster, "selected", []IncidentData{{Fingerprint: canonicalFP, Namespace: "prod", Resource: "api", IssueType: "crash_loop", Severity: "critical", RestartCount: 11}}); err != nil {
				t.Fatal(err)
			}
			var selectedScan string
			if err := s.db.QueryRow(`SELECT last_scan_id FROM incidents WHERE id=?`, ids[tt.wantFingerprint]).Scan(&selectedScan); err != nil || selectedScan != "selected" {
				t.Fatalf("selected last_scan_id=%q err=%v", selectedScan, err)
			}
		})
	}
}

func insertIncidentEventTxForTest(s *SQLiteStore, id int64, scanID string, at int64) error {
	_, err := s.db.Exec(`INSERT INTO incident_events
		(incident_id,scan_id,occurred_at,event_type,event_reason,restart_count,severity,state,message)
		VALUES (?,?,?,?,?,?,?,?,?)`, id, scanID, at, "DETECTED", "Detected", 10, "critical", "active", "legacy")
	return err
}

func TestEarliestRetainedObservationIsClusterScopedAndBoundary(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Unix()
	if _, err := s.db.Exec(`INSERT INTO scan_history (scan_id,cluster,scanned_at,success) VALUES
		('failed','a',?,0),('success','a',?,1),('other','b',?,1)`, now-9000, now-5000, now-10000); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetEarliestRetainedObservation("a")
	if err != nil || got.Unix() != now-5000 {
		t.Fatalf("got=%v err=%v", got, err)
	}
	unknown, err := s.GetEarliestRetainedObservation("unknown")
	if err != nil || !unknown.IsZero() {
		t.Fatalf("unknown=%v err=%v", unknown, err)
	}
	if !AtObservationBoundary(got.Add(5*time.Minute), got) || AtObservationBoundary(got.Add(5*time.Minute+time.Second), got) {
		t.Fatal("five-minute boundary is not inclusive and exact")
	}
}

func TestEarliestRetainedObservationFallsBackToIncidents(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Unix()
	if _, err := s.db.Exec(`INSERT INTO incidents
		(fingerprint,cluster,namespace,resource,issue_type,severity,first_seen,last_seen,status,current_restart_count,missing_scans)
		VALUES ('ns/Workload/api/crash_loop','fallback','ns','api','crash_loop','critical',?,?, 'active',0,0)`, now-7200, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetEarliestRetainedObservation("fallback")
	if err != nil || got.Unix() != now-7200 {
		t.Fatalf("fallback=%v err=%v", got, err)
	}
}

func TestRestartTrendApplicabilityUsesCanonicalFamilies(t *testing.T) {
	for _, issueType := range []string{"crash_loop", "CrashLoopBackOff", "probe_failure", "ProbeFailure", "oomkilled", "OOMKilled"} {
		if !RestartTrendApplies(issueType) {
			t.Errorf("%q should support restart trends", issueType)
		}
	}
	for _, issueType := range []string{"image_pull_backoff", "ImagePullBackOff", "high_restart_count"} {
		if RestartTrendApplies(issueType) {
			t.Errorf("%q must not support restart trends", issueType)
		}
	}
}
