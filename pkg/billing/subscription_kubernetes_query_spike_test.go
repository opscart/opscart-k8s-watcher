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
	"regexp"
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
// A live run against a real subscription (see the manual diagnostic,
// subscription_kubernetes_query_diagnostic_test.go) found: the request
// succeeded (correct auth, endpoint, period, and response decoding all
// work), the response declared columns Cluster, ResourceLocation, Cost,
// Currency, and it returned zero rows — the composite "and" Cluster
// filter matched nothing. This file now supports two filter
// representations so that can be narrowed down without ever guessing
// blind against the real API:
//
//   - subscriptionKubernetesQueryFilterModeCapturedAnd: the exact shape
//     captured from the portal — an "and" of two separate Cluster/In
//     expressions, one per case variant.
//   - subscriptionKubernetesQueryFilterModeCombinedValues: a single
//     Cluster/In dimensions expression carrying both case variants in one
//     values array, in case the API rejects (or silently empties) a
//     composite "and" over the same dimension.
//   - subscriptionKubernetesQueryFilterModeDiscovery: omits dataSet.filter
//     entirely, so the API returns every Cluster dimension value it is
//     willing to report for this subscription/period. A live run found
//     both captured-and and combined-values were accepted but returned
//     zero rows; discovery mode exists to determine, without guessing
//     blind, whether Azure exposes Cluster rows at all here and whether
//     their shape matches the configured AKS ARM ID — see
//     runSubscriptionKubernetesDiscoveryQuery. Every Cluster value it
//     examines is used only transiently, in memory, to compute safe
//     counts; none is ever printed, logged, returned in an error, or
//     retained (not even as a hash) — see that function's doc comment.
//
// Every other field below still reproduces the captured request exactly:
//   - endpoint: subscription scope (no resource group), preview API
//     version 2023-04-01-preview — distinct from production's
//     costManagementAPIVersion (2023-11-01)
//   - type: "AmortizedCost" (not production's default ActualCost)
//   - a top-level "provider": "Microsoft.ContainerService" field, which
//     production's resource-group-scoped query does not send
//   - the top-level dataset field is spelled "dataSet" (capital S), not
//     production's "dataset"
//   - a Custom timePeriod with millisecond-precision timestamps
//     ("...000Z", not production's second-precision "...Z")
//   - dataSet.aggregation serialized as the empty object {} — present,
//     not omitted
//   - dataSet.sorting by Cost, lowercase "descending"
//   - dataSet.grouping by Cluster then ResourceLocation, in that order
//
// Types here deliberately duplicate rather than extend query_client.go's
// production queryRequestBody/queryDataset (which have none of the above)
// — this keeps the spike fully isolated from the production request
// shape so exploring this one doesn't risk the other.
//
// No real corporate subscription, resource group, or cluster value
// appears anywhere in this file — every fixture uses an obviously
// synthetic placeholder (e.g. "11111111-1111-1111-1111-111111111111",
// "My-RG", "My-AKS-Cluster").

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
// capture), Operator is "In", and Values holds either one value (the
// captured-and mode's per-expression case variant) or both case variants
// together (the combined-values mode's single expression).
type subscriptionKubernetesQueryFilterDimension struct {
	Name     string   `json:"name"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

// subscriptionKubernetesQueryFilterExpression is one leaf of the
// captured-and filter's "and" composite, and is also, on its own, the
// entire filter body for combined-values mode (see
// buildSubscriptionKubernetesQueryFilter).
type subscriptionKubernetesQueryFilterExpression struct {
	Dimensions subscriptionKubernetesQueryFilterDimension `json:"dimensions"`
}

// subscriptionKubernetesQueryFilter is the captured-and mode's composite
// filter: an "and" (not "or") of two Cluster-dimension expressions. The
// captured request sends both the canonical (as-configured) cluster ARM
// resource ID and its fully lowercased form, rather than relying on the
// Cost Management API to compare case-insensitively.
type subscriptionKubernetesQueryFilter struct {
	And []subscriptionKubernetesQueryFilterExpression `json:"and"`
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

// subscriptionKubernetesQueryDataset's Filter is json.RawMessage rather
// than a fixed struct type: the two filter modes (captured-and,
// combined-values) marshal to genuinely different JSON shapes — one keyed
// by "and", the other a single "dimensions" object — and RawMessage lets
// buildSubscriptionKubernetesQueryFilter produce either without an
// interface{} field forcing every reader to type-switch. A caller that
// wants structural assertions unmarshal Filter into whichever concrete
// type its filter mode implies (see the tests below); the raw bytes are
// always available for literal wire-shape checks.
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
	// Filter's "omitempty" matters only for discovery mode, whose
	// buildSubscriptionKubernetesQueryFilter returns a nil RawMessage —
	// without omitempty a nil []byte still marshals as the JSON literal
	// `null`, which is not the same as omitting the property entirely.
	Filter json.RawMessage `json:"filter,omitempty"`
}

// subscriptionKubernetesQueryBody's Dataset field is deliberately tagged
// "dataSet" (capital S) — the captured request's literal top-level key,
// distinct from production's "dataset" (query_client.go's
// queryRequestBody).
type subscriptionKubernetesQueryBody struct {
	Type       string                             `json:"type"`
	Timeframe  string                             `json:"timeframe"`
	Provider   string                             `json:"provider"`
	TimePeriod subscriptionKubernetesQueryPeriod  `json:"timePeriod"`
	Dataset    subscriptionKubernetesQueryDataset `json:"dataSet"`
}

// subscriptionKubernetesQueryTimeFormat is the millisecond-precision
// timestamp format the captured request used for timePeriod.from/to —
// distinct from production's buildResourceGroupQuery, which uses
// second-precision "2006-01-02T15:04:05Z".
const subscriptionKubernetesQueryTimeFormat = "2006-01-02T15:04:05.000Z"

// subscriptionKubernetesQueryFilterMode selects which of the two filter
// shapes buildSubscriptionKubernetesQuery sends. There is no default and
// no automatic fallback from one to the other — see
// runSubscriptionKubernetesQuery's doc comment and
// TestSubscriptionKubernetesQuerySpikeEachFilterModeMakesOneRequestAndReportsNoDataOnZeroRows.
type subscriptionKubernetesQueryFilterMode string

const (
	// subscriptionKubernetesQueryFilterModeCapturedAnd is the exact shape
	// captured from the Azure Portal: dataSet.filter.and, two Cluster/In
	// expressions.
	subscriptionKubernetesQueryFilterModeCapturedAnd subscriptionKubernetesQueryFilterMode = "captured-and"
	// subscriptionKubernetesQueryFilterModeCombinedValues is a single
	// Cluster/In dimensions expression carrying both case variants in one
	// values array — no "and"/"or" composite at all.
	subscriptionKubernetesQueryFilterModeCombinedValues subscriptionKubernetesQueryFilterMode = "combined-values"
	// subscriptionKubernetesQueryFilterModeDiscovery omits dataSet.filter
	// entirely — see runSubscriptionKubernetesDiscoveryQuery.
	subscriptionKubernetesQueryFilterModeDiscovery subscriptionKubernetesQueryFilterMode = "discovery"
)

// buildSubscriptionKubernetesQueryFilter builds the dataSet.filter bytes
// for the given mode. clusterResourceID is the canonical (as-configured)
// AKS cluster ARM resource ID; its fully lowercased form is always the
// second value/expression, never a separately-configured value, so the
// two can never drift apart.
func buildSubscriptionKubernetesQueryFilter(clusterResourceID string, filterMode subscriptionKubernetesQueryFilterMode) (json.RawMessage, error) {
	lowercased := strings.ToLower(clusterResourceID)
	switch filterMode {
	case subscriptionKubernetesQueryFilterModeCapturedAnd:
		return json.Marshal(subscriptionKubernetesQueryFilter{
			And: []subscriptionKubernetesQueryFilterExpression{
				{Dimensions: subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{clusterResourceID}}},
				{Dimensions: subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{lowercased}}},
			},
		})
	case subscriptionKubernetesQueryFilterModeCombinedValues:
		return json.Marshal(subscriptionKubernetesQueryFilterExpression{
			Dimensions: subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{clusterResourceID, lowercased}},
		})
	case subscriptionKubernetesQueryFilterModeDiscovery:
		// nil, nil: the dataSet.filter property is omitted entirely (see
		// subscriptionKubernetesQueryDataset.Filter's "omitempty" tag),
		// not sent as an empty object or null. clusterResourceID is
		// unused for request construction in this mode — it is used only
		// afterward, in memory, to match against whatever Cluster values
		// the unfiltered response returns (runSubscriptionKubernetesDiscoveryQuery).
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown filter mode %q: use %q, %q, or %q", filterMode, subscriptionKubernetesQueryFilterModeCapturedAnd, subscriptionKubernetesQueryFilterModeCombinedValues, subscriptionKubernetesQueryFilterModeDiscovery)
	}
}

// buildSubscriptionKubernetesQuery reproduces the Cost Management Query
// API request captured from the Azure Portal's AKS Cost Analysis view for
// a subscription scope filtered to one AKS cluster by its ARM resource
// ID — see this file's package doc comment for the full field-by-field
// reproduction, and buildSubscriptionKubernetesQueryFilter for the two
// supported filter shapes.
func buildSubscriptionKubernetesQuery(clusterResourceID string, start, end time.Time, filterMode subscriptionKubernetesQueryFilterMode) (subscriptionKubernetesQueryBody, error) {
	filter, err := buildSubscriptionKubernetesQueryFilter(clusterResourceID, filterMode)
	if err != nil {
		return subscriptionKubernetesQueryBody{}, err
	}
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
				{Direction: "descending", Name: "Cost"},
			},
			Filter: filter,
		},
	}, nil
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

// subscriptionKubernetesQueryNoDataError means the Cost Management API
// returned a successful (HTTP 200) response with zero rows. This is
// distinct from a real zero-cost result (one or more rows summing to
// zero, see TestSubscriptionKubernetesQuerySpikeZeroCostRowIsValid) — a
// caller must never treat "no rows at all" as "$0 total, status ok",
// since that would silently hide a scoping/filter mistake (e.g. a Cluster
// filter that matched nothing, as the live run that motivated this file's
// two filter modes found) behind an apparently valid zero total.
// It carries no dynamic content, so it is always safe to print verbatim.
type subscriptionKubernetesQueryNoDataError struct{}

func (e *subscriptionKubernetesQueryNoDataError) Error() string {
	return subscriptionKubernetesQueryOperation + ": succeeded with zero rows (no data for the requested period/cluster/filter mode)"
}

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

// columnNamesOf returns just the declared column names from columns —
// response schema metadata, never row values — safe for the manual
// diagnostic to print (see subscription_kubernetes_query_diagnostic_test.go).
func columnNamesOf(columns []queryColumn) []string {
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = c.Name
	}
	return names
}

// runSubscriptionKubernetesQuery issues exactly ONE HTTP request, for
// exactly the one filterMode passed in, and returns its summed total,
// displayed currency, the number of rows the response declared, and the
// response's declared column names (schema only — never row values).
// There is no fallback: if the caller wants to compare filter modes, it
// must call this twice with two different filterMode values and two
// separate, deliberate invocations — this function itself never tries a
// second shape after the first, on any outcome (see
// TestSubscriptionKubernetesQuerySpikeEachFilterModeMakesOneRequestAndReportsNoDataOnZeroRows).
// Unlike production's queryClient.query, it also never retries and never
// follows a nextLink continuation — both the synthetic tests below and
// the manually invoked diagnostic
// (subscription_kubernetes_query_diagnostic_test.go) depend on "at most
// one request" holding here.
//
// rowCount/columnNames are populated as soon as the response body is
// successfully decoded, even when a later validation step (zero rows, or
// a missing Cost/CostUSD column) causes this to return an error — that
// lets a caller report *why* without ever printing a response value.
//
// The response is parsed by column NAME only (hasColumn, queryRow), never
// by fixed column index, since the API does not guarantee column order.
// A response with zero rows returns *subscriptionKubernetesQueryNoDataError
// — never a $0 "success". When the response declares a CostUSD column,
// that is summed and returned with currency "USD" (grouping by Cluster
// and ResourceLocation can return more than one row — e.g. one per region
// — so every row is summed, not just the first). Otherwise Cost is summed
// and the response's own Currency column is used, with the same
// never-silently-mix-currencies check production's aggregateRows applies.
// A row whose Cost is legitimately 0 (with a valid currency) is a normal,
// valid result — it is not confused with "no rows returned".
//
// Every failure is returned as an *AzureAPIError, *SafeError (same types
// production uses), or *subscriptionKubernetesQueryNoDataError — all
// three are already safe to print or log verbatim: never a token,
// subscription ID, cluster resource ID, raw response body, or request
// URL.
func runSubscriptionKubernetesQuery(ctx context.Context, httpClient *http.Client, credential azcore.TokenCredential, endpoint, subscriptionID, clusterResourceID string, start, end time.Time, filterMode subscriptionKubernetesQueryFilterMode) (total float64, currency string, rowCount int, columnNames []string, err error) {
	body, buildErr := buildSubscriptionKubernetesQuery(clusterResourceID, start, end, filterMode)
	if buildErr != nil {
		return 0, "", 0, nil, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidRequest, buildErr)
	}
	payload, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return 0, "", 0, nil, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidRequest, marshalErr)
	}

	requestURL := strings.TrimRight(endpoint, "/") + subscriptionOnlyScope(subscriptionID) + "/providers/Microsoft.CostManagement/query?api-version=" + subscriptionKubernetesQueryAPIVersion

	token, tokenErr := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{armTokenScope}})
	if tokenErr != nil {
		return 0, "", 0, nil, newSafeError(subscriptionKubernetesQueryOperation, safeReasonAuthentication, tokenErr)
	}

	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if reqErr != nil {
		return 0, "", 0, nil, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidRequest, reqErr)
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, doErr := httpClient.Do(req) // the one and only request this call ever makes, for this one filterMode
	if doErr != nil {
		return 0, "", 0, nil, newSafeError(subscriptionKubernetesQueryOperation, safeReasonNetwork, doErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, "", 0, nil, newAzureAPIError(subscriptionKubernetesQueryOperation, resp)
	}

	raw, readErr := readBounded(resp.Body, subscriptionKubernetesQueryOperation) // bounded: see maxResponseBytes, query_client.go
	if readErr != nil {
		return 0, "", 0, nil, readErr
	}

	var decoded queryResponseBody
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return 0, "", 0, nil, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, err)
	}
	columnNames = columnNamesOf(decoded.Properties.Columns)

	rows, rowsErr := rowsFromColumns(decoded.Properties.Columns, decoded.Properties.Rows)
	if rowsErr != nil {
		return 0, "", 0, columnNames, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, rowsErr)
	}
	rowCount = len(rows)

	if rowCount == 0 {
		return 0, "", 0, columnNames, &subscriptionKubernetesQueryNoDataError{}
	}

	hasCostUSD := hasColumn(decoded.Properties.Columns, "CostUSD")
	hasCost := hasColumn(decoded.Properties.Columns, "Cost")
	if !hasCostUSD && !hasCost {
		return 0, "", rowCount, columnNames, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("response declares neither a Cost nor a CostUSD column"))
	}

	if hasCostUSD {
		for _, row := range rows {
			v, ok := row.float64("CostUSD")
			if !ok {
				return 0, "", rowCount, columnNames, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("row missing a usable CostUSD value"))
			}
			total += v
		}
		return total, "USD", rowCount, columnNames, nil
	}

	for _, row := range rows {
		cost, ok := row.float64("Cost")
		if !ok {
			return 0, "", rowCount, columnNames, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("row missing a usable Cost value"))
		}
		rowCurrency, ok := row.string("Currency")
		if !ok || rowCurrency == "" {
			return 0, "", rowCount, columnNames, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("missing Currency column"))
		}
		if currency == "" {
			currency = rowCurrency
		} else if !strings.EqualFold(currency, rowCurrency) {
			return 0, "", rowCount, columnNames, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("response mixes currencies"))
		}
		total += cost
	}
	return total, currency, rowCount, columnNames, nil
}

// subscriptionKubernetesQueryMaxDiscoveryRows bounds how many rows
// runSubscriptionKubernetesDiscoveryQuery will process. A discovery
// response — no filter at all — could in principle return every
// Kubernetes-cost-bearing cluster in the subscription; this is a
// defensive ceiling, not an expected count, and a response exceeding it
// is rejected outright rather than partially processed.
const subscriptionKubernetesQueryMaxDiscoveryRows = 1000

// subscriptionKubernetesQueryDiscoveryNoMatchError means discovery
// completed successfully (a bounded number of rows, each with a usable
// Cluster/Cost-or-CostUSD value) but none of them safely matched the
// configured target cluster ARM resource ID — not even after
// case-insensitive or normalized-ARM comparison. This is distinct from
// subscriptionKubernetesQueryNoDataError (zero rows at all): here, other
// clusters' cost data exists in the subscription, just not a safe match
// for the configured one. It carries no dynamic content — never a
// cluster ID examined during matching — so it is always safe to print
// verbatim.
type subscriptionKubernetesQueryDiscoveryNoMatchError struct{}

func (e *subscriptionKubernetesQueryDiscoveryNoMatchError) Error() string {
	return subscriptionKubernetesQueryOperation + ": discovery found rows but none safely matched the configured target (see the match counts)"
}

// armResourceIDShapePattern is a generic "looks like a full Azure ARM
// resource ID" check (/subscriptions/{x}/resourceGroups/{y}/providers/{...})
// used only to COUNT how many discovered Cluster dimension values have
// this shape. It never extracts, logs, or retains any matched value —
// only the count of matches is ever surfaced.
var armResourceIDShapePattern = regexp.MustCompile(`(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/[^/]+(?:/[^/]+)+$`)

// normalizeARMResourceIDForComparison applies only the normalizations the
// matching spec allows: trim surrounding whitespace, trim one trailing
// slash, and lowercase for a case-insensitive compare. It deliberately
// never rewrites the subscription, resource group, or provider segments
// themselves — this is a comparison aid, not a canonicalizer.
func normalizeARMResourceIDForComparison(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimSuffix(id, "/")
	return strings.ToLower(id)
}

// finalResourceNameSegment returns the last "/"-separated segment of id —
// the managedClusters resource's own name, with no subscription/resource-
// group/provider context. Used only to COUNT a name-only match (matching
// spec tier 4); the segment itself is never returned, printed, or
// retained beyond this function's local, transient comparison.
func finalResourceNameSegment(id string) string {
	id = strings.TrimSuffix(strings.TrimSpace(id), "/")
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		return id[idx+1:]
	}
	return id
}

// subscriptionKubernetesDiscoveryResult is every value discovery mode is
// allowed to compute and surface — see this package's file-level doc
// comment and the "Safe output" list in the task this file implements.
// Every field is a count, a schema name, a currency code, or a matched
// total: never a cluster ID, a resource-group name, a subscription ID, or
// a raw row. HasSafeMatch distinguishes "matched, and the total is
// legitimately 0" from "no safe match at all" (see
// subscriptionKubernetesQueryDiscoveryNoMatchError) without relying on a
// zero-value sentinel in MatchedTotal.
type subscriptionKubernetesDiscoveryResult struct {
	RowCount                   int
	ColumnNames                []string
	UniqueClusterValueCount    int
	ARMShapedClusterValueCount int
	ExactMatchCount            int
	CaseInsensitiveMatchCount  int
	NormalizedARMMatchCount    int
	NameOnlyMatchCount         int
	HasSafeMatch               bool
	MatchedTotal               float64
	MatchedCurrency            string
}

// runSubscriptionKubernetesDiscoveryQuery issues exactly ONE HTTP request
// — the discovery-mode query (see buildSubscriptionKubernetesQueryFilter),
// with dataSet.filter omitted entirely — and returns only the safe,
// aggregate information listed on subscriptionKubernetesDiscoveryResult.
// Like runSubscriptionKubernetesQuery, it never retries, never follows a
// nextLink continuation, and never falls back to a different filter mode
// on any outcome.
//
// Every Cluster dimension value the response declares is examined only
// transiently, inside this function's row loop, for shape and equality
// checks (see armResourceIDShapePattern, normalizeARMResourceIDForComparison,
// finalResourceNameSegment) — no value is ever appended to a slice
// outside that loop, logged, hashed, returned in an error, or included in
// the returned result. A response reporting more than
// subscriptionKubernetesQueryMaxDiscoveryRows rows is rejected outright,
// defensively, before any row is examined for matching.
//
// Matching runs four independent, non-exclusive checks per row — exact
// string equality, strings.EqualFold, normalized-ARM equality, and a
// final-resource-name-only comparison — and counts each separately so an
// operator can tell which normalization (if any) would be needed. Only
// the first three ("safe" full-ID matches, at increasingly forgiving
// normalization) ever contribute to MatchedTotal: a name-only match alone
// never proves cluster identity (the same reasoning that already governs
// this package's resource-group-scope attribution — a namespace-like name
// is not ownership) and is reported only as a count. When the response
// declares a CostUSD column, matched rows are summed in USD; otherwise
// Cost is summed using the response's own Currency column, with the same
// never-silently-mix-currencies check runSubscriptionKubernetesQuery
// applies. If no row is a safe match, this returns
// *subscriptionKubernetesQueryDiscoveryNoMatchError — a legitimate
// zero-cost safe match is never confused with "no match" (HasSafeMatch
// distinguishes them).
func runSubscriptionKubernetesDiscoveryQuery(ctx context.Context, httpClient *http.Client, credential azcore.TokenCredential, endpoint, subscriptionID, clusterResourceID string, start, end time.Time) (subscriptionKubernetesDiscoveryResult, error) {
	body, buildErr := buildSubscriptionKubernetesQuery(clusterResourceID, start, end, subscriptionKubernetesQueryFilterModeDiscovery)
	if buildErr != nil {
		return subscriptionKubernetesDiscoveryResult{}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidRequest, buildErr)
	}
	payload, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return subscriptionKubernetesDiscoveryResult{}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidRequest, marshalErr)
	}

	requestURL := strings.TrimRight(endpoint, "/") + subscriptionOnlyScope(subscriptionID) + "/providers/Microsoft.CostManagement/query?api-version=" + subscriptionKubernetesQueryAPIVersion

	token, tokenErr := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{armTokenScope}})
	if tokenErr != nil {
		return subscriptionKubernetesDiscoveryResult{}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonAuthentication, tokenErr)
	}

	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if reqErr != nil {
		return subscriptionKubernetesDiscoveryResult{}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidRequest, reqErr)
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, doErr := httpClient.Do(req) // the one and only request this call ever makes
	if doErr != nil {
		return subscriptionKubernetesDiscoveryResult{}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonNetwork, doErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return subscriptionKubernetesDiscoveryResult{}, newAzureAPIError(subscriptionKubernetesQueryOperation, resp)
	}

	raw, readErr := readBounded(resp.Body, subscriptionKubernetesQueryOperation) // bounded: see maxResponseBytes, query_client.go
	if readErr != nil {
		return subscriptionKubernetesDiscoveryResult{}, readErr
	}

	var decoded queryResponseBody
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return subscriptionKubernetesDiscoveryResult{}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, err)
	}
	columnNames := columnNamesOf(decoded.Properties.Columns)

	rows, rowsErr := rowsFromColumns(decoded.Properties.Columns, decoded.Properties.Rows)
	if rowsErr != nil {
		return subscriptionKubernetesDiscoveryResult{ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, rowsErr)
	}
	rowCount := len(rows)

	if rowCount == 0 {
		return subscriptionKubernetesDiscoveryResult{ColumnNames: columnNames}, &subscriptionKubernetesQueryNoDataError{}
	}
	if rowCount > subscriptionKubernetesQueryMaxDiscoveryRows {
		// Rejected before any row is examined for matching.
		return subscriptionKubernetesDiscoveryResult{RowCount: rowCount, ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonTooManyRows, fmt.Errorf("discovery returned more than %d rows", subscriptionKubernetesQueryMaxDiscoveryRows))
	}

	if !hasColumn(decoded.Properties.Columns, "Cluster") {
		return subscriptionKubernetesDiscoveryResult{RowCount: rowCount, ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("response declares no Cluster column"))
	}
	hasCostUSD := hasColumn(decoded.Properties.Columns, "CostUSD")
	hasCost := hasColumn(decoded.Properties.Columns, "Cost")
	if !hasCostUSD && !hasCost {
		return subscriptionKubernetesDiscoveryResult{RowCount: rowCount, ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("response declares neither a Cost nor a CostUSD column"))
	}
	if !hasCostUSD && !hasColumn(decoded.Properties.Columns, "Currency") {
		return subscriptionKubernetesDiscoveryResult{RowCount: rowCount, ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("response declares Cost but no Currency column, and no CostUSD column"))
	}

	targetNormalized := normalizeARMResourceIDForComparison(clusterResourceID)
	targetName := strings.ToLower(finalResourceNameSegment(clusterResourceID))

	uniqueValues := make(map[string]struct{}, rowCount)
	var (
		armShapedCount, exactCount, foldCount, normalizedCount, nameOnlyCount int
		matchedTotal                                                          float64
		matchedCurrency                                                       string
		hasSafeMatch                                                          bool
	)

	for _, row := range rows {
		// clusterValue lives only for the duration of this loop
		// iteration: it is compared, then discarded. It is never
		// appended to a slice outside this scope, logged, or returned.
		clusterValue, ok := row.string("Cluster")
		if !ok || clusterValue == "" {
			return subscriptionKubernetesDiscoveryResult{RowCount: rowCount, ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("row missing a usable Cluster value"))
		}

		if _, seen := uniqueValues[clusterValue]; !seen {
			uniqueValues[clusterValue] = struct{}{}
			if armResourceIDShapePattern.MatchString(clusterValue) {
				armShapedCount++
			}
		}

		exact := clusterValue == clusterResourceID
		fold := strings.EqualFold(clusterValue, clusterResourceID)
		normalized := normalizeARMResourceIDForComparison(clusterValue) == targetNormalized
		nameOnly := strings.EqualFold(finalResourceNameSegment(clusterValue), targetName)
		if exact {
			exactCount++
		}
		if fold {
			foldCount++
		}
		if normalized {
			normalizedCount++
		}
		if nameOnly {
			nameOnlyCount++
		}

		// Name-only alone never proves cluster identity — it does not
		// contribute to the matched total, only to NameOnlyMatchCount
		// above.
		if !(exact || fold || normalized) {
			continue
		}

		var rowTotal float64
		var rowCurrency string
		if hasCostUSD {
			v, ok := row.float64("CostUSD")
			if !ok {
				return subscriptionKubernetesDiscoveryResult{RowCount: rowCount, ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("row missing a usable CostUSD value"))
			}
			rowTotal, rowCurrency = v, "USD"
		} else {
			v, ok := row.float64("Cost")
			if !ok {
				return subscriptionKubernetesDiscoveryResult{RowCount: rowCount, ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("row missing a usable Cost value"))
			}
			rc, ok := row.string("Currency")
			if !ok || rc == "" {
				return subscriptionKubernetesDiscoveryResult{RowCount: rowCount, ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("missing Currency column"))
			}
			rowTotal, rowCurrency = v, rc
		}

		if hasSafeMatch && !strings.EqualFold(matchedCurrency, rowCurrency) {
			return subscriptionKubernetesDiscoveryResult{RowCount: rowCount, ColumnNames: columnNames}, newSafeError(subscriptionKubernetesQueryOperation, safeReasonInvalidResponse, fmt.Errorf("matched rows mix currencies"))
		}
		matchedCurrency = rowCurrency
		matchedTotal += rowTotal
		hasSafeMatch = true
	}

	result := subscriptionKubernetesDiscoveryResult{
		RowCount:                   rowCount,
		ColumnNames:                columnNames,
		UniqueClusterValueCount:    len(uniqueValues),
		ARMShapedClusterValueCount: armShapedCount,
		ExactMatchCount:            exactCount,
		CaseInsensitiveMatchCount:  foldCount,
		NormalizedARMMatchCount:    normalizedCount,
		NameOnlyMatchCount:         nameOnlyCount,
		HasSafeMatch:               hasSafeMatch,
		MatchedTotal:               matchedTotal,
		MatchedCurrency:            matchedCurrency,
	}
	if !hasSafeMatch {
		return result, &subscriptionKubernetesQueryDiscoveryNoMatchError{}
	}
	return result, nil
}

func TestBuildSubscriptionKubernetesQueryPayloadShape(t *testing.T) {
	const clusterResourceID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"
	lowercased := strings.ToLower(clusterResourceID)

	start := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC)

	body, err := buildSubscriptionKubernetesQuery(clusterResourceID, start, end, subscriptionKubernetesQueryFilterModeCapturedAnd)
	if err != nil {
		t.Fatalf("buildSubscriptionKubernetesQuery: %v", err)
	}

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
		t.Errorf("Dataset.Grouping = %+v, want %+v (Cluster before ResourceLocation)", body.Dataset.Grouping, wantGrouping)
	}
	wantSorting := []subscriptionKubernetesQuerySort{{Direction: "descending", Name: "Cost"}}
	if !reflect.DeepEqual(body.Dataset.Sorting, wantSorting) {
		t.Errorf("Dataset.Sorting = %+v, want %+v (lowercase \"descending\")", body.Dataset.Sorting, wantSorting)
	}

	var filter subscriptionKubernetesQueryFilter
	if err := json.Unmarshal(body.Dataset.Filter, &filter); err != nil {
		t.Fatalf("unmarshal Dataset.Filter as the captured-and shape: %v", err)
	}
	if len(filter.And) != 2 {
		t.Fatalf("Dataset.Filter.And has %d expressions, want 2", len(filter.And))
	}
	wantFirst := subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{clusterResourceID}}
	if !reflect.DeepEqual(filter.And[0].Dimensions, wantFirst) {
		t.Errorf("Dataset.Filter.And[0].Dimensions = %+v, want %+v (canonical ARM ID)", filter.And[0].Dimensions, wantFirst)
	}
	wantSecond := subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{lowercased}}
	if !reflect.DeepEqual(filter.And[1].Dimensions, wantSecond) {
		t.Errorf("Dataset.Filter.And[1].Dimensions = %+v, want %+v (fully lowercased form)", filter.And[1].Dimensions, wantSecond)
	}
	if filter.And[0].Dimensions.Values[0] == filter.And[1].Dimensions.Values[0] {
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
		`"dataSet":`,
		`"from":"2026-08-17T00:00:00.000Z"`,
		`"to":"2026-09-15T23:59:59.000Z"`,
		`"granularity":"None"`,
		`"aggregation":{}`, // present and empty — not omitted, not null
		`"grouping":[{"type":"Dimension","name":"Cluster"},{"type":"Dimension","name":"ResourceLocation"}]`,
		`"sorting":[{"direction":"descending","name":"Cost"}]`,
		`"and":[{"dimensions":{"name":"Cluster","operator":"In","values":["` + clusterResourceID + `"]}},{"dimensions":{"name":"Cluster","operator":"In","values":["` + lowercased + `"]}}]`,
	} {
		if !strings.Contains(wireBody, want) {
			t.Errorf("marshaled request body missing %q; got %s", want, wireBody)
		}
	}
	// Negative checks use the exact `"key":` JSON-key pattern rather than
	// a bare word: a bare "or" substring check would false-positive on
	// "Am-or-tizedCost", which legitimately appears in this same payload.
	for _, forbidden := range []string{`"dataset":`, `"or":`, "Descending"} {
		if strings.Contains(wireBody, forbidden) {
			t.Errorf("marshaled request body must not contain %q; got %s", forbidden, wireBody)
		}
	}
	if strings.Contains(wireBody, `"aggregation":null`) {
		t.Error("aggregation must never marshal as null")
	}
}

// TestBuildSubscriptionKubernetesQueryCombinedValuesModeMarshalsSingleDimensionsFilter
// proves combined-values mode sends one Cluster/In dimensions expression
// carrying both case variants in a single values array — not wrapped in
// an "and" or "or" composite at all.
func TestBuildSubscriptionKubernetesQueryCombinedValuesModeMarshalsSingleDimensionsFilter(t *testing.T) {
	const clusterResourceID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"
	lowercased := strings.ToLower(clusterResourceID)
	start := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC)

	body, err := buildSubscriptionKubernetesQuery(clusterResourceID, start, end, subscriptionKubernetesQueryFilterModeCombinedValues)
	if err != nil {
		t.Fatalf("buildSubscriptionKubernetesQuery: %v", err)
	}

	var expr subscriptionKubernetesQueryFilterExpression
	if err := json.Unmarshal(body.Dataset.Filter, &expr); err != nil {
		t.Fatalf("unmarshal Dataset.Filter as a single dimensions expression: %v", err)
	}
	wantDims := subscriptionKubernetesQueryFilterDimension{Name: "Cluster", Operator: "In", Values: []string{clusterResourceID, lowercased}}
	if !reflect.DeepEqual(expr.Dimensions, wantDims) {
		t.Errorf("Dataset.Filter dimensions = %+v, want %+v (one expression, both values)", expr.Dimensions, wantDims)
	}

	wireFilter := string(body.Dataset.Filter)
	if strings.Contains(wireFilter, `"and":`) || strings.Contains(wireFilter, `"or":`) {
		t.Errorf("combined-values filter must not be wrapped in and/or; got %s", wireFilter)
	}
	if !strings.Contains(wireFilter, `"values":["`+clusterResourceID+`","`+lowercased+`"]`) {
		t.Errorf("combined-values filter must carry both values in one values array; got %s", wireFilter)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"dataSet":`) {
		t.Error("marshaled body missing the \"dataSet\" key — filter mode must not affect any other field")
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

	total, currency, rowCount, columnNames, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, subscriptionID, clusterResourceID, start, end, subscriptionKubernetesQueryFilterModeCapturedAnd)
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
	var capturedFilter subscriptionKubernetesQueryFilter
	if err := json.Unmarshal(capturedBody.Dataset.Filter, &capturedFilter); err != nil {
		t.Fatalf("unmarshal captured Dataset.Filter: %v", err)
	}
	if len(capturedFilter.And) != 2 {
		t.Errorf("Filter.And (as actually sent over HTTP) has %d expressions, want 2", len(capturedFilter.And))
	}

	// CostUSD preferred and summed across both rows: 1100 + 220.
	if total != 1320 {
		t.Errorf("total = %v, want 1320 (CostUSD summed across both rows)", total)
	}
	if currency != "USD" {
		t.Errorf("currency = %q, want USD (CostUSD present, so USD is forced regardless of the row Currency column)", currency)
	}
	if rowCount != 2 {
		t.Errorf("rowCount = %d, want 2", rowCount)
	}
	wantColumns := []string{"Cost", "CostUSD", "Currency", "Cluster", "ResourceLocation"}
	if !reflect.DeepEqual(columnNames, wantColumns) {
		t.Errorf("columnNames = %v, want %v", columnNames, wantColumns)
	}
}

