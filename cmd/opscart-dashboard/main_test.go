package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestOpenDashboardStoreRequiredPersistenceFailure(t *testing.T) {
	_, persistent, err := openDashboardStore(t.TempDir(), true)
	if err == nil {
		t.Fatal("required persistence unexpectedly accepted an unopenable database path")
	}
	if persistent {
		t.Fatal("required persistence failure reported persistent=true")
	}
	if !strings.Contains(err.Error(), "required persistence unavailable") ||
		!strings.Contains(err.Error(), "open SQLite database") {
		t.Fatalf("error lacks persistence context: %v", err)
	}
}

func TestOpenDashboardStoreOptionalPersistenceFallsBack(t *testing.T) {
	db, persistent, err := openDashboardStore(t.TempDir(), false)
	if err != nil {
		t.Fatalf("optional persistence returned an error: %v", err)
	}
	defer db.Close()
	if persistent {
		t.Fatal("optional persistence fallback reported persistent=true")
	}
	if _, ok := db.(*store.NullStore); !ok {
		t.Fatalf("fallback store = %T, want *store.NullStore", db)
	}
}

func TestBackgroundRefreshStopsAfterActiveScanCompletes(t *testing.T) {
	srv := newServer([]string{"test-ctx"}, &store.NullStore{}, 90, false)
	state := srv.getState("test-ctx")
	state.mu.Lock()
	state.scan = &clusterScan{}
	state.mu.Unlock()

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	srv.refreshState = func(_ *dashboardState, _ []string) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	srv.startBackgroundRefresh(ctx, time.Millisecond)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scheduled scan did not start")
	}

	cancel()
	stopped := make(chan struct{})
	go func() {
		srv.backgroundWG.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("background worker stopped before its active scan completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("background worker did not stop after active scan completed")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("scheduled scans after cancellation = %d, want 1 total call", got)
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
	if err := sqlDB.WriteScanHistory("test-ctx", "seed", store.ScanMeta{Success: true}); err != nil {
		t.Fatalf("WriteScanHistory: %v", err)
	}

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
	for _, want := range []string{"Earliest retained observation:", "Configured retention:", "90 days", "Storage:", "Persistent"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("Overview Operational Memory card missing %q", want)
		}
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

	t.Run("StatefulSet replica issue rolls up into its owning StatefulSet, not a separate cell", func(t *testing.T) {
		// Reproduces the reported bug: a crash-looping StatefulSet replica
		// (prometheus-0) previously showed the parent StatefulSet as
		// healthy while the replica appeared as its own orphaned critical
		// workload cell. buildWorkloadHealthGrid resolves the rollup via
		// scan.PodWorkloads — confirmed ownership from real
		// OwnerReferences, populated alongside AllWorkloads by
		// AnalyzeClusterResources in production — not a name guess.
		scan := &clusterScan{
			AllWorkloads: []models.WorkloadRef{
				{Name: "prometheus", Kind: "StatefulSet", Namespace: "monitoring"},
			},
			PodWorkloads: map[string]models.WorkloadRef{
				"monitoring/prometheus-0": {Name: "prometheus", Kind: "StatefulSet", Namespace: "monitoring"},
			},
			wasteAudit: &analyzer.WasteAudit{
				StalePods: []analyzer.StalePod{
					{Name: "prometheus-0", Namespace: "monitoring", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 12, AgeDays: 1},
				},
			},
		}
		got := buildWorkloadHealthGrid(scan)
		if len(got) != 1 {
			t.Fatalf("expected exactly one cell (the StatefulSet, not a separate replica cell), got %d: %+v", len(got), got)
		}
		if got[0].Name != "prometheus" || got[0].Severity != "critical" {
			t.Fatalf("expected {prometheus, critical}, got %+v", got[0])
		}
	})

	t.Run("ordinal-suffixed pod with no matching StatefulSet is not misattributed", func(t *testing.T) {
		// A bare pod coincidentally named like "worker-0" with no
		// confirmed ownership mapping must NOT be collapsed into any
		// StatefulSet.
		scan := &clusterScan{
			wasteAudit: &analyzer.WasteAudit{
				StalePods: []analyzer.StalePod{
					{Name: "worker-0", Namespace: "batch", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 5, AgeDays: 1},
				},
			},
		}
		got := buildWorkloadHealthGrid(scan)
		if len(got) != 1 || got[0].Name != "worker-0" {
			t.Fatalf("expected the bare pod's own cell (no false StatefulSet rollup), got %+v", got)
		}
	})

	t.Run("Deployment pod named like a StatefulSet replica is not misattributed to a colliding StatefulSet", func(t *testing.T) {
		// The exact collision scenario: a Deployment named "worker-0"
		// produces a pod also named "worker-0" (Deployment names without
		// a ReplicaSet hash suffix are unusual but not impossible — e.g.
		// a single-replica Deployment whose pod template name collides).
		// The namespace ALSO has an unrelated StatefulSet named "worker".
		// A pure name-pattern check (old behavior) would wrongly roll this
		// pod's issue into the StatefulSet "worker". Confirmed ownership
		// via PodWorkloads must prevent that: this pod's real owner is
		// the Deployment, not the StatefulSet.
		scan := &clusterScan{
			AllWorkloads: []models.WorkloadRef{
				{Name: "worker", Kind: "StatefulSet", Namespace: "batch"},
				{Name: "worker-0", Kind: "Deployment", Namespace: "batch"},
			},
			PodWorkloads: map[string]models.WorkloadRef{
				"batch/worker-0": {Name: "worker-0", Kind: "Deployment", Namespace: "batch"},
			},
			wasteAudit: &analyzer.WasteAudit{
				StalePods: []analyzer.StalePod{
					{Name: "worker-0", Namespace: "batch", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 5, AgeDays: 1},
				},
			},
		}
		got := buildWorkloadHealthGrid(scan)
		byName := map[string]string{}
		for _, cell := range got {
			byName[cell.Name] = cell.Severity
		}
		if sev, ok := byName["worker"]; ok && sev == "critical" {
			t.Fatalf("StatefulSet 'worker' must NOT be marked critical — the failing pod belongs to the unrelated Deployment 'worker-0': %+v", got)
		}
		if sev := byName["worker-0"]; sev != "critical" {
			t.Fatalf("Deployment 'worker-0' should be critical (it's the pod's real owner), got %+v", got)
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
				MemoryLine: "First detected 2d ago · accelerating",
			},
			{
				Rank: 2, Title: "2 namespaces with NetworkPolicy coverage gaps", Subtitle: "Including checkout, payments",
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
		RecentEvents: formatChangeLines([]store.RecentEvent{
			{Resource: "payments-api", EventReason: "RestartMilestone", OccurredAt: time.Now().Add(-5 * time.Minute)},
			{Resource: "checkout-worker", EventReason: "Resolved", OccurredAt: time.Now().Add(-2 * time.Hour)},
		}),

		ChangesSinceLastView: formatChangeLines([]store.RecentEvent{
			{Resource: "payments-api", EventReason: "SeverityChanged", OccurredAt: time.Now().Add(-1 * time.Minute)},
			{Resource: "checkout-api", EventReason: "Detected", OccurredAt: time.Now().Add(-10 * time.Minute)},
			{Resource: "checkout-api", EventReason: "Reopened", OccurredAt: time.Now().Add(-20 * time.Minute)},
		}),
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

		NodePoolCount: 3, PodCount: 48, CPUUtilization: 92, MemUtilization: 61, UtilizationStatus: "Critical",

		IncidentScore: 62, IncidentScoreColor: "orange", IncidentScoreLabel: "Needs attention",
		SecurityScore: 78, WasteCount: 6, MonthlyCost: 1234.56,
		CostAvailable: true, CostCoverage: "3 of 3 nodes priced",

		DashHref:        "/?cluster=prod-eastus",
		InfraHref:       "/infrastructure?cluster=prod-eastus",
		NsHref:          "/namespaces?cluster=prod-eastus",
		OptHref:         "/optimizations?cluster=prod-eastus",
		WrHref:          "/warroom?cluster=prod-eastus",
		CostsHref:       "/costs?cluster=prod-eastus",
		WasteHref:       "/waste?cluster=prod-eastus",
		SecurityHref:    "/security?cluster=prod-eastus",
		IncidentsHref:   "/incidents?cluster=prod-eastus",
		DiagnosticsHref: "/settings/diagnostics?cluster=prod-eastus",
		SettingsHref:    "/settings?cluster=prod-eastus",
		WrURL:           "/warroom?cluster=prod-eastus",
		NSsURL:          "/namespaces?cluster=prod-eastus",

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
	for _, want := range []string{
		"prod-eastus",
		"OpsCart has prioritized what needs your attention.",
		"Highest priority",
		"payments-api",
		"First detected 2d ago · accelerating",
		"Why #1",
		"Affected scope",
		"Recommended next step",
		"Open in War Room",
		"Other priority issues",
		"Recent changes",
		"Since your last visit",
		"Longest active",
		"Most unstable namespace",
		"Cluster information",
		"Configured retention:",
		"Storage:",
		"Cluster posture at a glance",
		"Nodes",
		"Namespaces",
		"Security",
		"Cost",
		"Waste &amp; Drift",
		"Node Optimization",
		"In development",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
}

// TestOverviewTemplate_PriorityHeroSeverityStyles protects the semantic
// severity classes used by the redesigned top-priority hero.
func TestOverviewTemplate_PriorityHeroSeverityStyles(t *testing.T) {
	for _, severity := range []string{"critical", "high", "medium", "low"} {
		t.Run(severity, func(t *testing.T) {
			data := fullyPopulatedOverviewData()
			data.TopIssues = data.TopIssues[:1]
			data.TopIssues[0].Severity = severity
			data.TopIssues[0].SeverityLbl = strings.ToUpper(severity)

			var buf strings.Builder
			if err := getOverviewTmpl().Execute(&buf, data); err != nil {
				t.Fatalf("template execution failed: %v", err)
			}
			if !strings.Contains(buf.String(), `class="sev-badge `+severity+`"`) {
				t.Fatalf("top issue did not render semantic severity class %q", severity)
			}
		})
	}
}

// TestOverviewTemplate_CompactPostureLinksPreserveCluster verifies that each
// currently actionable posture card links to its detailed page in the active cluster.
func TestOverviewTemplate_CompactPostureLinksPreserveCluster(t *testing.T) {
	data := fullyPopulatedOverviewData()
	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}
	out := buf.String()
	for _, href := range []string{
		data.InfraHref,
		data.NsHref,
		data.SecurityHref,
		data.CostsHref,
		data.WasteHref,
	} {
		if href == "" {
			t.Fatalf("fixture has empty posture href")
		}
		if !strings.Contains(out, `class="posture-card" href="`+href+`"`) {
			t.Errorf("overview posture missing link %q", href)
		}
		if !strings.Contains(href, "cluster=prod-eastus") {
			t.Errorf("posture link did not preserve active cluster: %q", href)
		}
	}
	if !strings.Contains(out, `class="posture-card posture-card-muted node-optimization-card"`) {
		t.Errorf("expected non-actionable Node Optimization status card")
	}
}

func TestOverviewTemplate_CostRequiresPricingAvailability(t *testing.T) {
	t.Run("unavailable pricing never falls back to zero", func(t *testing.T) {
		data := fullyPopulatedOverviewData()
		data.MonthlyCost = 0
		data.CostAvailable = false
		data.CostCoverage = "0 of 3 nodes priced"

		var buf strings.Builder
		if err := getOverviewTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("template execution failed: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, `<div class="posture-value unavailable">Unavailable</div>`) {
			t.Fatalf("expected unavailable cost state")
		}
		if strings.Contains(out, "$0/mo") {
			t.Fatalf("unavailable pricing rendered a zero-dollar fallback")
		}
		if !strings.Contains(out, "0 of 3 nodes priced") {
			t.Fatalf("expected pricing coverage evidence")
		}
	})

	t.Run("available pricing renders reported amount", func(t *testing.T) {
		data := fullyPopulatedOverviewData()

		var buf strings.Builder
		if err := getOverviewTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("template execution failed: %v", err)
		}
		if !strings.Contains(buf.String(), "$1,235/mo") {
			t.Fatalf("available provider pricing did not render reported amount")
		}
	})
}

func TestBuildOverviewData_CostAvailabilityUsesPricingEvidence(t *testing.T) {
	tests := []struct {
		name      string
		scan      *clusterScan
		available bool
	}{
		{name: "no report", scan: &clusterScan{}, available: false},
		{name: "unpriced pool", scan: &clusterScan{report: &models.CloudCostReport{
			NodePoolCosts: []models.NodePoolCost{{Name: "workers", NodeCount: 3, PricingAvailable: false}},
		}}, available: false},
		{name: "provider-priced pool", scan: &clusterScan{report: &models.CloudCostReport{
			NodePoolCosts: []models.NodePoolCost{{Name: "workers", NodeCount: 3, PricingAvailable: true}},
		}}, available: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildOverviewData(tt.scan, "test-cluster", []string{"test-cluster"}, nil, time.Now())
			if data.CostAvailable != tt.available {
				t.Fatalf("CostAvailable = %v, want %v", data.CostAvailable, tt.available)
			}
		})
	}
}

func TestOverviewTemplate_NodeOptimizationIsConservative(t *testing.T) {
	data := fullyPopulatedOverviewData()
	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"In development", "Simulation foundation exists", "No operator action yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("Node Optimization card missing %q", want)
		}
	}
	for _, forbidden := range []string{"simulation-backed node efficiency analysis", "Open optimization →"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("Node Optimization card overstates availability with %q", forbidden)
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

// TestOverviewTemplate_RecentChangesCapAtFive verifies the redesigned single
// recent-changes feed is capped at exactly five entries.
func TestOverviewTemplate_RecentChangesCapAtFive(t *testing.T) {
	data := fullyPopulatedOverviewData()
	makeEvents := func(n int) []changeLine {
		raw := make([]store.RecentEvent, n)
		for i := 0; i < n; i++ {
			raw[i] = store.RecentEvent{
				Resource:    "svc-" + strconv.Itoa(i),
				EventReason: "Detected",
				OccurredAt:  time.Now().Add(-time.Duration(i) * time.Minute),
			}
		}
		return formatChangeLines(raw)
	}
	data.ChangesSinceLastView = makeEvents(9)
	data.RecentEvents = makeEvents(7)
	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}
	if got := strings.Count(buf.String(), `class="change-row"`); got != 5 {
		t.Fatalf("expected 5 Recent Changes rows, got %d", got)
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
	topIssues := []topIssue{
		{
			Title:              "1 namespace with NetworkPolicy coverage gap",
			Namespace:          "payments-prod",
			Resource:           "namespace",
			IssueType:          "unprotected_namespace",
			GroupSize:          1,
			FirstDetectedLabel: "3d ago",
		},
	}

	line1, _ := buildOverviewVerdict(topIssues, 0, time.Now())

	if strings.Contains(line1, "namespace has been") {
		t.Fatalf("expected verdict to use the real namespace, not the literal placeholder %q: got %q", "namespace", line1)
	}
	if !strings.Contains(line1, "payments-prod has been") {
		t.Fatalf("expected verdict to mention namespace %q, got: %q", "payments-prod", line1)
	}
}

// TestBuildOverviewVerdict_IdleNamespaceUsesNamespaceNotLiteralResource
// covers the same substitution for idle_namespace while ensuring recurrence
// aggregates do not alter summary prose.
func TestBuildOverviewVerdict_IdleNamespaceUsesNamespaceNotLiteralResource(t *testing.T) {
	topIssues := []topIssue{
		{
			Title:              "1 idle namespace",
			Namespace:          "batch-jobs",
			Resource:           "namespace",
			IssueType:          "idle_namespace",
			GroupSize:          1,
			FirstDetectedLabel: "5d ago",
			ReopenCountVal:     2,
		},
	}

	line1, _ := buildOverviewVerdict(topIssues, 0, time.Now())

	if strings.Contains(line1, "namespace has been") {
		t.Fatalf("expected verdict to use the real namespace, not the literal placeholder %q: got %q", "namespace", line1)
	}
	if !strings.Contains(line1, "batch-jobs has been") || strings.Contains(line1, "reopened") || strings.Contains(line1, "reoccurred") {
		t.Fatalf("expected verdict to mention namespace %q, got: %q", "batch-jobs", line1)
	}
}

// TestBuildOverviewVerdict_MatchesTopIssuesZero is Fix 2's core regression
// test: the verdict sentence must be built from the exact same topIssues[0]
// the "if you only fix one thing today" banner and Top 5's first row
// render — previously buildOverviewVerdict ran its own independent
// db.QueryIncidents call and could (and did) disagree with Top 5 about
// which issue was "worst".
func TestBuildOverviewVerdict_MatchesTopIssuesZero(t *testing.T) {
	topIssues := []topIssue{
		{
			Title: "5 pods crash-looping", Namespace: "checkout", Resource: "checkout-api-abc123",
			IssueType: "crash_loop", GroupSize: 5, FirstDetectedLabel: "2d ago", TrendVal: "accelerating",
		},
		{
			Title: "2 namespaces with NetworkPolicy coverage gaps", Namespace: "payments", Resource: "namespace",
			IssueType: "unprotected_namespace", GroupSize: 2, FirstDetectedLabel: "1d ago",
		},
	}

	line1, _ := buildOverviewVerdict(topIssues, 0, time.Now())

	if !strings.Contains(line1, "checkout-api-abc123") {
		t.Fatalf("expected verdict to describe topIssues[0] (checkout-api-abc123), got: %q", line1)
	}
	if strings.Contains(line1, "payments") {
		t.Fatalf("expected verdict to NOT describe topIssues[1], got: %q", line1)
	}
	if !strings.Contains(line1, "5 workloads have active incidents") {
		t.Fatalf("expected verdict's count to come from topIssues[0].GroupSize (5), got: %q", line1)
	}
}

// TestBuildOverviewVerdict_EmptyTopIssues covers the "no active incidents"
// fallback now that there's no db.QueryIncidents call to short-circuit on.
func TestBuildOverviewVerdict_EmptyTopIssues(t *testing.T) {
	line1, line2 := buildOverviewVerdict(nil, 0, time.Now())
	if line1 != "No active incidents detected." || line2 != "" {
		t.Fatalf("got line1=%q line2=%q, want the no-incidents fallback", line1, line2)
	}
}

// TestBuildOverviewVerdict_AggregateRowFallsBackToTitle covers topIssues[0]
// being an aggregate row (no single Namespace/Resource/IssueType) — e.g. a
// cluster with only orphaned-PVC waste and no crash-looping pods.
func TestBuildOverviewVerdict_AggregateRowFallsBackToTitle(t *testing.T) {
	topIssues := []topIssue{
		{Title: "3 orphaned PVCs wasting money", Severity: "medium"},
	}
	line1, _ := buildOverviewVerdict(topIssues, 0, time.Now())
	if !strings.Contains(line1, "3 orphaned PVCs wasting money") {
		t.Fatalf("expected verdict to fall back to the aggregate row's title, got: %q", line1)
	}
}

// TestBuildOverviewVerdict_Line2ResolvedSinceCursor covers Fix 2: line2
// shows the resolved-since-cursor count when >0, empty when 0 — and never
// a hollow "0 incidents resolved" sentence. Also covers line2 appearing
// even when there are no active incidents (topIssues empty) — confirming
// resolved-since-cursor and topIssues are independent.
func TestBuildOverviewVerdict_Line2ResolvedSinceCursor(t *testing.T) {
	t.Run("zero resolved leaves line2 empty", func(t *testing.T) {
		_, line2 := buildOverviewVerdict(nil, 0, time.Now().Add(-24*time.Hour))
		if line2 != "" {
			t.Fatalf("expected empty line2 for zero resolved, got %q", line2)
		}
	})

	t.Run("one resolved uses singular wording", func(t *testing.T) {
		_, line2 := buildOverviewVerdict(nil, 1, time.Now().Add(-24*time.Hour))
		if line2 != "1 incident resolved since yesterday." {
			t.Fatalf("line2 = %q, want %q", line2, "1 incident resolved since yesterday.")
		}
	})

	t.Run("multiple resolved uses plural wording and correct count", func(t *testing.T) {
		_, line2 := buildOverviewVerdict(nil, 3, time.Now().Add(-24*time.Hour))
		if line2 != "3 incidents resolved since yesterday." {
			t.Fatalf("line2 = %q, want %q", line2, "3 incidents resolved since yesterday.")
		}
	})

	t.Run("line2 appears alongside a real line1 when there IS an active issue", func(t *testing.T) {
		topIssues := []topIssue{
			{Title: "1 pod crash-looping", Namespace: "payments", Resource: "payments-api-abc123", IssueType: "crash_loop", GroupSize: 1, FirstDetectedLabel: "2d ago"},
		}
		line1, line2 := buildOverviewVerdict(topIssues, 2, time.Now().Add(-24*time.Hour))
		if !strings.Contains(line1, "payments-api-abc123") {
			t.Fatalf("expected line1 to still describe the active issue, got %q", line1)
		}
		if line2 != "2 incidents resolved since yesterday." {
			t.Fatalf("line2 = %q, want %q", line2, "2 incidents resolved since yesterday.")
		}
	})
}

// TestSinceCursorPhrase covers the "since yesterday" vs "since your last
// visit" wording choice: "yesterday" only reads honestly when the cursor
// is roughly a day old (the default for a first-time visitor); anything
// meaningfully more recent or older uses the always-accurate fallback.
func TestSinceCursorPhrase(t *testing.T) {
	tests := []struct {
		name    string
		elapsed time.Duration
		want    string
	}{
		{"exactly 24h (the first-time-visitor default) reads as yesterday", 24 * time.Hour, "since yesterday"},
		{"18h lower bound reads as yesterday", 18 * time.Hour, "since yesterday"},
		{"36h upper bound reads as yesterday", 36 * time.Hour, "since yesterday"},
		{"a few minutes ago is not yesterday", 5 * time.Minute, "since your last visit"},
		{"a week ago is not yesterday", 7 * 24 * time.Hour, "since your last visit"},
		{"zero elapsed (impossible but defensive) is not yesterday", 0, "since your last visit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sinceCursorPhrase(tt.elapsed); got != tt.want {
				t.Errorf("sinceCursorPhrase(%v) = %q, want %q", tt.elapsed, got, tt.want)
			}
		})
	}
}

