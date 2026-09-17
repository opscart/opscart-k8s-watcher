package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	rows, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
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
	var page2URL string
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/subscriptions/s/resourceGroups/rg1/providers/Microsoft.CostManagement/query", func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
			[][]any{{1.0, "USD", "/r/1", "rg1"}},
			page2URL)
	})
	mux.HandleFunc("/page2", func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
			[][]any{{2.0, "USD", "/r/2", "rg1"}},
			"")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	page2URL = server.URL + "/page2"

	client := newTestClient(t, server)
	rows, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
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
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
	if err == nil {
		t.Fatal("expected an error for a cross-host continuation, got nil")
	}
	if evilCalls != 0 {
		t.Errorf("cross-host continuation was followed: %d calls to the untrusted host", evilCalls)
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
	rows, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
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
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
	if err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}
	if calls != maxRetryAttempts {
		t.Errorf("server received %d calls, want exactly %d (bounded retry)", calls, maxRetryAttempts)
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
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
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
	rows, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
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
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
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
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
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
	_, err := client.query(ctx, "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()))
	if err == nil {
		t.Fatal("expected a context deadline error, got nil")
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
