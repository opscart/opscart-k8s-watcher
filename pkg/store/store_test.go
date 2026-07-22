package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// driveToResolved runs ResolveMissing resolveThreshold times so that any
// active incident absent from `present` crosses the debounce threshold and
// flips to resolved. Mirrors the real scan loop: UpsertIncidents then
// ResolveMissing, once per simulated scan.
func driveToResolved(t *testing.T, s *SQLiteStore, cluster string, present []IncidentData, scanPrefix string) {
	t.Helper()
	for i := 0; i < resolveThreshold; i++ {
		scanID := fmt.Sprintf("%s-%d", scanPrefix, i)
		if err := s.UpsertIncidents(cluster, scanID, present); err != nil {
			t.Fatalf("UpsertIncidents(%s): %v", scanID, err)
		}
		if _, err := s.ResolveMissing(cluster, scanID); err != nil {
			t.Fatalf("ResolveMissing(%s): %v", scanID, err)
		}
	}
}

// backdateIncidentEvents shifts every recorded event for the given incident
// back in time by `age`, simulating a resolution (and the history leading
// up to it) that happened well in the past without sleeping in tests, since
// UpsertIncidents/ResolveMissing always stamp events with the real wall
// clock. Shifting the whole history (not just the RESOLVED event) preserves
// event ordering — only backdating RESOLVED would place it before DETECTED.
func backdateIncidentEvents(t *testing.T, s *SQLiteStore, cluster, fingerprint string, age time.Duration) {
	t.Helper()
	var incidentID int64
	if err := s.db.QueryRow(
		"SELECT id FROM incidents WHERE cluster=? AND fingerprint=?",
		cluster, fingerprint,
	).Scan(&incidentID); err != nil {
		t.Fatalf("lookup incident id: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE incident_events SET occurred_at = occurred_at - ? WHERE incident_id=?`,
		int64(age.Seconds()), incidentID,
	); err != nil {
		t.Fatalf("backdate incident events: %v", err)
	}
}

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

func TestWALModeActive(t *testing.T) {
	s := openTestStore(t)

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected journal_mode=wal, got %q", mode)
	}
}

func TestClose_SubsequentOpsFailCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "close.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := s.WriteSnapshot("test-cluster", "scan-1", SnapshotData{}); err == nil {
		t.Fatalf("expected WriteSnapshot to fail on a closed store")
	}
	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{{Fingerprint: "fp"}}); err == nil {
		t.Fatalf("expected UpsertIncidents to fail on a closed store")
	}
	if _, err := s.GetIncidentTimeline("test-cluster", "fp"); err == nil {
		t.Fatalf("expected GetIncidentTimeline to fail on a closed store")
	}

	// Close is safe to call again (mirrors main.go's single deferred Close
	// alongside any explicit shutdown-path close).
	if err := s.Close(); err != nil {
		t.Fatalf("second Close should not error, got: %v", err)
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

	// svc-b must be absent for resolveThreshold consecutive scans before
	// it resolves (debounced to avoid CrashLoopBackOff flapping).
	for i := 0; i < resolveThreshold-1; i++ {
		scanID := fmt.Sprintf("scan-b%d", i)
		if err := s.UpsertIncidents("test-cluster", scanID, []IncidentData{incA1}); err != nil {
			t.Fatalf("UpsertIncidents(%s): %v", scanID, err)
		}
		resolved, err := s.ResolveMissing("test-cluster", scanID)
		if err != nil {
			t.Fatalf("ResolveMissing(%s): %v", scanID, err)
		}
		if resolved != 0 {
			t.Fatalf("expected no resolution before threshold, got %d at iteration %d", resolved, i)
		}
		recB, err := s.GetIncidentHistory("test-cluster", incA2.Fingerprint)
		if err != nil {
			t.Fatalf("GetIncidentHistory(B): %v", err)
		}
		if recB.Status != "active" {
			t.Fatalf("expected svc-b still active before threshold, got %s", recB.Status)
		}
	}

	if err := s.UpsertIncidents("test-cluster", "scan-b-final", []IncidentData{incA1}); err != nil {
		t.Fatalf("UpsertIncidents(scan-b-final): %v", err)
	}
	resolved, err := s.ResolveMissing("test-cluster", "scan-b-final")
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
	sb, err := s.GetMemoryScoreboard("c")
	if err != nil || sb == nil {
		t.Fatalf("GetMemoryScoreboard: %+v, %v", sb, err)
	}
	events, err := s.GetRecentEvents("c", time.Now(), 10)
	if err != nil || events != nil {
		t.Fatalf("GetRecentEvents: %+v, %v", events, err)
	}
	changes, err := s.GetChangesSince("c", time.Now(), 10)
	if err != nil || changes != nil {
		t.Fatalf("GetChangesSince: %+v, %v", changes, err)
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
	driveToResolved(t, s, "test-cluster", []IncidentData{incA}, "scan-miss")

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
	// resolve it: incident absent for resolveThreshold consecutive scans
	driveToResolved(t, s, "test-cluster", nil, "scan-miss")
	// push the resolution outside flapAbsorptionWindow so this reappearance
	// reads as a genuine recurrence, not a flap
	backdateIncidentEvents(t, s, "test-cluster", inc.Fingerprint, flapAbsorptionWindow+time.Minute)

	// reappears after genuine resolution
	if err := s.UpsertIncidents("test-cluster", "scan-reopen", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(reopen): %v", err)
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

// ── Batched incident lookups ────────────────────────────────────────────────

// TestBatchGetIncidentHistory_ReturnsAllKeyed seeds 5 incidents and confirms
// a single BatchGetIncidentHistory call returns every one of them, correctly
// keyed by fingerprint, plus its incident id (needed to chain into
// BatchGetReopenCounts) — rather than requiring one GetIncidentHistory call
// per fingerprint.
func TestBatchGetIncidentHistory_ReturnsAllKeyed(t *testing.T) {
	s := openTestStore(t)

	var incs []IncidentData
	for i := 0; i < 5; i++ {
		incs = append(incs, IncidentData{
			Fingerprint: fmt.Sprintf("default/Deployment/svc-%d/crash_loop", i),
			Namespace:   "default",
			Resource:    fmt.Sprintf("svc-%d", i),
			IssueType:   "crash_loop",
			Severity:    "critical",
			DetailsJSON: fmt.Sprintf(`{"n":%d}`, i),
		})
	}
	if err := s.UpsertIncidents("test-cluster", "scan-1", incs); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}
	// A fingerprint with no incident must simply be absent from the result.
	fingerprints := []string{
		"default/Deployment/svc-0/crash_loop",
		"default/Deployment/svc-1/crash_loop",
		"default/Deployment/svc-2/crash_loop",
		"default/Deployment/svc-3/crash_loop",
		"default/Deployment/svc-4/crash_loop",
		"default/Deployment/nonexistent/crash_loop",
	}

	got, err := s.BatchGetIncidentHistory("test-cluster", fingerprints)
	if err != nil {
		t.Fatalf("BatchGetIncidentHistory: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 records, got %d: %+v", len(got), got)
	}
	if _, ok := got["default/Deployment/nonexistent/crash_loop"]; ok {
		t.Fatalf("expected no record for a fingerprint with no incident")
	}

	seenIDs := make(map[int64]bool)
	for i, inc := range incs {
		rec, ok := got[inc.Fingerprint]
		if !ok {
			t.Fatalf("missing record for %s", inc.Fingerprint)
		}
		if rec.Fingerprint != inc.Fingerprint {
			t.Errorf("record %d: Fingerprint = %q, want %q", i, rec.Fingerprint, inc.Fingerprint)
		}
		if rec.Status != "active" {
			t.Errorf("record %d: Status = %q, want active", i, rec.Status)
		}
		if rec.ID == 0 {
			t.Errorf("record %d: expected non-zero incident id", i)
		}
		if seenIDs[rec.ID] {
			t.Errorf("record %d: id %d reused across incidents", i, rec.ID)
		}
		seenIDs[rec.ID] = true
		if !strings.Contains(rec.DetailsJSON, fmt.Sprintf(`"n":%d`, i)) {
			t.Errorf("record %d: DetailsJSON = %q, want it to contain n:%d", i, rec.DetailsJSON, i)
		}
	}
}

func TestBatchGetIncidentHistory_EmptyInput(t *testing.T) {
	s := openTestStore(t)
	got, err := s.BatchGetIncidentHistory("test-cluster", nil)
	if err != nil {
		t.Fatalf("BatchGetIncidentHistory: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result for empty input, got %+v", got)
	}
}

// TestBatchGetReopenCounts_MatchesQueryIncidentsReopenCount confirms the
// exported BatchGetReopenCounts wrapper returns the same counts QueryIncidents
// already derives internally via the unexported batchReopenCounts it wraps.
func TestBatchGetReopenCounts_MatchesQueryIncidentsReopenCount(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}
	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}
	driveToResolved(t, s, "test-cluster", nil, "scan-miss")
	backdateIncidentEvents(t, s, "test-cluster", inc.Fingerprint, flapAbsorptionWindow+time.Minute)
	if err := s.UpsertIncidents("test-cluster", "scan-reopen", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(reopen): %v", err)
	}

	rec, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil || rec == nil {
		t.Fatalf("GetIncidentHistory: %v, %+v", err, rec)
	}

	var incidentID int64
	if err := s.db.QueryRow(
		"SELECT id FROM incidents WHERE cluster=? AND fingerprint=?",
		"test-cluster", inc.Fingerprint,
	).Scan(&incidentID); err != nil {
		t.Fatalf("lookup incident id: %v", err)
	}

	counts, err := s.BatchGetReopenCounts([]int64{incidentID})
	if err != nil {
		t.Fatalf("BatchGetReopenCounts: %v", err)
	}
	if counts[incidentID] != 1 {
		t.Fatalf("BatchGetReopenCounts[%d] = %d, want 1", incidentID, counts[incidentID])
	}

	items, _, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster"})
	if err != nil {
		t.Fatalf("QueryIncidents: %v", err)
	}
	if len(items) != 1 || items[0].ReopenCount != counts[incidentID] {
		t.Fatalf("QueryIncidents ReopenCount diverges from BatchGetReopenCounts: %+v vs %d", items, counts[incidentID])
	}
}

