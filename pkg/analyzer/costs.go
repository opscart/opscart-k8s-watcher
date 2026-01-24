package analyzer

import (
	"fmt"
	"strings"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// CostAnalyzer performs cost analysis and optimization scenario modeling
type CostAnalyzer struct {
	resourceAnalysis *models.ClusterResourceAnalysis
}

// NewCostAnalyzer creates a new cost analyzer from resource analysis
func NewCostAnalyzer(resourceAnalysis *models.ClusterResourceAnalysis) *CostAnalyzer {
	return &CostAnalyzer{
		resourceAnalysis: resourceAnalysis,
	}
}

// AnalyzeCosts calculates cost estimates with ranges and optimization scenarios
func (ca *CostAnalyzer) AnalyzeCosts(totalClusterCost float64) (*models.CostEstimate, error) {
	if totalClusterCost <= 0 {
		return nil, fmt.Errorf("total cluster cost must be greater than 0")
	}

	estimate := &models.CostEstimate{
		TotalClusterCost: totalClusterCost,
		Method:           "request_proportional",
		Confidence:       "medium",
		Assumptions:      ca.generateAssumptions(),
		Disclaimers:      ca.generateDisclaimers(),
	}

	// Calculate namespace costs
	estimate.NamespaceCosts = ca.calculateNamespaceCosts(totalClusterCost)

	// Generate optimization scenarios
	estimate.OptimizationScenarios = ca.generateOptimizationScenarios(totalClusterCost)

	// Calculate total savings potential
	estimate.TotalSavingsPotential = ca.calculateTotalSavings(estimate.OptimizationScenarios)

	return estimate, nil
}

// calculateNamespaceCosts allocates cluster cost proportionally to namespaces
func (ca *CostAnalyzer) calculateNamespaceCosts(totalCost float64) []models.NamespaceCostInfo {
	var costs []models.NamespaceCostInfo

	for _, ns := range ca.resourceAnalysis.Namespaces {
		// Calculate weighted share (CPU + Memory) / 2
		weightedShare := (ns.CPUPercent + ns.MemoryPercent) / 2.0 / 100.0

		// Base cost allocation
		baseCost := totalCost * weightedShare

		// Calculate range based on uncertainty factors
		costRange := ca.calculateCostRange(baseCost, ns)

		costs = append(costs, models.NamespaceCostInfo{
			Name:          ns.Name,
			EstimatedCost: costRange,
			CPUShare:      ns.CPUPercent / 100.0,
			MemoryShare:   ns.MemoryPercent / 100.0,
			WeightedShare: weightedShare,
		})
	}

	return costs
}

// calculateCostRange calculates low/best/high cost estimates with confidence-based uncertainty
func (ca *CostAnalyzer) calculateCostRange(baseCost float64, ns models.NamespaceResourceUsage) models.CostRange {
	// Base cost is our "best" estimate
	best := baseCost

	// Calculate confidence level based on namespace characteristics
	confidence := ca.calculateConfidence(ns)

	// Uncertainty decreases with higher confidence
	// High confidence (0.8): ±20% range
	// Medium confidence (0.6): ±40% range
	// Low confidence (0.4): ±60% range
	uncertainty := 1.0 - confidence

	// Apply spot discount potential for low estimate
	spotPotential := 0.0
	if ns.SpotEligiblePods > 0 {
		spotRatio := float64(ns.SpotEligiblePods) / float64(ns.PodCount)
		spotPotential = spotRatio * 0.7 // 70% discount for spot-eligible portion
	}

	// Low estimate: Best case (spot discount + good utilization + low uncertainty)
	low := baseCost * (1 - spotPotential) * (1 - (uncertainty * 0.5))

	// High estimate: Worst case (on-demand + waste + high uncertainty)
	wasteFactor := 0.0
	if ns.WasteScore > 50 {
		wasteFactor = 0.3 // 30% waste for high waste score
	} else if ns.WasteScore > 30 {
		wasteFactor = 0.15 // 15% waste for medium waste score
	}
	high := baseCost * (1 + wasteFactor) * (1 + (uncertainty * 0.5))

	return models.CostRange{
		Low:  low,
		Best: best,
		High: high,
	}
}

// calculateConfidence determines confidence level (0.0-1.0) based on namespace characteristics
func (ca *CostAnalyzer) calculateConfidence(ns models.NamespaceResourceUsage) float64 {
	confidence := 0.5 // Start with medium confidence

	// Higher confidence for larger resource consumers (more stable allocation)
	if ns.WeightedShare() > 0.15 { // >15% of cluster
		confidence += 0.2
	} else if ns.WeightedShare() > 0.05 { // 5-15% of cluster
		confidence += 0.1
	} else if ns.WeightedShare() < 0.02 { // <2% of cluster
		confidence -= 0.1 // Lower confidence for tiny namespaces
	}

	// Lower confidence for high waste (unpredictable costs)
	if ns.WasteScore > 50 {
		confidence -= 0.2
	} else if ns.WasteScore > 30 {
		confidence -= 0.1
	}

	// Higher confidence for namespaces with many pods (more predictable)
	if ns.PodCount > 10 {
		confidence += 0.1
	} else if ns.PodCount < 3 {
		confidence -= 0.1
	}

	// Clamp to 0.3-0.9 range
	if confidence < 0.3 {
		confidence = 0.3
	}
	if confidence > 0.9 {
		confidence = 0.9
	}

	return confidence
}

// generateOptimizationScenarios creates actionable cost optimization scenarios
func (ca *CostAnalyzer) generateOptimizationScenarios(totalCost float64) []models.OptimizationScenario {
	var scenarios []models.OptimizationScenario

	// Scenario 1: Move spot-eligible workloads to spot nodes
	spotScenario := ca.generateSpotScenario(totalCost)
	if spotScenario != nil {
		scenarios = append(scenarios, *spotScenario)
	}

	// Scenario 2: Delete idle namespaces
	idleScenario := ca.generateIdleScenario(totalCost)
	if idleScenario != nil {
		scenarios = append(scenarios, *idleScenario)
	}

	// Scenario 3: Right-size over-provisioned workloads
	rightsizeScenario := ca.generateRightsizeScenario(totalCost)
	if rightsizeScenario != nil {
		scenarios = append(scenarios, *rightsizeScenario)
	}

	// Scenario 4: Add Horizontal Pod Autoscalers
	hpaScenario := ca.generateHPAScenario(totalCost)
	if hpaScenario != nil {
		scenarios = append(scenarios, *hpaScenario)
	}

	return scenarios
}

// generateSpotScenario calculates savings from moving to spot instances
func (ca *CostAnalyzer) generateSpotScenario(totalCost float64) *models.OptimizationScenario {
	spotEligibleCPU := 0.0
	spotEligibleMemory := 0.0
	spotEligibleCost := 0.0
	affectedNamespaces := []string{}

	for _, ns := range ca.resourceAnalysis.Namespaces {
		// Use actual spot eligibility data from resource scanner
		if ns.SpotEligiblePods > 0 {
			spotRatio := float64(ns.SpotEligiblePods) / float64(ns.PodCount)

			// Only recommend if significant portion is spot-eligible
			// Require at least 2 pods and >50% spot-eligible
			if spotRatio > 0.5 && ns.SpotEligiblePods >= 2 {
				// Calculate resources that could move to spot
				spotCPU := ns.CPUCoresRequested * spotRatio
				spotMemory := ns.MemoryGBRequested * spotRatio
				spotEligibleCPU += spotCPU
				spotEligibleMemory += spotMemory

				// Calculate this namespace's cost (proportional allocation)
				weightedShare := (ns.CPUPercent + ns.MemoryPercent) / 2.0 / 100.0
				nsCost := totalCost * weightedShare * spotRatio
				spotEligibleCost += nsCost

				affectedNamespaces = append(affectedNamespaces, fmt.Sprintf("%s (%d/%d pods)",
					ns.Name, ns.SpotEligiblePods, ns.PodCount))
			}
		}
	}

	// Need at least $50/month to make it worthwhile for large clusters
	// For smaller clusters (<$2000), lower threshold proportionally
	minThreshold := 50.0
	if totalCost < 2000 {
		minThreshold = totalCost * 0.05 // 5% of cluster cost minimum
		if minThreshold < 20 {
			minThreshold = 20 // Absolute minimum $20/month
		}
	}

	if spotEligibleCost < minThreshold {
		return nil
	}

	// Spot instances typically save 70% (range: 60-80%)
	savingsLow := spotEligibleCost * 0.60
	savingsBest := spotEligibleCost * 0.70
	savingsHigh := spotEligibleCost * 0.80

	return &models.OptimizationScenario{
		Name: "Migrate to Spot Instances",
		Description: fmt.Sprintf("Move %d namespaces to spot node pools (%.1f CPU cores, %.1f GB memory)",
			len(affectedNamespaces), spotEligibleCPU, spotEligibleMemory),
		CurrentCost: models.CostRange{
			Low:  spotEligibleCost * 0.9,
			Best: spotEligibleCost,
			High: spotEligibleCost * 1.1,
		},
		AfterCost: models.CostRange{
			Low:  spotEligibleCost * 0.20,
			Best: spotEligibleCost * 0.30,
			High: spotEligibleCost * 0.40,
		},
		Savings: models.CostRange{
			Low:  savingsLow,
			Best: savingsBest,
			High: savingsHigh,
		},
		Impact: fmt.Sprintf("%.1f CPU cores, %.1f GB memory eligible for spot (%.0f%% cost reduction)",
			spotEligibleCPU, spotEligibleMemory, savingsBest/spotEligibleCost*100),
		Effort:   "Medium",
		Risk:     "Low",
		Timeline: "1-2 weeks",
		Actions: []string{
			"Create spot node pool in AKS with appropriate VM size",
			fmt.Sprintf("Add tolerations to deployments in: %s", strings.Join(affectedNamespaces, ", ")),
			"Add node selector: kubernetes.azure.com/scalesetpriority=spot",
			"Test application tolerance for evictions (spot instances can be reclaimed)",
			"Set appropriate PodDisruptionBudgets to handle evictions gracefully",
		},
	}
}

// generateIdleScenario calculates savings from deleting idle resources
func (ca *CostAnalyzer) generateIdleScenario(totalCost float64) *models.OptimizationScenario {
	idleCost := 0.0
	idleCPU := 0.0
	idleMemory := 0.0
	idleNamespaces := []string{}

	for _, ns := range ca.resourceAnalysis.Namespaces {
		// Use WasteScore from resource analysis
		// High waste score (>70) indicates idle/unused resources
		if ns.WasteScore > 70 {
			weightedShare := (ns.CPUPercent + ns.MemoryPercent) / 2.0 / 100.0
			nsCost := totalCost * weightedShare

			// Only recommend deletion if cost is significant enough
			if nsCost > 20 {
				idleCost += nsCost
				idleCPU += ns.CPUCoresRequested
				idleMemory += ns.MemoryGBRequested

				idleReason := ""
				if ns.IdlePods > 0 {
					idleReason = fmt.Sprintf("%d idle pods", ns.IdlePods)
				} else {
					idleReason = "high waste score"
				}
				idleNamespaces = append(idleNamespaces, fmt.Sprintf("%s (%s)", ns.Name, idleReason))
			}
		}
	}

	// Need at least $30/month for large clusters, scale down for smaller clusters
	minThreshold := 30.0
	if totalCost < 2000 {
		minThreshold = totalCost * 0.03 // 3% of cluster cost minimum
		if minThreshold < 15 {
			minThreshold = 15 // Absolute minimum $15/month
		}
	}

	if idleCost < minThreshold {
		return nil
	}

	// Deleting idle resources = 100% savings on those resources
	return &models.OptimizationScenario{
		Name: "Delete Idle Namespaces",
		Description: fmt.Sprintf("Remove %d idle namespaces (%.1f CPU, %.1f GB memory)",
			len(idleNamespaces), idleCPU, idleMemory),
		CurrentCost: models.CostRange{
			Low:  idleCost * 0.9,
			Best: idleCost,
			High: idleCost * 1.1,
		},
		AfterCost: models.CostRange{
			Low:  0,
			Best: 0,
			High: 0,
		},
		Savings: models.CostRange{
			Low:  idleCost * 0.9,
			Best: idleCost,
			High: idleCost * 1.1,
		},
		Impact: fmt.Sprintf("Free %.1f CPU cores, %.1f GB memory (%.0f%% of cluster)",
			idleCPU, idleMemory, (idleCPU/ca.resourceAnalysis.TotalCPUCores)*100),
		Effort:   "Low",
		Risk:     "Low",
		Timeline: "1 day",
		Actions: []string{
			fmt.Sprintf("Verify these namespaces are truly unused: %s", strings.Join(idleNamespaces, ", ")),
			"Check for any important data or configs that need backup",
			"Delete idle namespaces: kubectl delete namespace <name>",
			"Monitor for any application dependencies",
		},
	}
}

// generateRightsizeScenario calculates savings from right-sizing
func (ca *CostAnalyzer) generateRightsizeScenario(totalCost float64) *models.OptimizationScenario {
	oversizedCost := 0.0
	oversizedCPU := 0.0
	oversizedNamespaces := []string{}

	for _, ns := range ca.resourceAnalysis.Namespaces {
		// Check if over-provisioned (high requests, low pod count)
		if ns.PodCount > 0 && ns.PodCount < 5 {
			avgCPU := ns.CPUCoresRequested / float64(ns.PodCount)
			if avgCPU > 2.0 { // More than 2 CPU per pod suggests over-provisioning
				weightedShare := (ns.CPUPercent + ns.MemoryPercent) / 2.0 / 100.0
				nsCost := totalCost * weightedShare
				oversizedCost += nsCost
				oversizedCPU += ns.CPUCoresRequested
				oversizedNamespaces = append(oversizedNamespaces, ns.Name)
			}
		}
	}

	// Need meaningful savings for large clusters, scale for smaller ones
	minThreshold := 50.0
	if totalCost < 2000 {
		minThreshold = totalCost * 0.05 // 5% of cluster cost
		if minThreshold < 20 {
			minThreshold = 20
		}
	}

	if oversizedCost < minThreshold {
		return nil
	}

	// Right-sizing typically saves 40-60% on over-provisioned resources
	savingsLow := oversizedCost * 0.40
	savingsBest := oversizedCost * 0.50
	savingsHigh := oversizedCost * 0.60

	return &models.OptimizationScenario{
		Name:        "Right-size Over-provisioned Workloads",
		Description: fmt.Sprintf("Reduce resource requests for %d over-provisioned namespaces", len(oversizedNamespaces)),
		CurrentCost: models.CostRange{Low: oversizedCost * 0.9, Best: oversizedCost, High: oversizedCost * 1.1},
		AfterCost:   models.CostRange{Low: oversizedCost * 0.35, Best: oversizedCost * 0.50, High: oversizedCost * 0.65},
		Savings:     models.CostRange{Low: savingsLow, Best: savingsBest, High: savingsHigh},
		Impact:      fmt.Sprintf("Free %.1f CPU cores through right-sizing", oversizedCPU*0.5),
		Effort:      "Medium",
		Risk:        "Medium",
		Timeline:    "2-3 weeks",
		Actions: []string{
			"Install metrics-server to track actual usage",
			"Monitor actual CPU/memory usage for 1-2 weeks",
			"Adjust resource requests in: " + formatList(oversizedNamespaces),
			"Test performance after changes",
		},
	}
}

// calculateTotalSavings sums up all optimization scenario savings
func (ca *CostAnalyzer) calculateTotalSavings(scenarios []models.OptimizationScenario) models.CostRange {
	var totalLow, totalBest, totalHigh float64

	for _, scenario := range scenarios {
		totalLow += scenario.Savings.Low
		totalBest += scenario.Savings.Best
		totalHigh += scenario.Savings.High
	}

	return models.CostRange{
		Low:  totalLow,
		Best: totalBest,
		High: totalHigh,
	}
}

// generateAssumptions creates the list of assumptions
func (ca *CostAnalyzer) generateAssumptions() []string {
	return []string{
		"Cost allocation based on CPU + Memory resource requests (not actual usage)",
		"Does NOT include: storage costs, networking egress, load balancers, public IPs",
		"Spot instance savings assume 70% discount vs on-demand",
		"Assumes proportional sharing of node costs across pods",
		"Cluster cost provided by user - not validated against actual Azure billing",
	}
}

// generateDisclaimers creates warning disclaimers
func (ca *CostAnalyzer) generateDisclaimers() []string {
	return []string{
		"⚠️  These are ESTIMATES with ranges - not exact costs",
		"⚠️  Actual costs depend on: VM sizes, reserved instances, spot pricing, node utilization",
		"⚠️  Use Azure Cost Management for actual billing data",
		"⚠️  Optimization savings are potential - results may vary",
	}
}

// generateHPAScenario detects namespaces that could benefit from autoscaling
func (ca *CostAnalyzer) generateHPAScenario(totalCost float64) *models.OptimizationScenario {
	hpaCandidateCost := 0.0
	hpaCandidateCPU := 0.0
	hpaCandidates := []string{}

	for _, ns := range ca.resourceAnalysis.Namespaces {
		// Good HPA candidates: production namespaces with multiple pods and no waste
		// (if they had HPA with waste, they'd scale down already)
		if ns.PodCount >= 3 && ns.PodCount <= 20 {
			// Skip if already appears to be autoscaling (many identical pods suggests HPA)
			// Also good candidates if they have fixed sizing in prod
			if ns.Name != "kube-system" && ns.Name != "istio-system" {
				weightedShare := ns.WeightedShare()
				nsCost := totalCost * weightedShare

				// Only recommend for namespaces with reasonable cost
				if nsCost > 100 {
					hpaCandidateCost += nsCost
					hpaCandidateCPU += ns.CPUCoresRequested
					hpaCandidates = append(hpaCandidates, fmt.Sprintf("%s (%d pods)", ns.Name, ns.PodCount))
				}
			}
		}
	}

	// Need at least $100/month for large clusters, scale for smaller ones
	minThreshold := 100.0
	if totalCost < 2000 {
		minThreshold = totalCost * 0.10 // 10% of cluster cost
		if minThreshold < 30 {
			minThreshold = 30
		}
	}

	if hpaCandidateCost < minThreshold || len(hpaCandidates) == 0 {
		return nil
	}

	// HPA typically saves 20-40% during off-peak hours (assume 40% of the time)
	savingsLow := hpaCandidateCost * 0.15  // 15% conservative
	savingsBest := hpaCandidateCost * 0.25 // 25% realistic
	savingsHigh := hpaCandidateCost * 0.35 // 35% optimistic

	return &models.OptimizationScenario{
		Name:        "Add Horizontal Pod Autoscalers",
		Description: fmt.Sprintf("Configure HPA for %d namespaces to scale based on load", len(hpaCandidates)),
		CurrentCost: models.CostRange{
			Low:  hpaCandidateCost * 0.9,
			Best: hpaCandidateCost,
			High: hpaCandidateCost * 1.1,
		},
		AfterCost: models.CostRange{
			Low:  hpaCandidateCost * 0.65,
			Best: hpaCandidateCost * 0.75,
			High: hpaCandidateCost * 0.85,
		},
		Savings: models.CostRange{
			Low:  savingsLow,
			Best: savingsBest,
			High: savingsHigh,
		},
		Impact:   fmt.Sprintf("Dynamic scaling for %.1f CPU cores (save ~25%% during off-peak)", hpaCandidateCPU),
		Effort:   "Medium",
		Risk:     "Low",
		Timeline: "1-2 weeks",
		Actions: []string{
			"Install metrics-server if not already present: kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml",
			fmt.Sprintf("Configure HPA for deployments in: %s", strings.Join(hpaCandidates, ", ")),
			"Example: kubectl autoscale deployment <name> --cpu-percent=70 --min=2 --max=10",
			"Monitor scaling behavior and adjust thresholds as needed",
			"Set appropriate PodDisruptionBudgets to maintain availability during scale-down",
		},
	}
}

// formatList formats a slice of strings for display
func formatList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) == 1 {
		return items[0]
	}
	if len(items) <= 3 {
		result := ""
		for i, item := range items {
			if i > 0 {
				result += ", "
			}
			result += item
		}
		return result
	}
	// More than 3, show first 2 and count
	return fmt.Sprintf("%s, %s, and %d more", items[0], items[1], len(items)-2)
}
