package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/billing"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func TestBuildBillingPageDataNotConfigured(t *testing.T) {
	data := buildBillingPageData(billing.Snapshot{}, false, "", 0, 0)
	if data.Configured {
		t.Error("Configured = true, want false")
	}
	if data.Total != 0 || data.Currency != "" {
		t.Error("unconfigured billing must not report any total/currency")
	}
}

func TestBuildBillingPageDataAvailable(t *testing.T) {
	snap := billing.Snapshot{
		Status: billing.StatusAvailable, Total: 7345, Currency: "USD",
		CostBasis:   billing.CostBasisActualCost,
		PeriodStart: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC),
		Source:      "Azure Cost Management API", Coverage: "resource groups X and Y",
		RetrievedAt: time.Date(2026, 9, 17, 6, 0, 0, 0, time.UTC),
	}
	data := buildBillingPageData(snap, true, "", 0, 0)
	if !data.Configured || data.StatusLabel != "Live" {
		t.Fatalf("data = %+v", data)
	}
	if data.Total != 7345 || data.Currency != "USD" {
		t.Errorf("Total/Currency = %v %v", data.Total, data.Currency)
	}
	if data.PeriodLabel != "2026-08-17 to 2026-09-15" {
		t.Errorf("PeriodLabel = %q", data.PeriodLabel)
	}
	if data.CostBasisLabel != "Actual cost" {
		t.Errorf("CostBasisLabel = %q", data.CostBasisLabel)
	}
}

// testSubscriptionID is a recognizable, obviously-synthetic subscription
// UUID used throughout this file's fixtures — tests assert it (and the
// literal "/subscriptions/" prefix) never appear anywhere in rendered
// billing HTML, since the full resource IDs built from it are kept
// internally for attribution matching and sort ordering only.
const testSubscriptionID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// testARMResourceID builds a syntactically valid, fully-qualified Azure
// resource ID for a non-cluster resource (a disk, by convention) — enough
// for sanitizeResourceID to extract a distinguishable resource type/name
// pair for test assertions.
func testARMResourceID(resourceGroup, name string) string {
	return "/subscriptions/" + testSubscriptionID + "/resourceGroups/" + resourceGroup + "/providers/Microsoft.Compute/disks/" + name
}

// testARMClusterResourceID builds a syntactically valid AKS managedClusters
// resource ID — the shape ClusterResourceID/attribution matching expects.
func testARMClusterResourceID(resourceGroup, name string) string {
	return "/subscriptions/" + testSubscriptionID + "/resourceGroups/" + resourceGroup + "/providers/Microsoft.ContainerService/managedClusters/" + name
}

func testAttributionSnapshot() billing.Snapshot {
	clusterID := testARMClusterResourceID("rg-cluster", "aks")
	return billing.Snapshot{
		Status: billing.StatusAvailable, Total: 300, Currency: "USD",
		ClusterResourceID: clusterID,
		AttributedTotal:   100,
		UnattributedTotal: 200,
		Lines: []billing.ResourceCost{
			{ResourceID: testARMResourceID("rg-node", "disk-z"), ResourceGroup: "rg-node", Cost: 50, Currency: "USD"},
			{ResourceID: clusterID, ResourceGroup: "rg-cluster", Cost: 100, Currency: "USD", Attributed: true},
			{ResourceID: testARMResourceID("rg-node", "disk-a"), ResourceGroup: "rg-node", Cost: 150, Currency: "USD"},
		},
	}
}

