package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// These tests prove handleDashboard's billing-rows pagination branch is
// strictly cache-only: it must be handled BEFORE the empty-HTML fallback
// that calls state.refresh() (an Azure/Kubernetes acquisition call), for
// both a populated and an empty per-cluster cache. newTestServer's
// bogusClusterCtx never has acquisition started (state.acquisition stays
// nil — see investigation_test.go), so state.refresh always fails fast
// with "acquisition unavailable" and no real Kubernetes call is ever
// attempted; that failure text is exactly what would leak into the
// response if this branch ever fell through into the refresh fallback,
// making it a reliable regression signal for "pagination triggered a
// refresh" without needing a mock acquisition runtime.

func TestHandleDashboardBillingPaginationEmptyCacheRendersSafeStateWithoutRefresh(t *testing.T) {
	srv := newTestServer()
	state := srv.getState(bogusClusterCtx)
	if state.scan != nil {
		t.Fatal("test setup: expected no cached scan")
	}

	req := httptest.NewRequest(http.MethodGet, "/costs?billingPage=2&cluster="+bogusClusterCtx, nil)
	rec := httptest.NewRecorder()
	srv.handleDashboard(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK (cache-only, no refresh attempted), got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "scan failed") || strings.Contains(body, "acquisition unavailable") {
		t.Error("response contains the refresh-fallback's error text — pagination fell through into state.refresh()")
	}
	if !strings.Contains(body, "No cached scan is available yet") {
		t.Error("expected the safe empty-cache placeholder, not a mostly-empty full page")
	}
	if !strings.Contains(body, "cluster="+bogusClusterCtx) {
		t.Error("the placeholder's own link back to /costs should preserve the selected cluster")
	}

	state.mu.RLock()
	scanStillNil := state.scan == nil
	htmlPageStillEmpty := state.htmlPage == ""
	state.mu.RUnlock()
	if !scanStillNil {
		t.Error("state.scan became non-nil — a refresh must have run")
	}
	if !htmlPageStillEmpty {
		t.Error("state.htmlPage became non-empty — a refresh must have run and published a scan")
	}
}

func TestHandleDashboardBillingPaginationPopulatedCacheRendersFromCacheWithoutRefresh(t *testing.T) {
	srv := newTestServer()
	state := srv.getState(bogusClusterCtx)
	state.scan = &clusterScan{report: &models.CloudCostReport{
		Timestamp: time.Now(), ClusterName: "cached-scan-marker", Currency: "USD",
	}}

	req := httptest.NewRequest(http.MethodGet, "/costs?billingPage=1&cluster="+bogusClusterCtx, nil)
	rec := httptest.NewRecorder()
	srv.handleDashboard(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK (cache-only, no refresh attempted), got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "scan failed") || strings.Contains(body, "acquisition unavailable") {
		t.Error("response contains the refresh-fallback's error text — pagination fell through into state.refresh()")
	}
	if !strings.Contains(body, "cached-scan-marker") {
		t.Error("expected the page rendered from the pre-set cached scan, not a fresh (and here, failing) refresh")
	}

	state.mu.RLock()
	htmlPageStillEmpty := state.htmlPage == ""
	state.mu.RUnlock()
	if !htmlPageStillEmpty {
		t.Error("state.htmlPage became non-empty — the pagination branch must never populate the full-page cache itself")
	}
}

// TestHandleDashboardPageTwoPreviousLinkStaysCacheOnlyWithEmptyCache proves
// the specific regression billingPageURL always including billingPage (even
// for page 1) fixes: previously, a page-1 link omitted billingPage
// entirely, so following "Previous" from page 2 back to page 1 produced a
// URL indistinguishable from a plain, non-paginated /costs request — one
// that, on an empty per-cluster cache, falls through to
// state.refresh(). With billingPage=1 always present, that same link
// keeps landing in handleDashboard's cache-only pagination branch.
//
// billingPageURL and the resulting href are exercised directly via
// renderCostPage rather than the full handler+billing.Runtime stack for
// step one, because billing.Runtime only exposes a populated Snapshot
// after a real refresh (gated behind an unskippable startup jitter) —
// renderCostPage takes a billing.Snapshot value directly, so it renders
// the exact same page-2 HTML handleDashboard would if this cluster's
// billing were configured and available. Step two — actually following
// that link — goes through the real srv.handleDashboard.
func TestHandleDashboardPageTwoPreviousLinkStaysCacheOnlyWithEmptyCache(t *testing.T) {
	srv := newTestServer()
	state := srv.getState(bogusClusterCtx)
	scan := &clusterScan{report: &models.CloudCostReport{
		Timestamp: time.Now(), ClusterName: "cached-scan-marker", Currency: "USD",
	}}

	page2HTML := renderCostPage(scan, bogusClusterCtx, srv.clusterList, largeResourceLineSnapshot(120), true, 2, 0)
	prevHref := `href="/costs?billingPage=1&amp;cluster=` + bogusClusterCtx + `"`
	if !strings.Contains(page2HTML, prevHref) {
		t.Fatalf("page 2 did not render the expected Previous link %s", prevHref)
	}

	// This cluster's cache is empty when the Previous link is followed
	// (e.g. a process restart between page loads) — handleDashboard must
	// still treat billingPage=1 as a cache-only pagination request, never
	// falling through to state.refresh().
	state.mu.Lock()
	state.scan = nil
	state.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/costs?billingPage=1&cluster="+bogusClusterCtx, nil)
	rec := httptest.NewRecorder()
	srv.handleDashboard(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK following the Previous link (cache-only, no refresh attempted), got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "scan failed") || strings.Contains(body, "acquisition unavailable") {
		t.Error("response contains the refresh-fallback's error text — the page-1 Previous link fell through into state.refresh()")
	}
	if !strings.Contains(body, "No cached scan is available yet") {
		t.Error("expected the safe empty-cache placeholder for the page-1 link too, not a refresh attempt")
	}

	state.mu.RLock()
	scanStillNil := state.scan == nil
	state.mu.RUnlock()
	if !scanStillNil {
		t.Error("state.scan became non-nil — a refresh must have run")
	}
}
