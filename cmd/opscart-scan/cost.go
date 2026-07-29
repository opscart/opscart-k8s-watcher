package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awspricing "github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/report"
)

func runOptimizeScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	ra := analyzer.NewResourceAnalyzer(clientset)
	analysis, err := ra.AnalyzeClusterResources(namespace)
	if err != nil {
		return fmt.Errorf("analyzing resources: %w", err)
	}

	analyzer.PrintOptimizationSummary(analysis.Optimizations)
	return nil
}

func runCostsScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	// First get resource analysis
	ra := analyzer.NewResourceAnalyzer(clientset)
	resourceAnalysis, err := ra.AnalyzeClusterResources(namespace)
	if err != nil {
		return fmt.Errorf("analyzing resources: %w", err)
	}

	// Then perform cost analysis
	ca := analyzer.NewCostAnalyzer(resourceAnalysis)
	costEstimate, err := ca.AnalyzeCosts(monthlyCost) // monthlyCost=0 => resource-share mode
	if err != nil {
		return fmt.Errorf("analyzing costs: %w", err)
	}

	// Inject cluster name for HTML report
	costEstimate.ClusterName = clusterContext

	// Optionally enrich with deployment-level breakdown
	if breakdown == "deployment" {
		da := analyzer.NewDeploymentCostAnalyzer(clientset)
		enriched, err := da.EnrichWithDeployments(costEstimate.NamespaceCosts)
		if err == nil {
			costEstimate.NamespaceCosts = enriched
		}
	}

	if err := analyzer.PrintCostAnalysis(costEstimate, format); err != nil {
		return fmt.Errorf("rendering output: %w", err)
	}
	return nil
}