func TestBuildBillingPageDataComputesSubtotalsByResourceGroupDeterministically(t *testing.T) {
	snap := testAttributionSnapshot()
	data := buildBillingPageData(snap, true, "", 0, 0)

	if data.ClusterResourceType != "Microsoft.ContainerService/managedClusters" || data.ClusterResourceName != "aks" {
		t.Errorf("ClusterResourceType/ClusterResourceName = %q/%q, want Microsoft.ContainerService/managedClusters/aks", data.ClusterResourceType, data.ClusterResourceName)
	}
	if data.AttributedTotal != 100 || data.UnattributedTotal != 200 {
		t.Errorf("AttributedTotal/UnattributedTotal = %v/%v, want 100/200", data.AttributedTotal, data.UnattributedTotal)
	}
	if len(data.ResourceGroupSubtotals) != 2 {
		t.Fatalf("len(ResourceGroupSubtotals) = %d, want 2", len(data.ResourceGroupSubtotals))
	}
	// Groups sorted by name: "rg-cluster" before "rg-node".
	if data.ResourceGroupSubtotals[0].ResourceGroup != "rg-cluster" || data.ResourceGroupSubtotals[1].ResourceGroup != "rg-node" {
		t.Fatalf("ResourceGroupSubtotals order = %+v", data.ResourceGroupSubtotals)
	}
	nodeSubtotal := data.ResourceGroupSubtotals[1]
	if nodeSubtotal.Total != 200 || nodeSubtotal.Count != 2 {
		t.Errorf("rg-node subtotal = %v (%d rows), want 200 (2 rows)", nodeSubtotal.Total, nodeSubtotal.Count)
	}
	clusterSubtotal := data.ResourceGroupSubtotals[0]
	if clusterSubtotal.Total != 100 || clusterSubtotal.Count != 1 {
		t.Errorf("rg-cluster subtotal = %+v, want {Total:100 Count:1}", clusterSubtotal)
	}

	if len(data.ResourceRows) != 3 {
		t.Fatalf("len(ResourceRows) = %d, want 3 (fits on one page)", len(data.ResourceRows))
	}
	// Flat row list sorted by (resource group, then the full resource ID
	// — never exposed on billingResourceRow itself): "rg-cluster" before
	// "rg-node", "disk-a" before "disk-z" within rg-node.
	wantOrder := []string{clusterSubtotal.ResourceGroup, "rg-node", "rg-node"}
	for i, want := range wantOrder {
		if data.ResourceRows[i].ResourceGroup != want {
			t.Fatalf("ResourceRows[%d].ResourceGroup = %q, want %q (order = %+v)", i, data.ResourceRows[i].ResourceGroup, want, data.ResourceRows)
		}
	}
	if data.ResourceRows[1].ResourceName != "disk-a" || data.ResourceRows[2].ResourceName != "disk-z" {
		t.Fatalf("rg-node rows order = %+v", data.ResourceRows[1:])
	}
	if data.ResourceRows[1].ResourceType != "Microsoft.Compute/disks" {
		t.Errorf("ResourceType = %q, want Microsoft.Compute/disks", data.ResourceRows[1].ResourceType)
	}
	if !data.ResourceRows[0].Attributed {
		t.Errorf("the exact-match row should be Attributed: %+v", data.ResourceRows[0])
	}
}