// TestSubscriptionKubernetesQuerySpikeFallsBackToCostAndCurrencyWithoutCostUSD
// proves the "otherwise use Cost plus the response currency" fallback,
// summed across every row returned by the Cluster/ResourceLocation
// grouping, and that mismatched currencies across rows are rejected
// rather than silently summed.
func TestSubscriptionKubernetesQuerySpikeFallsBackToCostAndCurrencyWithoutCostUSD(t *testing.T) {
	const clusterResourceID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"

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

	total, currency, rowCount, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", clusterResourceID, time.Now(), time.Now(), subscriptionKubernetesQueryFilterModeCapturedAnd)
	if err != nil {
		t.Fatalf("runSubscriptionKubernetesQuery: %v", err)
	}
	if total != 345.5 {
		t.Errorf("total = %v, want 345.5 (Cost summed across both rows)", total)
	}
	if currency != "EUR" {
		t.Errorf("currency = %q, want EUR (response's own Currency column, no CostUSD present)", currency)
	}
	if rowCount != 2 {
		t.Errorf("rowCount = %d, want 2", rowCount)
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

	_, _, _, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", "cluster-resource-id", time.Now(), time.Now(), subscriptionKubernetesQueryFilterModeCapturedAnd)
	if err == nil {
		t.Fatal("expected an error for mixed currencies with no CostUSD column, got nil")
	}
}

// TestSubscriptionKubernetesQuerySpikeZeroCostRowIsValid proves a
// legitimate row whose Cost is exactly 0 (with a valid currency) is a
// normal, successful result — it must never be confused with "no rows
// returned".
func TestSubscriptionKubernetesQuerySpikeZeroCostRowIsValid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w, []queryColumn{{Name: "Cost"}, {Name: "Currency"}}, [][]any{{0.0, "USD"}}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	total, currency, rowCount, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", "cluster-resource-id", time.Now(), time.Now(), subscriptionKubernetesQueryFilterModeCapturedAnd)
	if err != nil {
		t.Fatalf("runSubscriptionKubernetesQuery: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %v, want 0 (a real, valid zero-cost row)", total)
	}
	if currency != "USD" {
		t.Errorf("currency = %q, want USD", currency)
	}
	if rowCount != 1 {
		t.Errorf("rowCount = %d, want 1", rowCount)
	}
}

