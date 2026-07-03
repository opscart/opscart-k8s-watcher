package store

import (
	"path/filepath"
	"testing"
	"time"
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
	if version != 1 {
		t.Fatalf("expected user_version 1, got %d", version)
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
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