// TestCountResolvedSince covers the reused-not-requeried plumbing: the
// count comes from filtering the already-fetched changeLine slice
// (GetChangesSince's result), not a second query.
func TestCountResolvedSince(t *testing.T) {
	changes := []changeLine{
		{Phrase: "payments-api recovered", EventReason: "Resolved"},
		{Phrase: "checkout-api reopened", EventReason: "Reopened"},
		{Phrase: "New: fraud-svc", EventReason: "Detected"},
		{Phrase: "orders-api recovered", EventReason: "Resolved"},
	}
	if got := countResolvedSince(changes); got != 2 {
		t.Errorf("countResolvedSince = %d, want 2", got)
	}
	if got := countResolvedSince(nil); got != 0 {
		t.Errorf("countResolvedSince(nil) = %d, want 0", got)
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
			want: "First detected 3d ago",
		},
		{
			name: "multiple reopens use ×N", firstDetectedLabel: "3d ago", reopenCount: 4, trend: "stable", issueType: "crash_loop",
			want: "First detected 3d ago",
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
			want: "First detected 7d ago · accelerating",
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
	if strings.Contains(got[0].MemoryLine, "reopened") || !strings.Contains(got[0].MemoryLine, "accelerating") {
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

// TestOverviewTemplate_PriorityStates covers both states of the War Room-first
// hierarchy: a ranked hero when an issue exists and a calm state otherwise.
func TestOverviewTemplate_PriorityStates(t *testing.T) {
	t.Run("top issue renders prioritized state", func(t *testing.T) {
		data := fullyPopulatedOverviewData()
		var buf strings.Builder
		if err := getOverviewTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("template execution failed: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "Highest priority") {
			t.Errorf("expected populated priority state")
		}
		if !strings.Contains(out, "Open in War Room") {
			t.Errorf("expected direct War Room action")
		}
	})
	t.Run("no top issue renders calm empty state", func(t *testing.T) {
		data := fullyPopulatedOverviewData()
		data.TopIssues = nil
		data.HasTopIssue = false
		data.CriticalCount = 0
		var buf strings.Builder
		if err := getOverviewTmpl().Execute(&buf, data); err != nil {
			t.Fatalf("template execution failed: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "No urgent priorities") {
			t.Errorf("expected no-priority state")
		}
		if !strings.Contains(out, "No active issues need immediate attention.") {
			t.Errorf("expected explicit healthy priority message")
		}
	})
}

// TestOverviewTemplate_DoesNotDuplicateDetailedHealthGrids verifies detailed
// namespace/workload presentations stay on their dedicated pages.
func TestOverviewTemplate_DoesNotDuplicateDetailedHealthGrids(t *testing.T) {
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
	if strings.Contains(out, `class="ns-health-row"`) {
		t.Errorf("did not expect detailed namespace health rows on redesigned Overview")
	}
	if strings.Contains(out, `class="wh-dot`) {
		t.Errorf("did not expect workload health dot grid on redesigned Overview")
	}
	for _, forbidden := range []string{"Namespace Health", "Workload Health"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("did not expect removed Overview section %q", forbidden)
		}
	}
	for _, want := range []string{"Nodes", "Namespaces", "Security", "Cost", "Waste &amp; Drift", "Node Optimization"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected compact posture card %q", want)
		}
	}
}

// TestOverviewTemplate_RemovedPresentationStaysAbsent prevents the obsolete
// Overview banner, briefing, score strip, and large health cards returning.
func TestOverviewTemplate_RemovedPresentationStaysAbsent(t *testing.T) {
	data := fullyPopulatedOverviewData()

	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}
	out := buf.String()

	for _, forbidden := range []string{
		"If you only fix one thing today",
		"Situation Briefing",
		`class="bottom-strip"`,
		`class="bottom-val`,
		"Namespace Health",
		"Workload Health",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("removed Overview presentation returned: %q", forbidden)
		}
	}
}

