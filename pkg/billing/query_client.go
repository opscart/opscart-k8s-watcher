package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// errRedirectRejected is CheckRedirect's underlying error for every ARM
// http.Client this package constructs (see newARMHTTPClient). A redirect
// is never transient for these endpoints — the same redirect would happen
// again identically — so doWithRetry treats it as a permanent failure
// rather than retrying with backoff.
var errRedirectRejected = errors.New("refusing to follow redirect")

// defaultManagementEndpoint is the public Azure Commercial cloud ARM
// endpoint — the only one allowedManagementHosts (config.go) currently
// accepts; see that var's doc comment for why sovereign clouds are not yet
// supported. ClusterConfig.ManagementEndpoint exists mainly for tests to
// point this package at an httptest server.
const defaultManagementEndpoint = "https://management.azure.com"

// costManagementAPIVersion pins the Cost Management Query API contract this
// client was written against (Query - Usage, type ActualCost/AmortizedCost,
// ResourceId grouping, named columns + nextLink pagination). Bump
// deliberately, not automatically, so a contract change is a reviewed
// decision.
const costManagementAPIVersion = "2023-11-01"

// armTokenScope is the OAuth resource scope for Azure Resource Manager,
// used for both the Cost Management Query API and the AKS managedClusters
// read.
const armTokenScope = "https://management.azure.com/.default"

// maxResponseBytes bounds a single HTTP response body this package will
// ever read into memory (a Cost Management query page or the AKS
// managedClusters read). A resource-group-scoped response for a real
// cluster is at most a few thousand rows of JSON — this is generous
// headroom while still bounding worst-case memory per response,
// independent of the page/attempt/scope multipliers requestBudget bounds
// separately. A var, not a const, so tests can shrink it to exercise the
// bound without allocating a real 10 MiB response body.
var maxResponseBytes int64 = 10 << 20 // 10 MiB

// maxTotalRows bounds the rows accumulated across every page of a single
// scope (one resource-group) query — FetchBilling calls query() up to
// twice per refresh (cluster resource group, node resource group), each
// with its own independent maxTotalRows budget, so the worst case across
// one refresh is 2x this value, not this value. Two resource groups
// belonging to one AKS cluster should never approach even one scope's
// share of this; it exists only to bound worst-case memory if Azure (or a
// misconfigured endpoint) ever returned far more than expected. A var for
// the same test-tunability reason as maxResponseBytes.
var maxTotalRows = 50000

// maxPages bounds pagination per scope query — a malformed or hostile
// nextLink chain cannot loop forever — independent of maxRetryAttempts and
// requestBudget, which bound different things (retries per HTTP attempt,
// and total HTTP attempts across an entire FetchBilling call,
// respectively).
const maxPages = 20

// newARMHTTPClient returns the http.Client every production ARM call in
// this package uses. It never follows redirects: ARM does not legitimately
// redirect these endpoints, and Go's default Client.Do follows redirects
// automatically — including forwarding the Authorization header when the
// redirect target's host matches under Go's own same-domain/subdomain
// rule (https://pkg.go.dev/net/http#Client) — so silently allowing that
// would let an unexpected redirect exfiltrate a bearer token to a
// destination this package never validated.
func newARMHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("%w to %s", errRedirectRejected, req.URL.Host)
		},
	}
}

// queryClient issues Azure Cost Management Query API calls over plain
// net/http, rather than the armcostmanagement SDK's own pager: this client
// validates every pagination continuation URL's host and path before ever
// attaching an Authorization header to it (see validatedContinuationURL),
// which the SDK's generic pager does not do.
type queryClient struct {
	httpClient *http.Client
	credential azcore.TokenCredential
	endpoint   string
	apiVersion string
}

func newQueryClient(httpClient *http.Client, credential azcore.TokenCredential, endpoint string) *queryClient {
	if httpClient == nil {
		httpClient = newARMHTTPClient()
	}
	if endpoint == "" {
		endpoint = defaultManagementEndpoint
	}
	return &queryClient{
		httpClient: httpClient,
		credential: credential,
		endpoint:   strings.TrimRight(endpoint, "/"),
		apiVersion: costManagementAPIVersion,
	}
}

