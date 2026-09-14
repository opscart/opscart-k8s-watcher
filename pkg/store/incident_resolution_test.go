package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// newResolutionIncident returns a distinct crash_loop IncidentData for name,
// used across this file wherever the specific issue type/fields don't matter.
func newResolutionIncident(name string) IncidentData {
	return IncidentData{
		Fingerprint: fmt.Sprintf("default/Deployment/%s/crash_loop", name),
		Namespace:   "default", Resource: name, IssueType: "crash_loop", Severity: "critical",
	}
}

func TestResolution_FirstAbsenceDoesNotResolve(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"
	inc := newResolutionIncident("svc-a")
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}

	resolved, err := s.ResolveMissing(cluster, "scan-2")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 0 {
		t.Fatalf("expected no resolution on first absence, got %d", resolved)
	}
	rec, err := s.GetIncidentHistory(cluster, inc.Fingerprint)
	if err != nil || rec.Status != "active" {
		t.Fatalf("expected still active, got %+v, err=%v", rec, err)
	}

	var absentSince *int64
	if err := s.db.QueryRow(
		"SELECT absent_since FROM incidents WHERE cluster=? AND fingerprint=?",
		cluster, inc.Fingerprint,
	).Scan(&absentSince); err != nil {
		t.Fatalf("query absent_since: %v", err)
	}
	if absentSince == nil {
		t.Fatalf("expected absent_since to be recorded on the first missing evaluation, got NULL")
	}
}

// TestResolution_StaleLastSeenDoesNotResolveImmediately is the required
// regression case: an incident whose last positive evidence is old (well
// past resolveAfter) must NOT resolve on the first missing evaluation. Its
// absence interval begins at that first evaluation, not at last_seen — the
// exact bug the anchor-on-last_seen design had.
func TestResolution_StaleLastSeenDoesNotResolveImmediately(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"
	inc := newResolutionIncident("svc-a")
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}

	// last_seen is far in the past (much older than resolveAfter), but the
	// incident has never actually been evaluated as absent before now.
	if _, err := s.db.Exec(
		`UPDATE incidents SET last_seen = ? WHERE cluster=? AND fingerprint=?`,
		time.Now().Add(-24*time.Hour).Unix(), cluster, inc.Fingerprint,
	); err != nil {
		t.Fatalf("age last_seen: %v", err)
	}

	resolved, err := s.ResolveMissing(cluster, "scan-2")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 0 {
		t.Fatalf("resolved on the first missing evaluation despite a stale last_seen — absence must start now, not at last_seen; got %d", resolved)
	}
	rec, err := s.GetIncidentHistory(cluster, inc.Fingerprint)
	if err != nil || rec.Status != "active" {
		t.Fatalf("expected still active, got %+v, err=%v", rec, err)
	}

	var absentSince int64
	if err := s.db.QueryRow(
		"SELECT absent_since FROM incidents WHERE cluster=? AND fingerprint=?",
		cluster, inc.Fingerprint,
	).Scan(&absentSince); err != nil {
		t.Fatalf("query absent_since: %v", err)
	}
	if age := time.Since(time.Unix(absentSince, 0)); age > time.Minute {
		t.Fatalf("expected absent_since to be established now (~0s old), got %s old", age)
	}

	// Now advance the absence itself (not last_seen) and prove it resolves
	// only once resolveAfter has elapsed since that established start.
	backdateAbsentSince(t, s, cluster, inc.Fingerprint, resolveAfter-2*time.Second)
	if resolved, err := s.ResolveMissing(cluster, "scan-3"); err != nil || resolved != 0 {
		t.Fatalf("resolved before resolveAfter elapsed since absent_since: resolved=%d err=%v", resolved, err)
	}

	backdateAbsentSince(t, s, cluster, inc.Fingerprint, resolveAfter)
	resolved, err = s.ResolveMissing(cluster, "scan-4")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("expected resolution once resolveAfter elapsed since absent_since, got %d", resolved)
	}
}