func TestBuildBillingPageDataPaginatesResourceRowsAndValidatesRequestedParams(t *testing.T) {
	lines := make([]billing.ResourceCost, 0, 120)
	for i := 0; i < 120; i++ {
		lines = append(lines, billing.ResourceCost{ResourceID: testARMResourceID("rg-node", fmt.Sprintf("disk-%03d", i)), ResourceGroup: "rg-node", Cost: 1})
	}
	snap := billing.Snapshot{Status: billing.StatusAvailable, Total: 120, Currency: "USD", Lines: lines}

	// Default (no page/size requested): 50 rows, page 1 of 3.
	data := buildBillingPageData(snap, true, "", 0, 0)
	if len(data.ResourceRows) != 50 {
		t.Fatalf("default page len(ResourceRows) = %d, want 50", len(data.ResourceRows))
	}
	if data.Pagination.Page != 1 || data.Pagination.PageSize != 50 || data.Pagination.TotalPages != 3 || data.Pagination.TotalRows != 120 {
		t.Fatalf("Pagination = %+v, want {Page:1 PageSize:50 TotalPages:3 TotalRows:120}", data.Pagination)
	}
	if data.Pagination.HasPrev || !data.Pagination.HasNext {
		t.Errorf("Pagination = %+v, want HasPrev=false HasNext=true on page 1", data.Pagination)
	}
	// Subtotal must reflect the complete dataset (120), not the 50-row page.
	if len(data.ResourceGroupSubtotals) != 1 || data.ResourceGroupSubtotals[0].Count != 120 {
		t.Fatalf("ResourceGroupSubtotals = %+v, want a single group with Count 120 regardless of pagination", data.ResourceGroupSubtotals)
	}

	// An out-of-range page number is clamped to the last valid page, not
	// rejected or left to panic on a negative slice index.
	data = buildBillingPageData(snap, true, "", 999, 0)
	if data.Pagination.Page != 3 {
		t.Errorf("Page = %d, want 3 (clamped to TotalPages)", data.Pagination.Page)
	}
	if len(data.ResourceRows) != 20 {
		t.Errorf("last page len(ResourceRows) = %d, want 20 (120 - 2*50)", len(data.ResourceRows))
	}
	if !data.Pagination.HasPrev || data.Pagination.HasNext {
		t.Errorf("Pagination = %+v, want HasPrev=true HasNext=false on the last page", data.Pagination)
	}

	// A zero/negative page number falls back to page 1, not an error.
	data = buildBillingPageData(snap, true, "", -5, 0)
	if data.Pagination.Page != 1 {
		t.Errorf("Page = %d, want 1 (negative request clamped to 1)", data.Pagination.Page)
	}

	// An oversized page size is bounded to maxBillingPageSize, not honored
	// verbatim — a huge page size must never make one render dump the
	// entire cached dataset back onto the page.
	data = buildBillingPageData(snap, true, "", 1, 100000)
	if data.Pagination.PageSize != maxBillingPageSize {
		t.Errorf("PageSize = %d, want %d (bounded)", data.Pagination.PageSize, maxBillingPageSize)
	}
	// The bounded page size (200) still exceeds the 120 available rows, so
	// the returned page is every row, not a page-size-shaped slice.
	if len(data.ResourceRows) != 120 {
		t.Errorf("len(ResourceRows) = %d, want 120 (all available rows)", len(data.ResourceRows))
	}
}

func TestBuildBillingPageDataRetailCollapsedState(t *testing.T) {
	tests := []struct {
		name       string
		configured bool
		status     billing.Status
		wantOpen   bool
	}{
		{"not configured", false, "", true},
		{"available", true, billing.StatusAvailable, false},
		{"stale", true, billing.StatusStale, false},
		{"no data", true, billing.StatusNoData, false},
		{"unavailable, no prior success", true, billing.StatusUnavailable, true},
		{"disabled (before first refresh)", true, billing.StatusDisabled, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildBillingPageData(billing.Snapshot{Status: tt.status}, tt.configured, "", 0, 0)
			gotOpen := !data.RetailCollapsed
			if gotOpen != tt.wantOpen {
				t.Errorf("RetailCollapsed = %v (open=%v), want open=%v", data.RetailCollapsed, gotOpen, tt.wantOpen)
			}
		})
	}
}

// TestBillingPageURLAlwaysIncludesBillingPageEvenForPageOne proves
// billingPage=1 is never omitted. handleDashboard's cache-only pagination
// branch (server.go) is only entered when billingPage or billingPageSize
// is present on the request; if the page-1 link omitted billingPage, a
// "back to page 1" link (e.g. Previous from page 2) would fall through
// into the default non-paginated request path instead, which — unlike
// the cache-only branch — calls state.refresh() when this cluster's page
// cache happens to be empty.
func TestBillingPageURLAlwaysIncludesBillingPageEvenForPageOne(t *testing.T) {
	_, pagination := paginateBillingRows(nil, 1, 0)
	if url := billingPageURL("", 1, pagination); url != "/costs?billingPage=1" {
		t.Errorf("billingPageURL(no cluster, page 1) = %q, want /costs?billingPage=1", url)
	}
	if url := billingPageURL("prod", 1, pagination); url != "/costs?billingPage=1&cluster=prod" {
		t.Errorf("billingPageURL(cluster, page 1) = %q, want /costs?billingPage=1&cluster=prod", url)
	}
	if url := billingPageURL("prod", 2, pagination); url != "/costs?billingPage=2&cluster=prod" {
		t.Errorf("billingPageURL(cluster, page 2) = %q, want /costs?billingPage=2&cluster=prod", url)
	}
}