// ── Resolve debounce ─────────────────────────────────────────────────────────

func TestDebounce_SingleMissStaysActiveNoEvent(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}
	// absent from scan-2 (one miss, below resolveThreshold=3)
	if err := s.UpsertIncidents("test-cluster", "scan-2", nil); err != nil {
		t.Fatalf("UpsertIncidents(2): %v", err)
	}
	resolved, err := s.ResolveMissing("test-cluster", "scan-2")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 0 {
		t.Fatalf("expected no resolution after a single miss, got %d", resolved)
	}

	rec, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec.Status != "active" {
		t.Fatalf("expected still active after single miss, got %s", rec.Status)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 1 || events[0].EventType != "DETECTED" {
		t.Fatalf("expected only the initial DETECTED event, got %+v", events)
	}
}

func TestDebounce_ReappearsWithinGraceWindowResetsCounter(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical", RestartCount: 5}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}
	// two consecutive misses (below resolveThreshold=3)
	for i := 0; i < 2; i++ {
		scanID := fmt.Sprintf("scan-miss-%d", i)
		if err := s.UpsertIncidents("test-cluster", scanID, nil); err != nil {
			t.Fatalf("UpsertIncidents(%s): %v", scanID, err)
		}
		if _, err := s.ResolveMissing("test-cluster", scanID); err != nil {
			t.Fatalf("ResolveMissing(%s): %v", scanID, err)
		}
	}

	// reappears with identical severity/restart count before hitting the threshold
	if err := s.UpsertIncidents("test-cluster", "scan-reappear", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(reappear): %v", err)
	}

	rec, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec.Status != "active" {
		t.Fatalf("expected active, got %s", rec.Status)
	}

	var missingScans int
	if err := s.db.QueryRow(
		"SELECT missing_scans FROM incidents WHERE cluster=? AND fingerprint=?",
		"test-cluster", inc.Fingerprint,
	).Scan(&missingScans); err != nil {
		t.Fatalf("query missing_scans: %v", err)
	}
	if missingScans != 0 {
		t.Fatalf("expected missing_scans reset to 0, got %d", missingScans)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected no RESOLVED/REOPENED events, only initial DETECTED, got %+v", events)
	}
}

func TestDebounce_ThreeConsecutiveMissesResolvesOnce(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}

	var totalResolved int
	for i := 0; i < resolveThreshold; i++ {
		scanID := fmt.Sprintf("scan-miss-%d", i)
		if err := s.UpsertIncidents("test-cluster", scanID, nil); err != nil {
			t.Fatalf("UpsertIncidents(%s): %v", scanID, err)
		}
		resolved, err := s.ResolveMissing("test-cluster", scanID)
		if err != nil {
			t.Fatalf("ResolveMissing(%s): %v", scanID, err)
		}
		totalResolved += resolved
	}
	if totalResolved != 1 {
		t.Fatalf("expected exactly 1 resolution across %d misses, got %d", resolveThreshold, totalResolved)
	}

	rec, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec.Status != "resolved" {
		t.Fatalf("expected resolved after %d consecutive misses, got %s", resolveThreshold, rec.Status)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	resolvedEvents := 0
	for _, e := range events {
		if e.EventType == "RESOLVED" {
			resolvedEvents++
		}
	}
	if resolvedEvents != 1 {
		t.Fatalf("expected exactly one RESOLVED event, got %d in %+v", resolvedEvents, events)
	}
}

func TestDebounce_ReopensAfterGenuineResolve(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}
	driveToResolved(t, s, "test-cluster", nil, "scan-miss")

	rec, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec.Status != "resolved" {
		t.Fatalf("expected genuinely resolved, got %s", rec.Status)
	}
	// push the resolution outside flapAbsorptionWindow so this reappearance
	// reads as a genuine recurrence, not a flap
	backdateIncidentEvents(t, s, "test-cluster", inc.Fingerprint, flapAbsorptionWindow+time.Minute)

	if err := s.UpsertIncidents("test-cluster", "scan-reopen", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(reopen): %v", err)
	}

	rec, err = s.GetIncidentHistory("test-cluster", inc.Fingerprint)
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
	last := events[len(events)-1]
	if last.EventType != "REOPENED" || last.EventReason != "Reopened" {
		t.Fatalf("expected trailing REOPENED event, got %+v", last)
	}
}

// ── Flap absorption ──────────────────────────────────────────────────────────

