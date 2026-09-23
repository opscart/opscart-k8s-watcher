package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// This file is a TEST-ONLY compatibility spike for the Azure subscription-
// scoped Kubernetes Cost Management query captured from the Azure Portal's
// AKS Cost Analysis view. It is deliberately NOT wired into
// AzureProvider, FetchBilling, or Runtime — production billing still only
// ever issues the two resource-group-scoped queries in azure_provider.go,
// and cmd/opscart-dashboard/templates/cost.html's "AKS cluster actual
// cost" figure remains explicitly "Unavailable" until that changes.
//
// Every field below reproduces the captured request exactly:
//   - endpoint: subscription scope (no resource group), preview API
//     version 2023-04-01-preview — distinct from production's
//     costManagementAPIVersion (2023-11-01)
//   - type: "AmortizedCost" (not production's default ActualCost)
//   - a top-level "provider": "Microsoft.ContainerService" field, which
//     production's resource-group-scoped query does not send
//   - a Custom timePeriod with millisecond-precision timestamps
//     ("...000Z", not production's second-precision "...Z")
//   - dataset.aggregation serialized as the empty object {} — present,
//     not omitted
//   - dataset.sorting by Cost, Descending
//   - dataset.filter: an "or" of two Cluster-dimension "In" expressions —
//     the canonical (as-configured) cluster ARM resource ID and its
//     fully lowercased form
//   - dataset.grouping by Cluster and ResourceLocation
//
// Types here deliberately duplicate rather than extend query_client.go's
// production queryRequestBody/queryDataset (which have none of the above)
// — this keeps the spike fully isolated from the production request
// shape so exploring this one doesn't risk the other.

// subscriptionKubernetesQueryAPIVersion is the preview Cost Management API
// version the captured portal request used — distinct from production's
// costManagementAPIVersion (query_client.go).
const subscriptionKubernetesQueryAPIVersion = "2023-04-01-preview"

type subscriptionKubernetesQueryPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// subscriptionKubernetesQueryFilterDimension is one Cost Management
// dimension comparison: Name is the dimension ("Cluster", per the portal
// capture), Operator is "In", and Values holds exactly one value — either
// the canonical cluster ARM resource ID or its fully lowercased form (see
// subscriptionKubernetesQueryFilter).
type subscriptionKubernetesQueryFilterDimension struct {
	Name     string   `json:"name"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

// subscriptionKubernetesQueryFilterExpression is one leaf of the filter's
// "or" composite.
type subscriptionKubernetesQueryFilterExpression struct {
	Dimensions subscriptionKubernetesQueryFilterDimension `json:"dimensions"`
}

// subscriptionKubernetesQueryFilter is the captured request's composite
// filter: an "or" of two Cluster-dimension expressions. The captured
// request sends both the canonical (as-configured) cluster ARM resource
// ID and its fully lowercased form, rather than relying on the Cost
// Management API to compare case-insensitively.
type subscriptionKubernetesQueryFilter struct {
	Or []subscriptionKubernetesQueryFilterExpression `json:"or"`
}

type subscriptionKubernetesQueryAggregate struct {
	Name     string `json:"name"`
	Function string `json:"function"`
}

type subscriptionKubernetesQueryGrouping struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type subscriptionKubernetesQuerySort struct {
	Direction string `json:"direction"`
	Name      string `json:"name"`
}

type subscriptionKubernetesQueryDataset struct {
	Granularity string `json:"granularity"`
	// Aggregation is the empty object {} the captured request sent — not
	// omitted. A nil map marshals to JSON `null`, not `{}`; this field
	// must always be built as a non-nil, empty map (see
	// buildSubscriptionKubernetesQuery) and must never gain an
	// `omitempty` tag, or `{}` would silently become an absent field.
	Aggregation map[string]subscriptionKubernetesQueryAggregate `json:"aggregation"`
	Grouping    []subscriptionKubernetesQueryGrouping           `json:"grouping"`
	Sorting     []subscriptionKubernetesQuerySort               `json:"sorting"`
	Filter      subscriptionKubernetesQueryFilter               `json:"filter"`
}

type subscriptionKubernetesQueryBody struct {
	Type       string                             `json:"type"`
	Timeframe  string                             `json:"timeframe"`
	Provider   string                             `json:"provider"`
	TimePeriod subscriptionKubernetesQueryPeriod  `json:"timePeriod"`
	Dataset    subscriptionKubernetesQueryDataset `json:"dataset"`
}

// subscriptionKubernetesQueryTimeFormat is the millisecond-precision
// timestamp format the captured request used for timePeriod.from/to —
// distinct from production's buildResourceGroupQuery, which uses
// second-precision "2006-01-02T15:04:05Z".
const subscriptionKubernetesQueryTimeFormat = "2006-01-02T15:04:05.000Z"

// buildSubscriptionKubernetesQuery reproduces the Cost Management Query
// API request captured from the Azure Portal's AKS Cost Analysis view for
// a subscription scope filtered to one AKS cluster by its ARM resource
// ID — see this file's package doc comment for the full field-by-field
// reproduction.
func buildSubscriptionKubernetesQuery(clusterResourceID string, start, end time.Time) subscriptionKubernetesQueryBody {
	return subscriptionKubernetesQueryBody{
		Type:      string(CostBasisAmortizedCost),
		Timeframe: "Custom",
		Provider:  "Microsoft.ContainerService",
		TimePeriod: subscriptionKubernetesQueryPeriod{
			From: start.UTC().Format(subscriptionKubernetesQueryTimeFormat),
			To:   end.UTC().Format(subscriptionKubernetesQueryTimeFormat),
		},
		Dataset: subscriptionKubernetesQueryDataset{
			Granularity: "None",
			Aggregation: map[string]subscriptionKubernetesQueryAggregate{}, // {} — deliberately empty, never nil
			Grouping: []subscriptionKubernetesQueryGrouping{
				{Type: "Dimension", Name: "Cluster"},
				{Type: "Dimension", Name: "ResourceLocation"},
			},
			Sorting: []subscriptionKubernetesQuerySort{
				{Direction: "Descending", Name: "Cost"},
			},
			Filter: subscriptionKubernetesQueryFilter{
				Or: []subscriptionKubernetesQueryFilterExpression{
					{Dimensions: subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{clusterResourceID}}},
					{Dimensions: subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{strings.ToLower(clusterResourceID)}}},
				},
			},
		},
	}
}

// subscriptionOnlyScope returns the Cost Management scope path for a bare
// subscription — no resource group — the shape this spike's query uses,
// as opposed to production's per-resource-group scope (resourceGroupScope,
// query_client.go).
func subscriptionOnlyScope(subscriptionID string) string {
	return "/subscriptions/" + subscriptionID
}

// subscriptionKubernetesQueryOperation labels every error
// runSubscriptionKubernetesQuery produces, and is safe to print verbatim —
// it never contains a subscription ID, cluster resource ID, or any
// request detail.
const subscriptionKubernetesQueryOperation = "subscription-scoped Kubernetes Cost Management query (spike)"

// hasColumn reports whether the response declared a column named name
// (case-insensitively, matching the Cost Management API's own column
// lookup convention elsewhere in this package — see queryRow).
func hasColumn(columns []queryColumn, name string) bool {
	for _, c := range columns {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

// runSubscriptionKubernetesQuery issues exactly ONE HTTP request for the
// subscription-scoped, Cluster-filtered query spike and returns its
// summed total and displayed currency. Unlike production's
// queryClient.query, it never retries and never follows a nextLink
// continuation — both the synthetic tests below and the manually invoked
// diagnostic (subscription_kubernetes_query_diagnostic_test.go) depend on
// "at most one request" holding here.
//
// The response is parsed by column NAME only (hasColumn, queryRow),
// never by fixed column index, since the API does not guarantee column
// order. When the response declares a CostUSD column, that is summed and
// returned with currency "USD" (grouping by Cluster and ResourceLocation
// can return more than one row — e.g. one per region — so every row is
// summed, not just the first). Otherwise Cost is summed and the
// response's own Currency column is used, with the same
// never-silently-mix-currencies check production's aggregateRows applies.
//
// Every failure is returned as an *AzureAPIError or *SafeError (same
// types production uses) — both are already safe to print or log
// verbatim: never a token, subscription ID, cluster resource ID, raw
// response body, or request URL.
func runSubscriptionKubernetesQuery(ctx context.Context, httpClient *http.Client, credential azcore.TokenCredential, endpoint, subscriptionID, clusterResourceID string, start, end time.Time) (total float64, currency string, err error) {
	payload, marshalErr := json.Marshal(buildSubscriptionKubernetesQuery(clusterResourceID, start, end))
	if marshalErr != nil {
		return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidRequest, marshalErr)
	}

	requestURL := strings.TrimRight(endpoint, "/") + subscriptionOnlyScope(subscriptionID) + "/providers/Microsoft.CostManagement/query?api-version=" + subscriptionKubernetesQueryAPIVersion

	token, tokenErr := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{armTokenScope}})
	if tokenErr != nil {
		return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonAuthentication, tokenErr)
	}

	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if reqErr != nil {
		return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidRequest, reqErr)
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, doErr := httpClient.Do(req) // the one and only request this call ever makes
	if doErr != nil {
		return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonNetwork, doErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, "", newAzureAPIError(subscriptionKubernetesQueryOperation, resp)
	}

	raw, readErr := readBounded(resp.Body, subscriptionKubernetesQueryOperation) // bounded: see maxResponseBytes, query_client.go
	if readErr != nil {
		return 0, "", readErr
	}

	var decoded queryResponseBody
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, err)
	}
	rows, err := rowsFromColumns(decoded.Properties.Columns, decoded.Properties.Rows)
	if err != nil {
		return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, err)
	}

	if hasColumn(decoded.Properties.Columns, "CostUSD") {
		for _, row := range rows {
			v, ok := row.float64("CostUSD")
			if !ok {
				return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("row missing CostUSD value"))
			}
			total += v
		}
		return total, "USD", nil
	}

	for _, row := range rows {
		cost, ok := row.float64("Cost")
		if !ok {
			return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("missing Cost column"))
		}
		rowCurrency, ok := row.string("Currency")
		if !ok || rowCurrency == "" {
			return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("missing Currency column"))
		}
		if currency == "" {
			currency = rowCurrency
		} else if !strings.EqualFold(currency, rowCurrency) {
			return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("response mixes currencies"))
		}
		total += cost
	}
	return total, currency, nil
}

func TestBuildSubscriptionKubernetesQueryPayloadShape(t *testing.T) {
	const clusterResourceID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"
	lowercased := strings.ToLower(clusterResourceID)

	start := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC)

	body := buildSubscriptionKubernetesQuery(clusterResourceID, start, end)

	if body.Type != "AmortizedCost" {
		t.Errorf("Type = %q, want AmortizedCost", body.Type)
	}
	if body.Timeframe != "Custom" {
		t.Errorf("Timeframe = %q, want Custom", body.Timeframe)
	}
	if body.Provider != "Microsoft.ContainerService" {
		t.Errorf("Provider = %q, want Microsoft.ContainerService", body.Provider)
	}
	if body.TimePeriod.From != "2026-08-17T00:00:00.000Z" {
		t.Errorf("TimePeriod.From = %q, want 2026-08-17T00:00:00.000Z (millisecond precision)", body.TimePeriod.From)
	}
	if body.TimePeriod.To != "2026-09-15T23:59:59.000Z" {
		t.Errorf("TimePeriod.To = %q, want 2026-09-15T23:59:59.000Z (millisecond precision)", body.TimePeriod.To)
	}
	if body.Dataset.Granularity != "None" {
		t.Errorf("Dataset.Granularity = %q, want None", body.Dataset.Granularity)
	}
	if body.Dataset.Aggregation == nil {
		t.Error("Dataset.Aggregation is nil, want a non-nil empty map (must serialize as {}, not null)")
	}
	if len(body.Dataset.Aggregation) != 0 {
		t.Errorf("Dataset.Aggregation = %+v, want empty", body.Dataset.Aggregation)
	}
	wantGrouping := []subscriptionKubernetesQueryGrouping{
		{Type: "Dimension", Name: "Cluster"},
		{Type: "Dimension", Name: "ResourceLocation"},
	}
	if !reflect.DeepEqual(body.Dataset.Grouping, wantGrouping) {
		t.Errorf("Dataset.Grouping = %+v, want %+v", body.Dataset.Grouping, wantGrouping)
	}
	wantSorting := []subscriptionKubernetesQuerySort{{Direction: "Descending", Name: "Cost"}}
	if !reflect.DeepEqual(body.Dataset.Sorting, wantSorting) {
		t.Errorf("Dataset.Sorting = %+v, want %+v", body.Dataset.Sorting, wantSorting)
	}
	if len(body.Dataset.Filter.Or) != 2 {
		t.Fatalf("Dataset.Filter.Or has %d expressions, want 2", len(body.Dataset.Filter.Or))
	}
	wantFirst := subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{clusterResourceID}}
	if !reflect.DeepEqual(body.Dataset.Filter.Or[0].Dimensions, wantFirst) {
		t.Errorf("Dataset.Filter.Or[0].Dimensions = %+v, want %+v (canonical ARM ID)", body.Dataset.Filter.Or[0].Dimensions, wantFirst)
	}
	wantSecond := subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{lowercased}}
	if !reflect.DeepEqual(body.Dataset.Filter.Or[1].Dimensions, wantSecond) {
		t.Errorf("Dataset.Filter.Or[1].Dimensions = %+v, want %+v (fully lowercased form)", body.Dataset.Filter.Or[1].Dimensions, wantSecond)
	}
	if body.Dataset.Filter.Or[0].Dimensions.Values[0] == body.Dataset.Filter.Or[1].Dimensions.Values[0] {
		t.Error("both filter expressions carry the same value — the fixture cluster ID must contain letters to actually differ when lowercased")
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	wireBody := string(raw)
	for _, want := range []string{
		`"type":"AmortizedCost"`,
		`"timeframe":"Custom"`,
		`"provider":"Microsoft.ContainerService"`,
		`"from":"2026-08-17T00:00:00.000Z"`,
		`"to":"2026-09-15T23:59:59.000Z"`,
		`"granularity":"None"`,
		`"aggregation":{}`, // present and empty — not omitted, not null
		`"grouping":[{"type":"Dimension","name":"Cluster"},{"type":"Dimension","name":"ResourceLocation"}]`,
		`"sorting":[{"direction":"Descending","name":"Cost"}]`,
		`"or":[{"dimensions":{"name":"Cluster","operator":"In","values":["` + clusterResourceID + `"]}},{"dimensions":{"name":"Cluster","operator":"In","values":["` + lowercased + `"]}}]`,
	} {
		if !strings.Contains(wireBody, want) {
			t.Errorf("marshaled request body missing %q; got %s", want, wireBody)
		}
	}
	if strings.Contains(wireBody, `"aggregation":null`) {
		t.Error("aggregation must never marshal as null")
	}
}

func TestSubscriptionKubernetesQuerySpikeAgainstSyntheticServer(t *testing.T) {
	const subscriptionID = "11111111-1111-1111-1111-111111111111"
	const clusterResourceID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"
	start := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC)

	var (
		requestCount  int
		capturedPath  string
		capturedQuery string
		capturedAuth  string
		capturedBody  subscriptionKubernetesQueryBody
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		capturedAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)

		// Grouping by Cluster and ResourceLocation can return more than
		// one row (e.g. one per region) — two rows here, both with a
		// CostUSD column, proving summation across rows and the
		// CostUSD-preferred branch.
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "CostUSD"}, {Name: "Currency"}, {Name: "Cluster"}, {Name: "ResourceLocation"}},
			[][]any{
				{1000.0, 1100.0, "EUR", clusterResourceID, "eastus2"},
				{200.0, 220.0, "EUR", clusterResourceID, "westus2"},
			}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "spike-token"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	total, currency, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, subscriptionID, clusterResourceID, start, end)
	if err != nil {
		t.Fatalf("runSubscriptionKubernetesQuery: %v", err)
	}

	if requestCount != 1 {
		t.Fatalf("server received %d requests, want exactly 1", requestCount)
	}
	wantPath := "/subscriptions/" + subscriptionID + "/providers/Microsoft.CostManagement/query"
	if capturedPath != wantPath {
		t.Errorf("path = %q, want %q (subscription scope, no resourceGroups segment)", capturedPath, wantPath)
	}
	if capturedQuery != "api-version="+subscriptionKubernetesQueryAPIVersion {
		t.Errorf("query = %q, want api-version=%s (preview version)", capturedQuery, subscriptionKubernetesQueryAPIVersion)
	}
	if capturedAuth != "Bearer spike-token" {
		t.Errorf("Authorization = %q, want Bearer spike-token", capturedAuth)
	}
	if capturedBody.Type != "AmortizedCost" || capturedBody.Provider != "Microsoft.ContainerService" {
		t.Errorf("Type/Provider (as actually sent over HTTP) = %q/%q, want AmortizedCost/Microsoft.ContainerService", capturedBody.Type, capturedBody.Provider)
	}
	if len(capturedBody.Dataset.Filter.Or) != 2 {
		t.Errorf("Filter.Or (as actually sent over HTTP) has %d expressions, want 2", len(capturedBody.Dataset.Filter.Or))
	}

	// CostUSD preferred and summed across both rows: 1100 + 220.
	if total != 1320 {
		t.Errorf("total = %v, want 1320 (CostUSD summed across both rows)", total)
	}
	if currency != "USD" {
		t.Errorf("currency = %q, want USD (CostUSD present, so USD is forced regardless of the row Currency column)", currency)
	}
}

// TestSubscriptionKubernetesQuerySpikeFallsBackToCostAndCurrencyWithoutCostUSD
// proves the "otherwise use Cost plus the response currency" fallback,
// summed across every row returned by the Cluster/ResourceLocation
// grouping, and that mismatched currencies across rows are rejected
// rather than silently summed.
func TestSubscriptionKubernetesQuerySpikeFallsBackToCostAndCurrencyWithoutCostUSD(t *testing.T) {
	const clusterResourceID = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "Cluster"}, {Name: "ResourceLocation"}},
			[][]any{
				{300.0, "EUR", clusterResourceID, "eastus2"},
				{45.5, "EUR", clusterResourceID, "westus2"},
			}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	total, currency, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "s", clusterResourceID, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("runSubscriptionKubernetesQuery: %v", err)
	}
	if total != 345.5 {
		t.Errorf("total = %v, want 345.5 (Cost summed across both rows)", total)
	}
	if currency != "EUR" {
		t.Errorf("currency = %q, want EUR (response's own Currency column, no CostUSD present)", currency)
	}
}

func TestSubscriptionKubernetesQuerySpikeRejectsMixedCurrenciesWithoutCostUSD(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}},
			[][]any{{100.0, "EUR"}, {50.0, "USD"}}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "s", "cluster-id", time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected an error for mixed currencies with no CostUSD column, got nil")
	}
}

// TestSubscriptionKubernetesQuerySpikeMakesExactlyOneRequestOnFailure
// proves the spike never retries — even against a 429 (Too Many Requests)
// response, which production's queryClient.query would retry with
// backoff. "At most one request" is a hard requirement for the manually
// invoked diagnostic that reuses this same call.
func TestSubscriptionKubernetesQuerySpikeMakesExactlyOneRequestOnFailure(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "sub", "cluster-id", time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected an error for the 429 response, got nil")
	}
	if requestCount != 1 {
		t.Fatalf("server received %d requests, want exactly 1 (no retry)", requestCount)
	}
	var apiErr *AzureAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *AzureAPIError", err, err)
	}
	if apiErr.Status != "throttled" {
		t.Errorf("Status = %q, want throttled", apiErr.Status)
	}
}

// TestSubscriptionKubernetesQuerySpikeRejectsRedirect proves the spike's
// HTTP client (newARMHTTPClient, shared with production) refuses to
// follow a redirect rather than silently forwarding the Authorization
// header to an unvalidated destination.
func TestSubscriptionKubernetesQuerySpikeRejectsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example/", http.StatusFound)
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "sub", "cluster-id", time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected an error when the server issues a redirect, got nil")
	}
}