func TestBuildBillingPageDataStaleRetainsPriorTotal(t *testing.T) {
	snap := billing.Snapshot{Status: billing.StatusStale, Stale: true, Total: 100, Currency: "USD", UnavailableReason: "throttled"}
	data := buildBillingPageData(snap, true, "", 0, 0)
	if !data.Stale || data.Total != 100 {
		t.Errorf("data = %+v", data)
	}
	if data.UnavailableReason == "" {
		t.Error("expected UnavailableReason to be surfaced for a stale snapshot")
	}
}

func TestBuildBillingPageDataDistinguishesLastAttemptFromLastSuccess(t *testing.T) {
	lastSuccess := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)
	lastAttempt := time.Date(2026, 9, 17, 6, 0, 0, 0, time.UTC)
	snap := billing.Snapshot{
		Status: billing.StatusStale, Stale: true, Total: 100, Currency: "USD",
		UnavailableReason: "throttled", RetrievedAt: lastSuccess, LastAttemptedAt: lastAttempt,
	}
	data := buildBillingPageData(snap, true, "", 0, 0)
	if !data.LastSuccess.Equal(lastSuccess) {
		t.Errorf("LastSuccess = %v, want %v", data.LastSuccess, lastSuccess)
	}
	if !data.LastAttempt.Equal(lastAttempt) {
		t.Errorf("LastAttempt = %v, want %v", data.LastAttempt, lastAttempt)
	}
}

func TestBuildBillingPageDataUnavailableNeverReportsZeroAsCost(t *testing.T) {
	snap := billing.Snapshot{Status: billing.StatusUnavailable, UnavailableReason: "authentication failed"}
	data := buildBillingPageData(snap, true, "", 0, 0)
	if data.Total != 0 {
		t.Errorf("Total = %v", data.Total)
	}
	// Total is legitimately the zero value here, but StatusLabel must make
	// clear this is "unavailable," not a real $0 result — the cost.html
	// template branches on Status, never on Total, to decide what to show.
	if data.StatusLabel != "Unavailable" {
		t.Errorf("StatusLabel = %q, want Unavailable", data.StatusLabel)
	}
}

func TestRenderCostPageShowsBillingWhenConfiguredAndAvailable(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{
		Timestamp: time.Now(), ClusterName: "rxr-rxp-e2e-01-cus-aks", Provider: "azure", Region: "centralus",
		Currency: "USD",
	}}
	snap := billing.Snapshot{
		Status: billing.StatusAvailable, Total: 7345.12, Currency: "USD",
		CostBasis:   billing.CostBasisActualCost,
		PeriodStart: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC),
		Source:      "Azure Cost Management API (Query - Usage, resource-group scope)",
		Coverage:    "Resource-level Azure billing for resource groups X and Y",
	}
	html := renderCostPage(scan, "", []string{""}, snap, true, 0, 0)
	if !strings.Contains(html, "Azure billing") {
		t.Fatal("billing section not rendered")
	}
	if !strings.Contains(html, "2026-08-17 to 2026-09-15") {
		t.Error("exact billing period dates not rendered")
	}
	if !strings.Contains(html, "Current retail node run-rate (estimate)") {
		t.Error("estimate hero was not relabeled")
	}
	if strings.Contains(html, "Estimate-only mode") {
		t.Error("estimate-only disclosure must not render when billing is configured")
	}
}

