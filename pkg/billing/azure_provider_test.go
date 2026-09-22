package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testNodeResourceGroup = "MC_rxr-rxp-e2e-01-cus-rg_rxr-rxp-e2e-01-cus-aks_centralus"

// newTestAzureProvider wires an AzureProvider at a single synthetic HTTP
// endpoint (both the Cost Management query calls and the AKS identity
// lookup), driven entirely by handler so each test controls exactly what
// each scope query and the identity lookup return.
func newTestAzureProvider(cfg ClusterConfig, server *httptest.Server) *AzureProvider {
	cred := &fakeCredential{token: "t"}
	return &AzureProvider{
		cfg:      cfg,
		client:   newQueryClient(server.Client(), cred, server.URL),
		resolver: newNodeResourceGroupResolver(server.Client(), cred, server.URL),
	}
}

// costQueryHandler returns rows for the resource group named in the
// request path, keyed by resourceGroup -> rows, and serves the AKS identity
// lookup from nodeResourceGroup.
func costQueryHandler(t *testing.T, rowsByResourceGroup map[string][][]any, nodeResourceGroup string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "Microsoft.ContainerService/managedClusters") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"properties": map[string]any{"nodeResourceGroup": nodeResourceGroup}})
			return
		}
		// Path shape: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.CostManagement/query
		parts := strings.Split(r.URL.Path, "/")
		var rg string
		for i, p := range parts {
			if strings.EqualFold(p, "resourceGroups") && i+1 < len(parts) {
				rg = parts[i+1]
			}
		}
		rows := rowsByResourceGroup[rg]
		writeQueryResponse(w,
			[]queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}},
			rows, "")
	}
}

func TestFetchBillingAggregatesClusterAndNodeResourceGroupsWithoutDoubleCounting(t *testing.T) {
	clusterRG := "rxr-rxp-e2e-01-cus-rg"
	rows := map[string][][]any{
		clusterRG:             {{100.0, "USD", "/subscriptions/s/resourceGroups/" + clusterRG + "/providers/Microsoft.ContainerService/managedClusters/x", clusterRG}},
		testNodeResourceGroup: {{5328.0, "USD", "/subscriptions/s/resourceGroups/" + testNodeResourceGroup + "/providers/Microsoft.Compute/virtualMachineScaleSets/user", testNodeResourceGroup}, {190.06, "USD", "/subscriptions/s/resourceGroups/" + testNodeResourceGroup + "/providers/Microsoft.Compute/virtualMachineScaleSets/system", testNodeResourceGroup}},
	}
	server := httptest.NewServer(costQueryHandler(t, rows, testNodeResourceGroup))
	defer server.Close()

	provider := newTestAzureProvider(validClusterConfig(), server)
	result, err := provider.FetchBilling(context.Background(), Request{
		PeriodStart: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC),
		CostBasis:   CostBasisActualCost,
	})
	if err != nil {
		t.Fatalf("FetchBilling: %v", err)
	}
	wantTotal := 100.0 + 5328.0 + 190.06
	if diff := result.Total - wantTotal; diff > 0.001 || diff < -0.001 {
		t.Errorf("Total = %v, want %v", result.Total, wantTotal)
	}
	if result.RowCount != 3 {
		t.Errorf("RowCount = %d, want 3", result.RowCount)
	}
	if len(result.Lines) != 3 {
		t.Errorf("len(Lines) = %d, want 3 (one per resource, no double counting)", len(result.Lines))
	}
}

// costTolerance is the tolerance used to compare reconciled cost totals.
// AttributedTotal + UnattributedTotal reconciles to Total by construction
// (see Result.UnattributedTotal's doc comment), not because float64
// addition/subtraction are exact inverses — tests must never compare
// reconciled totals with ==.
const costTolerance = 0.001

func almostEqual(a, b float64) bool {
	diff := a - b
	return diff <= costTolerance && diff >= -costTolerance
}

