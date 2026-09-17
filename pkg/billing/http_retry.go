package billing

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// maxRetryAttempts bounds every retried call in this package (Cost
// Management queries, pagination continuations, and the AKS identity
// lookup) to a small, finite number of tries — never an unbounded loop.
const maxRetryAttempts = 4

// maxRetryWait caps how long a single retry ever sleeps, whether from a
// Retry-After header or exponential backoff, so one throttled call cannot
// stall a refresh cycle indefinitely.
const maxRetryWait = 30 * time.Second

// retryableStatus reports whether resp warrants a retry under the bounded
// policy below. 429 (throttling) and 5xx (transient service errors) are
// retried; every other status, including 4xx auth/validation failures, is
// not — those are permanent until configuration or permissions change.
func retryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= 500
}

// retryDelay computes how long to wait before the next attempt (1-indexed).
// It honors a Retry-After response header (seconds, or an HTTP date) when
// present, and otherwise backs off exponentially from a 1s base, always
// bounded by maxRetryWait.
func retryDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil {
				return capDuration(time.Duration(secs) * time.Second)
			}
			if when, err := http.ParseTime(v); err == nil {
				return capDuration(time.Until(when))
			}
		}
	}
	backoff := time.Duration(1<<uint(attempt-1)) * time.Second
	return capDuration(backoff)
}

func capDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > maxRetryWait {
		return maxRetryWait
	}
	return d
}

// waitForRetry sleeps for d or returns ctx.Err() if ctx ends first — a retry
// must never outlive the caller's own bounded timeout.
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
