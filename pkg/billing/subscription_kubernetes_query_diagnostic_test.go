package billing

import (
	"context"
	"fmt"
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
// exactly the two supported modes — case-sensitive, no default, no
// inference from any other input. Like resolveSubscriptionKubernetesSpikePeriod,
// this is a pure function (no credential, no HTTP client, no import of
// azcore or net/http), so a missing or unknown mode is always rejected
// before TestManualSubscriptionKubernetesQueryDiagnostic can acquire a
// credential or make a request — see
// TestParseSubscriptionKubernetesQueryFilterModeRejectsMissingOrUnknown.
func parseSubscriptionKubernetesQueryFilterMode(value string) (subscriptionKubernetesQueryFilterMode, error) {
	switch subscriptionKubernetesQueryFilterMode(value) {
	case subscriptionKubernetesQueryFilterModeCapturedAnd, subscriptionKubernetesQueryFilterModeCombinedValues:
		return subscriptionKubernetesQueryFilterMode(value), nil
	default:
		return "", fmt.Errorf("OPSCART_BILLING_SPIKE_FILTER_MODE %q: use %q or %q", value, subscriptionKubernetesQueryFilterModeCapturedAnd, subscriptionKubernetesQueryFilterModeCombinedValues)
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
// period, and response decoding all work, but the captured-and Cluster
// filter matched zero rows. This diagnostic requires an explicit
// OPSCART_BILLING_SPIKE_FILTER_MODE so an operator can compare that exact
// captured shape against the combined-values alternative, one deliberate
// invocation at a time — see runSubscriptionKubernetesQuery's doc comment
// for why there is no automatic fallback between them.
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
// (Repeat with OPSCART_BILLING_SPIKE_FILTER_MODE=combined-values as a
// second, separate invocation to compare — never both in one run.)
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
//     be exactly "captured-and" or "combined-values", validated by
//     parseSubscriptionKubernetesQueryFilterMode.
//   - All of the above — subscription ID, cluster resource ID, period,
//     and filter mode — are validated before NewCredential is ever
//     called, so bad input can never reach the point of acquiring a
//     credential, let alone making a request.
//   - Requests: at most one HTTP call, for exactly the one filter mode
//     requested (runSubscriptionKubernetesQuery never retries, never
//     follows pagination, and never tries the other filter mode — see
//     TestSubscriptionKubernetesQuerySpikeMakesExactlyOneRequestOnFailure
//     and
//     TestSubscriptionKubernetesQuerySpikeEachFilterModeMakesOneRequestAndReportsNoDataOnZeroRows).
//   - Timeout: bounded to 15s via ctx, tighter than newARMHTTPClient's own
//     30s client-level timeout.
//   - Redirects: disabled (newARMHTTPClient, shared with production).
//   - Output: prints only status, total, currency, period, filter mode,
//     row count, and the response's column names (schema, never values)
//     — see AzureAPIError/SafeError/subscriptionKubernetesQueryNoDataError,
//     which every failure from runSubscriptionKubernetesQuery already
//     routes through. Never a token, Authorization header, subscription
//     ID, cluster resource ID, request URL, or response rows/body.
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

	total, currency, rowCount, columnNames, queryErr := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), credential, defaultManagementEndpoint, subscriptionID, clusterResourceID, start, end, filterMode)

	// queryErr, when non-nil, is always an *AzureAPIError, *SafeError, or
	// *subscriptionKubernetesQueryNoDataError — all three are already
	// safe to print verbatim (see their doc comments): fixed operation
	// label, fixed status classification, Azure's own request ID where
	// applicable. Never the raw response, subscription ID, cluster
	// resource ID, or request URL. columnNames is schema metadata (column
	// names only, never row values), and filterMode is one of two fixed
	// literal strings — both are safe to print for the same reason.
	if queryErr != nil {
		fmt.Printf("status: error: %s\nperiod: %s\nfilter mode: %s\nrow count: %d\nresponse columns: %s\n",
			queryErr.Error(), periodLabel, filterMode, rowCount, strings.Join(columnNames, ","))
		t.Fatalf("diagnostic query failed: %v", queryErr)
	}
	fmt.Printf("status: ok\ntotal: %.2f\ncurrency: %s\nperiod: %s\nfilter mode: %s\nrow count: %d\nresponse columns: %s\n",
		total, currency, periodLabel, filterMode, rowCount, strings.Join(columnNames, ","))
}
