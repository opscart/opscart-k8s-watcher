package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

func TestHandleHealth(t *testing.T) {
	tests := []struct {
		name            string
		dbPersistent    bool
		wantPersistence string
	}{
		{name: "SQLiteStore", dbPersistent: true, wantPersistence: "persistent"},
		{name: "NullStore", dbPersistent: false, wantPersistence: "ephemeral"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var db store.Store
			if tt.dbPersistent {
				sqlDB, err := store.OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
				if err != nil {
					t.Fatalf("OpenSQLite: %v", err)
				}
				defer sqlDB.Close()
				db = sqlDB
			} else {
				db = &store.NullStore{}
			}

			srv := newServer([]string{"test-ctx"}, db, 90, tt.dbPersistent)

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			srv.handleHealth(rec, req)

			resp := rec.Result()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want %q", ct, "application/json")
			}

			var body struct {
				Status      string `json:"status"`
				Persistence string `json:"persistence"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Status != "ok" {
				t.Errorf("status field = %q, want %q", body.Status, "ok")
			}
			if body.Persistence != tt.wantPersistence {
				t.Errorf("persistence = %q, want %q", body.Persistence, tt.wantPersistence)
			}
		})
	}
}

func TestFormatIntDelta(t *testing.T) {
	tests := []struct {
		name              string
		current, previous int
		hasHistory        bool
		want              string
	}{
		{name: "no history", current: 50, previous: 40, hasHistory: false, want: ""},
		{name: "increase", current: 55, previous: 40, hasHistory: true, want: "+15"},
		{name: "decrease", current: 30, previous: 40, hasHistory: true, want: "-10"},
		{name: "unchanged", current: 40, previous: 40, hasHistory: true, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatIntDelta(tt.current, tt.previous, tt.hasHistory); got != tt.want {
				t.Errorf("formatIntDelta(%d, %d, %v) = %q, want %q", tt.current, tt.previous, tt.hasHistory, got, tt.want)
			}
		})
	}
}

func TestReadLastViewedCursor(t *testing.T) {
	t.Run("missing cookie defaults to 24h ago", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		got := readLastViewedCursor(req)
		want := time.Now().Add(-24 * time.Hour)
		if diff := got.Sub(want); diff < -time.Minute || diff > time.Minute {
			t.Fatalf("expected ~24h ago, got %v (want ~%v)", got, want)
		}
	})

	t.Run("unparseable cookie defaults to 24h ago", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: lastViewedCookieName, Value: "not-a-timestamp"})
		got := readLastViewedCursor(req)
		want := time.Now().Add(-24 * time.Hour)
		if diff := got.Sub(want); diff < -time.Minute || diff > time.Minute {
			t.Fatalf("expected ~24h ago, got %v (want ~%v)", got, want)
		}
	})

	t.Run("valid cookie is parsed exactly", func(t *testing.T) {
		want := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: lastViewedCookieName, Value: strconv.FormatInt(want.Unix(), 10)})
		got := readLastViewedCursor(req)
		if !got.Equal(want) {
			t.Fatalf("readLastViewedCursor = %v, want %v", got, want)
		}
	})
}

// sinceCapturingStore wraps a real Store and records the `since` argument
// passed to GetChangesSince, so tests can verify the query used the cursor
// from the incoming request rather than one already advanced to "now".
type sinceCapturingStore struct {
	store.Store
	capturedSince time.Time
}

func (s *sinceCapturingStore) GetChangesSince(cluster string, since time.Time, limit int) ([]store.RecentEvent, error) {
	s.capturedSince = since
	return s.Store.GetChangesSince(cluster, since, limit)
}

// TestHandleOverviewPage_CursorSetAfterQuery drives the real handler
// end-to-end (not just the query function in isolation) to verify that the
// cursor cookie is refreshed to "now" only after it has been used to query
// GetChangesSince — otherwise this request's own changes would never be
// visible as "new" on the very next page load.
func TestHandleOverviewPage_CursorSetAfterQuery(t *testing.T) {
	sqlDB, err := store.OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer sqlDB.Close()
	wrapped := &sinceCapturingStore{Store: sqlDB}

	srv := newServer([]string{"test-ctx"}, wrapped, 90, true)

	// Seed a scan directly so the handler doesn't attempt a real cluster
	// scan (unavailable in this test environment).
	state := srv.getState("test-ctx")
	state.mu.Lock()
	state.scan = &clusterScan{}
	state.mu.Unlock()

	oldCursor := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	req := httptest.NewRequest(http.MethodGet, "/?cluster=test-ctx", nil)
	req.AddCookie(&http.Cookie{Name: lastViewedCookieName, Value: strconv.FormatInt(oldCursor.Unix(), 10)})
	rec := httptest.NewRecorder()

	srv.handleOverviewPage(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, rec.Body.String())
	}

	// The query must have used the cursor from the incoming cookie, proving
	// it wasn't already overwritten before being read.
	if !wrapped.capturedSince.Equal(oldCursor) {
		t.Fatalf("GetChangesSince called with since=%v, want incoming cookie value %v", wrapped.capturedSince, oldCursor)
	}

	var newCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == lastViewedCookieName {
			newCookie = c
		}
	}
	if newCookie == nil {
		t.Fatalf("expected Set-Cookie %s in response", lastViewedCookieName)
	}
	newSec, err := strconv.ParseInt(newCookie.Value, 10, 64)
	if err != nil {
		t.Fatalf("parse new cookie value %q: %v", newCookie.Value, err)
	}
	if newSec <= oldCursor.Unix() {
		t.Fatalf("expected refreshed cookie newer than the cursor used for the query: new=%d old=%d", newSec, oldCursor.Unix())
	}
	if newCookie.Path != "/" {
		t.Fatalf("expected cookie Path=/, got %q", newCookie.Path)
	}
	if newCookie.MaxAge != int(lastViewedMaxAge.Seconds()) {
		t.Fatalf("expected MaxAge=%d, got %d", int(lastViewedMaxAge.Seconds()), newCookie.MaxAge)
	}
}

func TestFormatCostDelta(t *testing.T) {
	tests := []struct {
		name       string
		delta      float64
		hasHistory bool
		want       string
	}{
		{name: "no history", delta: 45, hasHistory: false, want: ""},
		{name: "increase", delta: 45, hasHistory: true, want: "+$45"},
		{name: "decrease", delta: -12, hasHistory: true, want: "-$12"},
		{name: "unchanged", delta: 0, hasHistory: true, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatCostDelta(tt.delta, tt.hasHistory); got != tt.want {
				t.Errorf("formatCostDelta(%v, %v) = %q, want %q", tt.delta, tt.hasHistory, got, tt.want)
			}
		})
	}
}

func TestBuildNamespaceHealth(t *testing.T) {
	t.Run("nil scan returns nil, no panic", func(t *testing.T) {
		if got := buildNamespaceHealth(nil); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("scan with no report returns nil, no panic", func(t *testing.T) {
		if got := buildNamespaceHealth(&clusterScan{}); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("mixed ready/not-ready pods across namespaces", func(t *testing.T) {
		scan := &clusterScan{
			report: &models.CloudCostReport{
				NamespaceCosts: []models.NamespaceCostInfo{
					{Name: "payments", PodCount: 4},
					{Name: "checkout", PodCount: 3},
				},
			},
			wasteAudit: &analyzer.WasteAudit{
				StalePods: []analyzer.StalePod{
					{Name: "payments-api-abc123", Namespace: "payments", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 10, AgeDays: 2},
					{Name: "payments-api-def456", Namespace: "payments", Kind: analyzer.StalePodZombie, Status: "OOMKilled", RestartCount: 5, AgeDays: 1},
					// Idle (not zombie) — must NOT count against readiness.
					{Name: "checkout-worker-ghi789", Namespace: "checkout", Kind: analyzer.StalePodIdle, AgeDays: 20},
				},
			},
		}

		got := buildNamespaceHealth(scan)
		if len(got) != 2 {
			t.Fatalf("expected 2 namespaces, got %d: %+v", len(got), got)
		}
		// worst health first: payments (2/4 = 0.5) before checkout (3/3 = 1.0)
		if got[0] != (namespaceHealth{Name: "payments", Ready: 2, Total: 4}) {
			t.Fatalf("got[0] = %+v, want payments 2/4", got[0])
		}
		if got[1] != (namespaceHealth{Name: "checkout", Ready: 3, Total: 3}) {
			t.Fatalf("got[1] = %+v, want checkout 3/3", got[1])
		}
	})
}

func TestBuildWorkloadHealthGrid(t *testing.T) {
	t.Run("nil scan returns nil, no panic", func(t *testing.T) {
		if got := buildWorkloadHealthGrid(nil); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("empty scan returns empty, no panic", func(t *testing.T) {
		if got := buildWorkloadHealthGrid(&clusterScan{}); len(got) != 0 {
			t.Fatalf("expected empty, got %+v", got)
		}
	})

	t.Run("mixed healthy and unhealthy workloads", func(t *testing.T) {
		scan := &clusterScan{
			AllWorkloads: []models.WorkloadRef{
				{Name: "payments-api", Kind: "Deployment", Namespace: "payments"},
				{Name: "payments-worker", Kind: "Deployment", Namespace: "payments"},
				{Name: "checkout-api", Kind: "Deployment", Namespace: "checkout"},
			},
			wasteAudit: &analyzer.WasteAudit{
				StalePods: []analyzer.StalePod{
					{Name: "payments-api-abc123", Namespace: "payments", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 50, AgeDays: 3},
				},
			},
			secAudit: &models.SecurityAudit{
				Issues: []models.SecurityIssue{
					{Type: "privileged_container", Severity: "critical", Namespace: "checkout", Name: "checkout-api-xyz789", Resource: "pod"},
				},
			},
			netAudit: &analyzer.NetworkPolicyAudit{
				// Namespace-level issue — must NOT appear as a workload cell.
				UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{
					{Name: "payments", RiskLevel: "HIGH", PodCount: 3},
				},
			},
		}

		got := buildWorkloadHealthGrid(scan)
		byName := map[string]string{}
		for _, cell := range got {
			byName[cell.Name] = cell.Severity
		}

		if len(got) != 3 {
			t.Fatalf("expected 3 workload cells, got %d: %+v", len(got), got)
		}
		if sev := byName["payments-api"]; sev != "critical" {
			t.Fatalf("payments-api severity = %q, want critical", sev)
		}
		if sev := byName["checkout-api"]; sev != "critical" {
			t.Fatalf("checkout-api severity = %q, want critical", sev)
		}
		if sev, ok := byName["payments-worker"]; !ok || sev != "" {
			t.Fatalf("payments-worker severity = %q (present=%v), want \"\" (healthy)", sev, ok)
		}

		// most-severe-first, alphabetical tiebreak; healthy entries last.
		if got[0].Name != "checkout-api" || got[1].Name != "payments-api" || got[2].Name != "payments-worker" {
			t.Fatalf("unexpected order: %+v", got)
		}
	})

	t.Run("issue with no matching AllWorkloads entry still surfaces", func(t *testing.T) {
		scan := &clusterScan{
			wasteAudit: &analyzer.WasteAudit{
				StalePods: []analyzer.StalePod{
					{Name: "orphan-svc-abc123", Namespace: "default", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 8, AgeDays: 1},
				},
			},
		}
		got := buildWorkloadHealthGrid(scan)
		if len(got) != 1 || got[0].Name != "orphan-svc" || got[0].Severity != "critical" {
			t.Fatalf("expected single orphan-svc/critical cell, got %+v", got)
		}
	})
}

// fullyPopulatedOverviewData builds an overviewPageData with every optional
// field populated (non-nil Scoreboard with both memory-line branches
// non-empty, non-empty TopIssues/ChangesSinceLastView/RecentEvents/
// NamespaceHealthList/WorkloadHealthGrid, every delta text set, every
// WorkloadHealthGrid severity value including "" for healthy) so rendering
// it exercises every {{if}}/{{range}} branch in overview.html — not just
// the all-empty path a fresh scan takes.
func fullyPopulatedOverviewData() overviewPageData {
	return overviewPageData{
		ClusterName:    "prod-eastus",
		NamespaceCount: 4,
		ScannedAtMS:    time.Now().UnixMilli(),
		VerdictLine1:   "3 workloads need attention. payments-api has been crash-looping for 2 day(s) and its restart rate is accelerating.",
		VerdictLine2:   "2 incidents resolved since yesterday.",

		TopIssues: []topIssue{
			{
				Rank: 1, Title: "3 pods crash-looping", Subtitle: "payments-api, payments-worker, checkout-api",
				Action: "kubectl logs payments-api -n payments", Severity: "critical", SeverityLbl: "CRITICAL",
				CountText: "3 pods", URL: "/warroom",
				Namespace: "payments", Resource: "payments-api-7d8f9c6b5-abc12", IssueType: "crash_loop",
				FirstDetectedLabel: "2d ago", ReopenCountVal: 1, TrendVal: "accelerating",
				MemoryLine: "First detected 2d ago · reopened once · accelerating",
			},
			{
				Rank: 2, Title: "2 namespaces missing NetworkPolicy", Subtitle: "Including checkout, payments",
				Severity: "high", SeverityLbl: "HIGH", CountText: "2 ns", URL: "/warroom",
				Namespace: "checkout", Resource: "namespace", IssueType: "unprotected_namespace",
				FirstDetectedLabel: "5d ago",
				MemoryLine:         "First detected 5d ago",
			},
		},

		HasTopIssue:    true,
		TopIssueName:   "payments-api-7d8f9c6b5-abc12",
		TopIssueNS:     "payments",
		TopIssueTrend:  "accelerating",
		TopIssueReopen: 1,

		CostDeltaText:          "+$45",
		IncidentScoreDeltaText: "-10",
		SecurityScoreDeltaText: "+5",

		Scoreboard: &store.MemoryScoreboard{
			TotalSeen: 42, Resolved: 30, Reopened: 4, Accelerating: 2,
			LongestActiveDays: 12, LongestActiveName: "payments-api",
			MostUnstableNamespace: "payments", MostUnstableCount: 9,
		},
		RecentEvents: []store.RecentEvent{
			{Resource: "payments-api", EventReason: "RestartMilestone", OccurredAt: time.Now().Add(-5 * time.Minute)},
			{Resource: "checkout-worker", EventReason: "Resolved", OccurredAt: time.Now().Add(-2 * time.Hour)},
		},

		ChangesSinceLastView: []store.RecentEvent{
			{Resource: "payments-api", EventReason: "SeverityChanged", OccurredAt: time.Now().Add(-1 * time.Minute)},
			{Resource: "checkout-api", EventReason: "Detected", OccurredAt: time.Now().Add(-10 * time.Minute)},
			{Resource: "checkout-api", EventReason: "Reopened", OccurredAt: time.Now().Add(-20 * time.Minute)},
		},
		LastViewedLabel: "24h ago",

		NamespaceHealthList: []namespaceHealth{
			{Name: "payments", Ready: 2, Total: 4},
			{Name: "checkout", Ready: 3, Total: 3},
			{Name: "idle-ns", Ready: 0, Total: 0},
		},
		WorkloadHealthGrid: []workloadHealthCell{
			{Name: "payments-api", Severity: "critical"},
			{Name: "checkout-api", Severity: "high"},
			{Name: "payments-worker", Severity: "medium"},
			{Name: "checkout-worker", Severity: ""},
		},

		NodePoolCount: 3, PodCount: 48, CPUUtilization: 92, MemUtilization: 61,

		IncidentScore: 62, IncidentScoreColor: "orange", IncidentScoreLabel: "Needs attention",
		SecurityScore: 78, WasteCount: 6, MonthlyCost: 1234.56,

		CostsHref: "/costs", WasteHref: "/waste", WrURL: "/warroom", IncidentsHref: "/incidents", NSsURL: "/namespaces",

		ActivePage: "dashboard",
		Version:    "test",
	}
}

// TestOverviewTemplate_RendersFullyPopulatedData drives getOverviewTmpl()
// directly (not through buildOverviewData) with every optional field
// populated, so every {{if}}/{{range}} branch in overview.html — not just
// the empty-scan path other tests exercise — is proven to execute without
// a "can't evaluate field" template error.
func TestOverviewTemplate_RendersFullyPopulatedData(t *testing.T) {
	data := fullyPopulatedOverviewData()

	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	out := buf.String()
	wantSubstrings := []string{
		"prod-eastus",
		"If you only fix one thing today",
		data.VerdictLine1,
		data.VerdictLine2,
		"payments-api",
		"Longest active",
		"Most unstable namespace",
		"What's Changed Since Last Scan",
		"Recent Events",
		"checkout", /* namespace health row */
		// html/template escapes "+" as a numeric entity even in text
		// nodes; "-10" needs no such escaping.
		"&#43;$45 from last scan",
		"-10 from last scan",
		"&#43;5 from last scan",
		"First detected 2d ago · reopened once · accelerating", // MemoryLine
		"payments", // TopIssueNS in the "Highest Priority" bf-item
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
}

// TestOverviewTemplate_RendersEmptyData exercises the opposite path: every
// optional/slice field at its zero value (nil Scoreboard, no TopIssues, no
// feed entries, no health lists), proving the template's empty-state
// branches also execute cleanly rather than panicking on a nil pointer.
func TestOverviewTemplate_RendersEmptyData(t *testing.T) {
	data := overviewPageData{
		ClusterName: "empty-cluster",
		ScannedAtMS: time.Now().UnixMilli(),
		ActivePage:  "dashboard",
	}

	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed on empty data: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "If you only fix one thing today") {
		t.Errorf("expected fix-one-thing banner to be hidden when TopIssues is empty")
	}
	if !strings.Contains(out, "No operational memory yet") {
		t.Errorf("expected Operational Memory empty state when Scoreboard is nil")
	}
}

// TestOverviewTemplate_FeedsCapAtFive drives getOverviewTmpl with more than
// five ChangesSinceLastView/RecentEvents rows and asserts the rendered
// output shows exactly five rows per feed — the display cap the limitSlice
// FuncMap helper enforces, independent of how many rows were fetched.
func TestOverviewTemplate_FeedsCapAtFive(t *testing.T) {
	data := fullyPopulatedOverviewData()

	makeEvents := func(n int) []store.RecentEvent {
		out := make([]store.RecentEvent, n)
		for i := 0; i < n; i++ {
			out[i] = store.RecentEvent{
				Resource:    "svc-" + strconv.Itoa(i),
				EventReason: "Detected",
				OccurredAt:  time.Now().Add(-time.Duration(i) * time.Minute),
			}
		}
		return out
	}
	data.ChangesSinceLastView = makeEvents(9)
	data.RecentEvents = makeEvents(7)

	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	out := buf.String()
	// `class="change-row"` (the literal HTML attribute) rather than just
	// "change-row" — the latter also matches the CSS selector rules
	// earlier in the same document and would inflate the count.
	if got := strings.Count(out, `class="change-row"`); got != 10 {
		t.Fatalf("expected 5 change-row entries per feed (10 total) regardless of 9/7 fetched, got %d", got)
	}
}

// queryIncidentsStubStore returns a fixed QueryIncidents result, embedding
// store.Store so only the one method buildOverviewVerdict actually calls
// needs a real implementation.
type queryIncidentsStubStore struct {
	store.Store
	items []store.IncidentSummary
	total int
}

func (s *queryIncidentsStubStore) QueryIncidents(f store.IncidentFilter) ([]store.IncidentSummary, int, error) {
	return s.items, s.total, nil
}

// TestBuildOverviewVerdict_UnprotectedNamespaceUsesNamespaceNotLiteralResource
// covers the unprotected_namespace/idle_namespace case, where
// IncidentSummary.Resource is literally the string "namespace" (there's no
// pod/deployment involved) — the sentence must use Namespace instead, not
// that placeholder.
func TestBuildOverviewVerdict_UnprotectedNamespaceUsesNamespaceNotLiteralResource(t *testing.T) {
	stub := &queryIncidentsStubStore{
		items: []store.IncidentSummary{
			{
				Namespace: "payments-prod",
				Resource:  "namespace",
				IssueType: "unprotected_namespace",
				Severity:  "high",
				FirstSeen: time.Now().Add(-3 * 24 * time.Hour),
			},
		},
		total: 1,
	}

	line1, _ := buildOverviewVerdict(stub, "test-cluster")

	if strings.Contains(line1, "namespace has been") {
		t.Fatalf("expected verdict to use the real namespace, not the literal placeholder %q: got %q", "namespace", line1)
	}
	if !strings.Contains(line1, "payments-prod has been") {
		t.Fatalf("expected verdict to mention namespace %q, got: %q", "payments-prod", line1)
	}
}

// TestBuildOverviewVerdict_IdleNamespaceUsesNamespaceNotLiteralResource
// covers the same substitution for idle_namespace, and the ReopenCount
// branch rather than the default one.
func TestBuildOverviewVerdict_IdleNamespaceUsesNamespaceNotLiteralResource(t *testing.T) {
	stub := &queryIncidentsStubStore{
		items: []store.IncidentSummary{
			{
				Namespace:   "batch-jobs",
				Resource:    "namespace",
				IssueType:   "idle_namespace",
				Severity:    "medium",
				FirstSeen:   time.Now().Add(-5 * 24 * time.Hour),
				ReopenCount: 2,
			},
		},
		total: 1,
	}

	line1, _ := buildOverviewVerdict(stub, "test-cluster")

	if strings.Contains(line1, "namespace reoccurred") {
		t.Fatalf("expected verdict to use the real namespace, not the literal placeholder %q: got %q", "namespace", line1)
	}
	if !strings.Contains(line1, "batch-jobs reoccurred") {
		t.Fatalf("expected verdict to mention namespace %q, got: %q", "batch-jobs", line1)
	}
}

func TestBuildMemoryLine(t *testing.T) {
	tests := []struct {
		name               string
		firstDetectedLabel string
		reopenCount        int
		trend              string
		issueType          string
		want               string
	}{
		{
			name: "no reopens, no trend", firstDetectedLabel: "3d ago", reopenCount: 0, trend: "stable", issueType: "crash_loop",
			want: "First detected 3d ago",
		},
		{
			name: "one reopen", firstDetectedLabel: "3d ago", reopenCount: 1, trend: "stable", issueType: "crash_loop",
			want: "First detected 3d ago · reopened once",
		},
		{
			name: "multiple reopens use ×N", firstDetectedLabel: "3d ago", reopenCount: 4, trend: "stable", issueType: "crash_loop",
			want: "First detected 3d ago · reopened ×4",
		},
		{
			name: "accelerating trend included for restart-based issue type", firstDetectedLabel: "3d ago", reopenCount: 0, trend: "accelerating", issueType: "crash_loop",
			want: "First detected 3d ago · accelerating",
		},
		{
			name: "non-accelerating trend never shown", firstDetectedLabel: "3d ago", reopenCount: 0, trend: "stable", issueType: "crash_loop",
			want: "First detected 3d ago",
		},
		{
			name: "accelerating omitted for privileged_container (posture-only)", firstDetectedLabel: "3d ago", reopenCount: 0, trend: "accelerating", issueType: "privileged_container",
			want: "First detected 3d ago",
		},
		{
			name: "accelerating omitted for unprotected_namespace (posture-only)", firstDetectedLabel: "3d ago", reopenCount: 0, trend: "accelerating", issueType: "unprotected_namespace",
			want: "First detected 3d ago",
		},
		{
			name: "accelerating omitted for idle_namespace (posture-only)", firstDetectedLabel: "3d ago", reopenCount: 0, trend: "accelerating", issueType: "idle_namespace",
			want: "First detected 3d ago",
		},
		{
			name: "reopen and trend both present, joined with middle dot", firstDetectedLabel: "7d ago", reopenCount: 1, trend: "accelerating", issueType: "crash_loop",
			want: "First detected 7d ago · reopened once · accelerating",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildMemoryLine(tt.firstDetectedLabel, tt.reopenCount, tt.trend, tt.issueType)
			if got != tt.want {
				t.Errorf("buildMemoryLine(%q, %d, %q, %q) = %q, want %q", tt.firstDetectedLabel, tt.reopenCount, tt.trend, tt.issueType, got, tt.want)
			}
		})
	}
}

func TestTopIssueResourceLabel(t *testing.T) {
	tests := []struct {
		name, resource, namespace, issueType, want string
	}{
		{"crash_loop uses resource", "payments-api-abc123", "payments", "crash_loop", "payments-api-abc123"},
		{"unprotected_namespace uses namespace", "namespace", "payments", "unprotected_namespace", "payments"},
		{"idle_namespace uses namespace", "namespace", "batch-jobs", "idle_namespace", "batch-jobs"},
		{"privileged_container uses resource", "web-abc123", "default", "privileged_container", "web-abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := topIssueResourceLabel(tt.resource, tt.namespace, tt.issueType); got != tt.want {
				t.Errorf("topIssueResourceLabel(%q, %q, %q) = %q, want %q", tt.resource, tt.namespace, tt.issueType, got, tt.want)
			}
		})
	}
}

func TestDeriveTopIssueSummary(t *testing.T) {
	t.Run("empty issues yields all zero values", func(t *testing.T) {
		has, name, ns, trend, reopen := deriveTopIssueSummary(nil)
		if has || name != "" || ns != "" || trend != "" || reopen != 0 {
			t.Fatalf("expected all zero values for empty issues, got has=%v name=%q ns=%q trend=%q reopen=%d", has, name, ns, trend, reopen)
		}
	})

	t.Run("mirrors TopIssues[0]", func(t *testing.T) {
		issues := []topIssue{
			{
				Namespace: "payments", Resource: "payments-api-abc123", IssueType: "crash_loop",
				TrendVal: "accelerating", ReopenCountVal: 2,
			},
			{Namespace: "checkout", Resource: "checkout-api-xyz789", IssueType: "oom_killed"},
		}
		has, name, ns, trend, reopen := deriveTopIssueSummary(issues)
		if !has {
			t.Fatalf("expected has=true")
		}
		if name != "payments-api-abc123" {
			t.Errorf("name = %q, want %q", name, "payments-api-abc123")
		}
		if ns != "payments" {
			t.Errorf("ns = %q, want %q", ns, "payments")
		}
		if trend != "accelerating" {
			t.Errorf("trend = %q, want %q", trend, "accelerating")
		}
		if reopen != 2 {
			t.Errorf("reopen = %d, want %d", reopen, 2)
		}
	})

	t.Run("unprotected_namespace/idle_namespace use Namespace for name too", func(t *testing.T) {
		issues := []topIssue{
			{Namespace: "checkout", Resource: "namespace", IssueType: "unprotected_namespace"},
		}
		_, name, ns, _, _ := deriveTopIssueSummary(issues)
		if name != "checkout" {
			t.Errorf("name = %q, want %q (should use Namespace, not the literal placeholder %q)", name, "checkout", "namespace")
		}
		if ns != "checkout" {
			t.Errorf("ns = %q, want %q", ns, "checkout")
		}
	})
}

func TestEnrichTopIssues(t *testing.T) {
	stub := &queryIncidentsStubStore{
		items: []store.IncidentSummary{
			{
				Namespace: "payments", Resource: "payments-api-abc123", IssueType: "crash_loop",
				FirstSeen: time.Now().Add(-2 * 24 * time.Hour), ReopenCount: 1, Trend: "accelerating",
			},
		},
		total: 1,
	}

	issues := []topIssue{
		{Namespace: "payments", Resource: "payments-api-abc123", IssueType: "crash_loop"}, // matches
		{Namespace: "", Resource: "", IssueType: ""},                                      // aggregate row, no matching key
		{Namespace: "checkout", Resource: "checkout-api-xyz", IssueType: "oom_killed"},    // matching key, but no incident
	}

	got := enrichTopIssues(issues, stub, "test-cluster")

	if got[0].MemoryLine == "" {
		t.Fatalf("expected matched row to be enriched, got %+v", got[0])
	}
	if !strings.Contains(got[0].MemoryLine, "reopened once") || !strings.Contains(got[0].MemoryLine, "accelerating") {
		t.Errorf("unexpected MemoryLine: %q", got[0].MemoryLine)
	}
	if got[0].ReopenCountVal != 1 || got[0].TrendVal != "accelerating" {
		t.Errorf("unexpected enrichment fields: %+v", got[0])
	}

	if got[1].MemoryLine != "" {
		t.Errorf("expected row with no matching key to stay unenriched, got %+v", got[1])
	}
	if got[2].MemoryLine != "" {
		t.Errorf("expected row with a matching key but no incident to stay unenriched, got %+v", got[2])
	}
}

func TestEnrichTopIssues_NilDBOrEmptyIssues(t *testing.T) {
	issues := []topIssue{{Namespace: "payments", Resource: "payments-api", IssueType: "crash_loop"}}
	if got := enrichTopIssues(issues, nil, "test-cluster"); len(got) != 1 || got[0].MemoryLine != "" {
		t.Fatalf("expected passthrough with nil db, got %+v", got)
	}
	stub := &queryIncidentsStubStore{}
	if got := enrichTopIssues(nil, stub, "test-cluster"); len(got) != 0 {
		t.Fatalf("expected empty passthrough, got %+v", got)
	}
}

// TestOverviewTemplate_BriefingClassByCriticalCount covers Fix 3: the
// Situation Briefing card is danger-tinted ("briefing") when there's an
// active critical issue, success-tinted ("briefing-ok") otherwise.
func TestOverviewTemplate_BriefingClassByCriticalCount(t *testing.T) {
	t.Run("critical issues present uses briefing (danger tint)", func(t *testing.T) {
		data := fullyPopulatedOverviewData()
		data.CriticalCount = 2

		var buf strings.Builder
		if err := getOverviewTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("template execution failed: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, `class="section-card briefing"`) {
			t.Errorf("expected briefing class when CriticalCount>0")
		}
		if strings.Contains(out, `class="section-card briefing-ok"`) {
			t.Errorf("did not expect briefing-ok class when CriticalCount>0")
		}
	})

	t.Run("no critical issues uses briefing-ok (success tint)", func(t *testing.T) {
		data := fullyPopulatedOverviewData()
		data.CriticalCount = 0

		var buf strings.Builder
		if err := getOverviewTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("template execution failed: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, `class="section-card briefing-ok"`) {
			t.Errorf("expected briefing-ok class when CriticalCount==0")
		}
		if strings.Contains(out, `class="section-card briefing"`) {
			t.Errorf("did not expect plain briefing class when CriticalCount==0")
		}
	})
}

// TestOverviewTemplate_HealthCardsCapAndShowViewAll covers Fix 5: the
// Namespace Health list and Cluster Health's Workload Health dot strip are
// capped, with a "View all" link appearing once content exceeds the cap.
func TestOverviewTemplate_HealthCardsCapAndShowViewAll(t *testing.T) {
	data := fullyPopulatedOverviewData()

	nsList := make([]namespaceHealth, 9)
	for i := range nsList {
		nsList[i] = namespaceHealth{Name: fmt.Sprintf("ns-%d", i), Ready: i, Total: i + 1}
	}
	data.NamespaceHealthList = nsList

	whList := make([]workloadHealthCell, 45)
	for i := range whList {
		whList[i] = workloadHealthCell{Name: fmt.Sprintf("wl-%d", i)}
	}
	data.WorkloadHealthGrid = whList

	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}
	out := buf.String()

	if got := strings.Count(out, `class="ns-health-row"`); got != 6 {
		t.Errorf("expected 6 namespace health rows (capped from 9), got %d", got)
	}
	if !strings.Contains(out, "View all 9 namespaces") {
		t.Errorf("expected a 'View all 9 namespaces' link")
	}

	if got := strings.Count(out, `class="wh-dot`); got != 40 {
		t.Errorf("expected 40 workload health dots (capped from 45), got %d", got)
	}
	if !strings.Contains(out, "View all 45 workloads") {
		t.Errorf("expected a 'View all 45 workloads' link")
	}
}

// TestOverviewTemplate_HealthCardsNoViewAllUnderCap proves the "View all"
// links only appear once content actually exceeds the cap — fullyPopulated
// OverviewData's fixture has 3 namespaces and 4 workloads, both under it.
func TestOverviewTemplate_HealthCardsNoViewAllUnderCap(t *testing.T) {
	data := fullyPopulatedOverviewData()

	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "View all 3 namespaces") {
		t.Errorf("did not expect a namespaces View all link under the cap")
	}
	if strings.Contains(out, "View all 4 workloads") {
		t.Errorf("did not expect a workloads View all link under the cap")
	}
}