// TestSubscriptionKubernetesQuerySpikeEachFilterModeMakesOneRequestAndReportsNoDataOnZeroRows
// mirrors the live-Azure finding that motivated combined-values mode:
// columns Cluster, ResourceLocation, Cost, Currency, zero rows. For each
// filter mode it proves exactly one request is made and a no-data error
// is returned — never a $0 success, and never an automatic second
// request trying the other mode.
func TestSubscriptionKubernetesQuerySpikeEachFilterModeMakesOneRequestAndReportsNoDataOnZeroRows(t *testing.T) {
	for _, mode := range []subscriptionKubernetesQueryFilterMode{
		subscriptionKubernetesQueryFilterModeCapturedAnd,
		subscriptionKubernetesQueryFilterModeCombinedValues,
	} {
		t.Run(string(mode), func(t *testing.T) {
			requestCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				writeQueryResponse(w,
					[]queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Cost"}, {Name: "Currency"}},
					nil, "")
			}))
			defer server.Close()

			cred := &fakeCredential{token: "t"}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			total, currency, rowCount, columnNames, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", "cluster-resource-id", time.Now(), time.Now(), mode)

			if requestCount != 1 {
				t.Errorf("mode %s: server received %d requests, want exactly 1 (no automatic fallback to the other mode)", mode, requestCount)
			}
			var noData *subscriptionKubernetesQueryNoDataError
			if !errors.As(err, &noData) {
				t.Fatalf("mode %s: error = %v (%T), want *subscriptionKubernetesQueryNoDataError", mode, err, err)
			}
			if total != 0 || currency != "" {
				t.Errorf("mode %s: total/currency = %v/%q, want 0/\"\" alongside a no-data error", mode, total, currency)
			}
			if rowCount != 0 {
				t.Errorf("mode %s: rowCount = %d, want 0", mode, rowCount)
			}
			wantColumns := []string{"Cluster", "ResourceLocation", "Cost", "Currency"}
			if !reflect.DeepEqual(columnNames, wantColumns) {
				t.Errorf("mode %s: columnNames = %v, want %v (schema is still known even with zero rows)", mode, columnNames, wantColumns)
			}
		})
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

	_, _, _, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", "cluster-resource-id", time.Now(), time.Now(), subscriptionKubernetesQueryFilterModeCapturedAnd)
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

	_, _, _, _, err := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", "cluster-resource-id", time.Now(), time.Now(), subscriptionKubernetesQueryFilterModeCapturedAnd)
	if err == nil {
		t.Fatal("expected an error when the server issues a redirect, got nil")
	}
}