type queryRequestBody struct {
	Type       string       `json:"type"`
	Timeframe  string       `json:"timeframe"`
	TimePeriod queryPeriod  `json:"timePeriod"`
	Dataset    queryDataset `json:"dataset"`
}

type queryPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type queryDataset struct {
	Granularity string                    `json:"granularity"`
	Aggregation map[string]queryAggregate `json:"aggregation"`
	Grouping    []queryGrouping           `json:"grouping,omitempty"`
}

type queryAggregate struct {
	Name     string `json:"name"`
	Function string `json:"function"`
}

type queryGrouping struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// buildResourceGroupQuery builds the Query - Usage request body for a single
// resource-group scope, grouped by ResourceId so the response is a
// per-resource breakdown rather than one opaque total — required both to
// avoid double counting across the two scopes this package queries (cluster
// resource group and node resource group) and to preserve negative
// (credit) line items rather than netting them out before we can inspect
// them.
func buildResourceGroupQuery(basis CostBasis, start, end time.Time) queryRequestBody {
	return queryRequestBody{
		Type:      string(basis),
		Timeframe: "Custom",
		TimePeriod: queryPeriod{
			From: start.UTC().Format("2006-01-02T15:04:05Z"),
			To:   end.UTC().Format("2006-01-02T15:04:05Z"),
		},
		Dataset: queryDataset{
			Granularity: "None",
			Aggregation: map[string]queryAggregate{
				"totalCost": {Name: "Cost", Function: "Sum"},
			},
			Grouping: []queryGrouping{
				{Type: "Dimension", Name: "ResourceId"},
				{Type: "Dimension", Name: "ResourceGroupName"},
			},
		},
	}
}

type queryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type queryResponseBody struct {
	Properties struct {
		Columns  []queryColumn `json:"columns"`
		Rows     [][]any       `json:"rows"`
		NextLink string        `json:"nextLink"`
	} `json:"properties"`
}

// queryRow is one response row addressed by column name, never by position —
// the API does not guarantee column order, and this package's tests verify
// that a reordered response is still parsed correctly.
type queryRow map[string]any

// resourceGroupScope returns the Cost Management scope path for a
// subscription + resource group, e.g.
// /subscriptions/{sub}/resourceGroups/{rg}
func resourceGroupScope(subscriptionID, resourceGroup string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", subscriptionID, resourceGroup)
}

// query runs one Cost Management Query - Usage call at scope, following
// pagination up to maxPages, bounded by budget across every HTTP attempt
// it makes (including retries), and returns every row across all pages
// addressed by column name. operation labels every error this call
// produces (e.g. naming which resource group failed).
func (c *queryClient) query(ctx context.Context, scope string, body queryRequestBody, budget *requestBudget, operation string) ([]queryRow, error) {
	expectedPath := scope + "/providers/Microsoft.CostManagement/query"
	requestURL := c.endpoint + expectedPath + "?api-version=" + c.apiVersion
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, newSafeError(operation, safeReasonInvalidRequest, err)
	}

	var rows []queryRow
	seen := map[string]bool{requestURL: true}
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, newSafeError(operation, "exceeded page limit without terminating", nil)
		}

		respBody, statusCode, err := c.doWithRetry(ctx, http.MethodPost, requestURL, payload, operation, budget)
		if err != nil {
			return nil, err
		}
		if statusCode == http.StatusNoContent {
			respBody.Close()
			return rows, nil // 204: successful query, no matching data for this scope/period
		}

		raw, err := readBounded(respBody, operation)
		closeErr := respBody.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, newSafeError(operation, safeReasonNetwork, closeErr)
		}

		var decoded queryResponseBody
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, newSafeError(operation, safeReasonInvalidResponse, err)
		}

		pageRows, err := rowsFromColumns(decoded.Properties.Columns, decoded.Properties.Rows)
		if err != nil {
			return nil, newSafeError(operation, safeReasonInvalidResponse, err)
		}
		rows = append(rows, pageRows...)
		if len(rows) > maxTotalRows {
			return nil, newSafeError(operation, safeReasonTooManyRows, nil)
		}

		if decoded.Properties.NextLink == "" {
			return rows, nil
		}
		nextURL, err := validatedContinuationURL(decoded.Properties.NextLink, c.endpoint, expectedPath)
		if err != nil {
			return nil, newSafeError(operation, safeReasonInvalidContinuation, err)
		}
		if seen[nextURL] {
			return nil, newSafeError(operation, safeReasonInvalidContinuation, fmt.Errorf("nextLink repeated"))
		}
		seen[nextURL] = true
		requestURL = nextURL
		payload = nil // continuation requests carry no body (GET-style nextLink semantics)
	}
}