func TestFlapAbsorption_ReappearsWithinWindow_NoReopenEventEmitted(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/probe_failure", Namespace: "default", Resource: "svc-a", IssueType: "probe_failure", Severity: "warning"}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}
	driveToResolved(t, s, "test-cluster", nil, "scan-miss")

	rec, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec.Status != "resolved" {
		t.Fatalf("expected resolved before flap, got %s", rec.Status)
	}

	// reappears moments later (well within flapAbsorptionWindow) — this is
	// the flap this feature absorbs, not a genuine recurrence.
	if err := s.UpsertIncidents("test-cluster", "scan-flap", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(flap): %v", err)
	}

	rec, err = s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec.Status != "active" {
		t.Fatalf("expected active after absorbed flap, got %s", rec.Status)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 1 || events[0].EventType != "DETECTED" {
		t.Fatalf("expected only the initial DETECTED event (RESOLVED removed, no REOPENED), got %+v", events)
	}
}

func TestFlapAbsorption_ReappearsAfterWindow_ReopenedEmitted(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/probe_failure", Namespace: "default", Resource: "svc-a", IssueType: "probe_failure", Severity: "warning"}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}
	driveToResolved(t, s, "test-cluster", nil, "scan-miss")
	backdateIncidentEvents(t, s, "test-cluster", inc.Fingerprint, flapAbsorptionWindow+time.Minute)

	if err := s.UpsertIncidents("test-cluster", "scan-reopen", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(reopen): %v", err)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events (DETECTED, RESOLVED, REOPENED), got %d: %+v", len(events), events)
	}
	if events[1].EventType != "RESOLVED" {
		t.Fatalf("expected RESOLVED event to be retained (gap exceeded the window), got %+v", events[1])
	}
	if events[2].EventType != "REOPENED" || events[2].EventReason != "Reopened" {
		t.Fatalf("expected trailing REOPENED event, got %+v", events[2])
	}
}

func TestFlapAbsorption_MultipleRapidFlapsLeaveOnlyDetected(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/probe_failure", Namespace: "default", Resource: "svc-a", IssueType: "probe_failure", Severity: "warning"}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}

	// 5 resolve/reopen cycles, ~3 minutes apart — mirrors the flapping
	// probe_failure pattern this feature exists to absorb.
	for cycle := 0; cycle < 5; cycle++ {
		driveToResolved(t, s, "test-cluster", nil, fmt.Sprintf("cycle%d-miss", cycle))
		backdateIncidentEvents(t, s, "test-cluster", inc.Fingerprint, 3*time.Minute)

		scanID := fmt.Sprintf("cycle%d-reappear", cycle)
		if err := s.UpsertIncidents("test-cluster", scanID, []IncidentData{inc}); err != nil {
			t.Fatalf("UpsertIncidents(%s): %v", scanID, err)
		}
	}

	rec, err := s.GetIncidentHistory("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory: %v", err)
	}
	if rec.Status != "active" {
		t.Fatalf("expected active after flap cycles settle, got %s", rec.Status)
	}

	events, err := s.GetIncidentTimeline("test-cluster", inc.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline: %v", err)
	}
	if len(events) != 1 || events[0].EventType != "DETECTED" {
		t.Fatalf("expected only the original DETECTED event to survive 5 flap cycles, got %+v", events)
	}
}

// TestMigration_MissingScansColumnIdempotent verifies that opening a
// database from the previous schema version (which has current_restart_count
// but not missing_scans) adds the new column exactly once, without error or
// data loss, whether opened one or multiple times.
func TestMigration_MissingScansColumnIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE incidents (
		    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		    fingerprint           TEXT    NOT NULL,
		    cluster               TEXT    NOT NULL,
		    namespace             TEXT    NOT NULL,
		    resource              TEXT    NOT NULL,
		    issue_type            TEXT    NOT NULL,
		    severity              TEXT    NOT NULL,
		    first_seen            INTEGER NOT NULL,
		    last_seen             INTEGER NOT NULL,
		    details_json          TEXT,
		    status                TEXT    NOT NULL DEFAULT 'active',
		    last_scan_id          TEXT,
		    current_restart_count INTEGER NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX idx_inc_fp ON incidents(cluster, fingerprint);

		CREATE TABLE incident_events (
		    id            INTEGER PRIMARY KEY AUTOINCREMENT,
		    incident_id   INTEGER NOT NULL,
		    scan_id       TEXT    NOT NULL,
		    occurred_at   INTEGER NOT NULL,
		    event_type    TEXT    NOT NULL,
		    event_reason  TEXT,
		    restart_count INTEGER,
		    severity      TEXT,
		    state         TEXT,
		    message       TEXT
		);

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

		INSERT INTO incidents (fingerprint, cluster, namespace, resource, issue_type, severity, first_seen, last_seen, details_json, status, last_scan_id, current_restart_count)
		VALUES ('default/Deployment/svc-a/crash_loop', 'test-cluster', 'default', 'svc-a', 'crash_loop', 'critical', 1000, 2000, '{}', 'active', 'scan-old-1', 42);

		PRAGMA user_version = 2;
	`); err != nil {
		t.Fatalf("create v2 schema: %v", err)
	}
	db.Close()

	s1, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(1): %v", err)
	}
	var missingScans, restart int
	if err := s1.db.QueryRow(
		"SELECT missing_scans, current_restart_count FROM incidents WHERE fingerprint=?",
		"default/Deployment/svc-a/crash_loop",
	).Scan(&missingScans, &restart); err != nil {
		t.Fatalf("query after first open: %v", err)
	}
	if missingScans != 0 {
		t.Fatalf("expected missing_scans defaulted to 0, got %d", missingScans)
	}
	if restart != 42 {
		t.Fatalf("expected current_restart_count preserved as 42, got %d", restart)
	}
	s1.Close()

	// Reopen: migration must be a no-op the second time (idempotent).
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(2): %v", err)
	}
	defer s2.Close()

	var count int
	if err := s2.db.QueryRow("SELECT COUNT(*) FROM incidents").Scan(&count); err != nil {
		t.Fatalf("count after second open: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected no data loss, got %d rows", count)
	}

	var version int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("expected user_version=%d, got %d", schemaVersion, version)
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

// TestMigration_QueryIndexesIdempotent verifies that opening a database from
// the previous schema version (which lacks the incidents-registry query
// indexes) adds them exactly once, without error, whether opened one or
// multiple times.
func TestMigration_QueryIndexesIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v3 db: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE incidents (
		    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		    fingerprint           TEXT    NOT NULL,
		    cluster               TEXT    NOT NULL,
		    namespace             TEXT    NOT NULL,
		    resource              TEXT    NOT NULL,
		    issue_type            TEXT    NOT NULL,
		    severity              TEXT    NOT NULL,
		    first_seen            INTEGER NOT NULL,
		    last_seen             INTEGER NOT NULL,
		    details_json          TEXT,
		    status                TEXT    NOT NULL DEFAULT 'active',
		    last_scan_id          TEXT,
		    current_restart_count INTEGER NOT NULL DEFAULT 0,
		    missing_scans         INTEGER NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX idx_inc_fp ON incidents(cluster, fingerprint);

		CREATE TABLE incident_events (
		    id            INTEGER PRIMARY KEY AUTOINCREMENT,
		    incident_id   INTEGER NOT NULL,
		    scan_id       TEXT    NOT NULL,
		    occurred_at   INTEGER NOT NULL,
		    event_type    TEXT    NOT NULL,
		    event_reason  TEXT,
		    restart_count INTEGER,
		    severity      TEXT,
		    state         TEXT,
		    message       TEXT
		);
		CREATE INDEX idx_incident_events_incident ON incident_events(incident_id, occurred_at);

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

		PRAGMA user_version = 3;
	`); err != nil {
		t.Fatalf("create v3 schema: %v", err)
	}
	db.Close()

	assertMigrated := func() {
		s, err := OpenSQLite(path)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		defer s.Close()

		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatalf("PRAGMA user_version: %v", err)
		}
		if version != schemaVersion {
			t.Fatalf("expected user_version=%d, got %d", schemaVersion, version)
		}

		rows, err := s.db.Query(`PRAGMA index_list(incidents)`)
		if err != nil {
			t.Fatalf("PRAGMA index_list: %v", err)
		}
		defer rows.Close()
		names := map[string]bool{}
		for rows.Next() {
			var seq, unique, partial int
			var name, origin string
			if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
				t.Fatalf("scan index_list row: %v", err)
			}
			names[name] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("index_list rows: %v", err)
		}
		for _, want := range []string{"idx_incidents_cluster_status", "idx_incidents_cluster_ns", "idx_incidents_cluster_first"} {
			if !names[want] {
				t.Fatalf("expected index %s to exist, got %v", want, names)
			}
		}
	}

	// First open performs the migration; second open must be a no-op.
	assertMigrated()
	assertMigrated()
}

