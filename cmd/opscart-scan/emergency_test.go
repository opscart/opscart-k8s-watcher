package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// fakeStore embeds NullStore for its no-op defaults and overrides only the
// methods a given test needs to control.
type fakeStore struct {
	store.NullStore

	history     *store.IncidentRecord
	historyErr  error
	timeline    []store.IncidentEvent
	timelineErr error
	upsertErr   error
	resolveErr  error
}

func (f *fakeStore) GetIncidentHistory(cluster, fingerprint string) (*store.IncidentRecord, error) {
	return f.history, f.historyErr
}

func (f *fakeStore) GetIncidentTimeline(cluster, fingerprint string) ([]store.IncidentEvent, error) {
	return f.timeline, f.timelineErr
}

func (f *fakeStore) UpsertIncidents(cluster, scanID string, incidents []store.IncidentData) error {
	return f.upsertErr
}

func (f *fakeStore) ResolveMissing(cluster, scanID string) (int, error) {
	return 0, f.resolveErr
}

func TestIncidentFingerprint_MatchesStoreScheme(t *testing.T) {
	issue := models.EmergencyIssue{
		Namespace: "prod",
		Name:      "checkout-7d9f8b6c5-x7z2m9",
		Reason:    "CrashLoopBackOff",
	}

	got := incidentFingerprint(issue)
	want := "prod/Workload/checkout/CrashLoopBackOff"
	if got != want {
		t.Fatalf("incidentFingerprint = %q, want %q", got, want)
	}

	// Must match store.Fingerprint's own construction exactly, not a
	// parallel scheme that happens to agree on this one input.
	if want2 := store.Fingerprint("prod", "Workload", store.OwnerNameFromPod(issue.Name), issue.Reason); got != want2 {
		t.Fatalf("incidentFingerprint diverges from store.Fingerprint: got %q, store gives %q", got, want2)
	}
}

func TestMapIssuesToIncidents(t *testing.T) {
	issues := []models.EmergencyIssue{
		{
			Namespace: "prod",
			Name:      "checkout-7d9f8b6c5-x7z2m9",
			Reason:    "CrashLoopBackOff",
			Severity:  "critical",
			Message:   "Container app is crash looping",
			Age:       3 * 24 * time.Hour,
			Restarts:  42,
		},
	}

	got := mapIssuesToIncidents(issues)
	if len(got) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(got))
	}

	inc := got[0]
	if inc.Fingerprint != incidentFingerprint(issues[0]) {
		t.Errorf("Fingerprint = %q, want %q", inc.Fingerprint, incidentFingerprint(issues[0]))
	}
	if inc.Namespace != "prod" {
		t.Errorf("Namespace = %q, want prod", inc.Namespace)
	}
	if inc.Resource != "checkout-7d9f8b6c5-x7z2m9" {
		t.Errorf("Resource = %q, want checkout-7d9f8b6c5-x7z2m9", inc.Resource)
	}
	if inc.IssueType != "CrashLoopBackOff" {
		t.Errorf("IssueType = %q, want CrashLoopBackOff", inc.IssueType)
	}
	if inc.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", inc.Severity)
	}
	if inc.RestartCount != 42 {
		t.Errorf("RestartCount = %d, want 42", inc.RestartCount)
	}
	if !strings.Contains(inc.DetailsJSON, `"age_days":3`) {
		t.Errorf("DetailsJSON missing age_days:3, got %s", inc.DetailsJSON)
	}
	if !strings.Contains(inc.DetailsJSON, "crash looping") {
		t.Errorf("DetailsJSON missing message, got %s", inc.DetailsJSON)
	}
}

func TestEnrichIssues_PreExistingIncident(t *testing.T) {
	issue := models.EmergencyIssue{
		Namespace: "prod",
		Name:      "checkout-7d9f8b6c5-x7z2m9",
		Reason:    "CrashLoopBackOff",
		Severity:  "critical",
		Message:   "crash looping",
	}

	fs := &fakeStore{
		history: &store.IncidentRecord{
			FirstSeen: time.Now().Add(-5 * 24 * time.Hour),
			Status:    "active",
		},
		timeline: []store.IncidentEvent{
			{EventType: "DETECTED"},
			{EventType: "REOPENED"},
			{EventType: "REOPENED"},
		},
	}

	enriched := enrichIssues(fs, "prod-cluster", []models.EmergencyIssue{issue})
	if len(enriched) != 1 {
		t.Fatalf("expected 1 enriched issue, got %d", len(enriched))
	}
	if enriched[0].FirstDetected != "5d" {
		t.Errorf("FirstDetected = %q, want 5d", enriched[0].FirstDetected)
	}
	if enriched[0].ReopenCount != 2 {
		t.Errorf("ReopenCount = %d, want 2", enriched[0].ReopenCount)
	}

	var buf bytes.Buffer
	printEnrichedIssue(&buf, enriched[0])
	out := buf.String()
	if !strings.Contains(out, "First detected: 5d ago") {
		t.Errorf("output missing 'First detected: 5d ago' line, got:\n%s", out)
	}
	if !strings.Contains(out, "Reopened: 2x") {
		t.Errorf("output missing 'Reopened: 2x' line, got:\n%s", out)
	}
}

