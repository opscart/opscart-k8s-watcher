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
// scoped Kubernetes Cost Management query the Azure Portal's AKS Cost
// Analysis view issues when scoped to a subscription with a Cluster
// dimension filter. It is deliberately NOT wired into AzureProvider,
// FetchBilling, or Runtime — production billing still only ever issues
// the two resource-group-scoped queries in azure_provider.go, and
// cmd/opscart-dashboard/templates/cost.html's "AKS cluster actual cost"
// figure remains explicitly "Unavailable" until that changes.
//
// The request shape below (endpoint, cost basis, "Cluster" dimension
// filter name/operator, aggregation, and period encoding) is this spike's
// best reproduction of a payload captured from the portal. If a live
// capture shows a different filter dimension name (e.g. "ClusterName")
// or a different cost basis, update buildSubscriptionKubernetesQuery and
// its tests below — this file, not azure_provider.go, is where that
// reconciliation belongs until the shape is confirmed and a real
// productionization task brings it into the production path.
//
// Types here deliberately duplicate rather than extend query_client.go's
// production queryRequestBody/queryDataset (which have no filter support)
// — this keeps the spike fully isolated from the production request
// shape so exploring this one doesn't risk the other.

// subscriptionKubernetesQueryPeriod mirrors queryPeriod (query_client.go)
// for this spike's independent request type.
type subscriptionKubernetesQueryPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// subscriptionKubernetesQueryFilterDimension is one Cost Management
// dimension filter: Name is the dimension ("Cluster", per the portal
// capture this spike reproduces), Operator is "In", and Values holds
// exactly the one AKS cluster name being filtered to.
type subscriptionKubernetesQueryFilterDimension struct {
	Name     string   `json:"name"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

type subscriptionKubernetesQueryFilter struct {
	Dimensions subscriptionKubernetesQueryFilterDimension `json:"dimensions"`
}

type subscriptionKubernetesQueryAggregate struct {
	Name     string `json:"name"`
	Function string `json:"function"`
}

type subscriptionKubernetesQueryDataset struct {
	Granularity string                                          `json:"granularity"`
	Aggregation map[string]subscriptionKubernetesQueryAggregate `json:"aggregation"`
	Filter      subscriptionKubernetesQueryFilter               `json:"filter"`
}

type subscriptionKubernetesQueryBody struct {
	Type       string                             `json:"type"`
	Timeframe  string                             `json:"timeframe"`
	TimePeriod subscriptionKubernetesQueryPeriod  `json:"timePeriod"`
	Dataset    subscriptionKubernetesQueryDataset `json:"dataset"`
}

// buildSubscriptionKubernetesQuery reproduces the Cost Management Query
// API request captured from the Azure Portal's AKS Cost Analysis view for
// a subscription scope filtered to one AKS cluster by name:
//   - cost basis: ActualCost (matching production's default — see
//     ClusterConfig.EffectiveCostBasis)
//   - a "Cluster" dimension filter, operator "In", one value
//   - granularity "None": one aggregate total for the whole period, no
//     grouping — the filter alone scopes the result to the one cluster
//   - aggregation: Sum(Cost), same shape as buildResourceGroupQuery
func buildSubscriptionKubernetesQuery(clusterName string, start, end time.Time) subscriptionKubernetesQueryBody {
	return subscriptionKubernetesQueryBody{
		Type:      string(CostBasisActualCost),
		Timeframe: "Custom",
		TimePeriod: subscriptionKubernetesQueryPeriod{
			From: start.UTC().Format("2006-01-02T15:04:05Z"),
			To:   end.UTC().Format("2006-01-02T15:04:05Z"),
		},
		Dataset: subscriptionKubernetesQueryDataset{
			Granularity: "None",
			Aggregation: map[string]subscriptionKubernetesQueryAggregate{
				"totalCost": {Name: "Cost", Function: "Sum"},
			},
			Filter: subscriptionKubernetesQueryFilter{
				Dimensions: subscriptionKubernetesQueryFilterDimension{
					Name:     "Cluster",
					Operator: "In",
					Values:   []string{clusterName},
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
// it never contains a subscription ID, resource ID, or any request detail.
const subscriptionKubernetesQueryOperation = "subscription-scoped Kubernetes Cost Management query (spike)"

// runSubscriptionKubernetesQuery issues exactly ONE HTTP request for the
// subscription-scoped, Cluster-filtered query spike and returns its
// summed total and currency. Unlike production's queryClient.query, it
// never retries and never follows a nextLink continuation — both the
// synthetic tests below and the manually invoked diagnostic
// (subscription_kubernetes_query_diagnostic_test.go) depend on "at most
// one request" holding here.
//
// Every failure is returned as an *AzureAPIError or *SafeError (same
// types production uses) — both are already safe to print or log
// verbatim: never a token, subscription ID, resource ID, raw response
// body, or request URL.
func runSubscriptionKubernetesQuery(ctx context.Context, httpClient *http.Client, credential azcore.TokenCredential, endpoint, subscriptionID, clusterName string, start, end time.Time) (total float64, currency string, err error) {
	payload, marshalErr := json.Marshal(buildSubscriptionKubernetesQuery(clusterName, start, end))
	if marshalErr != nil {
		return 0, "", newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidRequest, marshalErr)
	}

	requestURL := strings.TrimRight(endpoint, "/") + subscriptionOnlyScope(subscriptionID) + "/providers/Microsoft.CostManagement/query?api-version=" + costManagementAPIVersion

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

	raw, readErr := readBounded(resp.Body, subscriptionKubernetesQueryOperation)
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
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	body := buildSubscriptionKubernetesQuery("my-aks-cluster", start, end)

	if body.Type != "ActualCost" {
		t.Errorf("Type = %q, want ActualCost", body.Type)
	}
	if body.Timeframe != "Custom" {
		t.Errorf("Timeframe = %q, want Custom", body.Timeframe)
	}
	if body.TimePeriod.From != "2026-09-01T00:00:00Z" {
		t.Errorf("TimePeriod.From = %q, want 2026-09-01T00:00:00Z", body.TimePeriod.From)
	}
	if body.TimePeriod.To != "2026-09-22T12:00:00Z" {
		t.Errorf("TimePeriod.To = %q, want 2026-09-22T12:00:00Z", body.TimePeriod.To)
	}
	if body.Dataset.Granularity != "None" {
		t.Errorf("Dataset.Granularity = %q, want None", body.Dataset.Granularity)
	}
	wantAgg := map[string]subscriptionKubernetesQueryAggregate{"totalCost": {Name: "Cost", Function: "Sum"}}
	if !reflect.DeepEqual(body.Dataset.Aggregation, wantAgg) {
		t.Errorf("Dataset.Aggregation = %+v, want %+v", body.Dataset.Aggregation, wantAgg)
	}
	wantFilter := subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{"my-aks-cluster"}}
	if !reflect.DeepEqual(body.Dataset.Filter.Dimensions, wantFilter) {
		t.Errorf("Dataset.Filter.Dimensions = %+v, want %+v", body.Dataset.Filter.Dimensions, wantFilter)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	wireBody := string(raw)
	for _, want := range []string{
		`"type":"ActualCost"`,
		`"timeframe":"Custom"`,
		`"from":"2026-09-01T00:00:00Z"`,
		`"to":"2026-09-22T12:00:00Z"`,
		`"granularity":"None"`,
		`"name":"Cluster"`,
		`"operator":"In"`,
		`"values":["my-aks-cluster"]`,
	} {
		if !strings.Contains(wireBody, want) {
			t.Errorf("marshaled request body missing %q; got %s", want, wireBody)
		}
	}
}

func TestSubscriptionKubernetesQuerySpikeAgainstSyntheticServer(t *testing.T) {
	const subscriptionID = "11111111-1111-1111-1111-111111111111"
	const clusterName = "my-aks-cluster"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

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

		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}},
			[][]any{{1234.56, "USD"}}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "spike-token"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	total, currency, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, subscriptionID, clusterName, start, end)
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
	if capturedQuery != "api-version="+costManagementAPIVersion {
		t.Errorf("query = %q, want api-version=%s", capturedQuery, costManagementAPIVersion)
	}
	if capturedAuth != "Bearer spike-token" {
		t.Errorf("Authorization = %q, want Bearer spike-token", capturedAuth)
	}
	if capturedBody.Type != "ActualCost" || capturedBody.Timeframe != "Custom" {
		t.Errorf("Type/Timeframe = %q/%q, want ActualCost/Custom", capturedBody.Type, capturedBody.Timeframe)
	}
	if capturedBody.TimePeriod.From != "2026-09-01T00:00:00Z" || capturedBody.TimePeriod.To != "2026-09-22T12:00:00Z" {
		t.Errorf("TimePeriod = %+v, want {2026-09-01T00:00:00Z 2026-09-22T12:00:00Z}", capturedBody.TimePeriod)
	}
	wantFilter := subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{clusterName}}
	if !reflect.DeepEqual(capturedBody.Dataset.Filter.Dimensions, wantFilter) {
		t.Errorf("Filter.Dimensions (as actually sent over HTTP) = %+v, want %+v", capturedBody.Dataset.Filter.Dimensions, wantFilter)
	}

	if total != 1234.56 {
		t.Errorf("total = %v, want 1234.56", total)
	}
	if currency != "USD" {
		t.Errorf("currency = %q, want USD", currency)
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

	_, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "sub", "cluster", time.Now(), time.Now())
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

	_, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "sub", "cluster", time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected an error when the server issues a redirect, got nil")
	}
}
