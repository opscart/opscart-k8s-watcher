package main

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/billing"
)

// defaultBillingPageSize is how many cached resource-level billing rows a
// page shows when no explicit page size was requested.
const defaultBillingPageSize = 50

// maxBillingPageSize bounds how many rows a single requested page size can
// ask for, so an out-of-range request (accidental or otherwise) can never
// make one render dump the entire cached dataset back onto the page.
const maxBillingPageSize = 200

// billingPageData is the Cost page's Azure billing view, built once per
// render directly from a billing.Snapshot already cached by that cluster's
// billing.Runtime (billing_runtime.go) — this function never triggers a
// network call itself, satisfying "no billing API calls during page
// rendering." Configured is false whenever the cluster has no billing
// configuration at all, in which case the Cost page renders exactly as it
// did before this feature existed.
type billingPageData struct {
	Configured  bool
	Status      string
	StatusLabel string

	Total          float64
	Currency       string
	CostBasisLabel string
	PeriodLabel    string
	Source         string
	Scope          string
	Coverage       string
	Disclosures    []string

	// ClusterResourceID, AttributedTotal, and UnattributedTotal describe
	// the increment-one attribution split: AttributedTotal is billed cost
	// on the single resource whose ID exactly matches ClusterResourceID;
	// UnattributedTotal is every other resource billed in the same two
	// resource groups, whose cluster ownership has not been verified.
	// AttributedTotal + UnattributedTotal reconciles to Total by
	// construction (see billing.Result.UnattributedTotal) — not bit-exact
	// float64 equality.
	ClusterResourceID string
	AttributedTotal   float64
	UnattributedTotal float64
	// ResourceGroupSubtotals is each queried resource group's subtotal and
	// row count computed from the COMPLETE cached dataset (snapshot.Lines),
	// independent of ResourceRows' pagination below. Sorted by resource
	// group name for a deterministic render.
	ResourceGroupSubtotals []billingResourceGroupSubtotal
	// ResourceRows is only the current page's rows from the complete,
	// deterministically ordered (resource group, then resource ID) row
	// list — see Pagination. Built entirely from the already-cached
	// Snapshot; selecting a page never triggers a network call.
	ResourceRows []billingResourceRow
	Pagination   billingPagination
	// RetailCollapsed is true when the page's retail-estimate/allocation/
	// idle-cost section should render collapsed by default: successful or
	// stale billing exists to show instead. It is false (expanded) when
	// billing is unconfigured, or configured but has never once
	// succeeded, since the retail estimate is then the only figure
	// available.
	RetailCollapsed bool

	Stale             bool
	UnavailableReason string
	// LastSuccess is when the last SUCCESSFUL refresh completed — zero if
	// billing has never once succeeded. LastAttempt is when the most
	// recent refresh attempt finished, success or failure, so a viewer can
	// tell "billing has been failing for days" from "billing just
	// refreshed" instead of seeing only one ambiguous timestamp.
	LastSuccess time.Time
	LastAttempt time.Time
}

// billingResourceRow is one cached resource-level billing line, as shown on
// one page of the paginated resource-rows table.
type billingResourceRow struct {
	ResourceID    string
	ResourceGroup string
	Cost          float64
	Attributed    bool
}

// billingResourceGroupSubtotal is one queried resource group's subtotal
// across the COMPLETE cached dataset — never just the current page.
type billingResourceGroupSubtotal struct {
	ResourceGroup string
	Total         float64
	Count         int
}

// billingPagination describes one page of the flat, deterministically
// ordered resource-row list. Page and PageSize always fall within valid
// bounds ([1, TotalPages] and [1, maxBillingPageSize] respectively)
// regardless of what was requested — see paginateBillingRows.
type billingPagination struct {
	Page       int
	PageSize   int
	TotalRows  int
	TotalPages int
	HasPrev    bool
	HasNext    bool
	// PrevURL/NextURL are empty when HasPrev/HasNext is false. They
	// already carry the active cluster's ?cluster= query parameter (see
	// billingPageURL), so the template can render them as plain links with
	// no JavaScript involved.
	PrevURL string
	NextURL string
}