func TestRenderCostPageEstimateOnlyModeWhenNotConfigured(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{Timestamp: time.Now(), ClusterName: "aks", Currency: "USD"}}
	html := renderCostPage(scan, "", []string{""}, billing.Snapshot{}, false, 0, 0)
	if !strings.Contains(html, "Estimate-only mode") {
		t.Error("expected the estimate-only disclosure when billing is not configured")
	}
	if strings.Contains(html, `<div class="hero-kicker">Azure billing`) {
		t.Error("billing hero must not render when not configured")
	}
	if !strings.Contains(html, `<span class="estimate-badge">ESTIMATED</span>`) {
		t.Error("the page-wide ESTIMATED badge should still show when billing is not configured")
	}
}

func TestRenderCostPageShowsAttributionSplitAndHidesEstimateBadgeWhenBillingAvailable(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{
		Timestamp: time.Now(), ClusterName: "rxr-rxp-e2e-01-cus-aks", Provider: "azure", Region: "centralus",
		Currency: "USD",
	}}
	clusterResourceID := testARMClusterResourceID("rg-cluster", "aks")
	nodeResourceID := testARMResourceID("rg-node", "disk-user")
	snap := billing.Snapshot{
		Status: billing.StatusAvailable, Total: 5000, Currency: "USD",
		CostBasis:         billing.CostBasisActualCost,
		PeriodStart:       time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		PeriodEnd:         time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC),
		Source:            "Azure Cost Management API (Query - Usage, resource-group scope)",
		Coverage:          "Resource-level Azure billing for resource groups X and Y",
		ClusterResourceID: clusterResourceID,
		AttributedTotal:   200,
		UnattributedTotal: 4800,
		Lines: []billing.ResourceCost{
			{ResourceID: clusterResourceID, ResourceGroup: "rg-cluster", Cost: 200, Currency: "USD", Attributed: true},
			{ResourceID: nodeResourceID, ResourceGroup: "rg-node", Cost: 4800, Currency: "USD"},
		},
	}
	html := renderCostPage(scan, "", []string{""}, snap, true, 0, 0)

	if strings.Contains(html, `<span class="estimate-badge">ESTIMATED</span>`) {
		t.Error("page-wide ESTIMATED badge must not render once real Azure billing is displayed")
	}
	if strings.Contains(html, "public/list pricing only") {
		t.Error("page-wide public/list-pricing-only wording must not render once real Azure billing is displayed")
	}
	if !strings.Contains(html, "Cluster-attributed") || !strings.Contains(html, "Unattributed") {
		t.Error("attribution split (cluster-attributed vs unattributed) not rendered")
	}
	if !strings.Contains(html, "AKS control-plane charge") {
		t.Error("the exact-match figure should be labeled as the control-plane charge, not implied to be the cluster's cost")
	}
	if !strings.Contains(html, "AKS cluster actual cost") || !strings.Contains(html, "Unavailable") {
		t.Error("the AKS cluster's actual cost should be explicitly shown as unavailable")
	}
	if !strings.Contains(html, "Scope total (reconciliation)") {
		t.Error("the reconciliation total should be labeled as a scope total, not implied to be the AKS cluster's cost")
	}
	if strings.Contains(html, "Two-resource-group total") {
		t.Error("the old \"two-resource-group total\" wording must not remain")
	}
	if strings.Contains(html, clusterResourceID) || strings.Contains(html, nodeResourceID) {
		t.Error("full Azure resource IDs must never appear in rendered billing HTML")
	}
	if strings.Contains(html, testSubscriptionID) || strings.Contains(html, "/subscriptions/") {
		t.Error("the subscription ID (or the literal \"/subscriptions/\" prefix) must never appear in rendered billing HTML")
	}
	if !strings.Contains(html, "Microsoft.ContainerService/managedClusters") || !strings.Contains(html, "Microsoft.Compute/disks") {
		t.Error("sanitized resource types should still be shown in place of the full resource ID")
	}
	if !strings.Contains(html, "rg-cluster") || !strings.Contains(html, "rg-node") {
		t.Error("resource-group subtotals not rendered")
	}
	if !strings.Contains(html, "Retail pricing estimate, allocation &amp; idle cost") {
		t.Error("retail/allocation/idle content must remain in its own clearly labeled section")
	}
}

