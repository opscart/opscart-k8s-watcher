package main

import (
	"bytes"
	"database/sql"
	"fmt"
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
	want := "prod/Workload/checkout/crash_loop"
	if got != want {
		t.Fatalf("incidentFingerprint = %q, want %q", got, want)
	}

	// Must match store.Fingerprint's own construction exactly, not a
	// parallel scheme that happens to agree on this one input.
	if want2 := store.Fingerprint("prod", "Workload", store.OwnerNameFromPod(issue.Name), canonicalIssueType(issue.Reason)); got != want2 {
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
	if inc.IssueType != "crash_loop" {
		t.Errorf("IssueType = %q, want crash_loop", inc.IssueType)
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
	incidents    []store.IncidentSummary
}

func (c *countingStore) QueryIncidents(f store.IncidentFilter) ([]store.IncidentSummary, int, error) {
	start := (f.Page - 1) * f.PerPage
	if start < 0 {
		start = 0
	}
	if start >= len(c.incidents) {
		return nil, len(c.incidents), nil
	}
	end := start + f.PerPage
	if end > len(c.incidents) {
		end = len(c.incidents)
	}
	return c.incidents[start:end], len(c.incidents), nil
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
	if !strings.Contains(out, "First observed by this OMA: 5d ago") {
		t.Errorf("output missing OMA first-observed line, got:\n%s", out)
	}
	if strings.Contains(out, "Reopened:") {
		t.Errorf("Emergency output must not render recurrence aggregates, got:\n%s", out)
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
	if !strings.Contains(out, "First observed by this OMA: 0s ago") {
		t.Errorf("output missing OMA first-observed line, got:\n%s", out)
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
	if !strings.Contains(got.String(), "First observed by this OMA: —") || strings.Contains(got.String(), "Reopened") {
		t.Errorf("stateless output should show unknown incident age and no recurrence aggregate, got:\n%s", got.String())
	}
}

func TestPrintEmergencyIssuesEnriched_NoIssues(t *testing.T) {
	var buf bytes.Buffer
	printEmergencyIssuesEnriched(&buf, nil)
	if buf.String() != "✅ No critical issues found!\n" {
		t.Errorf("got %q", buf.String())
	}
}

func TestEmergencyMcoreProbeFailureUsesCanonicalFirstSeen(t *testing.T) {
	issue := models.EmergencyIssue{
		Resource: "pod", Namespace: "demo-apps",
		Name:   "message-processor-68b7798664-p47cz",
		Reason: "CrashLoopBackOff (ProbeFailure)", Severity: "critical",
		Age: 10 * time.Second,
	}
	firstSeen := time.Now().Add(-12 * 24 * time.Hour)
	fp := "demo-apps/Workload/message-processor/probe_failure"
	cs := &countingStore{records: map[string]*store.IncidentRecord{
		fp: {ID: 77, Status: "active", FirstSeen: firstSeen},
	}}
	enriched := enrichIssues(cs, "prod", []models.EmergencyIssue{issue})
	var buf bytes.Buffer
	printEmergencyIssuesEnriched(&buf, enriched)
	out := buf.String()
	if !strings.Contains(out, "First observed by this OMA: 12d ago") {
		t.Fatalf("canonical incident first_seen was not rendered:\n%s", out)
	}
	if strings.Contains(out, "First observed by this OMA: 0s") {
		t.Fatalf("pod age leaked into incident age:\n%s", out)
	}
}

func TestHistoricalAliasReconciliationDisplaysEarliestOOMKilled(t *testing.T) {
	now := time.Now()
	issue := models.EmergencyIssue{Namespace: "data", Name: "stream-processor-66c474d5fd-9zpwq", Reason: "CrashLoopBackOff (OOMKilled)"}
	fp := incidentFingerprint(issue)
	cs := &countingStore{
		records: map[string]*store.IncidentRecord{fp: {ID: 2, Status: "active", FirstSeen: now.Add(-11 * time.Minute)}},
		incidents: []store.IncidentSummary{
			{Namespace: "data", Resource: issue.Name, IssueType: "OOMKilled", FirstSeen: now.Add(-10 * 24 * time.Hour)},
			{Namespace: "data", Resource: issue.Name, IssueType: "oomkilled", FirstSeen: now.Add(-11 * time.Minute), Status: "active"},
		},
	}
	got := enrichIssues(cs, "prod", []models.EmergencyIssue{issue})
	if got[0].FirstDetected != "10d" {
		t.Fatalf("FirstDetected = %q, want legacy OOM history of 10d", got[0].FirstDetected)
	}
}

func TestHistoricalAliasReconciliationDisplaysEarliestCrashLoop(t *testing.T) {
	now := time.Now()
	issue := models.EmergencyIssue{Namespace: "prod", Name: "token-service-786498c5c-phg2g", Reason: "CrashLoopBackOff"}
	fp := incidentFingerprint(issue)
	cs := &countingStore{
		records: map[string]*store.IncidentRecord{fp: {ID: 3, Status: "active", FirstSeen: now.Add(-11 * time.Minute)}},
		incidents: []store.IncidentSummary{
			{Namespace: "prod", Resource: issue.Name, IssueType: "CrashLoopBackOff", FirstSeen: now.Add(-9 * 24 * time.Hour)},
			{Namespace: "prod", Resource: issue.Name, IssueType: "crash_loop", FirstSeen: now.Add(-11 * time.Minute)},
		},
	}
	got := enrichIssues(cs, "prod", []models.EmergencyIssue{issue})
	if got[0].FirstDetected != "9d" {
		t.Fatalf("FirstDetected = %q, want legacy crash-loop history of 9d", got[0].FirstDetected)
	}
}

func TestHistoricalAliasIdentityIsolation(t *testing.T) {
	now := time.Now()
	issues := []models.EmergencyIssue{
		{Namespace: "risk", Name: "fraud-detection-7cddf79d98-jxmtx", Reason: "CrashLoopBackOff"},
		{Namespace: "risk", Name: "fraud-detection-7cddf79d98-jxmtx", Reason: "CrashLoopBackOff (ProbeFailure)"},
		{Namespace: "risk", Name: "fraud-detection-7cddf79d98-jxmtx", Reason: "CrashLoopBackOff (OOMKilled)"},
		{Namespace: "other", Name: "fraud-detection-7cddf79d98-jxmtx", Reason: "CrashLoopBackOff"},
	}
	legacy := []store.IncidentSummary{
		{Namespace: "risk", Resource: "fraud-detection-7cddf79d98-old12", IssueType: "CrashLoopBackOff", FirstSeen: now.Add(-10 * 24 * time.Hour)},
		{Namespace: "risk", Resource: "fraud-detection-7cddf79d98-old12", IssueType: "ProbeFailure", FirstSeen: now.Add(-8 * 24 * time.Hour)},
		{Namespace: "risk", Resource: "fraud-detection-7cddf79d98-old12", IssueType: "OOMKilled", FirstSeen: now.Add(-6 * 24 * time.Hour)},
		{Namespace: "other", Resource: "fraud-detection-7cddf79d98-old12", IssueType: "CrashLoopBackOff", FirstSeen: now.Add(-2 * 24 * time.Hour)},
	}
	got := reconcileHistoricalFirstSeen(issues, legacy)
	wants := []time.Time{now.Add(-10 * 24 * time.Hour), now.Add(-8 * 24 * time.Hour), now.Add(-6 * 24 * time.Hour), now.Add(-2 * 24 * time.Hour)}
	for i, issue := range issues {
		if value := got[findingMemoryIdentity(issue)]; !value.Equal(wants[i]) {
			t.Fatalf("identity %d reconciled to %s, want %s", i, value, wants[i])
		}
	}
}

func TestHistoricalAliasExactCanonicalRecordWithoutLegacy(t *testing.T) {
	firstSeen := time.Now().Add(-4 * time.Hour)
	issue := models.EmergencyIssue{Namespace: "prod", Name: "api-7cddf79d98-jxmtx", Reason: "CrashLoopBackOff"}
	got := reconcileHistoricalFirstSeen([]models.EmergencyIssue{issue}, []store.IncidentSummary{{
		Namespace: "prod", Resource: issue.Name, IssueType: "crash_loop", FirstSeen: firstSeen,
	}})
	if value := got[findingMemoryIdentity(issue)]; !value.Equal(firstSeen) {
		t.Fatalf("canonical first_seen = %s, want %s", value, firstSeen)
	}
}

func TestGroupedFailedCronJobPodsReconcileHeader(t *testing.T) {
	failedAt := time.Date(2026, 7, 19, 20, 49, 13, 0, time.UTC)
	issues := []enrichedIssue{
		{EmergencyIssue: models.EmergencyIssue{Resource: "pod", Namespace: "batch", Name: "backup-a", Reason: "PodFailed", Severity: "critical", OwnerKind: "CronJob", OwnerName: "backup", FailureObservedAt: failedAt}},
		{EmergencyIssue: models.EmergencyIssue{Resource: "pod", Namespace: "batch", Name: "backup-b", Reason: "PodFailed", Severity: "critical", OwnerKind: "CronJob", OwnerName: "backup", FailureObservedAt: failedAt.Add(time.Hour)}},
	}
	var buf bytes.Buffer
	printEmergencyIssuesEnriched(&buf, issues)
	out := buf.String()
	if !strings.Contains(out, "🔴 CRITICAL: 0    🟡 HIGH: 0    🟠 MEDIUM: 1") || !strings.Contains(out, "Failed pods: 2") {
		t.Fatalf("failed CronJob pods were not grouped/reconciled:\n%s", out)
	}
	if strings.Contains(out, "batch/backup-a") || strings.Contains(out, "batch/backup-b") {
		t.Fatalf("historical failed pods rendered independently:\n%s", out)
	}
}

func TestSuppressSecondaryRestartNoiseSameWorkload(t *testing.T) {
	issues := []enrichedIssue{
		{EmergencyIssue: models.EmergencyIssue{Resource: "pod", Namespace: "prod", Name: "api-68b7798664-a1b2c", Reason: "CrashLoopBackOff", Severity: "critical"}},
		{EmergencyIssue: models.EmergencyIssue{Resource: "pod", Namespace: "prod", Name: "api-68b7798664-d4e5f", Reason: "HighRestartCount", Severity: "medium"}},
		{EmergencyIssue: models.EmergencyIssue{Resource: "pod", Namespace: "other", Name: "api-68b7798664-g7h8i", Reason: "HighRestartCount", Severity: "medium"}},
	}
	got := suppressSecondaryRestartNoise(&store.NullStore{}, "prod", issues)
	if len(got) != 2 || got[1].Namespace != "other" {
		t.Fatalf("suppression crossed or failed workload/namespace boundary: %+v", got)
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

// TestApplyCriticalDebounce_KeepsCriticalWhenActiveIncidentExists is the
// core fixture: a pod with an existing active CRITICAL incident in the
// store, whose live scan this run shows as milder (Running/high-restart),
// must still display CRITICAL, not the live MEDIUM classification.
// fakeStore mirrors its single fake record across every fingerprint
// queried (see fakeStore.BatchGetIncidentHistory's doc comment), so it
// can't distinguish which of criticalDebounceReasons is "really" active —
// applyCriticalDebounce takes the first match in priority order, which is
// "CrashLoopBackOff (OOMKilled)".
func TestApplyCriticalDebounce_KeepsCriticalWhenActiveIncidentExists(t *testing.T) {
	live := []enrichedIssue{
		issue("prod", "payment-processor-6c9f8b6c5-x7z2m9", "medium", "HighRestartCount",
			"Container app has restarted 6412 times", 6412),
	}
	fs := &fakeStore{
		history: &store.IncidentRecord{Status: "active"},
	}

	got := applyCriticalDebounce(fs, "prod-cluster", live)

	if len(got) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(got))
	}
	if got[0].Severity != "critical" {
		t.Errorf("Severity = %q, want critical", got[0].Severity)
	}
	if !isCriticalDebounceReason(got[0].Reason) {
		t.Errorf("Reason = %q, want one of criticalDebounceReasons", got[0].Reason)
	}
	if got[0].Restarts != 6412 {
		t.Errorf("Restarts = %d, want unchanged 6412", got[0].Restarts)
	}
}

// TestApplyCriticalDebounce_NewIssueStaysAtLiveSeverity is the "genuinely
// new issue" fixture: no existing incident in the store means a
// MEDIUM-level live finding is left exactly as classified, not inflated
// to CRITICAL just because the debounce check ran.
func TestApplyCriticalDebounce_NewIssueStaysAtLiveSeverity(t *testing.T) {
	live := []enrichedIssue{
		issue("prod", "worker-6c9f8b6c5-x7z2m9", "medium", "HighRestartCount",
			"Container app has restarted 12 times", 12),
	}
	fs := &fakeStore{} // no history for any fingerprint

	got := applyCriticalDebounce(fs, "prod-cluster", live)

	if got[0].Severity != "medium" || got[0].Reason != "HighRestartCount" {
		t.Errorf("brand-new issue was altered: %+v", got[0])
	}
}

// TestApplyCriticalDebounce_ResolvedIncidentNotResurrected is the
// "existing RESOLVED incident, live scan shows it healthy" fixture: a
// resolved incident must never resurrect a milder live finding into
// CRITICAL.
func TestApplyCriticalDebounce_ResolvedIncidentNotResurrected(t *testing.T) {
	live := []enrichedIssue{
		issue("prod", "worker-6c9f8b6c5-x7z2m9", "medium", "HighRestartCount",
			"Container app has restarted 11 times", 11),
	}
	fs := &fakeStore{
		history: &store.IncidentRecord{Status: "resolved"},
	}

	got := applyCriticalDebounce(fs, "prod-cluster", live)

	if got[0].Severity != "medium" || got[0].Reason != "HighRestartCount" {
		t.Errorf("resolved incident was incorrectly resurrected: %+v", got[0])
	}
}

func TestApplyCriticalDebounce_ActiveCanonicalBeatsOlderResolvedLegacyAlias(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "aliases.db")
	sqlStore, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalFP := store.Fingerprint("auth", "Workload", "token-service", "crash_loop")
	if err := sqlStore.UpsertIncidents("opscart", "canonical", []store.IncidentData{{
		Fingerprint: canonicalFP, Namespace: "auth", Resource: "token-service-current",
		IssueType: "crash_loop", Severity: "critical", RestartCount: 40,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlStore.Close(); err != nil {
		t.Fatal(err)
	}

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := rawDB.Exec(`UPDATE incidents SET first_seen=? WHERE cluster=? AND fingerprint=?`, now-3600, "opscart", canonicalFP); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(`INSERT INTO incidents
		(fingerprint,cluster,namespace,resource,issue_type,severity,first_seen,last_seen,status,last_scan_id,current_restart_count,missing_scans)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, "auth/Workload/token-service/CrashLoopBackOff", "opscart", "auth", "token-service-old", "CrashLoopBackOff", "critical", now-86400, now-7200, "resolved", "legacy", 20, 3); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	sqlStore, err = store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlStore.Close()
	live := []enrichedIssue{issue("auth", "token-service-7cddf79d98-jxmtx", "medium", "HighRestartCount", "Container app has restarted 41 times", 41)}
	got := applyCriticalDebounce(sqlStore, "opscart", live)
	if len(got) != 1 || got[0].Severity != "critical" || got[0].Reason != "CrashLoopBackOff" {
		t.Fatalf("active canonical crash loop was not preserved: %+v", got)
	}
}

// TestApplyCriticalDebounce_NullStore_IsExactNoOp is the "--stateless
// equivalent" fixture: on NullStore, output must be byte-identical to
// running with applyCriticalDebounce's code path entirely absent, proving
// the no-explicit-stateless-branch design goal actually degrades
// correctly rather than by coincidence.
func TestApplyCriticalDebounce_NullStore_IsExactNoOp(t *testing.T) {
	live := []enrichedIssue{
		issue("prod", "worker-6c9f8b6c5-x7z2m9", "medium", "HighRestartCount",
			"Container app has restarted 11 times", 11),
	}

	withDebounce := applyCriticalDebounce(&store.NullStore{}, "prod-cluster", append([]enrichedIssue{}, live...))

	var got, want bytes.Buffer
	printEmergencyIssuesEnriched(&got, withDebounce)
	printEmergencyIssuesEnriched(&want, live)

	if got.String() != want.String() {
		t.Errorf("NullStore output diverges from debounce-absent output:\ngot:\n%s\nwant:\n%s", got.String(), want.String())
	}
}

// TestApplyCriticalDebounce_RealScenario_PaymentProcessor reproduces the
// exact session scenario that motivated this task: payment-processor was
// CRITICAL (CrashLoopBackOff) in run 1, and run 2 — five minutes later,
// landing during the pod's brief post-backoff Running window — saw it as
// milder. Run 2's display must still report it CRITICAL, using run 1's
// persisted state.
func TestApplyCriticalDebounce_RealScenario_PaymentProcessor(t *testing.T) {
	// Run 1: persisted to the store as an active CRITICAL incident.
	run1 := []models.EmergencyIssue{
		{Namespace: "prod", Name: "payment-processor-6c9f8b6c5-x7z2m9", Reason: "CrashLoopBackOff",
			Severity: "critical", Message: "Container app is crash looping: back-off restarting failed container", Restarts: 8},
	}
	fs := &fakeStore{
		history: &store.IncidentRecord{Status: "active"},
	}
	persistFindings(fs, "test-cluster", "scan-1", run1)

	// Run 2: live scan lands mid-backoff and classifies it milder.
	run2 := []enrichedIssue{
		issue("prod", "payment-processor-6c9f8b6c5-x7z2m9", "medium", "HighRestartCount",
			"Container app has restarted 9 times", 9),
	}

	got := applyCriticalDebounce(fs, "test-cluster", run2)

	if got[0].Severity != "critical" {
		t.Errorf("Severity = %q, want critical (run 1's persisted state should have been consulted)", got[0].Severity)
	}
	if !isCriticalDebounceReason(got[0].Reason) {
		t.Errorf("Reason = %q, want one of criticalDebounceReasons", got[0].Reason)
	}

	var buf bytes.Buffer
	printEmergencyIssuesEnriched(&buf, got)
	if !strings.Contains(buf.String(), "CrashLoopBackOff") {
		t.Errorf("printed output should still show CrashLoopBackOff, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "MEDIUM PRIORITY") {
		t.Errorf("payment-processor should not be counted under MEDIUM this run, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "🟠 MEDIUM: 0") {
		t.Errorf("expected MEDIUM count of 0 in header, got:\n%s", buf.String())
	}
}

// countPodBlocks counts how many times a pod identity line ("namespace/
// pod-name") appears in printed output — the number of separate entry
// blocks for that pod, regardless of tier or merge shape.
func countPodBlocks(output, namespace, name string) int {
	return strings.Count(output, namespace+"/"+name)
}

// TestFullPipeline_NoDuplicateWhenPodAlreadyLiveCritical is the regression
// fixture for the reported bug, run through the real pipeline order
// (classifyIssues, then enrich/debounce, then print): cluster.go's
// analyzePodForIssues runs independent per-container checks (not an
// if/else chain), so a pod that's genuinely CRITICAL this run (live
// OOMKilled, LastTerminationState persisting through a brief Running
// window) commonly ALSO produces a raw MEDIUM HighRestartCount signal for
// the same container in the same scan. classifyIssues collapses that pair
// to one issue before debounce or printing ever sees it, so — unlike the
// old print-time dedup this replaces — there is no second, synthetic
// entry to guard against downstream. History shows an active
// CrashLoopBackOff incident for this pod (from an earlier scan), which
// debounce is free to ignore since the live signal is already CRITICAL.
func TestFullPipeline_NoDuplicateWhenPodAlreadyLiveCritical(t *testing.T) {
	raw := []models.EmergencyIssue{
		// Raw order mirrors cluster.go's fixed per-container append
		// order: OOMKilled is appended before HighRestartCount.
		rawIssue("data-pipeline", "stream-processor-66c474d5fd-9zpwq", "critical", "OOMKilled",
			"Container stress killed due to out of memory", 6301),
		rawIssue("data-pipeline", "stream-processor-66c474d5fd-9zpwq", "medium", "HighRestartCount",
			"Container stress has restarted 6301 times", 6301),
	}
	fs := &fakeStore{
		history: &store.IncidentRecord{Status: "active"},
	}

	classified := classifyIssues(raw, nil)
	got := applyCriticalDebounce(fs, "test-cluster", enrichIssues(fs, "test-cluster", classified))

	var buf bytes.Buffer
	printEmergencyIssuesEnriched(&buf, got)
	out := buf.String()

	if n := countPodBlocks(out, "data-pipeline", "stream-processor-66c474d5fd-9zpwq"); n != 1 {
		t.Errorf("expected exactly 1 printed block for stream-processor, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "🔴 CRITICAL: 1") {
		t.Errorf("expected CRITICAL count of 1, got:\n%s", out)
	}
	if strings.Contains(out, "container ") || strings.Contains(out, "Container container") {
		t.Errorf("placeholder container name leaked into output, got:\n%s", out)
	}
	if !strings.Contains(out, "Container stress killed due to out of memory") {
		t.Errorf("expected the real, live OOMKilled message to survive, got:\n%s", out)
	}
}

// TestApplyCriticalDebounce_NoIncidentNoLiveIssue_AddsNothing is the
// "genuinely healthy pod" fixture: with no existing incident and nothing
// found live, applyCriticalDebounce must not manufacture an entry.
func TestApplyCriticalDebounce_NoIncidentNoLiveIssue_AddsNothing(t *testing.T) {
	fs := &fakeStore{} // no history for any fingerprint

	got := applyCriticalDebounce(fs, "test-cluster", nil)
	if len(got) != 0 {
		t.Errorf("expected no issues added for a pod with no live entry and no history, got %+v", got)
	}
}

// TestFullPipeline_EightPodsNotSixteen reproduces the full session
// scenario through the real pipeline order (classifyIssues first, then
// enrich/debounce, then print): 8 pods, each with an existing active
// CRITICAL incident, in a mix of raw states (still genuinely CRITICAL,
// CRITICAL+coexisting MEDIUM raw signals for the same container, and
// MEDIUM-only caught mid-backoff). classifyIssues collapses each pod to
// one issue before debounce ever runs. The final CRITICAL count must be
// 8, not 16 — every pod must appear exactly once.
func TestFullPipeline_EightPodsNotSixteen(t *testing.T) {
	var raw []models.EmergencyIssue
	raw = append(raw,
		// pod1: still genuinely CrashLoopBackOff this run.
		rawIssue("prod", "pod1-abc", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 100),
		// pod2: live OOMKilled + coexisting raw MEDIUM HighRestartCount for the same container.
		rawIssue("prod", "pod2-abc", "critical", "OOMKilled",
			"Container app killed due to out of memory", 200),
		rawIssue("prod", "pod2-abc", "medium", "HighRestartCount",
			"Container app has restarted 200 times", 200),
		// pod3: caught mid-backoff, MEDIUM-only this run.
		rawIssue("prod", "pod3-abc", "medium", "HighRestartCount",
			"Container app has restarted 300 times", 300),
		// pod4: both CrashLoopBackOff and OOMKilled live (real merge case).
		rawIssue("prod", "pod4-abc", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 400),
		rawIssue("prod", "pod4-abc", "critical", "OOMKilled",
			"Container app killed due to out of memory", 400),
		// pod5: caught mid-backoff, MEDIUM-only this run.
		rawIssue("prod", "pod5-abc", "medium", "HighRestartCount",
			"Container app has restarted 500 times", 500),
		// pod6: live CrashLoopBackOff + coexisting raw MEDIUM for the same container.
		rawIssue("prod", "pod6-abc", "critical", "CrashLoopBackOff",
			"Container app is crash looping: back-off restarting failed container", 600),
		rawIssue("prod", "pod6-abc", "medium", "HighRestartCount",
			"Container app has restarted 600 times", 600),
		// pod7: live OOMKilled only, no crash loop.
		rawIssue("prod", "pod7-abc", "critical", "OOMKilled",
			"Container app killed due to out of memory", 700),
		// pod8: caught mid-backoff, MEDIUM-only this run.
		rawIssue("prod", "pod8-abc", "medium", "HighRestartCount",
			"Container app has restarted 800 times", 800),
	)
	fs := &fakeStore{
		history: &store.IncidentRecord{Status: "active"},
	}

	classified := classifyIssues(raw, nil)
	got := applyCriticalDebounce(fs, "test-cluster", enrichIssues(fs, "test-cluster", classified))

	var buf bytes.Buffer
	printEmergencyIssuesEnriched(&buf, got)
	out := buf.String()

	if !strings.Contains(out, "🔴 CRITICAL: 8") {
		t.Errorf("expected CRITICAL count of 8, got:\n%s", out)
	}
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("pod%d-abc", i)
		if n := countPodBlocks(out, "prod", name); n != 1 {
			t.Errorf("expected exactly 1 printed block for %s, got %d:\n%s", name, n, out)
		}
	}
}
