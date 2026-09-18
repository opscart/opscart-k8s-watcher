package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestBudgetNilIsUnbounded(t *testing.T) {
	var b *requestBudget
	for i := 0; i < 1000; i++ {
		if !b.take() {
			t.Fatalf("nil budget refused take() on attempt %d, want unbounded", i)
		}
	}
}

func TestRequestBudgetExhausts(t *testing.T) {
	b := newRequestBudget(3)
	for i := 0; i < 3; i++ {
		if !b.take() {
			t.Fatalf("take() refused on attempt %d, want allowed (budget of 3)", i)
		}
	}
	if b.take() {
		t.Fatal("take() allowed a 4th attempt against a budget of 3")
	}
}

func TestRequestBudgetConcurrentSafe(t *testing.T) {
	b := newRequestBudget(50)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.take() {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 50 {
		t.Errorf("granted = %d, want exactly 50 (the budget), under concurrent access", granted)
	}
}

// TestQueryStopsAtRequestBudgetDuringPagination proves the budget actually
// bounds pagination independent of maxPages: a server that always responds
// with one more row and a valid, always-progressing nextLink would
// otherwise be paginated up to maxPages (20) times, but a much smaller
// explicit budget must stop it far sooner, with a safe budget-exhausted
// error rather than exceeding the budget or hanging.
func TestQueryStopsAtRequestBudgetDuringPagination(t *testing.T) {
	calls := 0
	var endpoint string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		next := endpoint + "/subscriptions/s/resourceGroups/rg1/providers/Microsoft.CostManagement/query?api-version=" + costManagementAPIVersion + "&$skiptoken=" + fmt.Sprint(calls)
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
			[][]any{{1.0, "USD", fmt.Sprintf("/r/%d", calls), "rg1"}},
			next) // always progresses (a new $skiptoken each time), so this never trips the repeated-continuation check
	}))
	defer server.Close()
	endpoint = server.URL

	client := newTestClient(t, server)
	budget := newRequestBudget(3)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), budget, "test query")
	if err == nil {
		t.Fatal("expected a budget-exhausted error, got nil")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error = %v, want a budget-exhausted classification", err)
	}
	if calls != 3 {
		t.Errorf("server received %d calls, want exactly 3 (the budget), not maxPages (%d)", calls, maxPages)
	}
}

// TestRequestBudgetIsSharedAcrossResolverAndQuery proves FetchBilling's
// single requestBudget is actually threaded through both the
// node-resource-group lookup and the Cost Management queries, not two
// independent budgets: exhausting it during resolution must leave zero
// budget for the query phase.
func TestRequestBudgetIsSharedAcrossResolverAndQuery(t *testing.T) {
	queryCalls := 0
	resolveCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "managedClusters") {
			resolveCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"properties":{"nodeResourceGroup":"MC_test_rg"}}`))
			return
		}
		queryCalls++
		writeQueryResponse(w, []queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}}, nil, "")
	}))
	defer server.Close()

	resolver := newNodeResourceGroupResolver(server.Client(), &fakeCredential{token: "t"}, server.URL)
	budget := newRequestBudget(1)
	if _, err := resolver.Resolve(context.Background(), validClusterConfig(), budget); err != nil {
		t.Fatalf("Resolve (consuming the only budget unit): %v", err)
	}
	if resolveCalls != 1 {
		t.Fatalf("resolveCalls = %d, want 1", resolveCalls)
	}

	client := newTestClient(t, server)
	_, err := client.query(context.Background(), "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), budget, "test query")
	if err == nil {
		t.Fatal("expected the query phase to fail against an already-exhausted shared budget")
	}
	if queryCalls != 0 {
		t.Errorf("queryCalls = %d, want 0 — the budget was already spent by resolution", queryCalls)
	}
}

// TestDoWithRetryPreservesCooldownWhenBudgetExhausted proves the mandated
// delay from a throttled response is preserved even when the request
// budget — not maxRetryAttempts — is what ends the call. The mandated
// delay is deliberately short (0s, honored immediately per the
// found-even-when-zero fix): with budget=2 and ctx.Background() (no
// deadline), a long delay here would legitimately be waited out before
// attempt 2 (nextRetryDelay has no deadline to violate, so it correctly
// chooses to wait rather than defer) — this test is about budget ending
// the call, not about deadline-driven deferral, so it avoids that by
// using a delay with nothing to wait for.
func TestDoWithRetryPreservesCooldownWhenBudgetExhausted(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("x-ms-request-id", "req-budget")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	budget := newRequestBudget(1) // exhausted after attempt 1; attempt 2 never sends a request
	_, _, err := client.doWithRetry(context.Background(), http.MethodGet, server.URL, nil, "test operation", budget)
	if calls != 1 {
		t.Fatalf("server received %d calls, want exactly 1 (budget allows only one)", calls)
	}
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var deferred *DeferredError
	if !errors.As(err, &deferred) {
		t.Fatalf("error = %v (%T), want a *DeferredError preserving the mandated cooldown despite budget exhaustion", err, err)
	}
	if deferred.StatusCode != http.StatusTooManyRequests || deferred.RequestID != "req-budget" {
		t.Errorf("deferred = %+v, want the throttled response's status/request ID preserved", deferred)
	}
}

func TestDoWithRetryReportsBudgetExhausted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	budget := newRequestBudget(0) // already exhausted
	_, _, err := client.doWithRetry(context.Background(), http.MethodGet, server.URL, nil, "test operation", budget)
	if err == nil {
		t.Fatal("expected an error for an exhausted budget, got nil")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error = %v, want a budget-exhausted classification", err)
	}
}