// largeResourceLineSnapshot returns a Snapshot with n cached lines spread
// across two resource groups, sorted so callers can assert on ordering.
// Each line's ResourceID is a syntactically valid, fully-qualified Azure
// resource ID (built from testSubscriptionID) so sanitizeResourceID
// produces distinguishable, index-numbered resource names.
func largeResourceLineSnapshot(n int) billing.Snapshot {
	lines := make([]billing.ResourceCost, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, billing.ResourceCost{ResourceID: testARMResourceID("rg-node", fmt.Sprintf("disk-%04d", i)), ResourceGroup: "rg-node", Cost: 1, Currency: "USD"})
	}
	return billing.Snapshot{Status: billing.StatusAvailable, Total: float64(n), Currency: "USD", Lines: lines}
}

// TestRenderCostPageActuallyPaginatesRowsInsteadOfHidingThemWithJS proves
// the rendered HTML itself contains only one page's worth of resource
// rows — not all rows client-side-hidden by CSS/JS the way the namespace
// filter above works. Only rows for the requested page should ever reach
// the response body. It also proves the full resource ID and subscription
// UUID never reach the response, on every page — only the sanitized
// resource type/name.
func TestRenderCostPageActuallyPaginatesRowsInsteadOfHidingThemWithJS(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{Timestamp: time.Now(), ClusterName: "aks", Currency: "USD"}}
	snap := largeResourceLineSnapshot(120)

	// Every line in this fixture sanitizes to the same resource type
	// ("Microsoft.Compute/disks"), so counting its Type cell is an exact
	// per-row count — unlike counting the generic .resource-cell class,
	// which now appears twice per row (type and name).
	const rowCellMarker = `<td class="resource-cell">Microsoft.Compute/disks</td>`

	page1 := renderCostPage(scan, "", []string{""}, snap, true, 0, 0)
	if got := strings.Count(page1, rowCellMarker); got != 50 {
		t.Fatalf("page 1: rendered %d resource rows, want 50 (only the current page, not all 120)", got)
	}
	if !strings.Contains(page1, "disk-0000") || strings.Contains(page1, "disk-0119") {
		t.Error("page 1 should contain the first row and not the last row")
	}
	if !strings.Contains(page1, "Page 1 of 3") {
		t.Error("pagination status text (\"Page 1 of 3\") not rendered")
	}
	if !strings.Contains(page1, `href="/costs?billingPage=2"`) {
		t.Error("a plain, working Next link to page 2 was not rendered")
	}
	if strings.Contains(page1, testSubscriptionID) || strings.Contains(page1, "/subscriptions/") {
		t.Error("page 1 must never render the full resource ID or subscription UUID")
	}

	page2 := renderCostPage(scan, "", []string{""}, snap, true, 2, 0)
	if got := strings.Count(page2, rowCellMarker); got != 50 {
		t.Fatalf("page 2: rendered %d resource rows, want 50", got)
	}
	if strings.Contains(page2, "disk-0000") || !strings.Contains(page2, "disk-0050") {
		t.Error("page 2 should not contain page 1's rows and should contain its own")
	}
	if strings.Contains(page2, testSubscriptionID) || strings.Contains(page2, "/subscriptions/") {
		t.Error("page 2 must never render the full resource ID or subscription UUID")
	}

	page3 := renderCostPage(scan, "", []string{""}, snap, true, 3, 0)
	if got := strings.Count(page3, rowCellMarker); got != 20 {
		t.Fatalf("page 3 (last, partial): rendered %d resource rows, want 20 (120 - 2*50)", got)
	}
	if !strings.Contains(page3, "disk-0119") {
		t.Error("the last page should contain the final row")
	}
	if strings.Contains(page3, testSubscriptionID) || strings.Contains(page3, "/subscriptions/") {
		t.Error("page 3 must never render the full resource ID or subscription UUID")
	}
}