// ── Incidents registry: QueryIncidents ──────────────────────────────────────

// insertRawIncident inserts an incidents row directly via SQL, bypassing
// UpsertIncidents, so tests can control first_seen/last_seen/status/
// restart_count precisely instead of relying on time.Now().
func insertRawIncident(t *testing.T, s *SQLiteStore, cluster string, inc IncidentData, status string, firstSeen, lastSeen time.Time) int64 {
	t.Helper()
	res, err := s.db.Exec(
		`INSERT INTO incidents (
			fingerprint, cluster, namespace, resource, issue_type, severity,
			first_seen, last_seen, details_json, status, last_scan_id,
			current_restart_count, missing_scans
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'scan-fixture', ?, 0)`,
		inc.Fingerprint, cluster, inc.Namespace, inc.Resource, inc.IssueType, inc.Severity,
		firstSeen.Unix(), lastSeen.Unix(), inc.DetailsJSON, status, inc.RestartCount,
	)
	if err != nil {
		t.Fatalf("insertRawIncident: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insertRawIncident LastInsertId: %v", err)
	}
	return id
}

// insertRawEvent inserts an incident_events row directly, for precise
// control over occurred_at/restart_count in trend and retention fixtures.
func insertRawEvent(t *testing.T, s *SQLiteStore, incidentID int64, eventType, eventReason string, restartCount int, occurredAt time.Time) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO incident_events (
			incident_id, scan_id, occurred_at, event_type, event_reason,
			restart_count, severity, state, message
		) VALUES (?, 'scan-fixture', ?, ?, ?, ?, 'critical', 'active', 'fixture')`,
		incidentID, occurredAt.Unix(), eventType, eventReason, restartCount,
	)
	if err != nil {
		t.Fatalf("insertRawEvent: %v", err)
	}
}

func TestQueryIncidents_Filters(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	incA := IncidentData{Fingerprint: "ns-a/Deployment/svc-a/crash_loop", Namespace: "ns-a", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical", RestartCount: 50}
	incB := IncidentData{Fingerprint: "ns-b/Deployment/svc-b/oom", Namespace: "ns-b", Resource: "svc-b", IssueType: "oom", Severity: "medium", RestartCount: 5}
	incC := IncidentData{Fingerprint: "ns-a/Deployment/svc-c/image_pull", Namespace: "ns-a", Resource: "svc-c", IssueType: "image_pull", Severity: "high", RestartCount: 0}

	insertRawIncident(t, s, "test-cluster", incA, "active", now.Add(-10*24*time.Hour), now.Add(-1*time.Hour))
	insertRawIncident(t, s, "test-cluster", incB, "resolved", now.Add(-5*24*time.Hour), now.Add(-2*time.Hour))
	insertRawIncident(t, s, "test-cluster", incC, "active", now.Add(-1*time.Hour), now)

	// Text filter matches resource/namespace/issue_type, case-insensitive.
	items, total, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", Text: "SVC-A"})
	if err != nil {
		t.Fatalf("QueryIncidents(text): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Resource != "svc-a" {
		t.Fatalf("expected 1 match for text=SVC-A, got total=%d items=%+v", total, items)
	}

	// Namespace filter, exact.
	if _, total, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", Namespace: "ns-a"}); err != nil {
		t.Fatalf("QueryIncidents(namespace): %v", err)
	} else if total != 2 {
		t.Fatalf("expected 2 incidents in ns-a, got %d", total)
	}

	// IssueType filter.
	if items, total, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", IssueType: "oom"}); err != nil {
		t.Fatalf("QueryIncidents(issue_type): %v", err)
	} else if total != 1 || items[0].IssueType != "oom" {
		t.Fatalf("expected 1 oom incident, got total=%d items=%+v", total, items)
	}

	// Severity filter.
	if _, total, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", Severity: "critical"}); err != nil {
		t.Fatalf("QueryIncidents(severity): %v", err)
	} else if total != 1 {
		t.Fatalf("expected 1 critical incident, got %d", total)
	}

	// Status filter: active vs resolved.
	if _, total, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", Status: "resolved"}); err != nil {
		t.Fatalf("QueryIncidents(status=resolved): %v", err)
	} else if total != 1 {
		t.Fatalf("expected 1 resolved incident, got %d", total)
	}
	if _, total, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", Status: "active"}); err != nil {
		t.Fatalf("QueryIncidents(status=active): %v", err)
	} else if total != 2 {
		t.Fatalf("expected 2 active incidents, got %d", total)
	}

	// SinceFirst: only incidents first_seen within the last 3 days -> incC.
	if _, total, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", SinceFirst: now.Add(-3 * 24 * time.Hour)}); err != nil {
		t.Fatalf("QueryIncidents(since_first): %v", err)
	} else if total != 1 {
		t.Fatalf("expected 1 incident first_seen within last 3 days, got %d", total)
	}
}

func TestQueryIncidents_Pagination(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	for i := 0; i < 5; i++ {
		inc := IncidentData{
			Fingerprint: fmt.Sprintf("default/Deployment/svc-%d/crash_loop", i),
			Namespace:   "default", Resource: fmt.Sprintf("svc-%d", i), IssueType: "crash_loop", Severity: "critical",
		}
		insertRawIncident(t, s, "test-cluster", inc, "active",
			now.Add(-time.Duration(i)*time.Hour), now.Add(-time.Duration(i)*time.Hour))
	}

	items1, total1, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", PerPage: 2, Page: 1, SortBy: "first_seen", SortDesc: true})
	if err != nil {
		t.Fatalf("QueryIncidents(page1): %v", err)
	}
	if total1 != 5 {
		t.Fatalf("expected total=5, got %d", total1)
	}
	if len(items1) != 2 {
		t.Fatalf("expected 2 items on page 1, got %d", len(items1))
	}

	items2, total2, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", PerPage: 2, Page: 2, SortBy: "first_seen", SortDesc: true})
	if err != nil {
		t.Fatalf("QueryIncidents(page2): %v", err)
	}
	if total2 != 5 {
		t.Fatalf("expected total=5 on page 2, got %d", total2)
	}
	if len(items2) != 2 {
		t.Fatalf("expected 2 items on page 2, got %d", len(items2))
	}

	items3, total3, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", PerPage: 2, Page: 3, SortBy: "first_seen", SortDesc: true})
	if err != nil {
		t.Fatalf("QueryIncidents(page3): %v", err)
	}
	if total3 != 5 {
		t.Fatalf("expected total=5 on page 3, got %d", total3)
	}
	if len(items3) != 1 {
		t.Fatalf("expected 1 item on page 3, got %d", len(items3))
	}

	seen := map[string]bool{}
	for _, it := range append(append(items1, items2...), items3...) {
		if seen[it.Fingerprint] {
			t.Fatalf("fingerprint %s appeared on more than one page", it.Fingerprint)
		}
		seen[it.Fingerprint] = true
	}
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct fingerprints across pages, got %d", len(seen))
	}

	// PerPage default (<=0) and cap (>200) both resolve without error; with
	// only 5 rows in the fixture we can't observe the cap directly, but this
	// exercises the clamp path.
	if _, _, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster"}); err != nil {
		t.Fatalf("QueryIncidents(default perPage): %v", err)
	}
	itemsCapped, _, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", PerPage: 9999})
	if err != nil {
		t.Fatalf("QueryIncidents(capped perPage): %v", err)
	}
	if len(itemsCapped) != 5 {
		t.Fatalf("expected all 5 rows with an oversized PerPage, got %d", len(itemsCapped))
	}
}

func TestQueryIncidents_SortOrders(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	low := IncidentData{Fingerprint: "default/Deployment/svc-low/crash_loop", Namespace: "default", Resource: "svc-low", IssueType: "crash_loop", Severity: "low", RestartCount: 1}
	crit := IncidentData{Fingerprint: "default/Deployment/svc-crit/crash_loop", Namespace: "default", Resource: "svc-crit", IssueType: "crash_loop", Severity: "critical", RestartCount: 100}
	med := IncidentData{Fingerprint: "default/Deployment/svc-med/crash_loop", Namespace: "default", Resource: "svc-med", IssueType: "crash_loop", Severity: "medium", RestartCount: 50}

	insertRawIncident(t, s, "test-cluster", low, "active", now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	insertRawIncident(t, s, "test-cluster", crit, "active", now.Add(-2*time.Hour), now.Add(-30*time.Minute))
	insertRawIncident(t, s, "test-cluster", med, "active", now.Add(-1*time.Hour), now.Add(-90*time.Minute))

	items, _, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", SortBy: "severity"})
	if err != nil {
		t.Fatalf("QueryIncidents(sort=severity): %v", err)
	}
	if len(items) != 3 || items[0].Severity != "critical" || items[1].Severity != "medium" || items[2].Severity != "low" {
		t.Fatalf("expected severity order critical,medium,low, got %+v", items)
	}

	items, _, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", SortBy: "restarts", SortDesc: true})
	if err != nil {
		t.Fatalf("QueryIncidents(sort=restarts desc): %v", err)
	}
	if items[0].RestartCount != 100 || items[2].RestartCount != 1 {
		t.Fatalf("expected restarts desc order, got %+v", items)
	}

	items, _, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", SortBy: "first_seen"})
	if err != nil {
		t.Fatalf("QueryIncidents(sort=first_seen): %v", err)
	}
	if items[0].Resource != "svc-low" {
		t.Fatalf("expected svc-low first (oldest first_seen), got %+v", items)
	}

	items, _, err = s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", SortBy: "last_seen", SortDesc: true})
	if err != nil {
		t.Fatalf("QueryIncidents(sort=last_seen desc): %v", err)
	}
	if items[0].Resource != "svc-crit" {
		t.Fatalf("expected svc-crit first (most recent last_seen), got %+v", items)
	}
}

func TestQueryIncidents_ReopenCountAndStatus(t *testing.T) {
	s := openTestStore(t)

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}
	other := IncidentData{Fingerprint: "default/Deployment/svc-b/oom", Namespace: "default", Resource: "svc-b", IssueType: "oom", Severity: "medium"}

	if err := s.UpsertIncidents("test-cluster", "scan-1", []IncidentData{inc, other}); err != nil {
		t.Fatalf("UpsertIncidents(1): %v", err)
	}
	// Resolve + reopen svc-a (inc) twice; svc-b (other) stays present/active
	// throughout and is never resolved.
	for i := 0; i < 2; i++ {
		driveToResolved(t, s, "test-cluster", []IncidentData{other}, fmt.Sprintf("scan-miss-%d", i))
		// push each resolution outside flapAbsorptionWindow so the reopen
		// below counts as a genuine recurrence, not an absorbed flap
		backdateIncidentEvents(t, s, "test-cluster", inc.Fingerprint, flapAbsorptionWindow+time.Minute)
		if err := s.UpsertIncidents("test-cluster", fmt.Sprintf("scan-reopen-%d", i), []IncidentData{inc, other}); err != nil {
			t.Fatalf("UpsertIncidents(reopen %d): %v", i, err)
		}
	}

	items, total, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", Status: "reopened"})
	if err != nil {
		t.Fatalf("QueryIncidents(status=reopened): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Fingerprint != inc.Fingerprint {
		t.Fatalf("expected only svc-a under status=reopened, got total=%d items=%+v", total, items)
	}
	if items[0].ReopenCount != 2 {
		t.Fatalf("expected ReopenCount=2, got %d", items[0].ReopenCount)
	}
	if items[0].Status != "active" {
		t.Fatalf("expected reopened incident status=active, got %s", items[0].Status)
	}

	itemsOther, totalOther, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", Text: "svc-b"})
	if err != nil {
		t.Fatalf("QueryIncidents(text=svc-b): %v", err)
	}
	if totalOther != 1 || itemsOther[0].ReopenCount != 0 {
		t.Fatalf("expected svc-b ReopenCount=0 (never resolved), got total=%d items=%+v", totalOther, itemsOther)
	}
}

func TestQueryIncidents_Trend(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	// Resolved incidents never get a trend, regardless of history.
	resolved := IncidentData{Fingerprint: "default/Deployment/svc-resolved/crash_loop", Namespace: "default", Resource: "svc-resolved", IssueType: "crash_loop", Severity: "critical", RestartCount: 20}
	insertRawIncident(t, s, "test-cluster", resolved, "resolved", now.Add(-10*24*time.Hour), now.Add(-5*24*time.Hour))

	// Active incident with only the initial DETECTED event: not enough
	// history to judge a rate -> "stable".
	single := IncidentData{Fingerprint: "default/Deployment/svc-single/crash_loop", Namespace: "default", Resource: "svc-single", IssueType: "crash_loop", Severity: "critical", RestartCount: 5}
	singleID := insertRawIncident(t, s, "test-cluster", single, "active", now.Add(-2*time.Hour), now.Add(-1*time.Hour))
	insertRawEvent(t, s, singleID, "DETECTED", "Detected", 0, now.Add(-2*time.Hour))

	// Active incident with a sharp recent spike vs. its lifetime rate -> "accelerating".
	fast := IncidentData{Fingerprint: "default/Deployment/svc-fast/crash_loop", Namespace: "default", Resource: "svc-fast", IssueType: "crash_loop", Severity: "critical", RestartCount: 130}
	fastID := insertRawIncident(t, s, "test-cluster", fast, "active", now.Add(-30*24*time.Hour), now.Add(-30*time.Minute))
	insertRawEvent(t, s, fastID, "DETECTED", "Detected", 0, now.Add(-30*24*time.Hour))
	insertRawEvent(t, s, fastID, "UPDATED", "RestartMilestone", 100, now.Add(-2*time.Hour))
	insertRawEvent(t, s, fastID, "UPDATED", "RestartMilestone", 130, now.Add(-1*time.Hour))

	// Active incident whose recent rate roughly matches its lifetime average -> "stable".
	slow := IncidentData{Fingerprint: "default/Deployment/svc-slow/crash_loop", Namespace: "default", Resource: "svc-slow", IssueType: "crash_loop", Severity: "critical", RestartCount: 52}
	slowID := insertRawIncident(t, s, "test-cluster", slow, "active", now.Add(-30*24*time.Hour), now.Add(-1*time.Hour))
	insertRawEvent(t, s, slowID, "DETECTED", "Detected", 0, now.Add(-30*24*time.Hour))
	insertRawEvent(t, s, slowID, "UPDATED", "RestartMilestone", 50, now.Add(-10*24*time.Hour))
	insertRawEvent(t, s, slowID, "UPDATED", "RestartMilestone", 52, now.Add(-1*time.Hour))

	items, _, err := s.QueryIncidents(IncidentFilter{Cluster: "test-cluster", SortBy: "first_seen"})
	if err != nil {
		t.Fatalf("QueryIncidents: %v", err)
	}

	byResource := map[string]IncidentSummary{}
	for _, it := range items {
		byResource[it.Resource] = it
	}

	if got := byResource["svc-resolved"].Trend; got != "" {
		t.Fatalf("expected resolved incident to have empty Trend, got %q", got)
	}
	if got := byResource["svc-single"].Trend; got != "stable" {
		t.Fatalf("expected svc-single Trend=stable (insufficient history), got %q", got)
	}
	if got := byResource["svc-fast"].Trend; got != "accelerating" {
		t.Fatalf("expected svc-fast Trend=accelerating, got %q", got)
	}
	if got := byResource["svc-slow"].Trend; got != "stable" {
		t.Fatalf("expected svc-slow Trend=stable, got %q", got)
	}
}

// ── Retention ────────────────────────────────────────────────────────────────

func TestRetentionCutoff(t *testing.T) {
	now := time.Now()

	if cutoff, enabled := RetentionCutoff(0, now); enabled || !cutoff.IsZero() {
		t.Fatalf("expected retention disabled (enabled=false, zero cutoff) for days=0, got enabled=%v cutoff=%v", enabled, cutoff)
	}
	if cutoff, enabled := RetentionCutoff(-5, now); enabled || !cutoff.IsZero() {
		t.Fatalf("expected retention disabled for negative days, got enabled=%v cutoff=%v", enabled, cutoff)
	}

	cutoff, enabled := RetentionCutoff(90, now)
	if !enabled {
		t.Fatalf("expected retention enabled for days=90")
	}
	if want := now.AddDate(0, 0, -90); !cutoff.Equal(want) {
		t.Fatalf("expected cutoff=%v, got %v", want, cutoff)
	}
}

// TestRetentionDisabled_PrunesNothing mirrors the scan-loop guard in
// cmd/opscart-dashboard/main.go: when RetentionCutoff reports the feature
// disabled (days<=0), the caller must never invoke PruneOlderThan at all —
// so even a very old, resolved incident is left untouched.
func TestRetentionDisabled_PrunesNothing(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	old := IncidentData{Fingerprint: "default/Deployment/svc-ancient/crash_loop", Namespace: "default", Resource: "svc-ancient", IssueType: "crash_loop", Severity: "critical"}
	insertRawIncident(t, s, "test-cluster", old, "resolved", now.Add(-400*24*time.Hour), now.Add(-400*24*time.Hour))

	if _, enabled := RetentionCutoff(0, now); enabled {
		t.Fatalf("expected retention disabled for days=0")
	}
	// No PruneOlderThan call is made — confirm nothing changed.

	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM incidents WHERE cluster = 'test-cluster'").Scan(&count); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the ancient incident untouched, got %d rows", count)
	}
}

func TestPruneOlderThan(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	cutoff := now.Add(-30 * 24 * time.Hour)

	// Resolved and old -> pruned, along with all of its events.
	oldResolved := IncidentData{Fingerprint: "default/Deployment/svc-old-resolved/crash_loop", Namespace: "default", Resource: "svc-old-resolved", IssueType: "crash_loop", Severity: "critical"}
	oldResolvedID := insertRawIncident(t, s, "test-cluster", oldResolved, "resolved", now.Add(-60*24*time.Hour), now.Add(-40*24*time.Hour))
	insertRawEvent(t, s, oldResolvedID, "DETECTED", "Detected", 0, now.Add(-60*24*time.Hour))
	insertRawEvent(t, s, oldResolvedID, "RESOLVED", "Resolved", 5, now.Add(-40*24*time.Hour))

	// Resolved but recent -> kept.
	recentResolved := IncidentData{Fingerprint: "default/Deployment/svc-recent-resolved/oom", Namespace: "default", Resource: "svc-recent-resolved", IssueType: "oom", Severity: "medium"}
	insertRawIncident(t, s, "test-cluster", recentResolved, "resolved", now.Add(-10*24*time.Hour), now.Add(-2*24*time.Hour))

	// Active and old (first_seen/last_seen far in the past) -> the incident
	// row is never deleted; only its stale non-DETECTED events are trimmed,
	// and DETECTED survives regardless of age.
	oldActive := IncidentData{Fingerprint: "default/Deployment/svc-old-active/crash_loop", Namespace: "default", Resource: "svc-old-active", IssueType: "crash_loop", Severity: "high"}
	oldActiveID := insertRawIncident(t, s, "test-cluster", oldActive, "active", now.Add(-90*24*time.Hour), now.Add(-1*time.Hour))
	insertRawEvent(t, s, oldActiveID, "DETECTED", "Detected", 0, now.Add(-90*24*time.Hour))
	insertRawEvent(t, s, oldActiveID, "UPDATED", "SeverityChanged", 3, now.Add(-89*24*time.Hour))
	insertRawEvent(t, s, oldActiveID, "UPDATED", "RestartMilestone", 10, now.Add(-1*time.Hour))

	if _, err := s.db.Exec(
		`INSERT INTO cluster_snapshots (scan_id, cluster, scanned_at, incident_score, critical_count, warning_count, security_score, waste_count, monthly_cost)
		 VALUES ('scan-old', 'test-cluster', ?, 10, 1, 1, 90, 0, 100.0)`,
		now.Add(-45*24*time.Hour).Unix(),
	); err != nil {
		t.Fatalf("insert old snapshot: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO cluster_snapshots (scan_id, cluster, scanned_at, incident_score, critical_count, warning_count, security_score, waste_count, monthly_cost)
		 VALUES ('scan-recent', 'test-cluster', ?, 10, 1, 1, 90, 0, 100.0)`,
		now.Add(-1*24*time.Hour).Unix(),
	); err != nil {
		t.Fatalf("insert recent snapshot: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO scan_history (scan_id, cluster, scanned_at, success) VALUES ('scan-old', 'test-cluster', ?, 1)`,
		now.Add(-45*24*time.Hour).Unix(),
	); err != nil {
		t.Fatalf("insert old scan_history: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO scan_history (scan_id, cluster, scanned_at, success) VALUES ('scan-recent', 'test-cluster', ?, 1)`,
		now.Add(-1*24*time.Hour).Unix(),
	); err != nil {
		t.Fatalf("insert recent scan_history: %v", err)
	}

	pruned, err := s.PruneOlderThan("test-cluster", cutoff)
	if err != nil {
		t.Fatalf("PruneOlderThan: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned incident, got %d", pruned)
	}

	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM incidents WHERE id = ?", oldResolvedID).Scan(&count); err != nil {
		t.Fatalf("count old-resolved incidents: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected old-resolved incident deleted, still have %d rows", count)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM incident_events WHERE incident_id = ?", oldResolvedID).Scan(&count); err != nil {
		t.Fatalf("count old-resolved events: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected old-resolved incident's events deleted, still have %d rows", count)
	}

	rec, err := s.GetIncidentHistory("test-cluster", recentResolved.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory(recent-resolved): %v", err)
	}
	if rec == nil {
		t.Fatalf("expected recent-resolved incident to survive pruning")
	}

	rec, err = s.GetIncidentHistory("test-cluster", oldActive.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentHistory(old-active): %v", err)
	}
	if rec == nil || rec.Status != "active" {
		t.Fatalf("expected old-active incident to survive pruning as active, got %+v", rec)
	}

	events, err := s.GetIncidentTimeline("test-cluster", oldActive.Fingerprint)
	if err != nil {
		t.Fatalf("GetIncidentTimeline(old-active): %v", err)
	}
	var hasDetected, hasSeverityChanged, hasMilestone bool
	for _, e := range events {
		switch e.EventReason {
		case "Detected":
			hasDetected = true
		case "SeverityChanged":
			hasSeverityChanged = true
		case "RestartMilestone":
			hasMilestone = true
		}
	}
	if !hasDetected {
		t.Fatalf("expected DETECTED event to survive pruning regardless of age, got %+v", events)
	}
	if hasSeverityChanged {
		t.Fatalf("expected old non-DETECTED event to be pruned, got %+v", events)
	}
	if !hasMilestone {
		t.Fatalf("expected recent event to survive, got %+v", events)
	}

	if err := s.db.QueryRow("SELECT COUNT(*) FROM cluster_snapshots WHERE scan_id = 'scan-old'").Scan(&count); err != nil {
		t.Fatalf("count old snapshot: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected old snapshot pruned, still have %d rows", count)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM cluster_snapshots WHERE scan_id = 'scan-recent'").Scan(&count); err != nil {
		t.Fatalf("count recent snapshot: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected recent snapshot kept, got %d rows", count)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM scan_history WHERE scan_id = 'scan-old'").Scan(&count); err != nil {
		t.Fatalf("count old scan_history: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected old scan_history pruned, still have %d rows", count)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM scan_history WHERE scan_id = 'scan-recent'").Scan(&count); err != nil {
		t.Fatalf("count recent scan_history: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected recent scan_history kept, got %d rows", count)
	}
}