// buildBillingPageData builds the Cost page's Azure billing view. It reads
// only the already-cached Snapshot — this function, and everything it
// calls, must never make an Azure or Kubernetes call, including when
// selecting a resource-rows page. requestedPage/requestedPageSize are the
// raw (unvalidated) values from the request, or 0 to mean "use the
// default" — see paginateBillingRows.
func buildBillingPageData(snapshot billing.Snapshot, configured bool, activeCtx string, requestedPage, requestedPageSize int) billingPageData {
	data := billingPageData{Configured: configured, Status: string(snapshot.Status), StatusLabel: billingStatusLabel(snapshot.Status)}
	if !configured {
		return data
	}
	data.Stale = snapshot.Stale
	data.UnavailableReason = snapshot.UnavailableReason
	data.LastSuccess = snapshot.RetrievedAt
	data.LastAttempt = snapshot.LastAttemptedAt

	switch snapshot.Status {
	case billing.StatusAvailable, billing.StatusStale, billing.StatusNoData:
		data.Total = snapshot.Total
		data.Currency = snapshot.Currency
		data.CostBasisLabel = billingCostBasisLabel(snapshot.CostBasis)
		data.PeriodLabel = billingPeriodLabel(snapshot.PeriodStart, snapshot.PeriodEnd)
		data.Source = snapshot.Source
		data.Scope = snapshot.Scope
		data.Coverage = snapshot.Coverage
		data.Disclosures = snapshot.Disclosures
		data.ClusterResourceID = snapshot.ClusterResourceID
		data.AttributedTotal = snapshot.AttributedTotal
		data.UnattributedTotal = snapshot.UnattributedTotal
		// Computed from the complete dataset (snapshot.Lines), not from
		// ResourceRows below — subtotals must never change depending on
		// which page is being viewed.
		data.ResourceGroupSubtotals = buildBillingResourceGroupSubtotals(snapshot.Lines)
		allRows := buildBillingResourceRows(snapshot.Lines)
		data.ResourceRows, data.Pagination = paginateBillingRows(allRows, requestedPage, requestedPageSize)
		data.Pagination.PrevURL = billingPageURL(activeCtx, data.Pagination.Page-1, data.Pagination)
		data.Pagination.NextURL = billingPageURL(activeCtx, data.Pagination.Page+1, data.Pagination)
		if !data.Pagination.HasPrev {
			data.Pagination.PrevURL = ""
		}
		if !data.Pagination.HasNext {
			data.Pagination.NextURL = ""
		}
		// Successful or stale billing exists to show — collapse the
		// retail-estimate/allocation/idle-cost section by default.
		data.RetailCollapsed = true
	}
	return data
}

// buildBillingResourceGroupSubtotals groups the cached resource-level
// billing rows by resource group and computes each group's subtotal and
// row count, over the complete dataset. Sorted by resource group name for
// a deterministic render, independent of whatever order the Cost
// Management API happened to return rows in.
func buildBillingResourceGroupSubtotals(lines []billing.ResourceCost) []billingResourceGroupSubtotal {
	byGroup := make(map[string]*billingResourceGroupSubtotal)
	var order []string
	for _, l := range lines {
		g, ok := byGroup[l.ResourceGroup]
		if !ok {
			g = &billingResourceGroupSubtotal{ResourceGroup: l.ResourceGroup}
			byGroup[l.ResourceGroup] = g
			order = append(order, l.ResourceGroup)
		}
		g.Total += l.Cost
		g.Count++
	}
	sort.Strings(order)

	subtotals := make([]billingResourceGroupSubtotal, 0, len(order))
	for _, name := range order {
		subtotals = append(subtotals, *byGroup[name])
	}
	return subtotals
}

// buildBillingResourceRows flattens the cached lines into a single,
// deterministically ordered list (by resource group, then resource ID),
// independent of whatever order the Cost Management API happened to
// return rows in. Pagination is applied afterward by paginateBillingRows.
func buildBillingResourceRows(lines []billing.ResourceCost) []billingResourceRow {
	rows := make([]billingResourceRow, len(lines))
	for i, l := range lines {
		rows[i] = billingResourceRow{ResourceID: l.ResourceID, ResourceGroup: l.ResourceGroup, Cost: l.Cost, Attributed: l.Attributed}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ResourceGroup != rows[j].ResourceGroup {
			return rows[i].ResourceGroup < rows[j].ResourceGroup
		}
		return rows[i].ResourceID < rows[j].ResourceID
	})
	return rows
}