func TestRenderCostPagePaginationLinksPreserveSelectedCluster(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{Timestamp: time.Now(), ClusterName: "aks", Currency: "USD"}}
	snap := largeResourceLineSnapshot(120)

	page1 := renderCostPage(scan, "prod-eastus", []string{"prod-eastus"}, snap, true, 1, 0)
	if !strings.Contains(page1, "cluster=prod-eastus") {
		t.Error("page 1's Next link should preserve the selected cluster")
	}
	if !strings.Contains(page1, "billingPage=2") {
		t.Error("page 1's Next link should point at page 2")
	}

	page2 := renderCostPage(scan, "prod-eastus", []string{"prod-eastus"}, snap, true, 2, 0)
	if !strings.Contains(page2, "cluster=prod-eastus") {
		t.Error("page 2's Previous/Next links should preserve the selected cluster")
	}
	if !strings.Contains(page2, `href="/costs?billingPage=1&amp;cluster=prod-eastus"`) {
		t.Error("page 2's Previous link should explicitly include billingPage=1 (not omit it) and keep the cluster")
	}
}

func TestRenderCostPageAttributionGridUsesResponsiveCSSNotInlineColumnOverride(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{Timestamp: time.Now(), ClusterName: "aks", Currency: "USD"}}
	html := renderCostPage(scan, "", []string{""}, testAttributionSnapshot(), true, 0, 0)
	if strings.Contains(html, `style="grid-template-columns:repeat(3,minmax(0,1fr))"`) {
		t.Error("attribution grid must use a responsive CSS class, not a fixed 3-column inline style")
	}
	if !strings.Contains(html, `class="attribution-grid"`) {
		t.Error("attribution grid should use the responsive .attribution-grid class")
	}
	if !strings.Contains(html, "resource-cell") {
		t.Error("resource type/name cells should carry the wrapping class so long values don't clip")
	}
}

func TestRenderCostPageKeepsRetailCollapsedWhenBillingAvailableAndExpandsWhenUnavailable(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{Timestamp: time.Now(), ClusterName: "aks", Currency: "USD"}}

	available := billing.Snapshot{Status: billing.StatusAvailable, Total: 100, Currency: "USD"}
	html := renderCostPage(scan, "", []string{""}, available, true, 0, 0)
	if !strings.Contains(html, `<details class="methodology" id="retail-estimate-section">`) {
		t.Error("retail section should render collapsed (no open attribute) when billing is available")
	}

	unavailable := billing.Snapshot{Status: billing.StatusUnavailable, UnavailableReason: "auth failed"}
	html = renderCostPage(scan, "", []string{""}, unavailable, true, 0, 0)
	if !strings.Contains(html, `<details class="methodology" id="retail-estimate-section" open>`) {
		t.Error("retail section should render expanded (open) when billing is unavailable with no prior success")
	}
	if strings.Contains(html, "collapsed") {
		t.Error(`unavailable-state wording must not call the (actually expanded) retail section "collapsed"`)
	}

	notConfigured := renderCostPage(scan, "", []string{""}, billing.Snapshot{}, false, 0, 0)
	if !strings.Contains(notConfigured, `<details class="methodology" id="retail-estimate-section" open>`) {
		t.Error("retail section should render expanded (open) when billing is not configured")
	}
	if strings.Contains(notConfigured, "collapsed") {
		t.Error(`not-configured wording must not call the (actually expanded) retail section "collapsed"`)
	}
}

func TestFormatMoneyHandlesNegativeCreditsWithoutMisplacedComma(t *testing.T) {
	tests := []struct {
		amount float64
		want   string
	}{
		{0, "0"},
		{999, "999"},
		{1234, "1,234"},
		{-999, "-999"}, // the bug: previously rendered "-,999"
		{-1234, "-1,234"},
		{-100000, "-100,000"}, // digit count a multiple of 3: previously "-1,00,000"-shaped corruption
		{-1000000, "-1,000,000"},
		{-12.34, "-12"},
	}
	for _, tt := range tests {
		if got := formatMoney(tt.amount); got != tt.want {
			t.Errorf("formatMoney(%v) = %q, want %q", tt.amount, got, tt.want)
		}
	}
}