// readBounded reads at most maxResponseBytes+1 from body, returning a safe
// "response exceeded size limit" error (never the partial content) if that
// bound is exceeded.
func readBounded(body io.Reader, operation string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, newSafeError(operation, safeReasonNetwork, err)
	}
	if int64(len(raw)) > maxResponseBytes {
		return nil, newSafeError(operation, safeReasonResponseTooLarge, nil)
	}
	return raw, nil
}

// validatedContinuationURL rejects any nextLink that does not target the
// exact same host AND the exact same resource-group-scoped Cost Management
// query path as the original request, before the caller ever attaches a
// bearer token to a request against it. A same-host continuation pointing
// at a different subscription, resource group, or ARM operation must never
// be followed with this request's token — matching the host alone (the
// package's original check) is not sufficient. This is the explicit
// "validate continuation destinations before sending authorization
// headers" requirement — the reason this package does not use the
// armcostmanagement SDK's own pager, which performs neither check.
func validatedContinuationURL(nextLink, endpoint, expectedPath string) (string, error) {
	parsedNext, err := url.Parse(nextLink)
	if err != nil {
		return "", fmt.Errorf("invalid nextLink: %w", err)
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid configured endpoint: %w", err)
	}
	if parsedNext.Scheme != parsedEndpoint.Scheme {
		return "", fmt.Errorf("nextLink scheme %q does not match configured endpoint scheme %q", parsedNext.Scheme, parsedEndpoint.Scheme)
	}
	if !strings.EqualFold(parsedNext.Host, parsedEndpoint.Host) {
		return "", fmt.Errorf("nextLink host %q does not match configured endpoint host %q", parsedNext.Host, parsedEndpoint.Host)
	}
	if !strings.EqualFold(strings.TrimRight(parsedNext.Path, "/"), strings.TrimRight(expectedPath, "/")) {
		return "", fmt.Errorf("nextLink path %q is outside the queried scope %q", parsedNext.Path, expectedPath)
	}
	return nextLink, nil
}

