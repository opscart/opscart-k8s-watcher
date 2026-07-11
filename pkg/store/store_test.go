package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenAndMigrate(t *testing.T) {
	s := openTestStore(t)

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("expected user_version %d, got %d", schemaVersion, version)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := openTestStore(t)

	base := time.Now().Add(-48 * time.Hour)
	first := SnapshotData{
		ScannedAt:     base,
		IncidentScore: 40,
		CriticalCount: 2,
		WarningCount:  5,
		SecurityScore: 70,
		WasteCount:    3,
		MonthlyCost:   100.0,
	}
	second := SnapshotData{
		ScannedAt:     base.Add(25 * time.Hour),
		IncidentScore: 55,
		CriticalCount: 1,
		WarningCount:  4,
		SecurityScore: 80,
		WasteCount:    2,
		MonthlyCost:   120.0,
	}

	if err := s.WriteSnapshot("test-cluster", "scan-1", first); err != nil {
		t.Fatalf("WriteSnapshot(first): %v", err)
	}
	if err := s.WriteSnapshot("test-cluster", "scan-2", second); err != nil {
		t.Fatalf("WriteSnapshot(second): %v", err)
	}

	trend, err := s.GetOverviewTrend("test-cluster")
	if err != nil {
		t.Fatalf("GetOverviewTrend: %v", err)
	}
	if !trend.HasHistory {
		t.Fatalf("expected HasHistory=true")
	}
	if trend.IncidentScore.Current != 55 || trend.IncidentScore.Previous != 40 {
		t.Fatalf("unexpected IncidentScore trend: %+v", trend.IncidentScore)
	}
	if trend.IncidentScore.Delta != 15 || trend.IncidentScore.Direction != "up" {
		t.Fatalf("unexpected delta/direction: %+v", trend.IncidentScore)
	}
	if trend.CostDelta != 20.0 {
		t.Fatalf("expected CostDelta=20.0, got %v", trend.CostDelta)
	}
	if len(trend.ScoreHistory) != 2 || trend.ScoreHistory[0] != 40 || trend.ScoreHistory[1] != 55 {
		t.Fatalf("unexpected ScoreHistory: %v", trend.ScoreHistory)
	}
}

func TestUpsertIncident(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{
		Fingerprint: "default/Deployment/fraud-detection/crash_loop",
		Namespace:   "default",
		Resource:    "fraud-detection",
		IssueType:   "crash_loop",
		Severity:    "critical",
		DetailsJSON: `{"restarts":10}`,
	}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}

	rec1, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory(1): %v", err)
	}
	if rec1 == nil {
		t.Fatalf("expected incident record, got nil")
	}

	time.Sleep(1100 * time.Millisecond)

	inc.DetailsJSON = `{"restarts":11}`
	if err := s.UpsertIncidents("test-cluster", "scan-2", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(2): %v", err)
	}

	rec2, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory(2): %v", err)
	}
	if rec2 == nil {
		t.Fatalf("expected incident record, got nil")
	}

	if !rec1.FirstSeen.Equal(rec2.FirstSeen) {
		t.Fatalf("expected first_seen unchanged: %v vs %v", rec1.FirstSeen, rec2.FirstSeen)
	}
	if !rec2.LastSeen.After(rec1.LastSeen) {
		t.Fatalf("expected last_seen updated: %v vs %v", rec1.LastSeen, rec2.LastSeen)
	}
	if rec2.DetailsJSON != `{"restarts":11}` {
		t.Fatalf("expected details_json updated, got %s", rec2.DetailsJSON)
	}

	var count int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM incidents WHERE cluster=? AND fingerprint=?",
		"test-cluster", inc.Fingerprint,
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected single row, got %d", count)
	}
}

func TestResolveMissing(t *testing.T) {
	s := openTestStore(t)

	incA1 := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}
	incA2 := IncidentData{Fingerprint: "default/Deployment/svc-b/oom", Namespace: "default", Resource: "svc-b", IssueType: "oom", Severity: "warning"}

	if err := s.UpsertIncidents("test-cluster", "scan-a", []IncidentData{incA1, incA2}); err != nil {
		t.Fatalf("UpsertIncidents(scan-a): %v", err)
	}

	if err := s.UpsertIncidents("test-cluster", "scan-b", []IncidentData{incA1}); err != nil {
		t.Fatalf("UpsertIncidents(scan-b): %v", err)
	}

	resolved, err := s.ResolveMissing("test-cluster", "scan-b")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("expected 1 resolved incident, got %d", resolved)
	}

	recA, err := s.GetIncidentHistory("test-cluster", incA1.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory(A): %v", err)
	}
	if recA.Status != "active" {
		t.Fatalf("expected svc-a active, got %s", recA.Status)
	}

	recB, err := s.GetIncidentHistory("test-cluster", incA2.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory(B): %v", err)
	}
	if recB.Status != "resolved" {
		t.Fatalf("expected svc-b resolved, got %s", recB.Status)
	}
}