// TestBuildSubscriptionKubernetesQueryDiscoveryModeOmitsFilterEntirely
// proves discovery mode's marshaled request has no "filter" property at
// all — not an empty object, not null — while every other field stays
// identical to the other filter modes.
func TestBuildSubscriptionKubernetesQueryDiscoveryModeOmitsFilterEntirely(t *testing.T) {
	const clusterResourceID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"
	start := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC)

	body, err := buildSubscriptionKubernetesQuery(clusterResourceID, start, end, subscriptionKubernetesQueryFilterModeDiscovery)
	if err != nil {
		t.Fatalf("buildSubscriptionKubernetesQuery: %v", err)
	}
	if body.Dataset.Filter != nil {
		t.Errorf("Dataset.Filter = %s, want nil (omitted)", body.Dataset.Filter)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	wireBody := string(raw)
	if strings.Contains(wireBody, `"filter"`) {
		t.Errorf("discovery request must omit the filter property entirely; got %s", wireBody)
	}
	for _, want := range []string{
		`"type":"AmortizedCost"`,
		`"timeframe":"Custom"`,
		`"provider":"Microsoft.ContainerService"`,
		`"dataSet":`,
		`"from":"2026-08-17T00:00:00.000Z"`,
		`"to":"2026-09-15T23:59:59.000Z"`,
		`"granularity":"None"`,
		`"aggregation":{}`,
		`"grouping":[{"type":"Dimension","name":"Cluster"},{"type":"Dimension","name":"ResourceLocation"}]`,
		`"sorting":[{"direction":"descending","name":"Cost"}]`,
	} {
		if !strings.Contains(wireBody, want) {
			t.Errorf("discovery request missing %q (every field but filter must match the other modes); got %s", want, wireBody)
		}
	}
}