func TestFetchBillingAttributesOnlyExactAKSResourceIDMatch(t *testing.T) {
	clusterRG := "rxr-rxp-e2e-01-cus-rg"
	rows := map[string][][]any{
		// The AKS managed-cluster control-plane charge: ResourceId matches
		// validAKSResourceID exactly, so this is the only attributed line.
		clusterRG: {{100.0, "USD", validAKSResourceID, clusterRG}},
		// Node VMSS and a credit, both in the node resource group. Despite
		// living in the node RG (an AKS-managed convention), neither has a
		// resource ID matching the configured AKS resource ID, so neither
		// is attributed — node-RG residence alone must not prove ownership.
		testNodeResourceGroup: {
			{5328.0, "USD", "/subscriptions/s/resourceGroups/" + testNodeResourceGroup + "/providers/Microsoft.Compute/virtualMachineScaleSets/user", testNodeResourceGroup},
			{-50.0, "USD", "/subscriptions/s/resourceGroups/" + testNodeResourceGroup + "/providers/Microsoft.Compute/virtualMachineScaleSets/credit", testNodeResourceGroup},
		},
	}
	server := httptest.NewServer(costQueryHandler(t, rows, testNodeResourceGroup))
	defer server.Close()

	provider := newTestAzureProvider(validClusterConfig(), server)
	result, err := provider.FetchBilling(context.Background(), Request{
		PeriodStart: time.Now().AddDate(0, 0, -1), PeriodEnd: time.Now(), CostBasis: CostBasisActualCost,
	})
	if err != nil {
		t.Fatalf("FetchBilling: %v", err)
	}

	if result.ClusterResourceID != validAKSResourceID {
		t.Errorf("ClusterResourceID = %q, want %q", result.ClusterResourceID, validAKSResourceID)
	}
	if !almostEqual(result.AttributedTotal, 100.0) {
		t.Errorf("AttributedTotal = %v, want ~100 (only the exact AKS resource ID match)", result.AttributedTotal)
	}
	wantUnattributed := 5328.0 - 50.0
	if !almostEqual(result.UnattributedTotal, wantUnattributed) {
		t.Errorf("UnattributedTotal = %v, want ~%v", result.UnattributedTotal, wantUnattributed)
	}
	if !almostEqual(result.AttributedTotal+result.UnattributedTotal, result.Total) {
		t.Errorf("AttributedTotal + UnattributedTotal = %v, want Total %v within tolerance (must reconcile, including the negative credit)", result.AttributedTotal+result.UnattributedTotal, result.Total)
	}

	attributedCount := 0
	for _, l := range result.Lines {
		if l.Attributed {
			attributedCount++
			if l.ResourceID != validAKSResourceID {
				t.Errorf("unexpected attributed line %q", l.ResourceID)
			}
		}
	}
	if attributedCount != 1 {
		t.Errorf("attributed line count = %d, want 1", attributedCount)
	}
}

