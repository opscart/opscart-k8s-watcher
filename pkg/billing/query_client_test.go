package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// fakeCredential is a synthetic azcore.TokenCredential fixture — tests in
// this package never contact Azure or acquire a real token.
type fakeCredential struct {
	token string
	err   error
	calls int
}

func (f *fakeCredential) GetToken(_ context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.calls++
	if f.err != nil {
		return azcore.AccessToken{}, f.err
	}
	return azcore.AccessToken{Token: f.token, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func newTestClient(t *testing.T, server *httptest.Server) *queryClient {
	t.Helper()
	return newQueryClient(server.Client(), &fakeCredential{token: "test-token"}, server.URL)
}

func writeQueryResponse(w http.ResponseWriter, columns []queryColumn, rows [][]any, nextLink string) {
	resp := queryResponseBody{}
	resp.Properties.Columns = columns
	resp.Properties.Rows = rows
	resp.Properties.NextLink = nextLink
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// TestBuildResourceGroupQueryPayloadForConfiguredPeriod pins the exact
// request payload buildResourceGroupQuery produces for the dashboard's
// documented example period (2026-08-17 to 2026-09-15, the inclusive UI
// dates ResolvePeriod resolves — see TestResolvePeriodCustomInclusiveEndDate
// in config_test.go). It checks both the Go struct fields and the literal
// marshaled JSON bytes, since the latter is exactly what query() sends to
// Azure (query_client.go).
func TestBuildResourceGroupQueryPayloadForConfiguredPeriod(t *testing.T) {
	start := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC)

	body := buildResourceGroupQuery(CostBasisActualCost, start, end)

	if body.Type != "ActualCost" {
		t.Errorf("Type = %q, want ActualCost", body.Type)
	}
	if body.Timeframe != "Custom" {
		t.Errorf("Timeframe = %q, want Custom", body.Timeframe)
	}
	// The From/To boundaries must cover the entire displayed period: from
	// the first instant of the inclusive start date through the last
	// instant (23:59:59) of the inclusive end date. Neither boundary may
	// be date-only (which would leave Azure to infer an implicit
	// time-of-day) nor truncated to midnight of the end date (which would
	// silently drop that day's charges).
	if body.TimePeriod.From != "2026-08-17T00:00:00Z" {
		t.Errorf("TimePeriod.From = %q, want 2026-08-17T00:00:00Z", body.TimePeriod.From)
	}
	if body.TimePeriod.To != "2026-09-15T23:59:59Z" {
		t.Errorf("TimePeriod.To = %q, want 2026-09-15T23:59:59Z (last instant of the inclusive end date)", body.TimePeriod.To)
	}
	if body.Dataset.Granularity != "None" {
		t.Errorf("Dataset.Granularity = %q, want None (one summed total for the whole period, not daily buckets)", body.Dataset.Granularity)
	}
	wantAgg := map[string]queryAggregate{"totalCost": {Name: "Cost", Function: "Sum"}}
	if !reflect.DeepEqual(body.Dataset.Aggregation, wantAgg) {
		t.Errorf("Dataset.Aggregation = %+v, want %+v (Sum, matching a resource-level cost total)", body.Dataset.Aggregation, wantAgg)
	}
	wantGrouping := []queryGrouping{{Type: "Dimension", Name: "ResourceId"}, {Type: "Dimension", Name: "ResourceGroupName"}}
	if !reflect.DeepEqual(body.Dataset.Grouping, wantGrouping) {
		t.Errorf("Dataset.Grouping = %+v, want %+v", body.Dataset.Grouping, wantGrouping)
	}

	// Marshal to JSON and check the literal wire bytes for the period and
	// cost-type fields — this is exactly what query() (query_client.go)
	// sends to the Cost Management API, not just the Go struct.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	wireBody := string(raw)
	for _, want := range []string{
		`"type":"ActualCost"`,
		`"timeframe":"Custom"`,
		`"from":"2026-08-17T00:00:00Z"`,
		`"to":"2026-09-15T23:59:59Z"`,
		`"granularity":"None"`,
	} {
		if !strings.Contains(wireBody, want) {
			t.Errorf("marshaled request body missing %q; got %s", want, wireBody)
		}
	}
}

// TestBuildResourceGroupQueryUsesConfiguredCostBasis confirms the request
// Type tracks whatever CostBasis the caller passes (ActualCost by default
// per ClusterConfig.EffectiveCostBasis, AmortizedCost only when explicitly
// configured) rather than a value hardcoded independent of it.
func TestBuildResourceGroupQueryUsesConfiguredCostBasis(t *testing.T) {
	body := buildResourceGroupQuery(CostBasisAmortizedCost, time.Now(), time.Now())
	if body.Type != "AmortizedCost" {
		t.Errorf("Type = %q, want AmortizedCost", body.Type)
	}
}

func TestQueryParsesColumnsRegardlessOfOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want Bearer test-token", got)
		}
		// Deliberately reordered vs. the request-builder's own column
		// expectations, and includes an extra column the client does not
		// look for — the client must still find Cost/Currency/ResourceId by
		// name.
		writeQueryResponse(w,
			[]queryColumn{{Name: "ResourceGroupName", Type: "String"}, {Name: "Currency", Type: "String"}, {Name: "ResourceId", Type: "String"}, {Name: "Cost", Type: "Number"}, {Name: "UsageDate", Type: "Number"}},
			[][]any{{"rg1", "USD", "/subscriptions/s/resourceGroups/rg1/providers/x/y", 12.5, float64(20260901)}},
			"")
	}))
	defer server.Close()

	client := newTestClient(t, server)
	rows, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	cost, ok := rows[0].float64("Cost")
	if !ok || cost != 12.5 {
		t.Errorf("Cost = %v, %v; want 12.5, true", cost, ok)
	}
	currency, ok := rows[0].string("Currency")
	if !ok || currency != "USD" {
		t.Errorf("Currency = %v, %v; want USD, true", currency, ok)
	}
}