// paginateBillingRows selects one page out of all, validating and bounding
// both requested values rather than trusting them: requestedPageSize <= 0
// or > maxBillingPageSize falls back to defaultBillingPageSize (clamped to
// maxBillingPageSize), and requestedPage is clamped into [1, TotalPages].
// Totals in the returned billingPagination (TotalRows, TotalPages) are
// always computed from all, the complete dataset — never from the
// returned page slice.
func paginateBillingRows(all []billingResourceRow, requestedPage, requestedPageSize int) ([]billingResourceRow, billingPagination) {
	pageSize := requestedPageSize
	if pageSize <= 0 {
		pageSize = defaultBillingPageSize
	}
	if pageSize > maxBillingPageSize {
		pageSize = maxBillingPageSize
	}

	total := len(all)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	page := requestedPage
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	pageRows := append([]billingResourceRow(nil), all[start:end]...)
	return pageRows, billingPagination{
		Page: page, PageSize: pageSize, TotalRows: total, TotalPages: totalPages,
		HasPrev: page > 1, HasNext: page < totalPages,
	}
}

// billingPageURL builds a /costs link that selects a resource-rows page,
// preserving the active cluster's ?cluster= query parameter. billingPage
// is always included, even for page 1: handleDashboard's cache-only
// pagination branch (server.go) is only entered when billingPage or
// billingPageSize is present on the request. Omitting it for page 1 would
// make a "back to page 1" link (e.g. Previous from page 2) fall through
// into the default non-paginated request path instead — which, unlike
// this one, calls state.refresh() when this cluster's page cache happens
// to be empty. Always including billingPage keeps every pagination link,
// including the one that lands back on page 1, on the strictly
// cache-only path.
func billingPageURL(activeCtx string, page int, pagination billingPagination) string {
	v := url.Values{}
	if activeCtx != "" {
		v.Set("cluster", activeCtx)
	}
	v.Set("billingPage", strconv.Itoa(page))
	if pagination.PageSize != 0 && pagination.PageSize != defaultBillingPageSize {
		v.Set("billingPageSize", strconv.Itoa(pagination.PageSize))
	}
	return "/costs?" + v.Encode()
}

func billingStatusLabel(status billing.Status) string {
	switch status {
	case billing.StatusAvailable:
		return "Live"
	case billing.StatusStale:
		return "Stale"
	case billing.StatusNoData:
		return "No billed usage"
	case billing.StatusUnavailable:
		return "Unavailable"
	default:
		return "Disabled"
	}
}

func billingCostBasisLabel(basis billing.CostBasis) string {
	if basis == billing.CostBasisAmortizedCost {
		return "Amortized cost"
	}
	return "Actual cost"
}

// billingPeriodLabel formats the exact queried dates, never as a "/month"
// figure — a partial-period billing total must never be presented as a
// monthly run rate.
func billingPeriodLabel(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return ""
	}
	return start.Format("2006-01-02") + " to " + end.Format("2006-01-02")
}

// formatBillingMoney formats a float64 as a comma-grouped decimal string
// with exactly two decimal places (cents preserved), for actual Azure
// billing figures only — resource rows, resource-group subtotals, the
// attributed/unattributed split, and the reconciliation total. Retail/
// estimate figures elsewhere keep formatMoney's whole-dollar rounding
// unchanged; the two must never be conflated onto one formatter, since
// Azure bills in fractional currency units and rounding those to whole
// dollars would silently drop real cents from a reconciliation figure.
//
// The sign is stripped before grouping and reattached after, for the same
// reason formatMoney does: grouping the full (possibly signed) string
// shifts the comma-placement math by one digit for negative amounts. The
// amount is rounded to cents before the zero check, so a sub-cent negative
// amount (e.g. -0.001) that rounds to zero cents is displayed as "0.00",
// never "-0.00".
func formatBillingMoney(amount float64) string {
	sign := ""
	if amount < 0 {
		sign = "-"
		amount = -amount
	}
	cents := math.Round(amount * 100)
	if cents == 0 {
		sign = ""
	}
	whole := int64(cents) / 100
	frac := int64(cents) % 100

	digits := strconv.FormatInt(whole, 10)
	var grouped strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(c)
	}
	return fmt.Sprintf("%s%s.%02d", sign, grouped.String(), frac)
}