func TestFormatBillingMoneyPreservesCents(t *testing.T) {
	tests := []struct {
		amount float64
		want   string
	}{
		{19.99, "19.99"},
		{5308.01, "5,308.01"},
		{-12.34, "-12.34"},
		{0, "0.00"},
		{-0.0, "0.00"},
		// Sub-cent rounding: rounds to the nearest cent, and an amount
		// that rounds to exactly zero cents is never shown as "-0.00".
		{0.004, "0.00"},
		{0.006, "0.01"},
		{-0.004, "0.00"},
		{-0.006, "-0.01"},
		// Thousands separators combine correctly with cents and with a
		// negative amount whose digit count is a multiple of 3 (the exact
		// shape that exposed formatMoney's sign-placement bug).
		{1234567.891, "1,234,567.89"},
		{-100000.5, "-100,000.50"},
	}
	for _, tt := range tests {
		if got := formatBillingMoney(tt.amount); got != tt.want {
			t.Errorf("formatBillingMoney(%v) = %q, want %q", tt.amount, got, tt.want)
		}
	}
}

// TestRenderCostPageBillingFiguresShowCentsRetailStaysWholeDollar proves
// the cents-preserving formatter reaches the rendered page for billing
// figures specifically (resource rows, resource-group subtotals,
// attributed/unattributed, and the reconciliation total), while retail
// estimate figures elsewhere on the same page keep formatMoney's
// whole-dollar rounding unchanged.
func TestRenderCostPageBillingFiguresShowCentsRetailStaysWholeDollar(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{
		Timestamp: time.Now(), ClusterName: "aks", Provider: "azure", Region: "eastus2", Currency: "USD",
		TotalMonthlyCost: 1234.56, // a retail figure — must render as $1,235, not $1,234.56
		NodePoolCosts:    []models.NodePoolCost{{Name: "system", Provider: "azure", Region: "eastus2", NodeCount: 1, PricingAvailable: true, PricePerNodeMonth: 1234.56, TotalMonthly: 1234.56}},
	}}
	clusterResourceID := "/subscriptions/s/resourceGroups/rg-cluster/providers/Microsoft.ContainerService/managedClusters/aks"
	snap := billing.Snapshot{
		Status: billing.StatusAvailable, Total: 5295.67, Currency: "USD",
		ClusterResourceID: clusterResourceID,
		AttributedTotal:   19.99,
		UnattributedTotal: 5275.68,
		Lines: []billing.ResourceCost{
			{ResourceID: clusterResourceID, ResourceGroup: "rg-cluster", Cost: 19.99, Currency: "USD", Attributed: true},
			{ResourceID: "/r/credit", ResourceGroup: "rg-node", Cost: -12.34, Currency: "USD"},
			{ResourceID: "/r/vmss", ResourceGroup: "rg-node", Cost: 5288.02, Currency: "USD"},
		},
	}
	html := renderCostPage(scan, "", []string{""}, snap, true, 0, 0)

	for _, want := range []string{
		"19.99",    // attributed total and its resource row
		"5,275.68", // unattributed total
		"5,295.67", // reconciliation total (and the top hero, same value)
		"-12.34",   // negative credit resource row, cents preserved
		"5,288.02", // the other resource row
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered billing figures missing %q", want)
		}
	}
	if strings.Contains(html, "5,296") || strings.Contains(html, "$5296") {
		t.Error("billing total appears rounded to whole dollars — cents were dropped")
	}
	if !strings.Contains(html, "$1,235") {
		t.Error("retail run-rate figure should still be whole-dollar rounded (unchanged formatMoney behavior)")
	}
	if strings.Contains(html, "1,234.56") {
		t.Error("retail run-rate figure should not show cents — only billing figures use the cents formatter")
	}
}