// TestSubscriptionKubernetesQueryDiscoveryMakesExactlyOneRequest proves
// discovery mode, like the other two filter modes, issues exactly one
// HTTP request.
func TestSubscriptionKubernetesQueryDiscoveryMakesExactlyOneRequest(t *testing.T) {
	const target = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if strings.Contains(string(mustReadBody(t, r)), `"filter"`) {
			t.Error("discovery request body must never include a filter property")
		}
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Cost"}, {Name: "Currency"}},
			[][]any{{target, "eastus2", 12.5, "USD"}}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := runSubscriptionKubernetesDiscoveryQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", target, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("runSubscriptionKubernetesDiscoveryQuery: %v", err)
	}
	if requestCount != 1 {
		t.Errorf("requestCount = %d, want exactly 1", requestCount)
	}
}

// mustReadBody reads and restores r.Body so a handler can both inspect
// and let writeQueryResponse-style helpers proceed normally; used only by
// tests in this file.
func mustReadBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading request body: %v", err)
	}
	r.Body.Close()
	return raw
}

// TestSubscriptionKubernetesQueryDiscoveryMatchCountsAndTotalAreCorrect is
// the core matching-hierarchy test. Five synthetic rows, each engineered
// to land in exactly one additional matching tier than the last:
//
//   - rowExact: byte-identical to the target — matches all four tiers.
//   - rowTrailingSlash: target + "/" — not exact, not EqualFold (extra
//     character), but normalized-equal (trailing slash trimmed) and
//     name-only equal.
//   - rowDifferentCase: target uppercased — not exact, but EqualFold,
//     normalized, and name-only equal.
//   - rowNameOnly: a different subscription and resource group, but the
//     same cluster name — only name-only equal.
//   - rowUnrelated: a wholly different cluster — matches nothing.
//
// Only rowExact, rowTrailingSlash, and rowDifferentCase are safe matches;
// their costs (10, 20, 30) must sum to the returned total, and
// rowNameOnly's (9999) and rowUnrelated's (8888) must never appear in it.
func TestSubscriptionKubernetesQueryDiscoveryMatchCountsAndTotalAreCorrect(t *testing.T) {
	const target = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"
	rowExact := target
	rowTrailingSlash := target + "/"
	rowDifferentCase := strings.ToUpper(target)
	rowNameOnly := "/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/Other-RG/providers/Microsoft.ContainerService/managedClusters/my-aks-cluster"
	rowUnrelated := "/subscriptions/33333333-3333-3333-3333-333333333333/resourceGroups/Unrelated-RG/providers/Microsoft.ContainerService/managedClusters/totally-different"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Cost"}, {Name: "Currency"}},
			[][]any{
				{rowExact, "eastus2", 10.0, "USD"},
				{rowTrailingSlash, "eastus2", 20.0, "USD"},
				{rowDifferentCase, "eastus2", 30.0, "USD"},
				{rowNameOnly, "westus2", 9999.0, "USD"},
				{rowUnrelated, "centralus", 8888.0, "USD"},
			}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := runSubscriptionKubernetesDiscoveryQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", target, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("runSubscriptionKubernetesDiscoveryQuery: %v", err)
	}

	if result.RowCount != 5 {
		t.Errorf("RowCount = %d, want 5", result.RowCount)
	}
	if result.UniqueClusterValueCount != 5 {
		t.Errorf("UniqueClusterValueCount = %d, want 5", result.UniqueClusterValueCount)
	}
	if result.ARMShapedClusterValueCount != 4 {
		t.Errorf("ARMShapedClusterValueCount = %d, want 4 (every fixture value except rowTrailingSlash, whose trailing slash makes it not strictly ARM-shaped — that's exactly why normalization, not shape-checking, is what makes it match)", result.ARMShapedClusterValueCount)
	}
	if result.ExactMatchCount != 1 {
		t.Errorf("ExactMatchCount = %d, want 1 (rowExact only)", result.ExactMatchCount)
	}
	if result.CaseInsensitiveMatchCount != 2 {
		t.Errorf("CaseInsensitiveMatchCount = %d, want 2 (rowExact, rowDifferentCase)", result.CaseInsensitiveMatchCount)
	}
	if result.NormalizedARMMatchCount != 3 {
		t.Errorf("NormalizedARMMatchCount = %d, want 3 (rowExact, rowTrailingSlash, rowDifferentCase)", result.NormalizedARMMatchCount)
	}
	if result.NameOnlyMatchCount != 4 {
		t.Errorf("NameOnlyMatchCount = %d, want 4 (every row except rowUnrelated)", result.NameOnlyMatchCount)
	}
	if !result.HasSafeMatch {
		t.Fatal("HasSafeMatch = false, want true")
	}
	if result.MatchedTotal != 60 {
		t.Errorf("MatchedTotal = %v, want 60 (10+20+30 — rowNameOnly's 9999 and rowUnrelated's 8888 must be excluded)", result.MatchedTotal)
	}
	if result.MatchedCurrency != "USD" {
		t.Errorf("MatchedCurrency = %q, want USD", result.MatchedCurrency)
	}
}

