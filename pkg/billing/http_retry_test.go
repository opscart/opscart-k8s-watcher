package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func responseWithHeaders(headers map[string]string) *http.Response {
	h := make(http.Header, len(headers))
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h}
}

func TestMandatedRetryDelayPrefersLongestCostManagementHeader(t *testing.T) {
	resp := responseWithHeaders(map[string]string{
		"x-ms-ratelimit-microsoft.costmanagement-qpu-retry-after":    "5",
		"x-ms-ratelimit-microsoft.costmanagement-tenant-retry-after": "42",
		"Retry-After": "1",
	})
	delay, ok := mandatedRetryDelay(resp)
	if !ok {
		t.Fatal("expected a mandated delay, got none")
	}
	if delay != 42*time.Second {
		t.Errorf("delay = %v, want 42s (the longest of the quotas exhausted)", delay)
	}
}

func TestMandatedRetryDelayFallsBackToStandardRetryAfter(t *testing.T) {
	resp := responseWithHeaders(map[string]string{"Retry-After": "7"})
	delay, ok := mandatedRetryDelay(resp)
	if !ok || delay != 7*time.Second {
		t.Errorf("delay, ok = %v, %v; want 7s, true", delay, ok)
	}
}

func TestMandatedRetryDelayFoundEvenWhenZero(t *testing.T) {
	// A genuinely mandated 0-second delay must still register as found —
	// comparing against a zero-valued accumulator would otherwise treat it
	// as if no header were present at all, silently falling back to
	// exponential backoff instead of honoring "retry immediately."
	resp := responseWithHeaders(map[string]string{"Retry-After": "0"})
	delay, ok := mandatedRetryDelay(resp)
	if !ok {
		t.Fatal("expected a mandated delay of 0 to be found, got none")
	}
	if delay != 0 {
		t.Errorf("delay = %v, want 0", delay)
	}
}

func TestMandatedRetryDelayAbsentWithNoHeaders(t *testing.T) {
	resp := responseWithHeaders(nil)
	if _, ok := mandatedRetryDelay(resp); ok {
		t.Error("expected no mandated delay when no retry headers are present")
	}
}

func TestMandatedRetryDelayNilResponse(t *testing.T) {
	if _, ok := mandatedRetryDelay(nil); ok {
		t.Error("expected no mandated delay for a nil response (network error case)")
	}
}

func TestNextRetryDelayHonorsMandatedDelayWithinDeadline(t *testing.T) {
	resp := responseWithHeaders(map[string]string{"x-ms-ratelimit-microsoft.costmanagement-qpu-retry-after": "2"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	delay, deferred := nextRetryDelay(ctx, resp, 1)
	if deferred {
		t.Fatal("expected to wait, not defer, when the mandated delay fits the deadline")
	}
	if delay != 2*time.Second {
		t.Errorf("delay = %v, want 2s", delay)
	}
}

func TestNextRetryDelayDefersWhenMandatedDelayExceedsDeadline(t *testing.T) {
	resp := responseWithHeaders(map[string]string{"x-ms-ratelimit-microsoft.costmanagement-qpu-retry-after": "90"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	delay, deferred := nextRetryDelay(ctx, resp, 1)
	if !deferred {
		t.Fatal("expected to defer when the mandated delay exceeds the remaining deadline")
	}
	if delay != 90*time.Second {
		t.Errorf("delay = %v, want the mandated 90s so the deferral message can report it", delay)
	}
}

func TestNextRetryDelayUsesBoundedBackoffWithoutMandatedHeader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	delay, deferred := nextRetryDelay(ctx, responseWithHeaders(nil), 6) // 2^5s = 32s, over maxBackoffWait
	if deferred {
		t.Fatal("expected to wait, not defer, for our own bounded backoff within a long deadline")
	}
	if delay != maxBackoffWait {
		t.Errorf("delay = %v, want capped at maxBackoffWait (%v)", delay, maxBackoffWait)
	}
}

func TestQueryDefersWithoutWastingRetryBudgetOnUnfittableDelay(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Mandates a 5s wait — far longer than this call's own deadline —
		// on every attempt, so a client that retried early instead of
		// deferring would burn its whole bounded retry budget for nothing.
		w.Header().Set("x-ms-ratelimit-microsoft.costmanagement-qpu-retry-after", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.query(ctx, "/subscriptions/s/resourceGroups/rg1", buildResourceGroupQuery(CostBasisActualCost, time.Now(), time.Now()), nil, "test query")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a deferral error, got nil")
	}
	if elapsed > time.Second {
		t.Errorf("query took %v; a deferred retry must fail fast, not wait out an unfittable delay", elapsed)
	}
	if calls != 1 {
		t.Errorf("server received %d calls, want exactly 1 (deferred instead of retried)", calls)
	}
}