// TestFetchBillingReconciliationHoldsWithinToleranceForFractionalCosts uses
// fractional-cent line items — the shape most likely to expose float64
// rounding — to prove AttributedTotal + UnattributedTotal reconciles to
// Total within costTolerance. It deliberately does not assert bit-exact
// equality: UnattributedTotal is defined as Total - AttributedTotal by
// construction, but a + (b - a) is not guaranteed to be bit-identical to b
// under IEEE 754 float64 arithmetic for arbitrary a, b.
func TestFetchBillingReconciliationHoldsWithinToleranceForFractionalCosts(t *testing.T) {
	clusterRG := "rxr-rxp-e2e-01-cus-rg"
	rows := map[string][][]any{
		clusterRG: {{19.99, "USD", validAKSResourceID, clusterRG}},
		testNodeResourceGroup: {
			{5308.01, "USD", "/subscriptions/s/resourceGroups/" + testNodeResourceGroup + "/providers/Microsoft.Compute/virtualMachineScaleSets/user", testNodeResourceGroup},
			{-12.34, "USD", "/subscriptions/s/resourceGroups/" + testNodeResourceGroup + "/providers/Microsoft.Compute/virtualMachineScaleSets/credit", testNodeResourceGroup},
		},
	}
	server := httptest.NewServer(costQueryHandler(t, rows, testNodeResourceGroup))
	defer server.Close()

	provider := newTestAzureProvider(validClusterConfig(), server)
	result, err := provider.FetchBilling(context.Background(), Request{
		PeriodStart: time.Now().AddDate(0, 0, -1), PeriodEnd: time.Now(), CostBasis: CostBasisActualCost,
	})
	if err != nil {
		t.Fatalf("FetchBilling: %v", err)
	}

	wantTotal := 19.99 + 5308.01 - 12.34
	if !almostEqual(result.Total, wantTotal) {
		t.Fatalf("Total = %v, want ~%v", result.Total, wantTotal)
	}
	if !almostEqual(result.AttributedTotal, 19.99) {
		t.Errorf("AttributedTotal = %v, want ~19.99", result.AttributedTotal)
	}
	if !almostEqual(result.AttributedTotal+result.UnattributedTotal, result.Total) {
		t.Errorf("AttributedTotal + UnattributedTotal = %v, want Total %v within tolerance", result.AttributedTotal+result.UnattributedTotal, result.Total)
	}
}

func TestFetchBillingRejectsMixedCurrencies(t *testing.T) {
	clusterRG := "rxr-rxp-e2e-01-cus-rg"
	rows := map[string][][]any{
		clusterRG:             {{100.0, "USD", "/r/1", clusterRG}},
		testNodeResourceGroup: {{50.0, "EUR", "/r/2", testNodeResourceGroup}},
	}
	server := httptest.NewServer(costQueryHandler(t, rows, testNodeResourceGroup))
	defer server.Close()

	provider := newTestAzureProvider(validClusterConfig(), server)
	_, err := provider.FetchBilling(context.Background(), Request{
		PeriodStart: time.Now().AddDate(0, 0, -1), PeriodEnd: time.Now(), CostBasis: CostBasisActualCost,
	})
	if err == nil {
		t.Fatal("expected an error for mixed currencies, got nil")
	}
}

func TestFetchBillingPreservesCreditsAndValidZeroTotal(t *testing.T) {
	clusterRG := "rxr-rxp-e2e-01-cus-rg"
	rows := map[string][][]any{
		clusterRG:             {{10.0, "USD", "/r/1", clusterRG}},
		testNodeResourceGroup: {{-10.0, "USD", "/r/2", testNodeResourceGroup}}, // a credit exactly offsetting the charge
	}
	server := httptest.NewServer(costQueryHandler(t, rows, testNodeResourceGroup))
	defer server.Close()

	provider := newTestAzureProvider(validClusterConfig(), server)
	result, err := provider.FetchBilling(context.Background(), Request{
		PeriodStart: time.Now().AddDate(0, 0, -1), PeriodEnd: time.Now(), CostBasis: CostBasisActualCost,
	})
	if err != nil {
		t.Fatalf("FetchBilling: %v", err)
	}
	if result.Total != 0 {
		t.Errorf("Total = %v, want exactly 0 (credit offsetting charge, not unavailable)", result.Total)
	}
	if result.RowCount != 2 {
		t.Errorf("RowCount = %d, want 2 (valid zero cost, not no-data)", result.RowCount)
	}
	foundCredit := false
	for _, l := range result.Lines {
		if l.Cost < 0 {
			foundCredit = true
		}
	}
	if !foundCredit {
		t.Error("expected the negative credit line item to be preserved, not clamped to zero")
	}
}

func TestFetchBillingNoDataWhenBothScopesEmpty(t *testing.T) {
	rows := map[string][][]any{}
	server := httptest.NewServer(costQueryHandler(t, rows, testNodeResourceGroup))
	defer server.Close()

	provider := newTestAzureProvider(validClusterConfig(), server)
	result, err := provider.FetchBilling(context.Background(), Request{
		PeriodStart: time.Now().AddDate(0, 0, -1), PeriodEnd: time.Now(), CostBasis: CostBasisActualCost,
	})
	if err != nil {
		t.Fatalf("FetchBilling: %v", err)
	}
	if result.RowCount != 0 {
		t.Errorf("RowCount = %d, want 0", result.RowCount)
	}
}