// ── Memory scoreboard / recent events ───────────────────────────────────────

// setIncidentFirstSeen backdates an incident's first_seen column directly,
// simulating a long-lived incident without sleeping in tests. UpsertIncidents
// never overwrites first_seen on conflict, so this survives later upserts.
func setIncidentFirstSeen(t *testing.T, s *SQLiteStore, cluster, fingerprint string, firstSeen time.Time) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE incidents SET first_seen=? WHERE cluster=? AND fingerprint=?`,
		firstSeen.Unix(), cluster, fingerprint,
	); err != nil {
		t.Fatalf("set first_seen: %v", err)
	}
}

// insertTestEvent writes a synthetic incident_events row with an explicit
// occurred_at, letting ordering tests control timestamps precisely instead
// of relying on real wall-clock gaps between fast test operations.
func insertTestEvent(t *testing.T, s *SQLiteStore, incidentID int64, occurredAt int64, eventReason string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO incident_events (
			incident_id, scan_id, occurred_at, event_type, event_reason,
			restart_count, severity, state, message
		) VALUES (?, 'test-scan', ?, 'UPDATED', ?, 0, 'warning', 'active', 'test event')`,
		incidentID, occurredAt, eventReason,
	); err != nil {
		t.Fatalf("insert test event: %v", err)
	}
}