// TestResolution_RapidCallsDoNotAccelerate proves the exact scenario the
// architecture doc calls out: T+0, T+2s, T+4s, T+30s of absence, all well
// short of resolveAfter, must never resolve regardless of how many times
// ResolveMissing is evaluated in between, and regardless of how many of
// those calls happen after absent_since has already been recorded.
func TestResolution_RapidCallsDoNotAccelerate(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"
	inc := newResolutionIncident("svc-a")
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}

	// T+0: first evaluation records absent_since=now; it must not resolve.
	if resolved, err := s.ResolveMissing(cluster, "scan-check-0"); err != nil || resolved != 0 {
		t.Fatalf("ResolveMissing(T+0): resolved=%d err=%v", resolved, err)
	}

	// T+2s, T+4s, T+30s: each sets absent_since to that much elapsed time
	// and re-evaluates. None are anywhere near resolveAfter, and repeated
	// evaluation alone must never advance absent_since on its own.
	for i, elapsed := range []time.Duration{2 * time.Second, 4 * time.Second, 30 * time.Second} {
		backdateAbsentSince(t, s, cluster, inc.Fingerprint, elapsed)
		scanID := fmt.Sprintf("scan-check-%d", i+1)
		resolved, err := s.ResolveMissing(cluster, scanID)
		if err != nil {
			t.Fatalf("ResolveMissing(%d): %v", i+1, err)
		}
		if resolved != 0 {
			t.Fatalf("resolved after only %s of absence; rapid calls must not accelerate resolution", elapsed)
		}
	}

	rec, err := s.GetIncidentHistory(cluster, inc.Fingerprint)
	if err != nil || rec.Status != "active" {
		t.Fatalf("expected still active, got %+v, err=%v", rec, err)
	}
}

func TestResolution_BoundaryJustBeforeDurationDoesNotResolve(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"
	inc := newResolutionIncident("svc-a")
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}
	if _, err := s.ResolveMissing(cluster, "scan-2"); err != nil {
		t.Fatalf("ResolveMissing [record absence]: %v", err)
	}

	// 2s of margin (not 1s) avoids flakiness from the two-second-granularity
	// race between backdating and ResolveMissing's own time.Now() call.
	backdateAbsentSince(t, s, cluster, inc.Fingerprint, resolveAfter-2*time.Second)
	resolved, err := s.ResolveMissing(cluster, "scan-3")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 0 {
		t.Fatalf("expected no resolution just before resolveAfter elapses, got %d", resolved)
	}
}

func TestResolution_BoundaryAtDurationResolves(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"
	inc := newResolutionIncident("svc-a")
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}
	if _, err := s.ResolveMissing(cluster, "scan-2"); err != nil {
		t.Fatalf("ResolveMissing [record absence]: %v", err)
	}

	backdateAbsentSince(t, s, cluster, inc.Fingerprint, resolveAfter)
	resolved, err := s.ResolveMissing(cluster, "scan-3")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("expected resolution at exactly resolveAfter elapsed, got %d", resolved)
	}
	rec, err := s.GetIncidentHistory(cluster, inc.Fingerprint)
	if err != nil || rec.Status != "resolved" {
		t.Fatalf("expected resolved, got %+v, err=%v", rec, err)
	}
}

// TestResolution_ReappearanceBeforeDurationResetsAbsence proves reappearance
// clears the pending absence rather than merely pausing it: without the
// reset, the two partial absences below (each short of resolveAfter on
// their own) would together exceed it.
func TestResolution_ReappearanceBeforeDurationResetsAbsence(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"
	inc := newResolutionIncident("svc-a")
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}
	if _, err := s.ResolveMissing(cluster, "scan-2"); err != nil {
		t.Fatalf("ResolveMissing [record absence]: %v", err)
	}
	backdateAbsentSince(t, s, cluster, inc.Fingerprint, resolveAfter-30*time.Second)
	if resolved, err := s.ResolveMissing(cluster, "scan-2"); err != nil || resolved != 0 {
		t.Fatalf("ResolveMissing: resolved=%d err=%v", resolved, err)
	}

	// Reappears — this must clear absent_since, not just leave it stale.
	if err := s.UpsertIncidents(cluster, "scan-3", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(reappear): %v", err)
	}
	var absentSince *int64
	if err := s.db.QueryRow(
		"SELECT absent_since FROM incidents WHERE cluster=? AND fingerprint=?",
		cluster, inc.Fingerprint,
	).Scan(&absentSince); err != nil {
		t.Fatalf("query absent_since: %v", err)
	}
	if absentSince != nil {
		t.Fatalf("expected absent_since cleared on reappearance, got %v", *absentSince)
	}

	// A second absence, on its own short of resolveAfter, must not resolve
	// even though the first absence (before reappearing) was also close to
	// resolveAfter — the two must not add up.
	if _, err := s.ResolveMissing(cluster, "scan-4"); err != nil {
		t.Fatalf("ResolveMissing [record absence]: %v", err)
	}
	backdateAbsentSince(t, s, cluster, inc.Fingerprint, resolveAfter-30*time.Second)
	resolved, err := s.ResolveMissing(cluster, "scan-4")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 0 {
		t.Fatalf("expected reappearance to reset the absence clock, got resolved=%d", resolved)
	}
	rec, err := s.GetIncidentHistory(cluster, inc.Fingerprint)
	if err != nil || rec.Status != "active" {
		t.Fatalf("expected active, got %+v, err=%v", rec, err)
	}
}

