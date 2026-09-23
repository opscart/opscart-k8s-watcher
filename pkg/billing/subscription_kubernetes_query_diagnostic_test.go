package billing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// parseSubscriptionKubernetesSpikeDate parses value as a strict YYYY-MM-DD
// calendar date — no other format (RFC3339, slashes, a bare year, etc.)
// is accepted.
func parseSubscriptionKubernetesSpikeDate(value string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: use YYYY-MM-DD", value)
	}
	return t.UTC(), nil
}

// resolveSubscriptionKubernetesSpikePeriod converts the manual diagnostic's
// strict YYYY-MM-DD OPSCART_BILLING_SPIKE_FROM/OPSCART_BILLING_SPIKE_TO
// values into explicit UTC period boundaries: fromValue becomes
// 00:00:00.000 UTC of that day, toValue becomes 23:59:59.000 UTC of that
// day — the last instant of the inclusive end date, matching production's
// ResolvePeriod/endOfDay convention (config.go). The millisecond ".000" is
// produced later, when buildSubscriptionKubernetesQuery formats these
// values with subscriptionKubernetesQueryTimeFormat.
//
// This is a pure function: no credential, no HTTP client, no import of
// azcore or net/http. A missing, invalid, or reversed period is always
// rejected here — structurally before
// TestManualSubscriptionKubernetesQueryDiagnostic can ever call
// NewCredential or runSubscriptionKubernetesQuery — see
// TestResolveSubscriptionKubernetesSpikePeriodRejectsMissingOrReversedDates,
// which exercises exactly that and is not gated behind
// OPSCART_BILLING_SPIKE_MANUAL.
func resolveSubscriptionKubernetesSpikePeriod(fromValue, toValue string) (start, end time.Time, err error) {
	if strings.TrimSpace(fromValue) == "" || strings.TrimSpace(toValue) == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("OPSCART_BILLING_SPIKE_FROM and OPSCART_BILLING_SPIKE_TO are both required (YYYY-MM-DD)")
	}
	fromDate, err := parseSubscriptionKubernetesSpikeDate(fromValue)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("OPSCART_BILLING_SPIKE_FROM: %w", err)
	}
	toDate, err := parseSubscriptionKubernetesSpikeDate(toValue)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("OPSCART_BILLING_SPIKE_TO: %w", err)
	}
	start = time.Date(fromDate.Year(), fromDate.Month(), fromDate.Day(), 0, 0, 0, 0, time.UTC)
	end = time.Date(toDate.Year(), toDate.Month(), toDate.Day(), 23, 59, 59, 0, time.UTC)
	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("OPSCART_BILLING_SPIKE_TO %q must not be before OPSCART_BILLING_SPIKE_FROM %q", toValue, fromValue)
	}
	return start, end, nil
}

func TestResolveSubscriptionKubernetesSpikePeriodRejectsMissingOrReversedDates(t *testing.T) {
	tests := []struct {
		name, from, to string
	}{
		{"both missing", "", ""},
		{"from missing", "", "2026-09-15"},
		{"to missing", "2026-08-17", ""},
		{"reversed", "2026-09-15", "2026-08-17"},
		{"from invalid", "not-a-date", "2026-09-15"},
		{"to invalid", "2026-08-17", "not-a-date"},
		{"from wrong format", "08/17/2026", "2026-09-15"},
		{"to wrong format", "2026-08-17", "2026-09-15T00:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := resolveSubscriptionKubernetesSpikePeriod(tt.from, tt.to); err == nil {
				t.Errorf("resolveSubscriptionKubernetesSpikePeriod(%q, %q): expected error, got nil", tt.from, tt.to)
			}
		})
	}
}

func TestResolveSubscriptionKubernetesSpikePeriodInclusiveUTCConversion(t *testing.T) {
	start, end, err := resolveSubscriptionKubernetesSpikePeriod("2026-08-17", "2026-09-15")
	if err != nil {
		t.Fatalf("resolveSubscriptionKubernetesSpikePeriod: %v", err)
	}
	wantStart := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v (00:00:00.000 UTC of the FROM date)", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v (23:59:59.000 UTC of the TO date)", end, wantEnd)
	}
	if got := start.Format(subscriptionKubernetesQueryTimeFormat); got != "2026-08-17T00:00:00.000Z" {
		t.Errorf("start formatted = %q, want 2026-08-17T00:00:00.000Z", got)
	}
	if got := end.Format(subscriptionKubernetesQueryTimeFormat); got != "2026-09-15T23:59:59.000Z" {
		t.Errorf("end formatted = %q, want 2026-09-15T23:59:59.000Z", got)
	}
}