func TestOwnerNameFromPod(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"fraud-detection-5fc85b778-8j6s5", "fraud-detection"},
		{"stream-processor-66c474d5fd-9zpwq", "stream-processor"},
		{"namespace", "namespace"},
		{"nginx", "nginx"},
	}

	for _, c := range cases {
		got := OwnerNameFromPod(c.in)
		if got != c.want {
			t.Errorf("OwnerNameFromPod(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNullStore(t *testing.T) {
	var s Store = NullStore{}

	if err := s.WriteSnapshot("c", "scan", SnapshotData{}); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if err := s.UpsertIncidents("c", "scan", nil); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}
	if resolved, err := s.ResolveMissing("c", "scan"); err != nil || resolved != 0 {
		t.Fatalf("ResolveMissing: %d, %v", resolved, err)
	}
	if err := s.WriteScanHistory("c", "scan", ScanMeta{}); err != nil {
		t.Fatalf("WriteScanHistory: %v", err)
	}
	trend, err := s.GetOverviewTrend("c")
	if err != nil || trend == nil || trend.HasHistory {
		t.Fatalf("GetOverviewTrend: %+v, %v", trend, err)
	}
	snap, err := s.GetLatestSnapshot("c")
	if err != nil || snap != nil {
		t.Fatalf("GetLatestSnapshot: %+v, %v", snap, err)
	}
	rec, err := s.GetIncidentHistory("c", "fp")
	if err != nil || rec != nil {
		t.Fatalf("GetIncidentHistory: %+v, %v", rec, err)
	}
	tl, err := s.GetIncidentTimeline("c", "fp")
	if err != nil || tl != nil {
		t.Fatalf("GetIncidentTimeline: %+v, %v", tl, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ── Incident timeline / events ──────────────────────────────────────────────

func TestTimeline_NewIncidentEmitsDetected(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{
		Fingerprint: "default/Deployment/svc-a/crash_loop",
		Namespace:   "default", Resource: "svc-a", IssueType: "crash_loop",
		Severity: "critical", RestartCount: 5,
	}
	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].EventType != "DETECTED" || events[0].EventReason != "Detected" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
	if events[0].Message != "crash_loop first detected" {
		t.Fatalf("unexpected message: %q", events[0].Message)
	}
}

func TestTimeline_RestartMilestoneSingleCrossing(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{
		Fingerprint: "default/Deployment/svc-a/crash_loop",
		Namespace:   "default", Resource: "svc-a", IssueType: "crash_loop",
		Severity: "critical", RestartCount: 90,
	}
	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}

	inc.RestartCount = 110
	if err := s.UpsertIncidents("test-cluster", "scan-2", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(2): %v", err)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	// DETECTED (scan-1) + RestartMilestone (scan-2)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	milestone := events[1]
	if milestone.EventType != "UPDATED" || milestone.EventReason != "RestartMilestone" {
		t.Fatalf("unexpected event: %+v", milestone)
	}
	if milestone.Message != "Restart count exceeded 100" {
		t.Fatalf("unexpected message: %q", milestone.Message)
	}
}

func TestTimeline_RestartMilestoneHighestOnly(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{
		Fingerprint: "default/Deployment/svc-a/crash_loop",
		Namespace:   "default", Resource: "svc-a", IssueType: "crash_loop",
		Severity: "critical", RestartCount: 90,
	}
	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}

	inc.RestartCount = 600
	if err := s.UpsertIncidents("test-cluster", "scan-2", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(2): %v", err)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (DETECTED + one milestone), got %d: %+v", len(events), events)
	}
	milestone := events[1]
	if milestone.EventReason != "RestartMilestone" || milestone.Message != "Restart count exceeded 500" {
		t.Fatalf("expected single highest milestone 500, got: %+v", milestone)
	}
}

func TestTimeline_NoEventWithoutMilestoneCrossing(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{
		Fingerprint: "default/Deployment/svc-a/crash_loop",
		Namespace:   "default", Resource: "svc-a", IssueType: "crash_loop",
		Severity: "critical", RestartCount: 110,
	}
	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}

	inc.RestartCount = 115
	if err := s.UpsertIncidents("test-cluster", "scan-2", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(2): %v", err)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	// only the initial DETECTED event, no milestone/no-op event for 110->115
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
}

func TestTimeline_SeverityChanged(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{
		Fingerprint: "default/Deployment/svc-a/crash_loop",
		Namespace:   "default", Resource: "svc-a", IssueType: "crash_loop",
		Severity: "critical",
	}
	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}

	inc.Severity = "warning"
	if err := s.UpsertIncidents("test-cluster", "scan-2", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(2): %v", err)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	sev := events[1]
	if sev.EventType != "UPDATED" || sev.EventReason != "SeverityChanged" {
		t.Fatalf("unexpected event: %+v", sev)
	}
	if sev.Message != "Severity changed critical → warning" {
		t.Fatalf("unexpected message: %q", sev.Message)
	}
}

func TestTimeline_ResolvedEmitsEventAndStatus(t *testing.T) {
	s := openTestStore(t)

	incA := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}
	incB := IncidentData{Fingerprint: "default/Deployment/svc-b/oom", Namespace: "default", Resource: "svc-b", IssueType: "oom", Severity: "warning"}

	if err := s.UpsertIncidents("test-cluster", "scan-a", []IncidentData{incA, incB}); err != nil {
		t.Fatalf("UpsertIncidents(scan-a): %v", err)
	}
	if err := s.UpsertIncidents("test-cluster", "scan-b", []IncidentData{incA}); err != nil {
		t.Fatalf("UpsertIncidents(scan-b): %v", err)
	}
	if _, err := s.ResolveMissing("test-cluster", "scan-b"); err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}

	rec, err := s.GetIncidentHistory("test-cluster", incB.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec.Status != "resolved" {
		t.Fatalf("expected svc-b resolved, got %s", rec.Status)
	}

	events, err := s.GetIncidentTimeline("test-cluster", incB.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (DETECTED + RESOLVED), got %d: %+v", len(events), events)
	}
	resolved := events[1]
	if resolved.EventType != "RESOLVED" || resolved.EventReason != "Resolved" {
		t.Fatalf("unexpected event: %+v", resolved)
	}
	if resolved.Message != "Incident resolved" || resolved.State != "resolved" {
		t.Fatalf("unexpected event: %+v", resolved)
	}
}

