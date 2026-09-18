package billing

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// AzureAPIError is what every failed Azure Cost Management / ARM call in
// this package returns for a non-2xx response. It never carries the raw
// response body: a body can contain internal error detail an operator
// should not see reflected back through the dashboard. Instead it carries
// exactly what a support engineer needs — the failed operation, a safe
// status classification, and Azure's own request ID for correlation.
type AzureAPIError struct {
	Operation  string
	StatusCode int
	Status     string
	RequestID  string
}

func (e *AzureAPIError) Error() string {
	return fmt.Sprintf("%s failed: %s (HTTP %d, request ID %s)", e.Operation, e.Status, e.StatusCode, e.RequestID)
}

// newAzureAPIError builds an AzureAPIError from resp without ever reading
// or retaining its body.
func newAzureAPIError(operation string, resp *http.Response) *AzureAPIError {
	return &AzureAPIError{
		Operation:  operation,
		StatusCode: resp.StatusCode,
		Status:     safeAzureStatusLabel(resp.StatusCode),
		RequestID:  azureRequestID(resp),
	}
}

// safeAzureStatusLabel maps a status code to a fixed, safe label — never
// anything derived from the response body or headers, which could contain
// vendor-specific detail not meant for display.
func safeAzureStatusLabel(code int) string {
	switch {
	case code == http.StatusUnauthorized:
		return "authentication failed"
	case code == http.StatusForbidden:
		return "authorization failed"
	case code == http.StatusNotFound:
		return "resource not found"
	case code == http.StatusTooManyRequests:
		return "throttled"
	case code >= 500:
		return "Azure service error"
	case code >= 400:
		return "request rejected"
	default:
		return "unexpected response"
	}
}

// requestIDPattern is the character set OpsCart will ever echo back from a
// response header into an error string: Azure's own request/correlation
// IDs are GUIDs or similar bounded alphanumeric tokens. Anything else
// (oversized, control characters, unexpected punctuation) is replaced with
// "unknown" rather than reflected — this header is server-controlled input
// and this package treats it accordingly, even though that server is
// Azure.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,128}$`)

func sanitizeRequestID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || !requestIDPattern.MatchString(v) {
		return "unknown"
	}
	return v
}

// azureRequestID extracts Azure's own correlation ID for this response, so
// a support engineer can look up the exact server-side request without any
// response body ever reaching the dashboard. Bounded and validated by
// sanitizeRequestID before ever being embedded in an error string.
func azureRequestID(resp *http.Response) string {
	for _, header := range []string{"x-ms-request-id", "x-ms-client-request-id"} {
		if v := resp.Header.Get(header); v != "" {
			return sanitizeRequestID(v)
		}
	}
	return "unknown"
}

// Safe error reasons: fixed, review-visible classifications for every
// non-HTTP-response failure this package can produce (a response was
// never received, or was received but rejected before/instead of
// attaching meaning to its content). Every one of these is written to
// Snapshot.UnavailableReason, which the dashboard renders directly — none
// may ever be built from a raw underlying error's literal text (hostnames,
// TLS detail, azidentity diagnostic messages, JSON parse-position
// snippets, etc.).
const (
	safeReasonAuthentication      = "authentication failed"
	safeReasonNetwork             = "network error"
	safeReasonInvalidRequest      = "invalid request"
	safeReasonInvalidResponse     = "invalid or unexpected response"
	safeReasonResponseTooLarge    = "response exceeded size limit"
	safeReasonTooManyRows         = "response exceeded row limit"
	safeReasonInvalidContinuation = "invalid pagination continuation"
	safeReasonBudgetExhausted     = "request budget exhausted for this refresh"
)

// SafeError is what every non-HTTP-response failure in this package
// returns: token acquisition, transport/network errors, malformed or
// oversized responses, invalid pagination continuations, and exhausted
// request budgets. Like AzureAPIError, Error() never includes the
// underlying cause's literal text — only a fixed, safe classification and
// the operation that failed. The cause is still reachable via Unwrap for
// any future server-side-only diagnostic use; it is never part of the
// string this type renders.
type SafeError struct {
	Operation string
	Reason    string
	cause     error
}

func (e *SafeError) Error() string {
	return fmt.Sprintf("%s failed: %s", e.Operation, e.Reason)
}

func (e *SafeError) Unwrap() error { return e.cause }

func newSafeError(operation, reason string, cause error) *SafeError {
	return &SafeError{Operation: operation, Reason: reason, cause: cause}
}

// DeferredError means a refresh attempt gave up because Azure's mandated
// retry delay would not fit within this refresh's remaining time budget
// (see nextRetryDelay in http_retry.go). NotBefore is when that mandated
// delay elapses; Runtime uses it to suppress further refresh attempts
// until then, rather than retrying on its ordinary ticker and immediately
// hitting the same throttle again. StatusCode/RequestID are preserved from
// the response that mandated the delay, when one was available.
type DeferredError struct {
	Operation  string
	RetryAfter time.Duration
	NotBefore  time.Time
	StatusCode int    // 0 when no HTTP response was available (a deferred network-error backoff)
	RequestID  string // "" when StatusCode is 0
}

func (e *DeferredError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s: Azure mandates a %s wait before retrying (HTTP %d, request ID %s), exceeding this refresh's remaining time budget; deferring until %s",
			e.Operation, e.RetryAfter.Round(time.Second), e.StatusCode, e.RequestID, e.NotBefore.Format(time.RFC3339))
	}
	return fmt.Sprintf("%s: a %s wait is required before retrying, exceeding this refresh's remaining time budget; deferring until %s",
		e.Operation, e.RetryAfter.Round(time.Second), e.NotBefore.Format(time.RFC3339))
}

// errRetryDeferred builds the DeferredError nextRetryDelay's deferred=true
// result produces, preserving resp's status/request ID when a response was
// available.
func errRetryDeferred(operation string, mandated time.Duration, resp *http.Response) *DeferredError {
	e := &DeferredError{Operation: operation, RetryAfter: mandated, NotBefore: time.Now().Add(mandated)}
	if resp != nil {
		e.StatusCode = resp.StatusCode
		e.RequestID = azureRequestID(resp)
	}
	return e
}