func TestGetMemoryScoreboard(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"

	// ns-a: a1 stays active and is driven "accelerating"; a2 resolves and
	// stays resolved.
	a1 := IncidentData{Fingerprint: "ns-a/Deployment/svc-a1/crash_loop", Namespace: "ns-a", Resource: "svc-a1", IssueType: "crash_loop", Severity: "critical", RestartCount: 5}
	a2 := IncidentData{Fingerprint: "ns-a/Deployment/svc-a2/oom", Namespace: "ns-a", Resource: "svc-a2", IssueType: "oom", Severity: "warning"}

	// ns-b: b1 resolves then genuinely reopens; b2 resolves and stays
	// resolved; b3 stays active with no restart movement ("stable").
	b1 := IncidentData{Fingerprint: "ns-b/Deployment/svc-b1/crash_loop", Namespace: "ns-b", Resource: "svc-b1", IssueType: "crash_loop", Severity: "critical"}
	b2 := IncidentData{Fingerprint: "ns-b/Deployment/svc-b2/oom", Namespace: "ns-b", Resource: "svc-b2", IssueType: "oom", Severity: "warning"}
	b3 := IncidentData{Fingerprint: "ns-b/Deployment/svc-b3/probe_failure", Namespace: "ns-b", Resource: "svc-b3", IssueType: "probe_failure", Severity: "warning"}

	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{a1, a2, b1, b2, b3}); err != nil {
		t.Fatalf("UpsertIncidents(initial): %v", err)
	}

	// Cross a1's 10-restart milestone so it has two DETECTED/RestartMilestone
	// events for deriveTrend to compare.
	time.Sleep(1100 * time.Millisecond)
	a1.RestartCount = 20
	if err := s.UpsertIncidents(cluster, "scan-2", []IncidentData{a1, a2, b1, b2, b3}); err != nil {
		t.Fatalf("UpsertIncidents(a1 restart bump): %v", err)
	}
	// Backdate a1's first_seen so its lifetime restart rate is low, making
	// the recent restart burst read as "accelerating" rather than "stable".
	setIncidentFirstSeen(t, s, cluster, a1.Fingerprint, time.Now().Add(-10*24*time.Hour-time.Hour))

	// Resolve a2 and b2 — absent from every scan below, they stay resolved.
	driveToResolved(t, s, cluster, []IncidentData{a1, b1, b3}, "scan-resolve-a2b2")

	// Resolve b1, then reopen it past the flap-absorption window (a genuine
	// recurrence, not a flap).
	driveToResolved(t, s, cluster, []IncidentData{a1, b3}, "scan-resolve-b1")
	backdateIncidentEvents(t, s, cluster, b1.Fingerprint, flapAbsorptionWindow+time.Minute)
	if err := s.UpsertIncidents(cluster, "scan-reopen-b1", []IncidentData{a1, b1, b3}); err != nil {
		t.Fatalf("UpsertIncidents(reopen b1): %v", err)
	}

	sb, err := s.GetMemoryScoreboard(cluster)
	if err != nil {
		t.Fatalf("GetMemoryScoreboard: %v", err)
	}

	if sb.TotalSeen != 5 {
		t.Fatalf("expected TotalSeen=5, got %d", sb.TotalSeen)
	}
	if sb.Resolved != 2 {
		t.Fatalf("expected Resolved=2 (a2, b2), got %d", sb.Resolved)
	}
	if sb.Reopened != 1 {
		t.Fatalf("expected Reopened=1 (b1), got %d", sb.Reopened)
	}
	if sb.Accelerating != 1 {
		t.Fatalf("expected Accelerating=1 (a1), got %d", sb.Accelerating)
	}
	if sb.LongestActiveName != "svc-a1" {
		t.Fatalf("expected LongestActiveName=svc-a1, got %q", sb.LongestActiveName)
	}
	if sb.LongestActiveDays < 9 || sb.LongestActiveDays > 11 {
		t.Fatalf("expected LongestActiveDays ~10, got %d", sb.LongestActiveDays)
	}
	if sb.MostUnstableNamespace != "ns-b" {
		t.Fatalf("expected MostUnstableNamespace=ns-b, got %q", sb.MostUnstableNamespace)
	}
	if sb.MostUnstableCount != 3 {
		t.Fatalf("expected MostUnstableCount=3, got %d", sb.MostUnstableCount)
	}
}

