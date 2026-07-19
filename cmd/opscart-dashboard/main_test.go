package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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