// TestResolution_NewAbsenceAfterReopenStartsFreshDuration proves a later
// absence, after a genuine resolve/reopen cycle, requires its own full
// resolveAfter rather than inheriting anything from the earlier cycle.
func TestResolution_NewAbsenceAfterReopenStartsFreshDuration(t *testing.T) {
	s := openTestStore(t)
	cluster := "test-cluster"
	inc := newResolutionIncident("svc-a")
	if err := s.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}
	driveToResolved(t, s, cluster, nil, "scan-first-absence")

	rec, err := s.GetIncidentHistory(cluster, inc.Fingerprint)
	if err != nil || rec.Status != "resolved" {
		t.Fatalf("expected resolved, got %+v, err=%v", rec, err)
	}

	// Reopen outside the flap-absorption window so this is a genuine
	// recurrence, not an absorbed flap.
	backdateIncidentEvents(t, s, cluster, inc.Fingerprint, flapAbsorptionWindow+time.Minute)
	if err := s.UpsertIncidents(cluster, "scan-reopen", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents(reopen): %v", err)
	}

	if err := s.UpsertIncidents(cluster, "scan-second-miss", nil); err != nil {
		t.Fatalf("UpsertIncidents(miss): %v", err)
	}
	if resolved, err := s.ResolveMissing(cluster, "scan-second-miss"); err != nil || resolved != 0 {
		t.Fatalf("ResolveMissing [record absence]: resolved=%d err=%v", resolved, err)
	}

	backdateAbsentSince(t, s, cluster, inc.Fingerprint, resolveAfter-2*time.Second)
	resolved, err := s.ResolveMissing(cluster, "scan-second-miss")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 0 {
		t.Fatalf("expected the new absence to require its own full duration, got resolved=%d", resolved)
	}

	backdateAbsentSince(t, s, cluster, inc.Fingerprint, resolveAfter)
	resolved, err = s.ResolveMissing(cluster, "scan-second-miss")
	if err != nil {
		t.Fatalf("ResolveMissing: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("expected the second absence to resolve once its own duration elapsed, got %d", resolved)
	}
}

// TestResolution_SurvivesRestart proves the pending absence is not an
// in-memory timer: it must be exactly as far along after closing and
// reopening the store as it was before.
func TestResolution_SurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolution-restart.db")
	cluster := "test-cluster"
	inc := newResolutionIncident("svc-a")

	s1, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(1): %v", err)
	}
	if err := s1.UpsertIncidents(cluster, "scan-1", []IncidentData{inc}); err != nil {
		t.Fatalf("UpsertIncidents: %v", err)
	}
	if _, err := s1.ResolveMissing(cluster, "scan-2"); err != nil {
		t.Fatalf("ResolveMissing [record absence]: %v", err)
	}
	backdateAbsentSince(t, s1, cluster, inc.Fingerprint, resolveAfter-30*time.Second)
	if resolved, err := s1.ResolveMissing(cluster, "scan-2"); err != nil || resolved != 0 {
		t.Fatalf("ResolveMissing before restart: resolved=%d err=%v", resolved, err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(2): %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	resolved, err := s2.ResolveMissing(cluster, "scan-3")
	if err != nil {
		t.Fatalf("ResolveMissing(too early after restart): %v", err)
	}
	if resolved != 0 {
		t.Fatalf("expected the pending absence to survive restart without resolving early, got %d", resolved)
	}

	backdateAbsentSince(t, s2, cluster, inc.Fingerprint, resolveAfter)
	resolved, err = s2.ResolveMissing(cluster, "scan-4")
	if err != nil {
		t.Fatalf("ResolveMissing after restart: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("expected resolution once resolveAfter elapsed, got %d", resolved)
	}
}