// parseSubscriptionKubernetesQueryFilterMode validates the manual
// diagnostic's required OPSCART_BILLING_SPIKE_FILTER_MODE value against
// exactly the three supported modes — case-sensitive, no default, no
// inference from any other input. Like resolveSubscriptionKubernetesSpikePeriod,
// this is a pure function (no credential, no HTTP client, no import of
// azcore or net/http — this file's own net/http/httptest import is used
// only by this file's synthetic-server tests below), so a missing or
// unknown mode is always rejected before
// TestManualSubscriptionKubernetesQueryDiagnostic can acquire a credential
// or make a request — see
// TestParseSubscriptionKubernetesQueryFilterModeRejectsMissingOrUnknown.
func parseSubscriptionKubernetesQueryFilterMode(value string) (subscriptionKubernetesQueryFilterMode, error) {
	switch subscriptionKubernetesQueryFilterMode(value) {
	case subscriptionKubernetesQueryFilterModeCapturedAnd, subscriptionKubernetesQueryFilterModeCombinedValues, subscriptionKubernetesQueryFilterModeDiscovery:
		return subscriptionKubernetesQueryFilterMode(value), nil
	default:
		return "", fmt.Errorf("OPSCART_BILLING_SPIKE_FILTER_MODE %q: use %q, %q, or %q", value, subscriptionKubernetesQueryFilterModeCapturedAnd, subscriptionKubernetesQueryFilterModeCombinedValues, subscriptionKubernetesQueryFilterModeDiscovery)
	}
}

func TestParseSubscriptionKubernetesQueryFilterModeRejectsMissingOrUnknown(t *testing.T) {
	for _, value := range []string{"", "unknown", "captured-or", "CAPTURED-AND", "combinedvalues", " "} {
		if _, err := parseSubscriptionKubernetesQueryFilterMode(value); err == nil {
			t.Errorf("parseSubscriptionKubernetesQueryFilterMode(%q): expected error, got nil", value)
		}
	}
}

func TestParseSubscriptionKubernetesQueryFilterModeAcceptsExactValues(t *testing.T) {
	for _, value := range []subscriptionKubernetesQueryFilterMode{
		subscriptionKubernetesQueryFilterModeCapturedAnd,
		subscriptionKubernetesQueryFilterModeCombinedValues,
		subscriptionKubernetesQueryFilterModeDiscovery,
	} {
		got, err := parseSubscriptionKubernetesQueryFilterMode(string(value))
		if err != nil {
			t.Errorf("parseSubscriptionKubernetesQueryFilterMode(%q): %v", value, err)
		}
		if got != value {
			t.Errorf("parseSubscriptionKubernetesQueryFilterMode(%q) = %q, want %q", value, got, value)
		}
	}
}