func TestGetRecentEvents(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"

	incA := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}
	incB := IncidentData{Fingerprint: "default/Deployment/svc-b/oom", Namespace: "default", Resource: "svc-b", IssueType: "oom", Severity: "warning"}
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{incA, incB}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}

	var idA, idB int64
	if err := s.db.QueryRow("SELECT id FROM incidents WHERE cluster=? AND fingerprint=?", cluster, incA.Fingerprint).Scan(&idA); err != nil {
		t.Fatalf("lookup idA: %v", err)
	}
	if err := s.db.QueryRow("SELECT id FROM incidents WHERE cluster=? AND fingerprint=?", cluster, incB.Fingerprint).Scan(&idB); err != nil {
		t.Fatalf("lookup idB: %v", err)
	}

	// idA and idB each already have a DETECTED event from UpsertIncidents
	// above; layer two more, newest last, with explicit timestamps so
	// ordering doesn't depend on real wall-clock gaps.
	base := time.Now().Unix()
	insertTestEvent(t, s, idB, base+100, "RestartMilestone") // 2nd newest
	insertTestEvent(t, s, idA, base+200, "SeverityChanged")  // newest

	events, err := s.GetRecentEvents(cluster, time.Now().Add(-time.Hour), 2)
	if err != nil {
		t.Fatalf("GetRecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected limit=2 to return 2 events, got %d: %+v", len(events), events)
	}
	if events[0].Resource != "svc-a" || events[0].EventReason != "SeverityChanged" {
		t.Fatalf("expected newest event (svc-a/SeverityChanged) first, got %+v", events[0])
	}
	if events[1].Resource != "svc-b" || events[1].EventReason != "RestartMilestone" {
		t.Fatalf("expected second-newest event (svc-b/RestartMilestone) second, got %+v", events[1])
	}
	if !events[0].OccurredAt.After(events[1].OccurredAt) {
		t.Fatalf("expected descending order by OccurredAt: %+v vs %+v", events[0].OccurredAt, events[1].OccurredAt)
	}
}