func TestInvestigateURL(t *testing.T) {
	tests := []struct {
		name                                string
		namespace, resource, issueType, ctx string
		want                                string
	}{
		{
			name:      "all fields present builds deep link with escaped params",
			namespace: "payments prod", resource: "payments-api/abc123", issueType: "crash_loop", ctx: "my cluster",
			want: "/investigate?pod=payments-api%2Fabc123&ns=payments+prod&type=crash_loop&cluster=my+cluster&from=warroom",
		},
		{
			name:      "empty namespace falls back to /warroom",
			namespace: "", resource: "payments-api", issueType: "crash_loop", ctx: "test-cluster",
			want: "/warroom",
		},
		{
			name:      "empty resource falls back to /warroom",
			namespace: "payments", resource: "", issueType: "crash_loop", ctx: "test-cluster",
			want: "/warroom",
		},
		{
			name:      "empty issueType falls back to /warroom",
			namespace: "payments", resource: "payments-api", issueType: "", ctx: "test-cluster",
			want: "/warroom",
		},
		{
			name:      "all three empty falls back to /warroom",
			namespace: "", resource: "", issueType: "", ctx: "test-cluster",
			want: "/warroom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := investigateURL(tt.namespace, tt.resource, tt.issueType, tt.ctx); got != tt.want {
				t.Errorf("investigateURL(%q, %q, %q, %q) = %q, want %q", tt.namespace, tt.resource, tt.issueType, tt.ctx, got, tt.want)
			}
		})
	}
}