// TestManualSubscriptionKubernetesQueryDiagnostic is a manually invoked,
// opt-in diagnostic — it is NEVER run by `go test ./...` (including CI)
// and is skipped unless explicitly enabled. It exists to empirically check
// whether the request shapes this package's synthetic spike tests
// validate (subscription_kubernetes_query_spike_test.go) are actually
// accepted — and matched by — the real Azure Cost Management API, using a
// real subscription an operator has access to.
//
// A prior live run found: authentication, authorization, endpoint,
// period, and response decoding all work, but both the captured-and and
// combined-values Cluster filters matched zero rows. This diagnostic
// requires an explicit OPSCART_BILLING_SPIKE_FILTER_MODE so an operator
// can compare captured-and, combined-values, and — to determine whether
// Azure exposes Cluster dimension rows here at all, and whether their
// shape matches the configured AKS ARM ID — discovery (which omits the
// filter entirely and reports only safe match counts; see
// runSubscriptionKubernetesDiscoveryQuery), one deliberate invocation at
// a time. See runSubscriptionKubernetesQuery's and
// runSubscriptionKubernetesDiscoveryQuery's doc comments for why there is
// no automatic fallback between any of these modes.
//
// Run it explicitly:
//
//	OPSCART_BILLING_SPIKE_MANUAL=1 \
//	OPSCART_BILLING_SPIKE_SUBSCRIPTION_ID=<subscription-id> \
//	OPSCART_BILLING_SPIKE_CLUSTER_RESOURCE_ID=<aks-cluster-arm-resource-id> \
//	OPSCART_BILLING_SPIKE_FROM=<YYYY-MM-DD> \
//	OPSCART_BILLING_SPIKE_TO=<YYYY-MM-DD> \
//	OPSCART_BILLING_SPIKE_FILTER_MODE=captured-and \
//	go test ./pkg/billing/ -run TestManualSubscriptionKubernetesQueryDiagnostic -v
//
// (Repeat with OPSCART_BILLING_SPIKE_FILTER_MODE set to combined-values,
// then discovery, as separate invocations to compare — never more than
// one mode in a single run.)
//
// Safety properties (all deliberate, none configurable):
//   - Credential: exactly AuthModeAzureCLI via the existing, already-
//     reviewed NewCredential — no other credential mode, no fallback
//     chain, and no new credential construction path.
//   - Permissions: requests exactly armTokenScope, the same ARM scope
//     production billing already uses — nothing broader.
//   - Period: OPSCART_BILLING_SPIKE_FROM/_TO are required, strict
//     YYYY-MM-DD, and validated by resolveSubscriptionKubernetesSpikePeriod.
//   - Filter mode: OPSCART_BILLING_SPIKE_FILTER_MODE is required and must
//     be exactly "captured-and", "combined-values", or "discovery",
//     validated by parseSubscriptionKubernetesQueryFilterMode.
//   - All of the above — subscription ID, cluster resource ID, period,
//     and filter mode — are validated before NewCredential is ever
//     called, so bad input can never reach the point of acquiring a
//     credential, let alone making a request.
//   - Requests: at most one HTTP call, for exactly the one filter mode
//     requested — runSubscriptionKubernetesQuery and
//     runSubscriptionKubernetesDiscoveryQuery each never retry, never
//     follow pagination, and never try a different filter mode (see
//     TestSubscriptionKubernetesQuerySpikeMakesExactlyOneRequestOnFailure,
//     TestSubscriptionKubernetesQuerySpikeEachFilterModeMakesOneRequestAndReportsNoDataOnZeroRows,
//     and TestSubscriptionKubernetesQueryDiscoveryMakesExactlyOneRequest).
//   - Discovery additionally rejects a response of more than
//     subscriptionKubernetesQueryMaxDiscoveryRows rows, defensively,
//     before examining any row.
//   - Timeout: bounded to 15s via ctx, tighter than newARMHTTPClient's own
//     30s client-level timeout.
//   - Redirects: disabled (newARMHTTPClient, shared with production).
//   - Output: for captured-and/combined-values, prints only status,
//     total, currency, period, filter mode, row count, and the response's
//     column names (schema, never values). For discovery, prints only
//     status, period, filter mode, row count, response column names,
//     unique/ARM-shaped cluster-value counts, the four match-tier counts,
//     and — only when a safe match exists — the matched total and
//     currency (see writeDiscoverySafeOutput and
//     subscriptionKubernetesDiscoveryResult). Every failure is an
//     AzureAPIError, SafeError, subscriptionKubernetesQueryNoDataError, or
//     subscriptionKubernetesQueryDiscoveryNoMatchError, all already safe
//     to print verbatim. No mode ever prints a token, Authorization
//     header, subscription ID, cluster resource ID, resource-group name,
//     request URL, or response rows/body — discovery additionally never
//     prints a cluster value or any hash of one, even transiently
//     examined ones that didn't match.
//   - No automatic refresh: this is one manual invocation, not wired into
//     Runtime.Start or any ticker/schedule.
//
// It never touches state used by any other test and makes no change to
// the production billing path.
func TestManualSubscriptionKubernetesQueryDiagnostic(t *testing.T) {
	if os.Getenv("OPSCART_BILLING_SPIKE_MANUAL") != "1" {
		t.Skip("manual diagnostic — set OPSCART_BILLING_SPIKE_MANUAL=1 (see this test's doc comment) to run it against a real subscription")
	}
	subscriptionID := os.Getenv("OPSCART_BILLING_SPIKE_SUBSCRIPTION_ID")
	clusterResourceID := os.Getenv("OPSCART_BILLING_SPIKE_CLUSTER_RESOURCE_ID")
	if subscriptionID == "" || clusterResourceID == "" {
		t.Fatal("OPSCART_BILLING_SPIKE_SUBSCRIPTION_ID and OPSCART_BILLING_SPIKE_CLUSTER_RESOURCE_ID are both required")
	}

	// Every input below is validated before NewCredential is ever called.
	filterMode, modeErr := parseSubscriptionKubernetesQueryFilterMode(os.Getenv("OPSCART_BILLING_SPIKE_FILTER_MODE"))
	if modeErr != nil {
		t.Fatalf("invalid filter mode: %v", modeErr)
	}

	fromValue := os.Getenv("OPSCART_BILLING_SPIKE_FROM")
	toValue := os.Getenv("OPSCART_BILLING_SPIKE_TO")
	start, end, periodErr := resolveSubscriptionKubernetesSpikePeriod(fromValue, toValue)
	if periodErr != nil {
		t.Fatalf("invalid period: %v", periodErr)
	}
	periodLabel := fromValue + " to " + toValue // the exact supplied values, not a reformatted round-trip

	credential, err := NewCredential(AuthModeAzureCLI)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if filterMode == subscriptionKubernetesQueryFilterModeDiscovery {
		result, discoveryErr := runSubscriptionKubernetesDiscoveryQuery(ctx, newARMHTTPClient(), credential, defaultManagementEndpoint, subscriptionID, clusterResourceID, start, end)
		writeDiscoverySafeOutput(os.Stdout, periodLabel, filterMode, result, discoveryErr)
		if discoveryErr != nil {
			t.Fatalf("discovery query failed: %v", discoveryErr)
		}
		return
	}

	total, currency, rowCount, columnNames, queryErr := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), credential, defaultManagementEndpoint, subscriptionID, clusterResourceID, start, end, filterMode)

	// queryErr, when non-nil, is always an *AzureAPIError, *SafeError, or
	// *subscriptionKubernetesQueryNoDataError — all three are already
	// safe to print verbatim (see their doc comments): fixed operation
	// label, fixed status classification, Azure's own request ID where
	// applicable. Never the raw response, subscription ID, cluster
	// resource ID, or request URL. columnNames is schema metadata (column
	// names only, never row values), and filterMode is one of three fixed
	// literal strings — both are safe to print for the same reason.
	if queryErr != nil {
		fmt.Printf("status: error: %s\nperiod: %s\nfilter mode: %s\nrow count: %d\nresponse columns: %s\n",
			queryErr.Error(), periodLabel, filterMode, rowCount, strings.Join(columnNames, ","))
		t.Fatalf("diagnostic query failed: %v", queryErr)
	}
	fmt.Printf("status: ok\ntotal: %.2f\ncurrency: %s\nperiod: %s\nfilter mode: %s\nrow count: %d\nresponse columns: %s\n",
		total, currency, periodLabel, filterMode, rowCount, strings.Join(columnNames, ","))
}