func runCloudCostsScan(clusterContext string) error {
	if pricingSource != "auto" && pricingSource != "embedded" && pricingSource != "aws-api" {
		return fmt.Errorf("invalid pricing source %q: use auto, embedded, or aws-api", pricingSource)
	}
	providerOverride, err := analyzer.ParseCloudProviderOverride(cloudProvider)
	if err != nil {
		return err
	}
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	// ── Step 1: Analyze node pools and compute real VM costs ──────────────
	npa := analyzer.NewNodePoolCostAnalyzer(clientset, region)
	npa.SetCloudProviderOverride(providerOverride)
	if pricingSource == "aws-api" {
		cfg, cfgErr := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion("us-east-1"))
		if cfgErr == nil {
			npa.SetPricingProvider(analyzer.NewAWSPricingProvider(awspricing.NewFromConfig(cfg), 24*time.Hour))
		}
	}
	poolCosts, _, err := npa.AnalyzeNodePoolCosts()
	if err != nil {
		return fmt.Errorf("analyzing node pool costs: %w", err)
	}
	totalNodeCost := analyzer.TotalClusterCostFromPools(poolCosts)

	// ── Step 2: Get resource analysis for namespace breakdown ─────────────
	ra := analyzer.NewResourceAnalyzer(clientset)
	resourceAnalysis, err := ra.AnalyzeClusterResources(namespace)
	if err != nil {
		return fmt.Errorf("analyzing resources: %w", err)
	}

	// ── Step 3: Allocate costs to namespaces based on real pricing ────────
	nsCosts := npa.AllocateNamespaceCosts(
		poolCosts,
		resourceAnalysis.Namespaces,
		resourceAnalysis.TotalCPUCores,
		resourceAnalysis.TotalMemoryGB,
	)

	// ── Step 4: Optionally enrich with per-deployment breakdown ───────────
	if breakdown == "deployment" {
		da := analyzer.NewDeploymentCostAnalyzer(clientset)
		enriched, err := da.EnrichWithDeployments(nsCosts)
		if err == nil {
			nsCosts = enriched
		}
	}

	// ── Step 5: Generate optimization scenarios using real costs ─────────
	ca := analyzer.NewCostAnalyzer(resourceAnalysis)
	costEstimate, _ := ca.AnalyzeCosts(totalNodeCost)
	scenarios := costEstimate.OptimizationScenarios
	savingsPotential := costEstimate.TotalSavingsPotential
	if npa.Provider() != analyzer.CloudProviderAzure {
		scenarios = nil
		savingsPotential = models.CostRange{}
	}

	// ── Step 7: Detect region (from node labels if not specified) ─────────
	detectedRegion := region
	if detectedRegion == "" && len(poolCosts) > 0 {
		// Pull from node pool builder
		detectedRegion = "auto-detected"
	}
	matchedNodes, totalNodes := 0, 0
	for _, pool := range poolCosts {
		totalNodes += pool.NodeCount
		if pool.PricingAvailable {
			matchedNodes += pool.NodeCount
		}
	}

	// ── Step 8: Build CloudCostReport ────────────────────────────────────
	costReport := &models.CloudCostReport{
		Timestamp:             time.Now(),
		ClusterName:           clusterContext,
		Region:                detectedRegion,
		Provider:              string(npa.Provider()),
		DetectedProvider:      string(npa.DetectedProvider()),
		EffectiveProvider:     string(npa.Provider()),
		ProviderDetectionMode: npa.ProviderDetectionMode(),
		ProviderWarning:       npa.ProviderWarning(),
		NodePoolCosts:         poolCosts,
		TotalNodeCost:         totalNodeCost,
		NamespaceCosts:        nsCosts,
		TotalMonthlyCost:      totalNodeCost,
		TotalAnnualCost:       totalNodeCost * 12,
		CostBreakdown: models.CostBreakdown{
			Compute: totalNodeCost,
		},
		OptimizationScenarios: scenarios,
		TotalSavingsPotential: savingsPotential,
		PricingSource:         pricingSource,
		PricingCoverage:       fmt.Sprintf("%d of %d nodes priced", matchedNodes, totalNodes),
		PricingWarnings:       npa.PricingWarnings(),
		Currency:              "USD",
		LastPriceRefresh:      npa.LastPriceRefresh(),
		Assumptions:           []string{"Cost allocation uses a weighted average of CPU and memory requests."},
		Disclaimers:           []string{"Public/list pricing estimates are not invoice values."},
	}

	// ── Step 9: Render output ────────────────────────────────────────────
	return analyzer.PrintCloudCostReport(costReport, costFormat)
}

func buildCostBreakdownFromScenarios(scenarios []models.OptimizationScenario, totalClusterCost float64) []report.CostItem {
	if len(scenarios) == 0 {
		return nil
	}

	sortedScenarios := make([]models.OptimizationScenario, len(scenarios))
	copy(sortedScenarios, scenarios)
	sort.Slice(sortedScenarios, func(i, j int) bool {
		return sortedScenarios[i].Savings.Best > sortedScenarios[j].Savings.Best
	})

	items := make([]report.CostItem, 0, len(sortedScenarios))
	for _, scenario := range sortedScenarios {
		impact := "Low"
		if totalClusterCost > 0 {
			savingsPercent := (scenario.Savings.Best / totalClusterCost) * 100
			if savingsPercent >= 10 {
				impact = "High"
			} else if savingsPercent >= 5 {
				impact = "Medium"
			}
		}

		action := scenario.Timeline
		if len(scenario.Actions) > 0 {
			action = scenario.Actions[0]
		}

		items = append(items, report.CostItem{
			Name:    scenario.Name,
			Impact:  impact,
			Savings: scenario.Savings.Best,
			Action:  action,
		})
	}

	return items
}

func deriveCostConfidence(weightedSharePct float64) string {
	if weightedSharePct >= 15 {
		return "High"
	}
	if weightedSharePct >= 5 {
		return "Medium"
	}
	if weightedSharePct < 2 {
		return "Low"
	}
	return "Medium"
}