func TestQueryFollowsPagination(t *testing.T) {
	var nextLink string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// A real Azure nextLink reuses the exact same scope/operation path
		// with a different query string (a continuation token) — this
		// distinguishes page 1 from page 2 by query, not path, matching
		// that real shape.
		if r.URL.Query().Get("$skiptoken") == "next" {
			writeQueryResponse(w,
				[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
				[][]any{{2.0, "USD", "/r/2", "rg1"}},
				"")
			return
		}
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
			[][]any{{1.0, "USD", "/r/1", "rg1"}},
			nextLink)
	}))
	defer server.Close()
	nextLink = server.URL + "/subscriptions/s/resourceGroups/rg1/providers/Microsoft.CostManagement/query?api-version=" + costManagementAPIVersion + "&$skiptoken=next"

	client := newTestClient(t, server)
	rows, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows across pages, want 2", len(rows))
	}
	if calls != 2 {
		t.Errorf("server received %d calls, want 2", calls)
	}
}

func TestQueryDoesNotFollowRedirects(t *testing.T) {
	evilCalls := 0
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal-token", http.StatusFound)
	}))
	defer server.Close()

	// newQueryClient's nil-httpClient fallback (newARMHTTPClient) is the
	// exact client production code uses — verify redirect rejection
	// through that path, not a test-only client configuration.
	client := newQueryClient(nil, &fakeCredential{token: "test-token"}, server.URL)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an error when the server responds with a redirect, got nil")
	}
	if evilCalls != 0 {
		t.Errorf("redirect was followed: %d calls to the untrusted host", evilCalls)
	}
}

func TestQueryRejectsContinuationToDifferentHost(t *testing.T) {
	evilCalls := 0
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
			[][]any{{1.0, "USD", "/r/1", "rg1"}},
			evil.URL+"/steal-token")
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an error for a cross-host continuation, got nil")
	}
	if evilCalls != 0 {
		t.Errorf("cross-host continuation was followed: %d calls to the untrusted host", evilCalls)
	}
}

func TestQueryRejectsContinuationToDifferentScope(t *testing.T) {
	rg2Calls := 0
	var evilPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "resourceGroups/rg2") {
			rg2Calls++
		}
		// Same host as the original request (the host check alone would
		// allow this), but nextLink below points at a different resource
		// group's Cost Management query path — must still be rejected.
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
			[][]any{{1.0, "USD", "/r/1", "rg1"}},
			evilPath)
	}))
	defer server.Close()
	evilPath = server.URL + "/subscriptions/s/resourceGroups/rg2/providers/Microsoft.CostManagement/query?api-version=" + costManagementAPIVersion

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an error for a same-host, different-scope continuation, got nil")
	}
	if rg2Calls != 0 {
		t.Errorf("same-host, different-scope continuation was followed: %d calls to resourceGroups/rg2", rg2Calls)
	}
}

func TestQueryRejectsRepeatedContinuation(t *testing.T) {
	calls := 0
	var loopLink string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
			[][]any{{1.0, "USD", "/r/1", "rg1"}},
			loopLink) // always points back at the exact same URL — must not loop
	}))
	defer server.Close()
	loopLink = server.URL + "/subscriptions/s/resourceGroups/rg1/providers/Microsoft.CostManagement/query?api-version=" + costManagementAPIVersion

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an error for a repeated (non-progressing) continuation, got nil")
	}
	if calls != 1 {
		t.Errorf("server received %d calls, want exactly 1 (rejected before repeating)", calls)
	}
}

func TestQueryRetriesThrottlingWithRetryAfter(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
			[][]any{{1.0, "USD", "/r/1", "rg1"}},
			"")
	}))
	defer server.Close()

	client := newTestClient(t, server)
	rows, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if calls != 2 {
		t.Errorf("server received %d calls, want 2 (one throttled, one success)", calls)
	}
}

