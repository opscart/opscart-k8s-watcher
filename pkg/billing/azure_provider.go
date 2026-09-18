package billing

import (
	"context"
	"fmt"
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
// query and the node-resource-group lookup. It validates the effective
// management endpoint against allowedManagementHosts (config.go) — this is
// the actual point an ARM bearer token is ever handed to an http.Client, so
// it is validated here as well as at config-load time (defense in depth),
// even though every production ClusterConfig reaching this constructor was
// already validated once by Load/Validate.
func NewAzureProvider(cfg ClusterConfig, credential azcore.TokenCredential) (*AzureProvider, error) {
	endpoint := cfg.EffectiveManagementEndpoint()
	if err := validateManagementEndpoint(endpoint); err != nil {
		return nil, err
	}
	httpClient := newARMHTTPClient()
	return &AzureProvider{
		cfg:      cfg,
		client:   newQueryClient(httpClient, credential, endpoint),
		resolver: newNodeResourceGroupResolver(httpClient, credential, endpoint),
	}, nil
}

func (p *AzureProvider) Name() string { return "azure" }

// FetchBilling issues both resource-group queries and aggregates them. Every
// error path returns early with an explicit error rather than a partial or
// zeroed Result — the caller (Runtime) is responsible for retaining the
// last good snapshot on failure, never converting this error into $0.
//
// Every HTTP attempt this call makes — both resource-group queries, every
// page of each, every bounded retry, and the node-resource-group lookup —
// shares one requestBudget, so the independently reasonable per-call bounds
// (maxPages, maxRetryAttempts) can never compound into an unbounded number
// of outbound requests for a single refresh.
func (p *AzureProvider) FetchBilling(ctx context.Context, req Request) (Result, error) {
	identity, err := ParseAKSResourceID(p.cfg.AKSResourceID)
	if err != nil {
		return Result{}, err
	}
	budget := newRequestBudget(maxRequestsPerRefresh)

	nodeRG, err := p.resolver.Resolve(ctx, p.cfg, budget)
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
		operation := fmt.Sprintf("Cost Management query for resource group %q", rg)
		rows, err := p.client.query(ctx, resourceGroupScope(identity.SubscriptionID, rg), body, budget, operation)
		if err != nil {
			return Result{}, err
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
		Disclosures: billingDisclosures(identity.ResourceGroup, nodeRG),
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

// coverageDescription leads with "two-resource-group" framing deliberately:
// this is a sum over every resource billed inside two resource groups, not
// a filtered "only AKS-owned resources" total. Neither resource group's
// membership is independently verified by this package: the node resource
// group is ordinarily AKS-managed, but an operator can override it to any
// value (ClusterConfig.NodeResourceGroup), and the cluster's own resource
// group is never guaranteed to contain only AKS-related resources either.
// See billingDisclosures for the explicit disclosure of that.
func coverageDescription(identity AKSIdentity, nodeRG string) string {
	return fmt.Sprintf("Two-resource-group Azure billing total: every resource billed inside resource group %q (the AKS cluster's own resource group) and resource group %q (the AKS cluster's node resource group), attributed by exact Azure resource ID. Includes AKS control-plane charges (if any), node VM/VMSS compute, managed disks, load balancers, and public IPs billed within these resource groups.",
		identity.ResourceGroup, nodeRG)
}

func billingDisclosures(clusterResourceGroup, nodeResourceGroup string) []string {
	return []string{
		fmt.Sprintf("This total includes all billed resources in both configured resource groups (%q and %q). Resources unrelated to this cluster may be included; cluster ownership has not been independently verified.", clusterResourceGroup, nodeResourceGroup),
		"Shared or externally hosted resources billed outside the cluster and node resource groups (for example a hub-network egress path, shared DNS, or cross-subscription resources) are not included in this total.",
		"Kubernetes-level Idle/Used/System allocation, as shown in the Azure Portal's AKS Cost Analysis view, is not exposed through a public API and is not shown here; this total reflects Azure resource billing only, not per-namespace or per-pod allocation.",
	}
}