func TestTimeline_ReopenedAfterResolve(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}
	// resolve it: incident absent from scan-2
	if err := s.UpsertIncidents("test-cluster", "scan-2", nil); err != nil {
		t.Fatalf("UpsertIncidents(2): %v", err)
	}
	if _, err := s.ResolveMissing("test-cluster", "scan-2"); err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}

	// reappears in scan-3
	if err := s.UpsertIncidents("test-cluster", "scan-3", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(3): %v", err)
	}

	rec, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec.Status != "active" {
		t.Fatalf("expected active after reopen, got %s", rec.Status)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events (DETECTED, RESOLVED, REOPENED), got %d: %+v", len(events), events)
	}
	if events[0].EventType != "DETECTED" {
		t.Fatalf("event[0] = %+v", events[0])
	}
	if events[1].EventType != "RESOLVED" {
		t.Fatalf("event[1] = %+v", events[1])
	}
	if events[2].EventType != "REOPENED" || events[2].EventReason != "Reopened" {
		t.Fatalf("event[2] = %+v", events[2])
	}
}

func TestTimeline_OrderedOldestFirst(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical", RestartCount: 5}
	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}

	inc.Severity = "warning"
	if err := s.UpsertIncidents("test-cluster", "scan-2", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(2): %v", err)
	}

	inc.RestartCount = 20
	if err := s.UpsertIncidents("test-cluster", "scan-3", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(3): %v", err)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}
	for i := 1; i < len(events); i++ {
		if events[i].OccurredAt.Before(events[i-1].OccurredAt) {
			t.Fatalf("events not ordered oldest-first: %+v", events)
		}
	}
	if events[0].EventType != "DETECTED" {
		t.Fatalf("expected first event DETECTED, got %+v", events[0])
	}
	if events[1].EventReason != "SeverityChanged" {
		t.Fatalf("expected second event SeverityChanged, got %+v", events[1])
	}
	if events[2].EventReason != "RestartMilestone" {
		t.Fatalf("expected third event RestartMilestone, got %+v", events[2])
	}
}

// ── Migration ────────────────────────────────────────────────────────────────