func TestSidebarTemplate_NewTaxonomyAndClusterPropagation(t *testing.T) {
	const clusterQuery = "?cluster=prod-eastus"
	data := sidebarData{
		DashHref:        "/" + clusterQuery,
		WrHref:          "/warroom" + clusterQuery,
		IncidentsHref:   "/incidents" + clusterQuery,
		InfraHref:       "/infrastructure" + clusterQuery,
		NsHref:          "/namespaces" + clusterQuery,
		CostsHref:       "/costs" + clusterQuery,
		OptHref:         "/optimizations" + clusterQuery,
		WasteHref:       "/waste" + clusterQuery,
		SecurityHref:    "/security" + clusterQuery,
		DiagnosticsHref: "/settings/diagnostics" + clusterQuery,
		SettingsHref:    "/settings" + clusterQuery,
		ActivePage:      "dashboard",
		ClusterName:     "prod-eastus",
		CriticalCount:   2,
		Clusters: []sidebarCluster{{
			Href:     "/?cluster=staging-west",
			Label:    "staging-west",
			IsActive: false,
		}},
	}

	var buf strings.Builder
	if err := getSidebarTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("sidebar template execution failed: %v", err)
	}
	out := buf.String()

	labels := []string{
		"Operations", "Overview", "War Room", "Incidents", "Nodes", "Namespaces",
		"Efficiency", "Cost", "Node Optimization", "Waste &amp; Drift",
		"Risk", "Security", "System", "Diagnostics", "Settings",
	}
	last := -1
	for _, label := range labels {
		pos := strings.Index(out, label)
		if pos < 0 {
			t.Fatalf("sidebar missing taxonomy label %q", label)
		}
		if pos < last {
			t.Fatalf("sidebar label %q rendered out of contract order", label)
		}
		last = pos
	}

	for _, href := range []string{
		data.DashHref, data.WrHref, data.IncidentsHref, data.InfraHref, data.NsHref,
		data.CostsHref, data.OptHref, data.WasteHref, data.SecurityHref,
		data.DiagnosticsHref, data.SettingsHref,
	} {
		if !strings.Contains(out, `href="`+href+`"`) {
			t.Errorf("sidebar missing cluster-scoped link %q", href)
		}
	}
	if !strings.Contains(out, `href="/?cluster=staging-west"`) {
		t.Errorf("sidebar missing cluster switch link")
	}
	for _, obsolete := range []string{"Cost Intelligence", "Security Posture", "> Infrastructure<"} {
		if strings.Contains(out, obsolete) {
			t.Errorf("sidebar rendered obsolete label %q", obsolete)
		}
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
			name:      "namespace finding omits synthetic pod parameter",
			namespace: "monitoring", resource: "namespace", issueType: "unprotected_namespace", ctx: "my cluster",
			want: "/investigate?ns=monitoring&type=unprotected_namespace&cluster=my+cluster&from=warroom",
		},
		{
			name:      "idle namespace omits synthetic pod parameter",
			namespace: "batch", resource: "namespace", issueType: "idle_namespace", ctx: "my cluster",
			want: "/investigate?ns=batch&type=idle_namespace&cluster=my+cluster&from=warroom",
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

func TestRenderWarRoomCardInvestigationLinks(t *testing.T) {
	namespaceCard := renderWarRoomCard(warRoomIssue{
		Resource: "namespace", Namespace: "monitoring", Type: "unprotected_namespace",
	}, "prod-cluster")
	if strings.Contains(namespaceCard, "pod=namespace") {
		t.Errorf("namespace card included synthetic pod parameter: %s", namespaceCard)
	}
	if !strings.Contains(namespaceCard, "/investigate?ns=monitoring&type=unprotected_namespace&cluster=prod-cluster&from=warroom") {
		t.Errorf("namespace card missing ns/type investigation link: %s", namespaceCard)
	}

	workloadCard := renderWarRoomCard(warRoomIssue{
		Resource: "fraud-detection-abc", Namespace: "payments", Type: "crash_loop",
	}, "prod-cluster")
	if !strings.Contains(workloadCard, "/investigate?pod=fraud-detection-abc&ns=payments&type=crash_loop&cluster=prod-cluster&from=warroom") {
		t.Errorf("workload card omitted Focus Pod: %s", workloadCard)
	}

	emptyContextCard := renderWarRoomCard(warRoomIssue{
		Resource: "fraud-detection-abc", Namespace: "payments", Type: "crash_loop",
	}, "")
	if strings.Contains(emptyContextCard, "cluster=current-context") {
		t.Errorf("empty active context emitted synthetic cluster context: %s", emptyContextCard)
	}
	if !strings.Contains(emptyContextCard, "&from=warroom") {
		t.Errorf("empty active context omitted source parameter: %s", emptyContextCard)
	}

	emptyResourceNamespaceCard := renderWarRoomCard(warRoomIssue{
		Namespace: "monitoring", Type: "unprotected_namespace",
	}, "prod-cluster")
	if !strings.Contains(emptyResourceNamespaceCard, "Investigate →") {
		t.Errorf("namespace card with empty Resource omitted Investigate action: %s", emptyResourceNamespaceCard)
	}
	if strings.Contains(emptyResourceNamespaceCard, "pod=") {
		t.Errorf("namespace card with empty Resource emitted pod parameter: %s", emptyResourceNamespaceCard)
	}
}

func TestRenderWarRoomPagePassesActiveContextToCards(t *testing.T) {
	scan := &clusterScan{
		wasteAudit: &analyzer.WasteAudit{
			StalePods: []analyzer.StalePod{{
				Name: "fraud-detection-abc", Namespace: "payments",
				Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff",
			}},
		},
		netAudit: &analyzer.NetworkPolicyAudit{
			UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{{
				Name: "monitoring", RiskLevel: "HIGH",
			}},
		},
	}
	body := renderWarRoomPage(scan, "prod-cluster", []string{"prod-cluster"})
	for _, want := range []string{
		"/investigate?pod=fraud-detection-abc&ns=payments&type=crash_loop&cluster=prod-cluster&from=warroom",
		"/investigate?ns=monitoring&type=unprotected_namespace&cluster=prod-cluster&from=warroom",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered War Room page missing context-aware link %q", want)
		}
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
	if issues[0].GroupSize != 1 {
		t.Errorf("GroupSize = %d, want 1", issues[0].GroupSize)
	}
	if issues[0].ButtonLabel != "Fix now →" {
		t.Errorf("ButtonLabel = %q, want %q", issues[0].ButtonLabel, "Fix now →")
	}
}

// TestBuildTopIssues_MultiPodGroupRoutesToIncidentsRegistry covers a group
// of more than one pod: rather than deep-linking to grp[0]'s pod as if it
// were "the" incident, the row must route to the Incidents registry
// filtered to this issue type, with a "View all N →" button.
func TestBuildTopIssues_MultiPodGroupRoutesToIncidentsRegistry(t *testing.T) {
	scan := &clusterScan{
		wasteAudit: &analyzer.WasteAudit{
			StalePods: []analyzer.StalePod{
				{Name: "payments-api-abc123", Namespace: "payments", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 10, AgeDays: 2},
				{Name: "payments-worker-def456", Namespace: "payments", Kind: analyzer.StalePodZombie, Status: "CrashLoopBackOff", RestartCount: 5, AgeDays: 1},
			},
		},
	}
	wrIssues := collectWarRoomIssues(scan, 0)
	issues := buildTopIssues(scan, wrIssues, "test-cluster")

	if len(issues) == 0 {
		t.Fatalf("expected at least one topIssue")
	}
	if issues[0].GroupSize != 2 {
		t.Fatalf("expected GroupSize=2, got %d", issues[0].GroupSize)
	}
	wantURL := "/incidents?cluster=test-cluster&type=crash_loop&status=active"
	if issues[0].URL != wantURL {
		t.Errorf("URL = %q, want %q", issues[0].URL, wantURL)
	}
	wantLabel := "View all 2 →"
	if issues[0].ButtonLabel != wantLabel {
		t.Errorf("ButtonLabel = %q, want %q", issues[0].ButtonLabel, wantLabel)
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
	if issues[0].ButtonLabel != "View all 1 →" {
		t.Errorf("expected aggregate row's button label to reflect its real PVC count, got %q", issues[0].ButtonLabel)
	}
	if issues[0].GroupSize != 1 {
		t.Errorf("expected aggregate row's GroupSize to reflect its real PVC count, got %d", issues[0].GroupSize)
	}
}

// TestFormatChangeLine covers every event_reason category the What's
// Changed / Recent Events feeds can show, asserting phrasing and that no
// case produces empty or malformed text. RestartMilestone specifically
// must NOT claim a trend judgment ("accelerating") or fabricate a
// percentage — store.RecentEvent carries no restart count or trend data to
// justify either.
func TestFormatChangeLine(t *testing.T) {
	tests := []struct {
		eventReason string
		wantPhrase  string
	}{
		{"Detected", "New: fraud-detection"},
		{"Resolved", "fraud-detection recovered"},
		{"Reopened", "fraud-detection reopened"},
		{"RestartMilestone", "fraud-detection restart milestone reached"},
		{"SeverityChanged", "fraud-detection severity changed"},
	}
	for _, tt := range tests {
		t.Run(tt.eventReason, func(t *testing.T) {
			got := formatChangeLine(store.RecentEvent{
				Resource: "fraud-detection", EventReason: tt.eventReason, OccurredAt: time.Now(),
			})
			if got.Phrase != tt.wantPhrase {
				t.Errorf("Phrase = %q, want %q", got.Phrase, tt.wantPhrase)
			}
			if got.Phrase == "" {
				t.Errorf("Phrase must not be empty for %q", tt.eventReason)
			}
			if got.EventReason != tt.eventReason {
				t.Errorf("EventReason = %q, want %q (must match verbatim for the CSS class to resolve)", got.EventReason, tt.eventReason)
			}
			if strings.Contains(got.Phrase, "%") {
				t.Errorf("Phrase %q must not fabricate a percentage — RestartMilestone carries no restart-count data", got.Phrase)
			}
			if strings.Contains(strings.ToLower(got.Phrase), "accelerat") {
				t.Errorf("Phrase %q must not claim a trend judgment RecentEvent has no data to support", got.Phrase)
			}
		})
	}

	t.Run("unknown reason falls back to resource name, never empty", func(t *testing.T) {
		got := formatChangeLine(store.RecentEvent{Resource: "mystery-svc", EventReason: "SomethingElse", OccurredAt: time.Now()})
		if got.Phrase == "" {
			t.Errorf("expected a non-empty fallback phrase, got empty")
		}
	})
}

// TestOverviewTemplate_ChangeDotCasingMatchesCSS is Fix 3's regression
// test: for every event_reason category, the rendered .change-dot class
// must match the CSS's Title-Case selectors (.change-dot.Detected,
// .change-dot.Resolved, etc.) exactly — case-sensitive, as CSS class
// matching always is.
func TestOverviewTemplate_ChangeDotCasingMatchesCSS(t *testing.T) {
	reasons := []string{"Detected", "Resolved", "Reopened", "RestartMilestone", "SeverityChanged"}

	data := fullyPopulatedOverviewData()
	var raw []store.RecentEvent
	for _, r := range reasons {
		raw = append(raw, store.RecentEvent{Resource: "svc", EventReason: r, OccurredAt: time.Now()})
	}
	data.RecentEvents = formatChangeLines(raw)

	var buf strings.Builder
	if err := getOverviewTmpl().Execute(&buf, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}
	out := buf.String()

	for _, r := range reasons {
		want := `class="change-dot ` + r + `"`
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in rendered output (must match .change-dot.%s CSS exactly), not found", want, r)
		}
	}
}

func renderSecurityForTest(t *testing.T, scan *clusterScan) string {
	t.Helper()
	srv := newServer([]string{"test-ctx"}, &store.NullStore{}, 90, false)
	state := srv.getState("test-ctx")
	state.mu.Lock()
	state.scan = scan
	state.mu.Unlock()
	req := httptest.NewRequest(http.MethodGet, "/security?cluster=test-ctx", nil)
	rec := httptest.NewRecorder()
	srv.handleSecurityPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("security status = %d; body: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func renderWasteForTest(t *testing.T, audit *analyzer.WasteAudit) string {
	t.Helper()
	srv := newServer([]string{"test-ctx"}, &store.NullStore{}, 90, false)
	state := srv.getState("test-ctx")
	state.mu.Lock()
	state.scan = &clusterScan{wasteAudit: audit}
	state.mu.Unlock()
	req := httptest.NewRequest(http.MethodGet, "/waste?cluster=test-ctx", nil)
	rec := httptest.NewRecorder()
	srv.handleWastePage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("waste status = %d; body: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestSecurityPageScanStatesAndClaims(t *testing.T) {
	t.Run("unavailable", func(t *testing.T) {
		body := renderSecurityForTest(t, &clusterScan{})
		if !strings.Contains(body, "Security scan unavailable/incomplete") {
			t.Fatal("missing unavailable state")
		}
		if strings.Contains(body, "0/100") || strings.Contains(body, "No active risks detected") {
			t.Fatal("unavailable scan rendered a zero score or clean state")
		}
	})

	t.Run("successful clean", func(t *testing.T) {
		audit := &models.SecurityAudit{}
		result := analyzer.CalculateCISScore(audit, nil)
		body := renderSecurityForTest(t, &clusterScan{secAudit: audit, cisResult: &result})
		if !strings.Contains(body, "No supported workload-security findings") {
			t.Fatal("successful scan without findings did not render clean state")
		}
		if !strings.Contains(body, "Passed checks") || !strings.Contains(body, "<details") {
			t.Fatal("passed checks are not rendered in a collapsed details element")
		}
	})

	t.Run("partial policy coverage reports actual directional gap", func(t *testing.T) {
		securityAudit := &models.SecurityAudit{TotalPodsAudited: 2}
		networkAudit := &analyzer.NetworkPolicyAudit{
			TotalNamespaces: 1,
			UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{{
				Name: "payments-prod", PodCount: 2, PolicyCount: 1,
				CoveredPodCount: 2, UncoveredPodCount: 0,
				FullyCoveredPodCount: 0, CoverageGapPodCount: 2,
				RiskLevel: "HIGH",
			}},
		}
		result := analyzer.CalculateCISScore(securityAudit, networkAudit)
		body := renderSecurityForTest(t, &clusterScan{
			secAudit: securityAudit, cisResult: &result, netAudit: networkAudit,
		})
		if !strings.Contains(body, "2 of 2 observed pods lack configured ingress and egress coverage") ||
			strings.Contains(body, "0 of 2 observed pods") ||
			strings.Contains(body, "No NetworkPolicy detected in namespace payments-prod") {
			t.Fatalf("Security page misrepresented existing policy coverage: %s", body)
		}
	})

	t.Run("audit warnings are visible and suppress false pass", func(t *testing.T) {
		securityAudit := &models.SecurityAudit{}
		networkAudit := &analyzer.NetworkPolicyAudit{
			TotalNamespaces:     2,
			ProtectedNamespaces: []analyzer.NamespaceNetworkStatus{{Name: "healthy"}},
			Warnings: []analyzer.NetworkAuditWarning{{
				Namespace: "blocked", Operation: "list NetworkPolicies", Message: "forbidden",
			}},
		}
		result := analyzer.CalculateCISScore(securityAudit, networkAudit)
		body := renderSecurityForTest(t, &clusterScan{
			secAudit: securityAudit, cisResult: &result, netAudit: networkAudit,
		})
		for _, want := range []string{
			"Scan coverage is Partial", "Unavailable NetworkPolicy Checks", "blocked", "list NetworkPolicies", "forbidden",
			"NetworkPolicy coverage could not be verified for every namespace",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("incomplete network audit omitted %q", want)
			}
		}
		if strings.Contains(body, "Every evaluated namespace has full configured NetworkPolicy coverage") {
			t.Fatal("incomplete network audit rendered a false passing state")
		}
	})

	t.Run("findings and safe claims", func(t *testing.T) {
		audit := &models.SecurityAudit{
			TotalPodsAudited: 4,
			Risks: models.SecurityRisks{
				RunningAsRoot:          2,
				PrivilegedContainers:   1,
				HostNetwork:            1,
				HostPID:                1,
				HostIPC:                1,
				HostPathVolumes:        1,
				DefaultServiceAccount:  1,
				MissingResourceLimits:  1,
				AddedCapabilities:      1,
				PrivilegeEscalation:    1,
				MissingProbes:          99,
				WritableFilesystem:     99,
				MissingNetworkPolicies: 99,
			},
			PriorityActions: []string{
				"Review containers without explicitly enforced non-root execution",
			},
			Issues: []models.SecurityIssue{
				{Type: "running_as_root", Severity: "medium", Resource: "container", Namespace: "app", Name: "api/main", Description: "Non-root execution not explicitly enforced in the pod spec"},
				{Type: "privileged_container", Severity: "medium", Resource: "container", Namespace: "kube-system", Name: "agent/main", Description: "Container running in privileged mode (expected for this infrastructure component)"},
				{Type: "host_network", Severity: "high", Resource: "pod", Namespace: "app", Name: "api", Description: "Pod uses host network namespace"},
				{Type: "host_pid", Severity: "critical", Resource: "pod", Namespace: "app", Name: "api", Description: "Pod uses host PID namespace"},
				{Type: "host_ipc", Severity: "high", Resource: "pod", Namespace: "app", Name: "api", Description: "Pod uses host IPC namespace"},
				{Type: "host_path_volume", Severity: "high", Resource: "pod", Namespace: "app", Name: "api", Description: "Pod mounts host path: /data"},
				{Type: "default_service_account", Severity: "medium", Resource: "pod", Namespace: "app", Name: "api", Description: "Pod uses default service account"},
				{Type: "missing_resource_limits", Severity: "medium", Resource: "container", Namespace: "app", Name: "api/main", Description: "Container missing a CPU limit in the pod spec"},
				{Type: "added_capabilities", Severity: "medium", Resource: "container", Namespace: "app", Name: "api/main", Description: "Container adds capabilities: NET_ADMIN"},
				{Type: "privilege_escalation", Severity: "medium", Resource: "container", Namespace: "app", Name: "api/main", Description: "Container allows privilege escalation"},
			},
		}
		result := analyzer.CalculateCISScore(audit, nil)
		body := renderSecurityForTest(t, &clusterScan{
			secAudit: audit, cisResult: &result,
			netAudit: &analyzer.NetworkPolicyAudit{
				ProtectedNamespaces: []analyzer.NamespaceNetworkStatus{{Name: "protected"}},
				UnprotectedNamespaces: []analyzer.NamespaceNetworkStatus{{
					Name: "app", PodCount: 2, RiskLevel: "HIGH", RiskReason: "critical infrastructure exposed; isolation recommended; unprotected",
				}},
			},
		})
		for _, want := range []string{
			"Workload Security Posture",
			"CIS-aligned workload checks and OpsCart operational checks",
			"Not a formal compliance assessment",
			"Situation Briefing",
			"10 security finding categories require review across 4 audited pods",
			"The network audit found 1 namespace with a NetworkPolicy coverage gap",
			"1 observation was recognized as expected infrastructure behavior",
			"Finding Categories",
			"1 / 2",
			"Namespaces With Coverage Gap",
			"Prioritized Findings",
			"Non-root enforcement",
			"OpsCart · Workload Security Posture",
			"Privileged containers",
			"Host network",
			"Host PID",
			"Host IPC",
			"HostPath mounts",
			"Default ServiceAccount",
			"Resource limits",
			"Added capabilities",
			"Privilege escalation",
			"Expected infrastructure",
			"Namespace Isolation",
			"Methodology",
			"kubectl get pod api -n app -o yaml",
			"kubectl get networkpolicy -n app",
			"No NetworkPolicy detected in namespace app.",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
		for _, forbidden := range []string{"CIS Kubernetes Benchmark v1.8", "w: 10.0", "Running as Root", "FinOps Engine", "Risk Breakdown", "Priority Actions", "Workload Security Checks", "critical infrastructure exposed", "isolation recommended", "unprotected"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("rendered unsupported/internal text %q", forbidden)
			}
		}
	})
}

func TestWastePageStatesCategoriesAndReconciliation(t *testing.T) {
	t.Run("complete clean", func(t *testing.T) {
		body := renderWasteForTest(t, &analyzer.WasteAudit{})
		if !strings.Contains(body, "No resource review findings were reported") || strings.Contains(body, "Scan incomplete") {
			t.Fatalf("unexpected clean state: %s", body)
		}
	})

	t.Run("partial", func(t *testing.T) {
		body := renderWasteForTest(t, &analyzer.WasteAudit{
			DetectorWarnings: []analyzer.WasteDetectorWarning{{Category: "Broken ingresses", Error: "forbidden"}},
		})
		for _, want := range []string{"Warnings reported", "Unavailable Checks", "Broken ingresses"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
		if strings.Contains(body, "No waste detected") {
			t.Fatal("partial scan rendered misleading clean state")
		}
	})

	t.Run("every counted category", func(t *testing.T) {
		audit := &analyzer.WasteAudit{
			AbandonedNamespaces:  []analyzer.AbandonedNamespace{{Name: "abandoned"}},
			StalePods:            []analyzer.StalePod{{Name: "zombie", Kind: analyzer.StalePodZombie}, {Name: "idle", Kind: analyzer.StalePodIdle}},
			OrphanedPVCs:         []analyzer.OrphanedPVC{{Name: "pvc"}},
			StaleJobs:            []analyzer.StaleJob{{Name: "job"}},
			ZeroReplicaWorkloads: []analyzer.ZeroReplicaWorkload{{Name: "zero"}},
			OrphanedServices:     []analyzer.OrphanedService{{Name: "service"}},
			BrokenIngresses:      []analyzer.BrokenIngress{{Name: "ingress"}},
			MisconfiguredHPAs:    []analyzer.MisconfiguredHPA{{Name: "hpa"}},
			OldReplicaSets:       []analyzer.OldReplicaSet{{Name: "rs"}},
			TotalWasteItems:      9,
		}
		body := renderWasteForTest(t, audit)
		for _, want := range []string{
			"Pod failure finding", "Pod ownership review", "PVC state / reference review",
			"Job / CronJob retention review", "Zero-replica workload", "Namespace activity review",
			"Service selector review", "Ingress backend evidence", "HPA configuration review",
			"Housekeeping / Retention", ">10<", "Distinct Resources",
			"PVC request volume", "Operational Findings", "Reported Check Coverage",
			"no financial-waste conclusion", "View active incidents", "Ranked Findings", "Drift", "kubectl get pvc pvc",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing category or total %q", want)
			}
		}
		for _, forbidden := range []string{"automatically safe", "safe to delete", "Est. Monthly", "$0", "Not calculated"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("rendered misleading claim %q", forbidden)
			}
		}
		visibleCount := len(audit.AbandonedNamespaces) + len(audit.StalePods) + len(audit.OrphanedPVCs) +
			len(audit.StaleJobs) + len(audit.ZeroReplicaWorkloads) + len(audit.OrphanedServices) +
			len(audit.BrokenIngresses) + len(audit.MisconfiguredHPAs)
		counts := analyzer.BuildWastePresentation(audit).Counts
		if visibleCount+len(audit.OldReplicaSets) != counts.Findings || counts.Findings != counts.Operational+counts.Retention+counts.Review {
			t.Fatalf("counts do not reconcile: %+v", counts)
		}
		if rows := buildWasteDashboardRows(audit); len(rows) != counts.Findings {
			t.Fatal("unified finding rows do not reconcile")
		}
	})
}

func TestDashboardWasteMinAgeDefault(t *testing.T) {
	if dashboardWasteMinAgeDays != 7 {
		t.Fatalf("dashboardWasteMinAgeDays = %d, want 7", dashboardWasteMinAgeDays)
	}
}

func TestSecurityAndWasteTemplatesUseSemanticResponsiveStructureWithoutEmoji(t *testing.T) {
	for _, name := range []string{"templates/security.html", "templates/waste.html"} {
		raw, err := templateFS.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		text := string(raw)
		for _, forbidden := range []string{"✅", "❌", "⚠️", "🔴", "🟡", "📊", "📋", "🔍", "💀", "💾", "⏸️", "📁", "⏰"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains emoji status/section icon %q", name, forbidden)
			}
		}
		wants := []string{"@media(max-width:", `stroke="currentColor"`}
		if name == "templates/security.html" {
			wants = append(wants, "<table", "<thead>", "<tbody>")
		} else {
			wants = append(wants, `role="list"`, `role="listitem"`, `<details class="why">`)
		}
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing semantic/responsive structure %q", name, want)
			}
		}
		for i, svg := range strings.Split(text, "<svg")[1:] {
			openTag := strings.SplitN(svg, ">", 2)[0]
			for _, attribute := range []string{`width="`, `height="`, `viewBox="0 0 24 24"`} {
				if !strings.Contains(openTag, attribute) {
					t.Errorf("%s svg %d missing explicit %s", name, i+1, attribute)
				}
			}
		}
	}
}