// TestBuildTopIssues_URLDeepLinksToInvestigation drives buildTopIssues
// end-to-end from a scan fixture (through collectWarRoomIssues, matching
// how buildOverviewData actually calls it) and asserts the grouped
// crash_loop row's URL is a real Investigation deep link, not the old
// hardcoded "/warroom".
func TestBuildTopIssues_URLDeepLinksToInvestigation(t *testing.T) {
	scan := &clusterScan{
		wasteAudit: &analyzer.WasteAudit{
			StalePods: []analyzer.StalePod{
				{Name: "payments-api-abc123", Namespace: "payments", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 10, AgeDays: 2},
			},
		},
	}
	wrIssues := collectWarRoomIssues(scan, 0)
	issues := buildTopIssues(scan, wrIssues, "test-cluster")

	if len(issues) == 0 {
		t.Fatalf("expected at least one topIssue")
	}
	want := "/investigate?pod=payments-api-abc123&ns=payments&type=crash_loop&cluster=test-cluster&from=warroom"
	if issues[0].URL != want {
		t.Errorf("URL = %q, want %q", issues[0].URL, want)
	}
}

// TestBuildTopIssues_AggregateRowsKeepTheirOwnURL proves the fix is scoped
// to grouped incident rows: the orphaned-PVCs aggregate row has no single
// Namespace/Resource/IssueType to deep-link to, so it must keep its
// existing "/optimizations" URL rather than falling back to "/warroom".
func TestBuildTopIssues_AggregateRowsKeepTheirOwnURL(t *testing.T) {
	scan := &clusterScan{
		wasteAudit: &analyzer.WasteAudit{
			OrphanedPVCs: []analyzer.OrphanedPVC{
				{Name: "pvc-1", Namespace: "default", SizeGB: 10, Status: analyzer.PVCReleased, AgeDays: 30},
			},
		},
	}
	issues := buildTopIssues(scan, nil, "test-cluster")
	if len(issues) == 0 {
		t.Fatalf("expected at least one topIssue")
	}
	if issues[0].URL != "/optimizations" {
		t.Errorf("expected aggregated orphaned-PVC row to keep its own URL, got %q", issues[0].URL)
	}
	if issues[0].Namespace != "" || issues[0].Resource != "" || issues[0].IssueType != "" {
		t.Errorf("expected aggregate row to have no matching key, got Namespace=%q Resource=%q IssueType=%q",
			issues[0].Namespace, issues[0].Resource, issues[0].IssueType)
	}
}
