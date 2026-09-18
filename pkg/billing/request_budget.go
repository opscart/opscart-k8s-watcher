package billing

import "sync"

// maxRequestsPerRefresh bounds the total number of outbound HTTP attempts
// one FetchBilling call may make — across both resource-group queries,
// every page of each, every bounded retry within doWithRetry, and the
// node-resource-group lookup. The per-call bounds alone (maxPages=20,
// maxRetryAttempts=4) compound to a much higher theoretical ceiling (2
// scopes x 20 pages x 4 attempts = 160) than any real refresh should ever
// need; this is the actual production ceiling, generous enough to cover
// realistic multi-page/retry scenarios without approaching that number.
const maxRequestsPerRefresh = 40

// requestBudget is shared across every HTTP attempt within a single
// FetchBilling call (see AzureProvider.FetchBilling), consumed one unit at
// a time by queryClient.doWithRetry. A nil *requestBudget is treated as
// unbounded — used only by tests that are not exercising budget behavior
// specifically; every production path constructs a real one.
type requestBudget struct {
	mu        sync.Mutex
	remaining int
}

func newRequestBudget(n int) *requestBudget {
	return &requestBudget{remaining: n}
}

// take consumes one unit, reporting false once the budget is exhausted —
// the caller must then fail the whole call rather than attempting another
// HTTP request.
func (b *requestBudget) take() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining <= 0 {
		return false
	}
	b.remaining--
	return true
}