// TestSubscriptionKubernetesQueryDiscoveryReturnsNoMatchErrorWhenNothingSafelyMatches
// proves that rows existing in the response (other clusters' cost data)
// without any safe match for the configured target produce a distinct,
// sanitized no-match error — never a $0 success and never the unrelated
// cluster's cost.
func TestSubscriptionKubernetesQueryDiscoveryReturnsNoMatchErrorWhenNothingSafelyMatches(t *testing.T) {
	const target = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"
	const other = "/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/Other-RG/providers/Microsoft.ContainerService/managedClusters/totally-different"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Cost"}, {Name: "Currency"}},
			[][]any{{other, "eastus2", 42.0, "USD"}}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := runSubscriptionKubernetesDiscoveryQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", target, time.Now(), time.Now())
	var noMatch *subscriptionKubernetesQueryDiscoveryNoMatchError
	if !errors.As(err, &noMatch) {
		t.Fatalf("error = %v (%T), want *subscriptionKubernetesQueryDiscoveryNoMatchError", err, err)
	}
	if result.HasSafeMatch {
		t.Error("HasSafeMatch = true, want false")
	}
	if strings.Contains(err.Error(), "totally-different") || strings.Contains(err.Error(), "Other-RG") {
		t.Errorf("no-match error must never contain the examined cluster value; got %q", err.Error())
	}
}

