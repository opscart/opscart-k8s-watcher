package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// defaultManagementEndpoint is the public Azure Commercial cloud ARM
// endpoint. ClusterConfig.ManagementEndpoint can override it for sovereign
// clouds or tests.
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

// queryClient issues Azure Cost Management Query API calls over plain
// net/http, rather than the armcostmanagement SDK's own pager: this client
// validates every pagination continuation URL's host before ever attaching
// an Authorization header to it (see followNextLink), which the SDK's
// generic pager does not do.
type queryClient struct {
	httpClient *http.Client
	credential azcore.TokenCredential
	endpoint   string
	apiVersion string
}

func newQueryClient(httpClient *http.Client, credential azcore.TokenCredential, endpoint string) *queryClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
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
// pagination up to a bounded number of pages, and returns every row across
// all pages addressed by column name.
func (c *queryClient) query(ctx context.Context, scope string, body queryRequestBody) ([]queryRow, error) {
	requestURL := c.endpoint + scope + "/providers/Microsoft.CostManagement/query?api-version=" + c.apiVersion
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding Cost Management query for scope %q: %w", scope, err)
	}

	var rows []queryRow
	const maxPages = 20 // bounded pagination: a malformed or hostile nextLink chain cannot loop forever
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("Cost Management query for scope %q exceeded %d pages without terminating", scope, maxPages)
		}

		respBody, statusCode, err := c.doWithRetry(ctx, http.MethodPost, requestURL, payload)
		if err != nil {
			return nil, err
		}
		if statusCode == http.StatusNoContent {
			respBody.Close()
			return rows, nil // 204: successful query, no matching data for this scope/period
		}

		var decoded queryResponseBody
		decodeErr := json.NewDecoder(respBody).Decode(&decoded)
		closeErr := respBody.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decoding Cost Management response for scope %q: %w", scope, decodeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("closing Cost Management response body for scope %q: %w", scope, closeErr)
		}

		pageRows, err := rowsFromColumns(decoded.Properties.Columns, decoded.Properties.Rows)
		if err != nil {
			return nil, fmt.Errorf("Cost Management response for scope %q: %w", scope, err)
		}
		rows = append(rows, pageRows...)

		if decoded.Properties.NextLink == "" {
			return rows, nil
		}
		nextURL, err := validatedContinuationURL(decoded.Properties.NextLink, c.endpoint)
		if err != nil {
			return nil, fmt.Errorf("Cost Management pagination for scope %q: %w", scope, err)
		}
		requestURL = nextURL
		payload = nil // continuation requests carry no body (GET-style nextLink semantics)
	}
}

// validatedContinuationURL rejects any nextLink whose scheme or host does
// not match the configured ARM endpoint, before the caller ever attaches a
// bearer token to a request against it. This is the explicit "validate
// continuation destinations before sending authorization headers"
// requirement — the reason this package does not use the armcostmanagement
// SDK's own pager, which does not perform this check.
func validatedContinuationURL(nextLink, endpoint string) (string, error) {
	parsedNext, err := url.Parse(nextLink)
	if err != nil {
		return "", fmt.Errorf("invalid nextLink %q: %w", nextLink, err)
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid configured endpoint %q: %w", endpoint, err)
	}
	if parsedNext.Scheme != "https" && parsedNext.Scheme != parsedEndpoint.Scheme {
		return "", fmt.Errorf("nextLink %q uses disallowed scheme %q", nextLink, parsedNext.Scheme)
	}
	if !strings.EqualFold(parsedNext.Host, parsedEndpoint.Host) {
		return "", fmt.Errorf("nextLink %q host %q does not match configured endpoint host %q; refusing to send credentials", nextLink, parsedNext.Host, parsedEndpoint.Host)
	}
	return nextLink, nil
}

// doWithRetry sends one request, acquiring a fresh bearer token each
// attempt, and retries a bounded number of times on 429/5xx honoring
// Retry-After. On success the caller must close the returned body.
func (c *queryClient) doWithRetry(ctx context.Context, method, requestURL string, body []byte) (io.ReadCloser, int, error) {
	var lastErr error
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		token, err := acquireToken(ctx, c.credential)
		if err != nil {
			return nil, 0, fmt.Errorf("acquiring Azure token: %w", err)
		}

		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
		if err != nil {
			return nil, 0, fmt.Errorf("building request to %q: %w", requestURL, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("calling %q: %w", requestURL, err)
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			if attempt == maxRetryAttempts {
				break
			}
			if waitErr := waitForRetry(ctx, retryDelay(nil, attempt)); waitErr != nil {
				return nil, 0, waitErr
			}
			continue
		}

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
			return resp.Body, resp.StatusCode, nil
		}

		responseSnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		lastErr = fmt.Errorf("%q returned HTTP %d: %s", requestURL, resp.StatusCode, strings.TrimSpace(string(responseSnippet)))

		if !retryableStatus(resp.StatusCode) || attempt == maxRetryAttempts {
			return nil, 0, lastErr
		}
		if waitErr := waitForRetry(ctx, retryDelay(resp, attempt)); waitErr != nil {
			return nil, 0, waitErr
		}
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
