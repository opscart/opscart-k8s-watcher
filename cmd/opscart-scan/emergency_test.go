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

// BatchGetIncidentHistory mirrors GetIncidentHistory's single fake record
// across every requested fingerprint, so tests written against the old
// per-issue API still exercise the same scenarios against the batched one.
func (f *fakeStore) BatchGetIncidentHistory(cluster string, fingerprints []string) (map[string]*store.IncidentRecord, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	out := make(map[string]*store.IncidentRecord)
	if f.history != nil {
		for _, fp := range fingerprints {
			out[fp] = f.history
		}
	}
	return out, nil
}

// BatchGetReopenCounts mirrors GetIncidentTimeline's fake REOPENED count
// for every requested id.
func (f *fakeStore) BatchGetReopenCounts(ids []int64) (map[int64]int, error) {
	if f.timelineErr != nil {
		return nil, f.timelineErr
	}
	count := 0
	for _, e := range f.timeline {
		if e.EventType == "REOPENED" {
			count++
		}
	}
	out := make(map[int64]int, len(ids))
	for _, id := range ids {
		out[id] = count
	}
	return out, nil
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

// countingStore counts calls made to each Store lookup method, so tests can
// assert enrichIssues batches its operational-memory lookups instead of
// regressing to the old N+1 pattern of one GetIncidentHistory/
// GetIncidentTimeline call per issue.
type countingStore struct {
	store.NullStore

	historyCalls      int // legacy per-fingerprint GetIncidentHistory
	timelineCalls     int // legacy per-fingerprint GetIncidentTimeline
	batchHistoryCalls int
	batchReopenCalls  int

	records      map[string]*store.IncidentRecord
	reopenCounts map[int64]int
}

func (c *countingStore) GetIncidentHistory(cluster, fingerprint string) (*store.IncidentRecord, error) {
	c.historyCalls++
	return nil, nil
}

func (c *countingStore) GetIncidentTimeline(cluster, fingerprint string) ([]store.IncidentEvent, error) {
	c.timelineCalls++
	return nil, nil
}

func (c *countingStore) BatchGetIncidentHistory(cluster string, fingerprints []string) (map[string]*store.IncidentRecord, error) {
	c.batchHistoryCalls++
	out := make(map[string]*store.IncidentRecord)
	for _, fp := range fingerprints {
		if rec, ok := c.records[fp]; ok {
			out[fp] = rec
		}
	}
	return out, nil
}

func (c *countingStore) BatchGetReopenCounts(ids []int64) (map[int64]int, error) {
	c.batchReopenCalls++
	out := make(map[int64]int, len(ids))
	for _, id := range ids {
		out[id] = c.reopenCounts[id]
	}
	return out, nil
}

func TestEnrichIssues_BatchesLookups_NoPerIssueQueries(t *testing.T) {
	issues := []models.EmergencyIssue{
		{Namespace: "prod", Name: "checkout-7d9f8b6c5-x7z2m9", Reason: "CrashLoopBackOff", Severity: "critical"},
		{Namespace: "prod", Name: "billing-6c9f8b6c5-x7z2m9", Reason: "OOMKilled", Severity: "critical"},
		{Namespace: "prod", Name: "auth-5c9f8b6c5-x7z2m9", Reason: "CrashLoopBackOff", Severity: "high"},
	}
	fp0 := incidentFingerprint(issues[0])
	fp1 := incidentFingerprint(issues[1])

	cs := &countingStore{
		records: map[string]*store.IncidentRecord{
			fp0: {ID: 1, FirstSeen: time.Now().Add(-2 * 24 * time.Hour)},
			fp1: {ID: 2, FirstSeen: time.Now().Add(-1 * time.Hour)},
			// issues[2]'s fingerprint has no record: brand-new issue.
		},
		reopenCounts: map[int64]int{1: 3, 2: 0},
	}

	enriched := enrichIssues(cs, "prod-cluster", issues)

	if cs.historyCalls != 0 || cs.timelineCalls != 0 {
		t.Fatalf("expected no per-issue lookups, got historyCalls=%d timelineCalls=%d", cs.historyCalls, cs.timelineCalls)
	}
	if cs.batchHistoryCalls != 1 {
		t.Errorf("batchHistoryCalls = %d, want exactly 1 regardless of issue count", cs.batchHistoryCalls)
	}
	if cs.batchReopenCalls != 1 {
		t.Errorf("batchReopenCalls = %d, want exactly 1 regardless of issue count", cs.batchReopenCalls)
	}

	if enriched[0].FirstDetected != "2d" || enriched[0].ReopenCount != 3 {
		t.Errorf("issue 0 = %+v, want FirstDetected=2d ReopenCount=3", enriched[0])
	}
	if enriched[1].FirstDetected != "1h" || enriched[1].ReopenCount != 0 {
		t.Errorf("issue 1 = %+v, want FirstDetected=1h ReopenCount=0", enriched[1])
	}
	if enriched[2].FirstDetected != "" || enriched[2].ReopenCount != 0 {
		t.Errorf("issue 2 (no history) = %+v, want zero enrichment", enriched[2])
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