// TestSubscriptionKubernetesQueryDiscoveryZeroCostSafeMatchIsValid proves a
// legitimate zero-cost safe match is reported as a real match (HasSafeMatch
// true, MatchedTotal 0), never confused with "no match" or "no data".
func TestSubscriptionKubernetesQueryDiscoveryZeroCostSafeMatchIsValid(t *testing.T) {
	const target = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Cost"}, {Name: "Currency"}},
			[][]any{{target, "eastus2", 0.0, "USD"}}, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := runSubscriptionKubernetesDiscoveryQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", target, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("runSubscriptionKubernetesDiscoveryQuery: %v", err)
	}
	if !result.HasSafeMatch {
		t.Error("HasSafeMatch = false, want true (a real, valid zero-cost match)")
	}
	if result.MatchedTotal != 0 {
		t.Errorf("MatchedTotal = %v, want 0", result.MatchedTotal)
	}
	if result.MatchedCurrency != "USD" {
		t.Errorf("MatchedCurrency = %q, want USD", result.MatchedCurrency)
	}
}

// TestSubscriptionKubernetesQueryDiscoveryRejectsMoreThanMaxRows proves a
// response exceeding subscriptionKubernetesQueryMaxDiscoveryRows fails
// safely and defensively, before any row is examined for matching.
func TestSubscriptionKubernetesQueryDiscoveryRejectsMoreThanMaxRows(t *testing.T) {
	rows := make([][]any, subscriptionKubernetesQueryMaxDiscoveryRows+1)
	for i := range rows {
		rows[i] = []any{
			fmt.Sprintf("/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster-%d", i),
			"eastus2", 1.0, "USD",
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeQueryResponse(w, []queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Cost"}, {Name: "Currency"}}, rows, "")
	}))
	defer server.Close()

	cred := &fakeCredential{token: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := runSubscriptionKubernetesDiscoveryQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", "cluster-resource-id", time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected an error for more than the max row count, got nil")
	}
	if result.RowCount != subscriptionKubernetesQueryMaxDiscoveryRows+1 {
		t.Errorf("RowCount = %d, want %d", result.RowCount, subscriptionKubernetesQueryMaxDiscoveryRows+1)
	}
	if result.ExactMatchCount != 0 || result.HasSafeMatch {
		t.Error("no matching should have been attempted once the row cap was exceeded")
	}
	var safeErr *SafeError
	if !errors.As(err, &safeErr) {
		t.Fatalf("error = %v (%T), want *SafeError", err, err)
	}
	if safeErr.Reason != safeReasonTooManyRows {
		t.Errorf("Reason = %q, want %q", safeErr.Reason, safeReasonTooManyRows)
	}
}