func TestSecurityBriefingIconIsExplicitlyConstrained(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/security.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		`.security-briefing-icon{width:20px;height:20px;flex:0 0 20px;display:inline-flex`,
		`.security-briefing-icon svg{display:block;width:20px;height:20px}`,
		`<span class="security-briefing-icon"><svg width="20" height="20" viewBox="0 0 24 24"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("security briefing icon constraint missing %q", want)
		}
	}
	for _, forbidden := range []string{
		".security-briefing-icon{width:100%",
		".security-briefing-icon svg{width:100%",
		"height:auto",
		"flex-grow",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("security briefing icon contains stretching rule %q", forbidden)
		}
	}
}

func TestRedesignedPagesPreserveClusterQueryLinks(t *testing.T) {
	waste := renderWasteForTest(t, &analyzer.WasteAudit{
		StalePods:       []analyzer.StalePod{{Name: "failed", Kind: analyzer.StalePodZombie}},
		TotalWasteItems: 1,
	})
	for _, want := range []string{
		`href="/incidents?cluster=test-ctx&amp;status=active"`,
		`href="/security?cluster=test-ctx"`,
		`href="/waste?cluster=test-ctx"`,
	} {
		if !strings.Contains(waste, want) {
			t.Errorf("waste page missing cluster-preserving link %q", want)
		}
	}
}

// ── Node findings in aggregate counters ─────────────────────────────────
//
// Node conditions are visible in War Room (via the DB-aware
// collectActiveNodeWarRoomIssues) but calcIncidentScore, countCriticalIssues,
// and the persisted snapshot's critical/warnings tally all read from the
// plain, non-DB collectWarRoomIssues — which never included node findings.
// These read scan.nodeHealth directly instead: it's already the current
// scan's data, no DB query, and no risk of the staleness that a DB-backed
// collector would introduce here (calcIncidentScore runs before this
// scan's node incidents are persisted).

func TestCountCriticalIssuesIncludesCriticalNodeFindings(t *testing.T) {
	scan := &clusterScan{
		nodeHealth: []models.NodeConditionFinding{
			{NodeName: "node-1", ConditionType: "Ready", ConditionStatus: "False"},
			{NodeName: "node-2", ConditionType: "DiskPressure", ConditionStatus: "True"},
		},
	}
	got := countCriticalIssues(scan)
	if got != 1 {
		t.Fatalf("countCriticalIssues = %d, want 1 (only Ready=False is critical; DiskPressure is high, not critical)", got)
	}
}

func TestCalcIncidentScorePenalizesCriticalNodeCondition(t *testing.T) {
	clean := &clusterScan{}
	cleanScore, _, _ := calcIncidentScore(clean)

	withCriticalNode := &clusterScan{
		nodeHealth: []models.NodeConditionFinding{
			{NodeName: "node-1", ConditionType: "Ready", ConditionStatus: "False"},
		},
	}
	gotScore, _, _ := calcIncidentScore(withCriticalNode)

	if gotScore >= cleanScore {
		t.Fatalf("score with a critical node condition (%d) should be lower than a clean scan (%d)", gotScore, cleanScore)
	}
	if cleanScore-gotScore != 10 {
		t.Errorf("expected exactly the -10 critical-node penalty, got a %d point drop", cleanScore-gotScore)
	}
}

func TestCalcIncidentScorePenalizesHighNodeConditionLessThanCritical(t *testing.T) {
	withHighNode := &clusterScan{
		nodeHealth: []models.NodeConditionFinding{
			{NodeName: "node-1", ConditionType: "DiskPressure", ConditionStatus: "True"},
		},
	}
	gotScore, _, _ := calcIncidentScore(withHighNode)
	if 100-gotScore != 4 {
		t.Errorf("expected exactly the -4 high-node penalty, got a %d point drop", 100-gotScore)
	}
}

func TestCalcIncidentScoreNodePenaltiesCap(t *testing.T) {
	var findings []models.NodeConditionFinding
	for i := 0; i < 10; i++ {
		findings = append(findings, models.NodeConditionFinding{
			NodeName: fmt.Sprintf("node-%d", i), ConditionType: "Ready", ConditionStatus: "False",
		})
	}
	gotScore, _, _ := calcIncidentScore(&clusterScan{nodeHealth: findings})
	if 100-gotScore != 30 {
		t.Errorf("expected the critical-node penalty capped at -30 (10 nodes x -10 would be -100), got a %d point drop", 100-gotScore)
	}
}

func TestTallySnapshotCountsIncludesNodeFindings(t *testing.T) {
	issues := []warRoomIssue{
		{Severity: "critical"},
		{Severity: "high"},
	}
	nodeHealth := []models.NodeConditionFinding{
		{NodeName: "node-1", ConditionType: "Ready", ConditionStatus: "False"},         // critical
		{NodeName: "node-2", ConditionType: "MemoryPressure", ConditionStatus: "True"}, // high -> warnings
	}
	critical, warnings := tallySnapshotCounts(issues, nodeHealth)
	if critical != 2 {
		t.Errorf("critical = %d, want 2 (1 pod issue + 1 Ready=False node)", critical)
	}
	if warnings != 2 {
		t.Errorf("warnings = %d, want 2 (1 pod issue + 1 MemoryPressure node)", warnings)
	}
}

func TestTallySnapshotCountsEmptyInputs(t *testing.T) {
	critical, warnings := tallySnapshotCounts(nil, nil)
	if critical != 0 || warnings != 0 {
		t.Errorf("tallySnapshotCounts(nil, nil) = (%d, %d), want (0, 0)", critical, warnings)
	}
}

func TestWastePhase1AuditTimestampAndUnknown(t *testing.T) {
	scanned := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		a         *analyzer.WasteAudit
		timestamp int64
		label     string
	}{
		{"audit timestamp", &analyzer.WasteAudit{ScannedAt: scanned}, scanned.UnixMilli(), "2024-03-04 05:06:07 UTC"},
		{"zero timestamp", &analyzer.WasteAudit{}, 0, "Unknown"},
		{"unavailable", nil, 0, "Unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderWasteForTest(t, tc.a)
			// Both the server-rendered label and the client-side elapsed-time source
			// must come from the audit, never handler execution time.
			if !strings.Contains(body, `id="waste-age">`+tc.label+`</strong>`) || !strings.Contains(body, fmt.Sprintf("var TS= %d", tc.timestamp)) && !strings.Contains(body, fmt.Sprintf("var TS=%d", tc.timestamp)) {
				t.Fatalf("audit timestamp/Unknown source missing")
			}
			if tc.timestamp == 0 && !strings.Contains(body, "if(!TS){if(age)age.textContent='Unknown';return}") {
				t.Fatal("zero timestamp would become an epoch age")
			}
		})
	}
}

func TestWastePhase1CrossSurfaceCountsAndQuantities(t *testing.T) {
	a := &analyzer.WasteAudit{
		TotalWasteItems: 6, EstimatedMonthlyWaste: 98765, OrphanedPVCStorageGB: 500,
		AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "app"}},
		StalePods:           []analyzer.StalePod{{Name: "pod", Namespace: "app", Kind: analyzer.StalePodZombie}},
		OrphanedPVCs:        []analyzer.OrphanedPVC{{Name: "pvc", Namespace: "app", Status: analyzer.PVCBoundNoPod, RequestKnown: true, RequestedBytes: 500 << 20}},
		StaleJobs:           []analyzer.StaleJob{{Name: "cron", Namespace: "app", IsCronJob: true, JobStatus: "NeverScheduled"}, {Name: "cron", Namespace: "app", IsCronJob: true, JobStatus: "NoHistoryLimit"}},
		MisconfiguredHPAs:   []analyzer.MisconfiguredHPA{{Name: "hpa", Namespace: "app", IsActive: true}},
		OldReplicaSets:      []analyzer.OldReplicaSet{{Name: "history", Namespace: "app"}},
	}
	scan := &clusterScan{wasteAudit: a, report: &models.CloudCostReport{}}
	counts := analyzer.BuildWastePresentation(a).Counts
	if counts.Findings != 7 || counts.DistinctResources != 6 || counts.Operational != 2 || counts.Retention != 3 || counts.Review != 2 {
		t.Fatalf("counts=%+v", counts)
	}
	if got := wasteCountByNS(scan); len(got) != 1 || got["app"] != 7 {
		t.Fatalf("namespace counts=%v", got)
	}
	overview := buildOverviewData(scan, "test-ctx", []string{"test-ctx"}, &store.NullStore{}, time.Time{})
	if overview.WasteCount != 7 {
		t.Fatalf("overview count=%d", overview.WasteCount)
	}
	srv := newServer([]string{"test-ctx"}, &store.NullStore{}, 90, false)
	srv.getState("test-ctx").scan = scan
	rec := httptest.NewRecorder()
	srv.handleSummary(rec, httptest.NewRequest(http.MethodGet, "/api/summary?cluster=test-ctx", nil))
	var summary struct {
		Legacy     int                  `json:"waste_total"`
		Definition string               `json:"waste_total_definition"`
		Counts     analyzer.WasteCounts `json:"waste_counts"`
		Available  bool                 `json:"waste_audit_available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Legacy != 6 || summary.Counts != counts || !summary.Available || !strings.Contains(summary.Definition, "excludes ReplicaSets") && !strings.Contains(summary.Definition, "excluding ReplicaSets") {
		t.Fatalf("summary contract=%+v", summary)
	}
	if strings.Contains(rec.Body.String(), "active_operational_findings") || !strings.Contains(rec.Body.String(), "operational_findings") {
		t.Fatal("misleading operational JSON name")
	}
	for name, body := range map[string]string{"waste": renderWasteForTest(t, a), "optimizations": renderOptimizationsPage(scan, "test-ctx", []string{"test-ctx"})} {
		if !strings.Contains(body, "500 MiB") {
			t.Errorf("%s lost byte quantity", name)
		}
		for _, bad := range []string{"500GB", "500 GB", "98765", "98,765", "safe to delete", "unused for", "wasting money"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s unsupported %q", name, bad)
			}
		}
	}
	body := renderWasteForTest(t, a)
	for _, want := range []string{"7 findings across 6 distinct resources", "2 operational findings, 3 housekeeping/retention findings, and 2 other review findings", "kubectl get cronjob cron -n app -o yaml", "Housekeeping / Retention"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	scan.wasteAudit = nil
	rec = httptest.NewRecorder()
	srv.handleSummary(rec, httptest.NewRequest(http.MethodGet, "/api/summary?cluster=test-ctx", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Available || summary.Legacy != -1 || summary.Counts != (analyzer.WasteCounts{}) {
		t.Fatalf("unavailable confused with zero findings: %+v", summary)
	}
}

func TestWastePhase1DashboardStableRanking(t *testing.T) {
	a := &analyzer.WasteAudit{
		AbandonedNamespaces:  []analyzer.AbandonedNamespace{{Name: "namespace", Score: 5}},
		StalePods:            []analyzer.StalePod{{Name: "pod", Kind: analyzer.StalePodIdle, Score: 5}},
		OrphanedPVCs:         []analyzer.OrphanedPVC{{Name: "pvc", Score: 10}},
		OrphanedServices:     []analyzer.OrphanedService{{Name: "service", Score: 5}},
		StaleJobs:            []analyzer.StaleJob{{Name: "job", Score: 5}},
		ZeroReplicaWorkloads: []analyzer.ZeroReplicaWorkload{{Name: "zero", Score: 5}},
		BrokenIngresses:      []analyzer.BrokenIngress{{Name: "ingress", Score: 10}},
		MisconfiguredHPAs:    []analyzer.MisconfiguredHPA{{Name: "hpa", Score: 5}},
		OldReplicaSets:       []analyzer.OldReplicaSet{{Name: "old-low", Score: 1}, {Name: "old-high", Score: 2}},
	}
	resource, drift, housekeeping := buildWasteReviewRows(a, nil)
	for _, tc := range []struct {
		rows []wasteReviewRow
		want string
	}{{resource, "pvc,namespace,pod,service"}, {drift, "ingress,job,zero,hpa"}, {housekeeping, "old-high,old-low"}} {
		var names []string
		for _, row := range tc.rows {
			names = append(names, row.Resource)
		}
		if got := strings.Join(names, ","); got != tc.want {
			t.Fatalf("stable score ranking=%s, want %s", got, tc.want)
		}
	}
}

func TestWastePhase2UnifiedRowsPreserveGroupPrecedenceAndRanking(t *testing.T) {
	a := &analyzer.WasteAudit{
		AbandonedNamespaces: []analyzer.AbandonedNamespace{{Name: "namespace", Score: 30}},
		StalePods: []analyzer.StalePod{
			{Name: "bare-pod", Kind: analyzer.StalePodIdle, Score: 40},
			{Name: "failure-pod", Kind: analyzer.StalePodZombie, Score: 70},
		},
		OrphanedPVCs:     []analyzer.OrphanedPVC{{Name: "pvc", Score: 50}},
		OrphanedServices: []analyzer.OrphanedService{{Name: "service", Score: 60}},
		StaleJobs: []analyzer.StaleJob{
			{Name: "job", Score: 20},
			{Name: "cronjob", IsCronJob: true, Score: 25},
		},
		ZeroReplicaWorkloads: []analyzer.ZeroReplicaWorkload{
			{Name: "deployment", Kind: "Deployment", Score: 15},
			{Name: "statefulset", Kind: "StatefulSet", Score: 10},
		},
		BrokenIngresses: []analyzer.BrokenIngress{{Name: "ingress", Score: 90}},
		MisconfiguredHPAs: []analyzer.MisconfiguredHPA{
			{Name: "active-hpa", IsActive: true, Score: 80},
			{Name: "review-hpa", Score: 100},
		},
		OldReplicaSets: []analyzer.OldReplicaSet{{Name: "replicaset", Score: 200}},
	}

	rows := buildWasteDashboardRows(a)
	var order []string
	keys := make(map[string]string)
	groups := make(map[string]string)
	for _, row := range rows {
		order = append(order, row.Resource)
		keys[row.Resource] = row.CategoryKey
		groups[row.Resource] = row.GroupKey
	}
	wantOrder := "ingress,active-hpa,failure-pod,service,pvc,bare-pod,namespace,review-hpa,cronjob,job,deployment,statefulset,replicaset"
	if got := strings.Join(order, ","); got != wantOrder {
		t.Fatalf("group precedence or stable within-group ranking changed:\ngot  %s\nwant %s", got, wantOrder)
	}
	for resource, want := range map[string]string{
		"namespace": "namespace", "bare-pod": "workload", "failure-pod": "failure",
		"pvc": "storage", "service": "network", "job": "job", "cronjob": "job",
		"deployment": "workload", "statefulset": "workload", "ingress": "failure",
		"active-hpa": "scaling", "review-hpa": "scaling", "replicaset": "replicaset",
	} {
		if keys[resource] != want {
			t.Errorf("%s category key = %q, want %q", resource, keys[resource], want)
		}
	}
	if groups["active-hpa"] != "operational" || groups["review-hpa"] != "drift" || groups["replicaset"] != "housekeeping" {
		t.Fatalf("canonical groups lost: %v", groups)
	}
	if got := wasteCategoryKey(analyzer.WasteFinding{Kind: "Unknown"}); got != "info" {
		t.Fatalf("fallback category key = %q", got)
	}
}

func TestWastePhase2CompactRowsFiltersPaginationAndActions(t *testing.T) {
	a := &analyzer.WasteAudit{
		AbandonedNamespaces:  []analyzer.AbandonedNamespace{{Name: "namespace-marker"}},
		StalePods:            []analyzer.StalePod{{Name: "failure-marker", Namespace: "app", Kind: analyzer.StalePodZombie}, {Name: "workload-marker", Namespace: "app", Kind: analyzer.StalePodIdle}},
		OrphanedPVCs:         []analyzer.OrphanedPVC{{Name: "storage-marker", Namespace: "data", Status: analyzer.PVCBoundNoPod, RequestKnown: true, RequestedBytes: 500 << 20}},
		StaleJobs:            []analyzer.StaleJob{{Name: "job-marker", Namespace: "batch"}},
		ZeroReplicaWorkloads: []analyzer.ZeroReplicaWorkload{{Name: "zero-marker", Namespace: "app", Kind: "Deployment"}},
		OrphanedServices:     []analyzer.OrphanedService{{Name: "network-marker", Namespace: "app"}},
		BrokenIngresses:      []analyzer.BrokenIngress{{Name: "ingress-marker", Namespace: "app"}},
		MisconfiguredHPAs:    []analyzer.MisconfiguredHPA{{Name: "scaling-marker", Namespace: "app"}},
		OldReplicaSets:       []analyzer.OldReplicaSet{{Name: "replicaset-marker", Namespace: "app"}},
	}
	body := renderWasteForTest(t, a)
	for _, want := range []string{
		`id="waste-search"`, `id="waste-category"`, `id="waste-namespace"`, `id="waste-confidence"`,
		`id="waste-page-size"`, "<option>25</option>", "<option>50</option>", "<option>100</option>",
		`role="list"`, `role="listitem"`, `<details class="why">`, "Inspect</summary>", "Why flagged?", "Copy kubectl",
		"Observation", "Inference", "Limitation / caveat", "Recommended review action",
		"Priority score (legacy heuristic)", "Category colors identify resource domains; they do not represent severity or confidence.",
		"cat-namespace", "cat-workload", "cat-replicaset", "cat-job", "cat-storage", "cat-network", "cat-scaling", "cat-failure",
		"500 MiB", "Operational", "Resource Review", "Drift", "Housekeeping / Retention",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("compact dashboard missing %q", want)
		}
	}
	if strings.Contains(body, `id="waste-score"`) {
		t.Fatal("legacy score was promoted to a primary filter")
	}

	raw, err := templateFS.ReadFile("templates/waste.html")
	if err != nil {
		t.Fatal(err)
	}
	templateText := string(raw)
	for _, forbidden := range []string{"fetch(", "XMLHttpRequest", "/api/"} {
		if strings.Contains(templateText, forbidden) {
			t.Errorf("Waste page filtering/action script introduces backend access %q", forbidden)
		}
	}
	why := strings.Index(templateText, `<details class="why">`)
	inference := strings.Index(templateText, "{{$row.Inference}}")
	if why < 0 || inference < why {
		t.Fatal("inference is visible outside the collapsed Why flagged details")
	}
}

func TestWastePhase2BoundsInitialRenderingAt25Rows(t *testing.T) {
	a := &analyzer.WasteAudit{}
	for i := 0; i < 26; i++ {
		a.OldReplicaSets = append(a.OldReplicaSets, analyzer.OldReplicaSet{Name: fmt.Sprintf("history-%02d", i), Score: float64(26 - i)})
	}
	body := renderWasteForTest(t, a)
	for _, id := range []string{"waste-finding-1", "waste-finding-25", "waste-finding-26"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Fatalf("missing paginated row %s", id)
		}
	}
	start := strings.Index(body, `id="waste-finding-26"`)
	end := strings.Index(body[start:], ">")
	if start < 0 || end < 0 || !strings.Contains(body[start:start+end], " hidden") {
		t.Fatal("row 26 is initially visible; default page must be bounded to 25")
	}
	start = strings.Index(body, `id="waste-finding-25"`)
	end = strings.Index(body[start:], ">")
	if start < 0 || end < 0 || strings.Contains(body[start:start+end], " hidden") {
		t.Fatal("row 25 should remain initially visible")
	}
}