// writeDiscoverySafeOutput writes exactly the fields discovery mode is
// allowed to surface (see subscriptionKubernetesDiscoveryResult's doc
// comment) to w. It takes an io.Writer rather than writing directly to
// stdout so TestSubscriptionKubernetesQueryDiscoverySafeOutputNeverExposesClusterValues
// can capture and inspect the exact text a real run would print, proving
// no cluster ID, resource-group name, or subscription ID ever reaches it
// — result and err are the only inputs, and neither type can carry one
// (see their doc comments in subscription_kubernetes_query_spike_test.go).
func writeDiscoverySafeOutput(w io.Writer, periodLabel string, filterMode subscriptionKubernetesQueryFilterMode, result subscriptionKubernetesDiscoveryResult, err error) {
	status := "ok"
	if err != nil {
		status = "error: " + err.Error()
	}
	// Row count and response column names are always safe to print —
	// they describe the response's shape, not the match outcome — so
	// they print unconditionally, even when processing stopped before
	// any counting happened (e.g. the row-count safety bound was
	// exceeded).
	fmt.Fprintf(w, "status: %s\nperiod: %s\nfilter mode: %s\nrow count: %d\nresponse columns: %s\n",
		status, periodLabel, filterMode, result.RowCount, strings.Join(result.ColumnNames, ","))

	if !result.CountsEvaluated {
		// Processing stopped before the matching loop ran (see
		// subscriptionKubernetesDiscoveryResult.CountsEvaluated) — these
		// six counts are not real computed zeros, so they must never be
		// printed as if they were.
		fmt.Fprintf(w, "match counts: not evaluated\n")
		return
	}
	fmt.Fprintf(w, "unique cluster values: %d\narm-shaped cluster values: %d\nexact matches: %d\ncase-insensitive matches: %d\nnormalized arm matches: %d\nname-only matches: %d\n",
		result.UniqueClusterValueCount, result.ARMShapedClusterValueCount, result.ExactMatchCount, result.CaseInsensitiveMatchCount, result.NormalizedARMMatchCount, result.NameOnlyMatchCount)
	if result.HasSafeMatch {
		fmt.Fprintf(w, "matched total: %.2f\nmatched currency: %s\n", result.MatchedTotal, result.MatchedCurrency)
	}
}