// TestGetRecentEvents_WindowExcludesOlderEvents covers the fixed lookback
// window (e.g. 7 days) buildOverviewData now passes: events older than
// since must be excluded even though GetRecentEvents is otherwise
// unfiltered by event_reason, ordering and limit still apply within the
// window.
func TestGetRecentEvents_WindowExcludesOlderEvents(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"

	inc := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}

	var id int64
	if err := s.db.QueryRow("SELECT id FROM incidents WHERE cluster=? AND fingerprint=?", cluster, inc.Fingerprint).Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}

	window := 7 * 24 * time.Hour
	since := time.Now().Add(-window)

	// One event well outside the window, two inside it.
	insertTestEvent(t, s, id, since.Add(-48*time.Hour).Unix(), "SeverityChanged") // outside: excluded
	insertTestEvent(t, s, id, since.Add(24*time.Hour).Unix(), "RestartMilestone") // inside: 2nd newest
	insertTestEvent(t, s, id, since.Add(6*24*time.Hour).Unix(), "Reopened")       // inside: newest

	events, err := s.GetRecentEvents(cluster, since, 20)
	if err != nil {
		t.Fatalf("GetRecentEvents: %v", err)
	}
	// +1 for the DETECTED event UpsertIncidents itself emitted (well
	// within the window, since the test runs "now").
	if len(events) != 3 {
		t.Fatalf("expected 3 in-window events (Detected, RestartMilestone, Reopened), got %d: %+v", len(events), events)
	}
	for _, e := range events {
		if e.OccurredAt.Before(since) {
			t.Fatalf("expected only events at/after the window start %v, got %+v", since, e)
		}
	}
	if events[0].EventReason != "Detected" {
		t.Fatalf("expected the most recent event (Detected, from UpsertIncidents) first, got %+v", events[0])
	}
	if events[1].EventReason != "Reopened" || events[2].EventReason != "RestartMilestone" {
		t.Fatalf("expected descending order Reopened, RestartMilestone, got %+v then %+v", events[1], events[2])
	}

	// Limit still applies within the window.
	limited, err := s.GetRecentEvents(cluster, since, 1)
	if err != nil {
		t.Fatalf("GetRecentEvents(limit=1): %v", err)
	}
	if len(limited) != 1 || limited[0].EventReason != "Detected" {
		t.Fatalf("expected limit=1 to return only the newest (Detected) event, got %+v", limited)
	}
}

func TestGetChangesSince(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"

	incA := IncidentData{Fingerprint: "default/Deployment/svc-a/crash_loop", Namespace: "default", Resource: "svc-a", IssueType: "crash_loop", Severity: "critical"}
	incB := IncidentData{Fingerprint: "default/Deployment/svc-b/oom", Namespace: "default", Resource: "svc-b", IssueType: "oom", Severity: "warning"}
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{incA, incB}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}

	var idA, idB int64
	if err := s.db.QueryRow("SELECT id FROM incidents WHERE cluster=? AND fingerprint=?", cluster, incA.Fingerprint).Scan(&idA); err != nil {
		t.Fatalf("lookup idA: %v", err)
	}
	if err := s.db.QueryRow("SELECT id FROM incidents WHERE cluster=? AND fingerprint=?", cluster, incB.Fingerprint).Scan(&idB); err != nil {
		t.Fatalf("lookup idB: %v", err)
	}

	// idA and idB's DETECTED events (from UpsertIncidents above) sit well
	// before the cursor. Layer explicit before/after events around a known
	// cursor timestamp.
	cursor := time.Now().Add(10 * time.Hour)
	insertTestEvent(t, s, idA, cursor.Add(-time.Hour).Unix(), "SeverityChanged")   // before cursor: excluded
	insertTestEvent(t, s, idB, cursor.Add(time.Minute).Unix(), "RestartMilestone") // after cursor: 2nd newest
	insertTestEvent(t, s, idA, cursor.Add(time.Hour).Unix(), "Resolved")           // after cursor: newest
	insertTestEvent(t, s, idB, cursor.Add(2*time.Hour).Unix(), "Reopened")         // after cursor: absolute newest, excluded by limit

	events, err := s.GetChangesSince(cluster, cursor, 2)
	if err != nil {
		t.Fatalf("GetChangesSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected limit=2 to return 2 events, got %d: %+v", len(events), events)
	}
	if events[0].Resource != "svc-b" || events[0].EventReason != "Reopened" {
		t.Fatalf("expected newest event (svc-b/Reopened) first, got %+v", events[0])
	}
	if events[1].Resource != "svc-a" || events[1].EventReason != "Resolved" {
		t.Fatalf("expected second-newest event (svc-a/Resolved) second, got %+v", events[1])
	}
	if !events[0].OccurredAt.After(events[1].OccurredAt) {
		t.Fatalf("expected descending order by OccurredAt: %+v vs %+v", events[0].OccurredAt, events[1].OccurredAt)
	}
	for _, e := range events {
		if e.OccurredAt.Before(cursor) {
			t.Fatalf("expected only events at/after cursor %v, got %+v", cursor, e)
		}
	}

	// Raise the limit to confirm the before-cursor SeverityChanged event
	// never appears, no matter how high the limit goes.
	all, err := s.GetChangesSince(cluster, cursor, 10)
	if err != nil {
		t.Fatalf("GetChangesSince(limit=10): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 events at/after cursor, got %d: %+v", len(all), all)
	}
	for _, e := range all {
		if e.EventReason == "SeverityChanged" {
			t.Fatalf("expected before-cursor SeverityChanged event to be excluded, got %+v", all)
		}
	}
}