func TestQueryExhaustsBoundedRetries(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}
	if calls != maxRetryAttempts {
		t.Errorf("server received %d calls, want exactly %d (bounded retry)", calls, maxRetryAttempts)
	}
}

// TestQueryPreservesFinalAttemptCooldown is a regression test: earlier
// versions of doWithRetry returned a plain *AzureAPIError once
// maxRetryAttempts was reached, silently discarding a mandated retry delay
// on that final attempt even when the response carried one — losing the
// one piece of information (when it's actually safe to retry) that
// matters most once every retry within this call is exhausted.
func TestQueryPreservesFinalAttemptCooldown(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("x-ms-request-id", "req-final")
		if calls < maxRetryAttempts {
			w.Header().Set("Retry-After", "0") // short — the earlier attempts retry normally
		} else {
			w.Header().Set("Retry-After", "28800") // 8 hours — mandated only on the final attempt
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	before := time.Now()
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if calls != maxRetryAttempts {
		t.Fatalf("server received %d calls, want exactly %d", calls, maxRetryAttempts)
	}
	if err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}

	var deferred *DeferredError
	if !errors.As(err, &deferred) {
		t.Fatalf("error = %v (%T), want a *DeferredError carrying the final attempt's mandated cooldown", err, err)
	}
	if deferred.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want %d", deferred.StatusCode, http.StatusTooManyRequests)
	}
	if deferred.RequestID != "req-final" {
		t.Errorf("RequestID = %q, want req-final", deferred.RequestID)
	}
	wantNotBefore := before.Add(8 * time.Hour)
	if diff := deferred.NotBefore.Sub(wantNotBefore); diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("NotBefore = %v, want approximately %v (8h from the final attempt)", deferred.NotBefore, wantNotBefore)
	}
}

func TestQueryDoesNotRetryAuthFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed"}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an authorization error, got nil")
	}
	if calls != 1 {
		t.Errorf("server received %d calls, want 1 (403 must not be retried)", calls)
	}
}

func TestQueryHandlesNoContentAsZeroRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	rows, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows for 204 No Content, want 0", len(rows))
	}
}

func TestQueryRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected a decode error for malformed JSON, got nil")
	}
}

func TestQueryRejectsMismatchedRowWidth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}},
			[][]any{{1.0}}, // one value, two declared columns
			"")
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an error for a row/column width mismatch, got nil")
	}
}

func TestQueryRespectsContextTimeout(t *testing.T) {
	// The handler sleeps past the client's deadline but still returns
	// promptly on its own, so httptest.Server.Close (in the deferred
	// server.Close below) never blocks waiting for an abandoned handler —
	// only the client-observed error is under test here.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.query(ctx, "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected a context deadline error, got nil")
	}
}

func TestQueryRejectsOversizedResponse(t *testing.T) {
	old := maxResponseBytes
	maxResponseBytes = 100 // shrink so the test doesn't allocate a real 10 MiB body
	defer func() { maxResponseBytes = old }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A well-formed but oversized response — padding via many rows,
		// not a huge single field, so this also proves the bound applies
		// to the whole body, not just one value.
		rows := make([][]any, 0, 20)
		for i := 0; i < 20; i++ {
			rows = append(rows, []any{1.0, "USD", fmt.Sprintf("/subscriptions/s/resourceGroups/rg1/providers/x/y%d", i), "rg1"})
		}
		writeQueryResponse(w, []queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}}, rows, "")
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an error for a response exceeding the size bound, got nil")
	}
	if !strings.Contains(err.Error(), "size limit") {
		t.Errorf("error = %v, want a size-limit classification", err)
	}
}

func TestQueryRejectsTooManyRows(t *testing.T) {
	old := maxTotalRows
	maxTotalRows = 5
	defer func() { maxTotalRows = old }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := make([][]any, 0, 10)
		for i := 0; i < 10; i++ {
			rows = append(rows, []any{1.0, "USD", fmt.Sprintf("/r/%d", i), "rg1"})
		}
		writeQueryResponse(w, []queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}}, rows, "")
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	if err == nil {
		t.Fatal("expected an error for a response exceeding the row bound, got nil")
	}
	if !strings.Contains(err.Error(), "row limit") {
		t.Errorf("error = %v, want a row-limit classification", err)
	}
}

func TestAcquireTokenPropagatesCredentialFailure(t *testing.T) {
	cred := &fakeCredential{err: fmt.Errorf("no cached token")}
	if _, err := acquireToken(context.Background(), cred); err == nil {
		t.Fatal("expected acquireToken to propagate credential failure")
	}
	if cred.calls != 1 {
		t.Errorf("credential called %d times, want 1", cred.calls)
	}
}