func TestEnrichIssues_BrandNewIncident(t *testing.T) {
	issue := models.EmergencyIssue{
		Namespace: "prod",
		Name:      "checkout-7d9f8b6c5-x7z2m9",
		Reason:    "CrashLoopBackOff",
		Severity:  "critical",
		Message:   "crash looping",
	}

	fs := &fakeStore{
		history: &store.IncidentRecord{
			FirstSeen: time.Now(), // written by this very scan
			Status:    "active",
		},
		timeline: []store.IncidentEvent{
			{EventType: "DETECTED"},
		},
	}

	enriched := enrichIssues(fs, "prod-cluster", []models.EmergencyIssue{issue})
	if enriched[0].FirstDetected != "0s" {
		t.Errorf("FirstDetected = %q, want 0s", enriched[0].FirstDetected)
	}
	if enriched[0].ReopenCount != 0 {
		t.Errorf("ReopenCount = %d, want 0", enriched[0].ReopenCount)
	}

	var buf bytes.Buffer
	printEnrichedIssue(&buf, enriched[0])
	out := buf.String()
	if !strings.Contains(out, "First detected: 0s ago") {
		t.Errorf("output missing 'First detected: 0s ago' line, got:\n%s", out)
	}
	if strings.Contains(out, "Reopened:") {
		t.Errorf("brand-new incident should not print a Reopened line, got:\n%s", out)
	}
}

func TestEnrichIssues_NullStore_NoEnrichment(t *testing.T) {
	issues := []models.EmergencyIssue{
		{
			Namespace: "prod",
			Name:      "checkout-7d9f8b6c5-x7z2m9",
			Reason:    "CrashLoopBackOff",
			Severity:  "critical",
			Message:   "crash looping",
			Restarts:  10,
			Age:       2 * time.Hour,
		},
	}

	enriched := enrichIssues(&store.NullStore{}, "prod-cluster", issues)
	if enriched[0].FirstDetected != "" {
		t.Errorf("FirstDetected = %q, want empty on NullStore", enriched[0].FirstDetected)
	}
	if enriched[0].ReopenCount != 0 {
		t.Errorf("ReopenCount = %d, want 0 on NullStore", enriched[0].ReopenCount)
	}

	var got bytes.Buffer
	printEmergencyIssuesEnriched(&got, enriched)

	var want bytes.Buffer
	printEmergencyIssuesEnriched(&want, []enrichedIssue{{EmergencyIssue: issues[0]}})

	if got.String() != want.String() {
		t.Errorf("stateless output diverges from plain (unenriched) output:\ngot:\n%s\nwant:\n%s", got.String(), want.String())
	}
	if strings.Contains(got.String(), "First detected") || strings.Contains(got.String(), "Reopened") {
		t.Errorf("stateless output should have no enrichment lines, got:\n%s", got.String())
	}
}

func TestPrintEmergencyIssuesEnriched_NoIssues(t *testing.T) {
	var buf bytes.Buffer
	printEmergencyIssuesEnriched(&buf, nil)
	if buf.String() != "✅ No critical issues found!\n" {
		t.Errorf("got %q", buf.String())
	}
}

func TestPersistFindings_WriteFailureDoesNotCrash(t *testing.T) {
	// A closed SQLiteStore rejects further writes ("sql: database is
	// closed"), giving a genuine store.Store failure without touching the
	// store package itself.
	dbPath := filepath.Join(t.TempDir(), "closed.db")
	sqlDB, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	issues := []models.EmergencyIssue{
		{Namespace: "prod", Name: "checkout-abc", Reason: "PodFailed", Severity: "critical"},
	}

	// Must not panic despite every store call failing.
	persistFindings(sqlDB, "prod-cluster", "scan-1", issues)

	enriched := enrichIssues(sqlDB, "prod-cluster", issues)
	if enriched[0].FirstDetected != "" || enriched[0].ReopenCount != 0 {
		t.Errorf("expected silent no-op enrichment on lookup failure, got %+v", enriched[0])
	}
}

func TestPersistFindings_FakeStoreErrorsDoNotCrash(t *testing.T) {
	fs := &fakeStore{
		upsertErr:  errString("boom"),
		resolveErr: errString("boom"),
	}
	issues := []models.EmergencyIssue{
		{Namespace: "prod", Name: "checkout-abc", Reason: "PodFailed", Severity: "critical"},
	}
	persistFindings(fs, "prod-cluster", "scan-1", issues)
}

type errString string

func (e errString) Error() string { return string(e) }