// doWithRetry sends one request, acquiring a fresh bearer token each
// attempt, and retries a bounded number of times on 429/5xx, honoring
// whichever documented retry-after delay Azure mandates (see
// nextRetryDelay) — deferring rather than retrying early when that delay
// would not fit in ctx's remaining deadline. Every attempt (including the
// first) consumes one unit of budget; once exhausted, this fails
// immediately rather than making another request. A retryable response's
// mandated delay is preserved as a *DeferredError even when this call
// terminates for an unrelated reason — attempts or budget exhausted — so
// the caller (Runtime) never loses track of a cooldown Azure actually
// asked for. On success the caller must close the returned body. Every
// returned error is an *AzureAPIError, a *SafeError, a *DeferredError, or
// ctx's own cancellation/deadline error — never raw Azure response content
// or an unsanitized underlying error's literal text.
func (c *queryClient) doWithRetry(ctx context.Context, method, requestURL string, body []byte, operation string, budget *requestBudget) (io.ReadCloser, int, error) {
	var lastErr error
	// pendingCooldown is the most recent mandated retry delay seen from
	// any retryable response this call has received, regardless of
	// whether that attempt went on to be retried. It must survive to
	// whichever bound ends this loop first — maxRetryAttempts or the
	// shared budget — because a mandated delay is a fact about Azure's
	// server-side state, not about this call's own retry budget: losing
	// it at the boundary would let the caller (and eventually Runtime's
	// own ticker) retry again before that delay has actually elapsed.
	var pendingCooldown *DeferredError
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		if !budget.take() {
			if pendingCooldown != nil {
				return nil, 0, pendingCooldown
			}
			return nil, 0, newSafeError(operation, safeReasonBudgetExhausted, nil)
		}

		token, err := acquireToken(ctx, c.credential)
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			return nil, 0, newSafeError(operation, safeReasonAuthentication, err)
		}

		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
		if err != nil {
			return nil, 0, newSafeError(operation, safeReasonInvalidRequest, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			if errors.Is(err, errRedirectRejected) {
				// A rejected redirect is deterministic — the same
				// redirect would happen again identically — so this is a
				// permanent failure, never retried.
				return nil, 0, newSafeError(operation, safeReasonNetwork, err)
			}
			lastErr = newSafeError(operation, safeReasonNetwork, err)
			if attempt == maxRetryAttempts {
				break
			}
			delay, deferred := nextRetryDelay(ctx, nil, attempt)
			if deferred {
				return nil, 0, errRetryDeferred(operation, delay, nil)
			}
			if waitErr := waitForRetry(ctx, delay); waitErr != nil {
				return nil, 0, waitErr
			}
			continue
		}

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
			return resp.Body, resp.StatusCode, nil
		}

		// Drain without ever retaining or exposing body content: errors
		// from this call are a safe status/operation/request ID only.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		apiErr := newAzureAPIError(operation, resp)

		if !retryableStatus(resp.StatusCode) {
			return nil, 0, apiErr
		}

		// Capture the mandated delay before any terminal return below —
		// including the maxRetryAttempts case right after this — so the
		// last attempt's own cooldown (e.g. an 8-hour Retry-After on
		// attempt 4) is never silently dropped in favor of a plain
		// terminal error that carries no timing information at all.
		if mandated, ok := mandatedRetryDelay(resp); ok {
			pendingCooldown = errRetryDeferred(operation, mandated, resp)
		}

		if attempt == maxRetryAttempts {
			if pendingCooldown != nil {
				return nil, 0, pendingCooldown
			}
			return nil, 0, apiErr
		}
		delay, deferred := nextRetryDelay(ctx, resp, attempt)
		if deferred {
			return nil, 0, errRetryDeferred(operation, delay, resp)
		}
		if waitErr := waitForRetry(ctx, delay); waitErr != nil {
			return nil, 0, waitErr
		}
		lastErr = apiErr
	}
	return nil, 0, lastErr
}

// acquireToken fetches a fresh ARM-scoped bearer token. azidentity
// credentials cache internally, so calling this once per (infrequent)
// refresh cycle does not add meaningful overhead.
func acquireToken(ctx context.Context, credential azcore.TokenCredential) (string, error) {
	token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{armTokenScope}})
	if err != nil {
		return "", err
	}
	return token.Token, nil
}

// rowsFromColumns maps each raw row into a queryRow keyed by column name,
// per the Query API contract's own documented response examples showing
// columns in varying order — this function is what makes column order
// irrelevant.
func rowsFromColumns(columns []queryColumn, rawRows [][]any) ([]queryRow, error) {
	rows := make([]queryRow, 0, len(rawRows))
	for _, raw := range rawRows {
		if len(raw) != len(columns) {
			return nil, fmt.Errorf("row has %d values but response declared %d columns", len(raw), len(columns))
		}
		row := make(queryRow, len(columns))
		for i, col := range columns {
			row[col.Name] = raw[i]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (r queryRow) float64(name string) (float64, bool) {
	v, ok := r[name]
	if !ok || v == nil {
		return 0, false
	}
	n, ok := v.(float64)
	return n, ok
}

func (r queryRow) string(name string) (string, bool) {
	v, ok := r[name]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
