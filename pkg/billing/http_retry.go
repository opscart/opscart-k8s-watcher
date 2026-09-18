package billing

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRetryAttempts bounds every retried call in this package (Cost
// Management queries, pagination continuations, and the AKS identity
// lookup) to a small, finite number of tries — never an unbounded loop.
const maxRetryAttempts = 4

// maxBackoffWait caps our own exponential backoff — used only when the
// response carries none of the documented retry headers below, so there is
// no server-mandated delay to honor. It never applies to a mandated delay:
// honoring what Azure actually asked for takes priority over this
// self-imposed cap (see mandatedRetryDelay).
const maxBackoffWait = 30 * time.Second

// retryableStatus reports whether resp warrants a retry under the bounded
// policy below. 429 (throttling) and 5xx (transient service errors) are
// retried; every other status, including 4xx auth/validation failures, is
// not — those are permanent until configuration or permissions change.
func retryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= 500
}

// costManagementRetryAfterHeaders lists every header Azure documents for
// this API's own throttling, most specific first:
// https://learn.microsoft.com/azure/cost-management-billing/costs/manage-automation#response-headers
// documents costmanagement-qpu-retry-after for the Query API's QPU quota;
// the entity/tenant/client/clienttype variants are the other quota tracks
// Cost Management throttling can independently exhaust (reported
// consistently across Azure support guidance for 429 responses from this
// API). All are in seconds.
var costManagementRetryAfterHeaders = []string{
	"x-ms-ratelimit-microsoft.costmanagement-qpu-retry-after",
	"x-ms-ratelimit-microsoft.costmanagement-entity-retry-after",
	"x-ms-ratelimit-microsoft.costmanagement-tenant-retry-after",
	"x-ms-ratelimit-microsoft.costmanagement-client-retry-after",
	"x-ms-ratelimit-microsoft.costmanagement-clienttype-retry-after",
}

// mandatedRetryDelay reads every header Azure (or Cost Management
// specifically) can return to mandate a retry delay, and returns the
// longest one found. More than one quota can be exhausted at once — QPU
// and tenant, say — and each has its own independent countdown, so
// honoring only the shortest would just be throttled again by whichever
// quota is still exhausted. ok is false when resp carries none of them, in
// which case the caller has no mandated delay to honor and falls back to
// its own backoff policy.
func mandatedRetryDelay(resp *http.Response) (delay time.Duration, ok bool) {
	if resp == nil {
		return 0, false
	}
	consider := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if secs, err := strconv.Atoi(v); err == nil {
			// !ok, not "d > delay" — a genuinely mandated 0-second delay
			// must still register as found; comparing against the
			// zero-valued accumulator would otherwise treat it as absent.
			if d := time.Duration(secs) * time.Second; !ok || d > delay {
				delay, ok = d, true
			}
			return
		}
		if when, err := http.ParseTime(v); err == nil {
			if d := time.Until(when); !ok || d > delay {
				delay, ok = d, true
			}
		}
	}
	for _, header := range costManagementRetryAfterHeaders {
		consider(resp.Header.Get(header))
	}
	consider(resp.Header.Get("Retry-After"))
	return delay, ok
}

// nextRetryDelay decides how long to wait before attempt+1, and whether to
// wait at all. When resp carries a mandated delay, that delay is honored
// exactly (see mandatedRetryDelay) — never capped by maxBackoffWait — but
// checked against ctx's own deadline first: if honoring it would run past
// that deadline, retrying now would be pointless (Azure has not lifted the
// throttle yet), so this returns deferred=true instead of a delay to wait
// out. The caller must then fail this attempt immediately rather than
// sleeping toward a deadline it cannot beat, leaving the next scheduled
// refresh (governed by the cluster's own refreshInterval, not this
// package) to try again with a fresh quota window.
//
// With no mandated delay (a network error, or a 5xx with neither header),
// this falls back to our own exponential backoff, bounded by
// maxBackoffWait — an arbitrary, self-imposed cap, since Azure gave no
// explicit guidance to honor in that case.
func nextRetryDelay(ctx context.Context, resp *http.Response, attempt int) (candidate time.Duration, deferred bool) {
	mandated, ok := mandatedRetryDelay(resp)
	if !ok {
		backoff := time.Duration(1<<uint(attempt-1)) * time.Second
		if backoff > maxBackoffWait {
			backoff = maxBackoffWait
		}
		mandated = backoff
	}
	if mandated <= 0 {
		return 0, false
	}
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline && mandated > time.Until(deadline) {
		return mandated, true
	}
	return mandated, false
}

// waitForRetry sleeps for d or returns ctx.Err() if ctx ends first — a
// retry must never outlive the caller's own bounded timeout.
func waitForRetry(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