// TestSubscriptionKubernetesQueryDiscoverySafeOutputNeverExposesClusterValues
// runs the real discovery flow (synthetic HTTP server, two clusters — the
// configured target and an unrelated one with a distinctive high cost)
// through writeDiscoverySafeOutput exactly as the manual diagnostic would,
// and asserts the captured output text contains neither cluster's full
// ARM ID, subscription ID, resource-group name, or cluster name — nor the
// unrelated cluster's cost — while still reporting the real match counts
// and the safely matched total.
func TestSubscriptionKubernetesQueryDiscoverySafeOutputNeverExposesClusterValues(t *testing.T) {
	const target = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"
	const secretOther = "/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/Other-RG/providers/Microsoft.ContainerService/managedClusters/secret-other-cluster"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Cost"}, {Name: "Currency"}},
			[][]any{
				{target, "eastus2", 10.0, "USD"},
				{secretOther, "westus2", 99999.0, "USD"},
			}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := runSubscriptionKubernetesDiscoveryQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", target, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("runSubscriptionKubernetesDiscoveryQuery: %v", err)
	}

	var buf bytes.Buffer
	writeDiscoverySafeOutput(&buf, "2026-08-17 to 2026-09-15", subscriptionKubernetesQueryFilterModeDiscovery, result, nil)
	output := buf.String()

	for _, forbidden := range []string{
		target, secretOther,
		"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222",
		"My-RG", "Other-RG", "My-AKS-Cluster", "secret-other-cluster",
		"/subscriptions/", "99999",
	} {
		if strings.Contains(output, forbidden) {
			t.Errorf("diagnostic output contains %q — must never expose cluster/subscription/resource-group identifiers or unrelated cost values; output:\n%s", forbidden, output)
		}
	}
	if !strings.Contains(output, "matched total: 10.00") {
		t.Errorf("diagnostic output missing the matched total; output:\n%s", output)
	}
	if !strings.Contains(output, "unique cluster values: 2") {
		t.Errorf("diagnostic output missing the unique cluster value count; output:\n%s", output)
	}
	if !strings.Contains(output, "exact matches: 1") {
		t.Errorf("diagnostic output missing the exact match count; output:\n%s", output)
	}
}

// TestSubscriptionKubernetesQueryDiscoverySafeOutputOmitsCountsWhenRowCapExceeded
// proves that once the row-count safety bound is exceeded — so
// runSubscriptionKubernetesDiscoveryQuery never ran its matching loop —
// writeDiscoverySafeOutput never prints any of the six match-count lines
// as a misleading zero. Row count and response column names, which are
// known regardless of whether counting ran, still print.
func TestSubscriptionKubernetesQueryDiscoverySafeOutputOmitsCountsWhenRowCapExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Cost"}, {Name: "Currency"}},
			discoveryRowFixtures(subscriptionKubernetesQueryMaxDiscoveryRows+1), "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := runSubscriptionKubernetesDiscoveryQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", "cluster-resource-id", time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected a too-many-rows error, got nil")
	}

	var buf bytes.Buffer
	writeDiscoverySafeOutput(&buf, "2026-08-17 to 2026-09-15", subscriptionKubernetesQueryFilterModeDiscovery, result, err)
	output := buf.String()

	for _, misleading := range []string{
		"exact matches: 0", "case-insensitive matches: 0", "normalized arm matches: 0",
		"name-only matches: 0", "unique cluster values: 0", "arm-shaped cluster values: 0",
	} {
		if strings.Contains(output, misleading) {
			t.Errorf("output must never print a zero match count when processing stopped before counting; got %q in:\n%s", misleading, output)
		}
	}
	if !strings.Contains(output, "not evaluated") {
		t.Errorf("output should explicitly say the match counts were not evaluated; got:\n%s", output)
	}
	if !strings.Contains(output, fmt.Sprintf("row count: %d", subscriptionKubernetesQueryMaxDiscoveryRows+1)) {
		t.Errorf("row count should still be printed even though counting did not run; got:\n%s", output)
	}
	if !strings.Contains(output, "response columns: Cluster,ResourceLocation,Cost,Currency") {
		t.Errorf("response columns should still be printed even though counting did not run; got:\n%s", output)
	}
}
