package billing

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// AzureProvider implements Provider using the Azure Cost Management Query
// API. It queries exactly two resource-group scopes — the AKS cluster's own
// resource group and its node resource group — never the whole
// subscription and never an unrelated/shared resource group, per the task's
// scoping requirement: filtering on the AKS resource ID alone would miss
// every node VM, disk, and load balancer, which live in the node resource
// group instead.
type AzureProvider struct {
	cfg      ClusterConfig
	client   *queryClient
	resolver *nodeResourceGroupResolver
}

// NewAzureProvider constructs the production Azure billing provider for one
// cluster's configuration, using credential for both the Cost Management
// query and the node-resource-group lookup.
func NewAzureProvider(cfg ClusterConfig, credential azcore.TokenCredential) *AzureProvider {
	endpoint := cfg.EffectiveManagementEndpoint()
	httpClient := &http.Client{Timeout: 30 * time.Second}
	return &AzureProvider{
		cfg:      cfg,
		client:   newQueryClient(httpClient, credential, endpoint),
		resolver: newNodeResourceGroupResolver(httpClient, credential, endpoint),
	}
}

func (p *AzureProvider) Name() string { return "azure" }

// FetchBilling issues both resource-group queries and aggregates them. Every
// error path returns early with an explicit error rather than a partial or
// zeroed Result — the caller (Runtime) is responsible for retaining the
// last good snapshot on failure, never converting this error into $0.
func (p *AzureProvider) FetchBilling(ctx context.Context, req Request) (Result, error) {
	identity, err := ParseAKSResourceID(p.cfg.AKSResourceID)
	if err != nil {
		return Result{}, err
	}
	nodeRG, err := p.resolver.Resolve(ctx, p.cfg)
	if err != nil {
		return Result{}, err
	}

	scopeResourceGroups := []string{identity.ResourceGroup}
	if !strings.EqualFold(nodeRG, identity.ResourceGroup) {
		scopeResourceGroups = append(scopeResourceGroups, nodeRG)
	}

	body := buildResourceGroupQuery(req.CostBasis, req.PeriodStart, req.PeriodEnd)

	var allRows []queryRow
	for _, rg := range scopeResourceGroups {
		rows, err := p.client.query(ctx, resourceGroupScope(identity.SubscriptionID, rg), body)
		if err != nil {
			return Result{}, fmt.Errorf("querying resource group %q: %w", rg, err)
		}
		allRows = append(allRows, rows...)
	}

	lines, currency, err := aggregateRows(allRows)
	if err != nil {
		return Result{}, err
	}

	total := 0.0
	for _, l := range lines {
		total += l.Cost
	}

	return Result{
		Total:       total,
		Currency:    currency,
		CostBasis:   req.CostBasis,
		PeriodStart: req.PeriodStart,
		PeriodEnd:   req.PeriodEnd,
		RetrievedAt: time.Now(),
		Source:      "Azure Cost Management API (Query - Usage, resource-group scope)",
		Scope:       scopeDescription(identity, nodeRG),
		Coverage:    coverageDescription(identity, nodeRG),
		Disclosures: billingDisclosures(),
		Lines:       lines,
		RowCount:    len(allRows),
	}, nil
}

// aggregateRows maps raw query rows into ResourceCost lines, enforcing two
// correctness invariants: it never sums two different currencies (an
// explicit error instead), and it never double-counts a resource ID that
// happened to appear in both queried scopes (defensive; the two scopes are
// always distinct resource groups by construction, but this makes that
// invariant unconditional rather than assumed).
func aggregateRows(rows []queryRow) ([]ResourceCost, string, error) {
	seen := make(map[string]bool, len(rows))
	var lines []ResourceCost
	currency := ""
	for _, row := range rows {
		cost, ok := row.float64("Cost")
		if !ok {
			return nil, "", fmt.Errorf("Cost Management response row is missing a numeric Cost column")
		}
		rowCurrency, ok := row.string("Currency")
		if !ok || rowCurrency == "" {
			return nil, "", fmt.Errorf("Cost Management response row is missing a Currency column")
		}
		if currency == "" {
			currency = rowCurrency
		} else if !strings.EqualFold(currency, rowCurrency) {
			return nil, "", fmt.Errorf("Cost Management response mixes currencies %q and %q; refusing to sum", currency, rowCurrency)
		}

		resourceID, _ := row.string("ResourceId")
		resourceGroup, _ := row.string("ResourceGroupName")
		key := strings.ToLower(resourceID)
		if key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		lines = append(lines, ResourceCost{ResourceID: resourceID, ResourceGroup: resourceGroup, Cost: cost, Currency: rowCurrency})
	}
	return lines, currency, nil
}

func scopeDescription(identity AKSIdentity, nodeRG string) string {
	return fmt.Sprintf("AKS cluster %q: resource group %q and node resource group %q (subscription %s)",
		identity.ClusterName, identity.ResourceGroup, nodeRG, identity.SubscriptionID)
}

func coverageDescription(identity AKSIdentity, nodeRG string) string {
	return fmt.Sprintf("Resource-level Azure billing for every resource inside resource groups %q and %q, attributed by exact Azure resource ID. Includes AKS control-plane charges (if any), node VM/VMSS compute, managed disks, load balancers, and public IPs billed within these resource groups.",
		identity.ResourceGroup, nodeRG)
}

func billingDisclosures() []string {
	return []string{
		"Shared or externally hosted resources billed outside the cluster and node resource groups (for example a hub-network egress path, shared DNS, or cross-subscription resources) are not included in this total.",
		"Kubernetes-level Idle/Used/System allocation, as shown in the Azure Portal's AKS Cost Analysis view, is not exposed through a public API and is not shown here; this total reflects Azure resource billing only, not per-namespace or per-pod allocation.",
	}
}