// TestSubscriptionKubernetesQueryDiscoveryFailsSafelyOnMissingColumns
// proves a response missing the Cluster column, both Cost and CostUSD, or
// Currency (with no CostUSD present) fails safely rather than silently
// treating the query as a match or a $0 success.
func TestSubscriptionKubernetesQueryDiscoveryFailsSafelyOnMissingColumns(t *testing.T) {
	const target = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/My-RG/providers/Microsoft.ContainerService/managedClusters/My-AKS-Cluster"
	tests := []struct {
		name    string
		columns []queryColumn
		rows    [][]any
	}{
		{
			name:    "missing Cluster column",
			columns: []queryColumn{{Name: "ResourceLocation"}, {Name: "Cost"}, {Name: "Currency"}},
			rows:    [][]any{{"eastus2", 1.0, "USD"}},
		},
		{
			name:    "missing Cost and CostUSD columns",
			columns: []queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Currency"}},
			rows:    [][]any{{target, "eastus2", "USD"}},
		},
		{
			name:    "missing Currency column with no CostUSD",
			columns: []queryColumn{{Name: "Cluster"}, {Name: "ResourceLocation"}, {Name: "Cost"}},
			rows:    [][]any{{target, "eastus2", 1.0}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeQueryResponse(w, tt.columns, tt.rows, "")
			}))
			defer server.Close()

			cred := &fakeCredential{token: "t"}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			_, err := runSubscriptionKubernetesDiscoveryQuery(ctx, newARMHTTPClient(), cred, server.URL, "11111111-1111-1111-1111-111111111111", target, time.Now(), time.Now())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}