// createLegacySchema creates a database matching a pre-id, pre-events
// version of the incidents table, to exercise the rebuild-on-migrate path.
func createLegacySchema(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE incidents (
		    fingerprint  TEXT    NOT NULL,
		    cluster      TEXT    NOT NULL,
		    namespace    TEXT    NOT NULL,
		    resource     TEXT    NOT NULL,
		    issue_type   TEXT    NOT NULL,
		    severity     TEXT    NOT NULL,
		    first_seen   INTEGER NOT NULL,
		    last_seen    INTEGER NOT NULL,
		    details_json TEXT,
		    status       TEXT    NOT NULL DEFAULT 'active',
		    last_scan_id TEXT
		);
		CREATE UNIQUE INDEX idx_inc_fp ON incidents(cluster, fingerprint);

		CREATE TABLE cluster_snapshots (
		    id              INTEGER PRIMARY KEY AUTOINCREMENT,
		    scan_id         TEXT    NOT NULL,
		    cluster         TEXT    NOT NULL,
		    scanned_at      INTEGER NOT NULL,
		    incident_score  INTEGER NOT NULL,
		    critical_count  INTEGER NOT NULL,
		    warning_count   INTEGER NOT NULL,
		    security_score  INTEGER NOT NULL,
		    waste_count     INTEGER NOT NULL,
		    monthly_cost    REAL    NOT NULL,
		    pod_count       INTEGER,
		    namespace_count INTEGER,
		    node_count      INTEGER
		);

		CREATE TABLE scan_history (
		    id          INTEGER PRIMARY KEY AUTOINCREMENT,
		    scan_id     TEXT    NOT NULL,
		    cluster     TEXT    NOT NULL,
		    scanned_at  INTEGER NOT NULL,
		    duration_ms INTEGER,
		    success     INTEGER NOT NULL DEFAULT 1,
		    error       TEXT,
		    version     TEXT
		);

		INSERT INTO incidents (fingerprint, cluster, namespace, resource, issue_type, severity, first_seen, last_seen, details_json, status, last_scan_id)
		VALUES ('default/Deployment/svc-a/crash_loop', 'test-cluster', 'default', 'svc-a', 'crash_loop', 'critical', 1000, 2000, '{}', 'active', 'scan-old-1');
		INSERT INTO incidents (fingerprint, cluster, namespace, resource, issue_type, severity, first_seen, last_seen, details_json, status, last_scan_id)
		VALUES ('default/Deployment/svc-b/oom', 'test-cluster', 'default', 'svc-b', 'oom', 'warning', 1500, 2500, '{}', 'resolved', 'scan-old-1');

		PRAGMA user_version = 1;
	`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
}

func TestMigration_RebuildsLegacyIncidentsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacySchema(t, path)

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()

	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM incidents").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows preserved, got %d", count)
	}

	rows, err := s.db.Query("SELECT id, fingerprint FROM incidents ORDER BY id")
	if err != nil {
		t.Fatalf("query ids: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		var fp string
		if err := rows.Scan(&id, &fp); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if id == 0 {
			t.Fatalf("expected non-zero id for fingerprint %s", fp)
		}
		ids = append(ids, id)
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("expected 2 distinct assigned ids, got %v", ids)
	}

	rec, err := s.GetIncidentHistory("test-cluster", "default/Deployment/svc-a/crash_loop")
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec == nil || rec.Status != "active" {
		t.Fatalf("expected preserved active incident, got %+v", rec)
	}

	// incident_events table should now exist and be queryable.
	if _, err := s.GetIncidentTimeline("test-cluster", "default/Deployment/svc-a/crash_loop"); err != nil {
		t.Fatalf("GetIncidentTimeline after migration: %v", err)
	}
}

func TestMigration_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacySchema(t, path)

	s1, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(1): %v", err)
	}
	var countAfterFirst int
	if err := s1.db.QueryRow("SELECT COUNT(*) FROM incidents").Scan(&countAfterFirst); err != nil {
		t.Fatalf("count after first open: %v", err)
	}
	s1.Close()

	// Reopen: migration should be a no-op the second time.
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(2): %v", err)
	}
	defer s2.Close()

	var countAfterSecond int
	if err := s2.db.QueryRow("SELECT COUNT(*) FROM incidents").Scan(&countAfterSecond); err != nil {
		t.Fatalf("count after second open: %v", err)
	}
	if countAfterSecond != countAfterFirst {
		t.Fatalf("data loss on re-migration: first=%d second=%d", countAfterFirst, countAfterSecond)
	}

	var version int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("expected user_version=%d, got %d", schemaVersion, version)
	}
}