func TestFetchBillingUsesExplicitNodeResourceGroupOverride(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "managedClusters") {
			calls++ // must never be called when NodeResourceGroup is explicitly configured
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeQueryResponse(w, []queryColumn{{Name: "Cost"}, {Name: "Currency"}, {Name: "ResourceId"}, {Name: "ResourceGroupName"}}, nil, "")
	}))
	defer server.Close()

	cfg := validClusterConfig()
	cfg.NodeResourceGroup = "MC_operator_override"
	provider := newTestAzureProvider(cfg, server)
	_, err := provider.FetchBilling(context.Background(), Request{
		PeriodStart: time.Now().AddDate(0, 0, -1), PeriodEnd: time.Now(), CostBasis: CostBasisActualCost,
	})
	if err != nil {
		t.Fatalf("FetchBilling: %v", err)
	}
	if calls != 0 {
		t.Errorf("managedClusters lookup called %d times despite an explicit override", calls)
	}
}

func TestNewAzureProviderRejectsUnsupportedManagementEndpoint(t *testing.T) {
	cfg := validClusterConfig()
	cfg.ManagementEndpoint = "https://attacker.example.com"
	if _, err := NewAzureProvider(cfg, &fakeCredential{token: "t"}); err == nil {
		t.Fatal("expected NewAzureProvider to reject an unsupported managementEndpoint, got nil")
	}
}

func TestNewAzureProviderAcceptsDefaultEndpoint(t *testing.T) {
	cfg := validClusterConfig()
	provider, err := NewAzureProvider(cfg, &fakeCredential{token: "t"})
	if err != nil {
		t.Fatalf("NewAzureProvider: %v", err)
	}
	if provider == nil {
		t.Fatal("expected a non-nil provider")
	}
}

func TestFetchBillingLabelsResultAsTwoResourceGroupTotal(t *testing.T) {
	clusterRG := "rxr-rxp-e2e-01-cus-rg"
	rows := map[string][][]any{
		clusterRG:             {{1.0, "USD", "/r/1", clusterRG}},
		testNodeResourceGroup: {{2.0, "USD", "/r/2", testNodeResourceGroup}},
	}
	server := httptest.NewServer(costQueryHandler(t, rows, testNodeResourceGroup))
	defer server.Close()

	provider := newTestAzureProvider(validClusterConfig(), server)
	result, err := provider.FetchBilling(context.Background(), Request{
		PeriodStart: time.Now().AddDate(0, 0, -1), PeriodEnd: time.Now(), CostBasis: CostBasisActualCost,
	})
	if err != nil {
		t.Fatalf("FetchBilling: %v", err)
	}
	if !strings.Contains(result.Coverage, "Two-resource-group") {
		t.Errorf("Coverage = %q, want it to lead with an explicit two-resource-group total label", result.Coverage)
	}
	if !strings.Contains(result.Coverage, clusterRG) || !strings.Contains(result.Coverage, testNodeResourceGroup) {
		t.Errorf("Coverage = %q, want both resource group names named explicitly", result.Coverage)
	}
	foundOverstateDisclosure := false
	for _, d := range result.Disclosures {
		// Must warn generically ("ownership has not been independently
		// verified") rather than asserting the node resource group is
		// safe — that would be a claim this package cannot actually
		// verify, since NodeResourceGroup can be set to any value.
		if strings.Contains(d, "ownership has not been independently verified") {
			foundOverstateDisclosure = true
		}
		if strings.Contains(d, "exclusively AKS-managed") {
			t.Errorf("disclosure %q asserts unverified ownership of the node resource group", d)
		}
	}
	if !foundOverstateDisclosure {
		t.Error("expected a disclosure warning that either configured resource group's total can include unrelated resources")
	}
}
