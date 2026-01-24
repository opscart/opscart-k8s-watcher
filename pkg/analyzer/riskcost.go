package analyzer

import (
	"fmt"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// RiskCostAnalyzer calculates financial risk from security vulnerabilities
type RiskCostAnalyzer struct {
	securityAudit *models.SecurityAudit
	config        *RiskCostConfig
}

// NewRiskCostAnalyzer creates a new risk-cost analyzer with default config
func NewRiskCostAnalyzer(securityAudit *models.SecurityAudit) *RiskCostAnalyzer {
	return &RiskCostAnalyzer{
		securityAudit: securityAudit,
		config:        DefaultConfig(),
	}
}

// NewRiskCostAnalyzerWithConfig creates analyzer with custom config
func NewRiskCostAnalyzerWithConfig(securityAudit *models.SecurityAudit, config *RiskCostConfig) *RiskCostAnalyzer {
	return &RiskCostAnalyzer{
		securityAudit: securityAudit,
		config:        config,
	}
}

// CalculateRiskCost maps security vulnerabilities to financial risk
func (rca *RiskCostAnalyzer) CalculateRiskCost() *models.RiskCostAnalysis {
	analysis := &models.RiskCostAnalysis{
		SecurityScore:     rca.securityAudit.SecurityScore,
		TotalRiskExposure: models.CostRange{},
		RiskCategories:    []models.RiskCategory{},
		RemediationPlan:   models.RemediationPlan{},
	}

	// Calculate risk for each security issue type
	analysis.RiskCategories = rca.calculateRiskCategories()

	// Sum total risk exposure
	analysis.TotalRiskExposure = rca.calculateTotalRisk(analysis.RiskCategories)

	// Calculate remediation cost and ROI
	analysis.RemediationPlan = rca.calculateRemediationPlan(analysis.RiskCategories)

	// Generate priority recommendations
	analysis.PriorityRecommendations = rca.generatePriorityRecommendations(analysis.RiskCategories)

	return analysis
}

// calculateRiskCategories maps each vulnerability type to financial risk
func (rca *RiskCostAnalyzer) calculateRiskCategories() []models.RiskCategory {
	var categories []models.RiskCategory

	risks := rca.securityAudit.Risks

	// CRITICAL: Privileged containers
	// Risk: Container escape → full cluster compromise
	if risks.PrivilegedContainers > 0 {
		avgBreachCost := rca.config.PrivilegedContainerCost
		probability := 0.15 // 15% probability over 1 year

		categories = append(categories, models.RiskCategory{
			Name:        "Privileged Containers",
			Severity:    "critical",
			Count:       risks.PrivilegedContainers,
			Description: "Containers with privileged mode can escape to host and compromise entire cluster",
			RiskExposure: models.CostRange{
				Low:  float64(risks.PrivilegedContainers) * avgBreachCost * (probability * 0.5),
				Best: float64(risks.PrivilegedContainers) * avgBreachCost * probability,
				High: float64(risks.PrivilegedContainers) * avgBreachCost * (probability * 2.0),
			},
			TypicalIncidents: []string{
				"Container escape leading to node compromise",
				"Lateral movement across cluster",
				"Data exfiltration from host filesystem",
			},
			IndustryExamples: []string{
				"Tesla 2018: Cryptomining via privileged container ($50K+ in compute costs)",
				"Average container escape incident cost: $25K (Ponemon 2023)",
			},
		})
	}

	// CRITICAL: hostPath volumes
	// Risk: Direct host filesystem access → data breach
	if risks.HostPathVolumes > 0 {
		avgBreachCost := rca.config.HostPathCost
		probability := 0.20 // 20% probability - easier to exploit

		categories = append(categories, models.RiskCategory{
			Name:        "Host Path Volumes",
			Severity:    "critical",
			Count:       risks.HostPathVolumes,
			Description: "Direct host filesystem access enables data exfiltration and credential theft",
			RiskExposure: models.CostRange{
				Low:  float64(risks.HostPathVolumes) * avgBreachCost * (probability * 0.5),
				Best: float64(risks.HostPathVolumes) * avgBreachCost * probability,
				High: float64(risks.HostPathVolumes) * avgBreachCost * (probability * 2.0),
			},
			TypicalIncidents: []string{
				"Access to /etc/shadow for credential theft",
				"Docker socket exploitation",
				"Reading application secrets from host",
			},
			IndustryExamples: []string{
				"Docker socket abuse: Average incident cost $35K",
				"Credential theft via hostPath: 23% of K8s breaches (Aqua Security 2023)",
			},
		})
	}

	// CRITICAL: Host PID namespace
	if risks.HostPID > 0 {
		avgBreachCost := rca.config.HostPIDCost
		probability := 0.12

		categories = append(categories, models.RiskCategory{
			Name:        "Host PID Namespace",
			Severity:    "critical",
			Count:       risks.HostPID,
			Description: "Access to host processes enables process injection and privilege escalation",
			RiskExposure: models.CostRange{
				Low:  float64(risks.HostPID) * avgBreachCost * (probability * 0.5),
				Best: float64(risks.HostPID) * avgBreachCost * probability,
				High: float64(risks.HostPID) * avgBreachCost * (probability * 2.0),
			},
			TypicalIncidents: []string{
				"Process injection into privileged processes",
				"Information disclosure via /proc",
				"Signal-based denial of service",
			},
			IndustryExamples: []string{
				fmt.Sprintf("Host PID exploitation: $%.0fK average incident cost", avgBreachCost/1000),
			},
		})
	}

	// HIGH: Running as root
	if risks.RunningAsRoot > 0 {
		avgBreachCost := rca.config.RunningAsRootCost
		probability := 0.10

		categories = append(categories, models.RiskCategory{
			Name:        "Containers Running as Root",
			Severity:    "high",
			Count:       risks.RunningAsRoot,
			Description: "Root user in containers amplifies damage from application vulnerabilities",
			RiskExposure: models.CostRange{
				Low:  float64(risks.RunningAsRoot) * avgBreachCost * (probability * 0.3),
				Best: float64(risks.RunningAsRoot) * avgBreachCost * probability,
				High: float64(risks.RunningAsRoot) * avgBreachCost * (probability * 1.5),
			},
			TypicalIncidents: []string{
				"CVE exploitation with root privileges",
				"Container filesystem modification",
				"Capability abuse for lateral movement",
			},
			IndustryExamples: []string{
				"Log4Shell + root user: 3x more damage than non-root",
				"Root containers: 67% of critical K8s CVEs (StackRox 2022)",
			},
		})
	}

	// HIGH: Host Network
	if risks.HostNetwork > 0 {
		avgBreachCost := rca.config.HostNetworkCost
		probability := 0.08

		categories = append(categories, models.RiskCategory{
			Name:        "Host Network Usage",
			Severity:    "high",
			Count:       risks.HostNetwork,
			Description: "Bypasses network policies enabling lateral movement and service impersonation",
			RiskExposure: models.CostRange{
				Low:  float64(risks.HostNetwork) * avgBreachCost * (probability * 0.5),
				Best: float64(risks.HostNetwork) * avgBreachCost * probability,
				High: float64(risks.HostNetwork) * avgBreachCost * (probability * 1.5),
			},
			TypicalIncidents: []string{
				"Bypass of network segmentation",
				"Service impersonation attacks",
				"Cluster-wide port scanning",
			},
			IndustryExamples: []string{
				fmt.Sprintf("Network policy bypass: $%.0fK average incident", avgBreachCost/1000),
			},
		})
	}

	// MEDIUM: Missing resource limits
	if risks.MissingResourceLimits > 0 {
		avgImpactCost := rca.config.MissingLimitsCost
		probability := 0.25

		categories = append(categories, models.RiskCategory{
			Name:        "Missing Resource Limits",
			Severity:    "medium",
			Count:       risks.MissingResourceLimits,
			Description: "Enables resource exhaustion attacks causing cluster-wide outages",
			RiskExposure: models.CostRange{
				Low:  float64(risks.MissingResourceLimits) * avgImpactCost * (probability * 0.5),
				Best: float64(risks.MissingResourceLimits) * avgImpactCost * probability,
				High: float64(risks.MissingResourceLimits) * avgImpactCost * (probability * 1.5),
			},
			TypicalIncidents: []string{
				"Memory leak causing node eviction",
				"CPU spike affecting cluster performance",
				"OOMKilled cascading failures",
			},
			IndustryExamples: []string{
				fmt.Sprintf("Average cost of 1-hour production outage: $%.0fK", avgImpactCost/1000),
				"Resource exhaustion: 18% of K8s incidents (CNCF 2023)",
			},
		})
	}

	// MEDIUM: Default service accounts
	if risks.DefaultServiceAccount > 0 {
		avgBreachCost := rca.config.DefaultSACost
		probability := 0.06

		categories = append(categories, models.RiskCategory{
			Name:        "Default Service Account Usage",
			Severity:    "medium",
			Count:       risks.DefaultServiceAccount,
			Description: "Default service accounts often have excessive permissions enabling privilege escalation",
			RiskExposure: models.CostRange{
				Low:  float64(risks.DefaultServiceAccount) * avgBreachCost * (probability * 0.5),
				Best: float64(risks.DefaultServiceAccount) * avgBreachCost * probability,
				High: float64(risks.DefaultServiceAccount) * avgBreachCost * (probability * 1.5),
			},
			TypicalIncidents: []string{
				"Over-privileged API access from compromised pod",
				"Secret enumeration via service account token",
				"Namespace-wide resource manipulation",
			},
			IndustryExamples: []string{
				"Service account abuse: $6K average incident",
			},
		})
	}

	return categories
}

// calculateTotalRisk sums up all risk categories
func (rca *RiskCostAnalyzer) calculateTotalRisk(categories []models.RiskCategory) models.CostRange {
	var totalLow, totalBest, totalHigh float64

	for _, cat := range categories {
		totalLow += cat.RiskExposure.Low
		totalBest += cat.RiskExposure.Best
		totalHigh += cat.RiskExposure.High
	}

	return models.CostRange{
		Low:  totalLow,
		Best: totalBest,
		High: totalHigh,
	}
}

// calculateRemediationPlan estimates cost to fix issues and ROI
func (rca *RiskCostAnalyzer) calculateRemediationPlan(categories []models.RiskCategory) models.RemediationPlan {
	var totalHours float64
	var criticalHours, highHours, mediumHours float64

	// Estimate remediation effort for each category
	for _, cat := range categories {
		var hoursPerIssue float64

		switch cat.Severity {
		case "critical":
			hoursPerIssue = 2.0 // Critical issues need careful testing
			criticalHours += float64(cat.Count) * hoursPerIssue
		case "high":
			hoursPerIssue = 1.0
			highHours += float64(cat.Count) * hoursPerIssue
		case "medium":
			hoursPerIssue = 0.5
			mediumHours += float64(cat.Count) * hoursPerIssue
		}

		totalHours += float64(cat.Count) * hoursPerIssue
	}

	// Calculate cost using configured engineer rate
	engineerRate := rca.config.EngineerHourlyRate
	totalCost := totalHours * engineerRate

	// Calculate ROI
	riskReduction := rca.calculateTotalRisk(categories).Best
	roi := riskReduction / totalCost

	// Calculate payback period (months)
	monthlyRiskReduction := riskReduction / 12.0 // Annualized risk
	paybackMonths := totalCost / monthlyRiskReduction

	return models.RemediationPlan{
		TotalHours:    totalHours,
		CriticalHours: criticalHours,
		HighHours:     highHours,
		MediumHours:   mediumHours,
		EstimatedCost: totalCost,
		RiskReduction: riskReduction,
		ROI:           roi,
		PaybackMonths: paybackMonths,
		Timeline:      rca.estimateTimeline(totalHours),
		Phases: []models.RemediationPhase{
			{
				Name:     "Phase 1: Critical Issues",
				Duration: rca.estimateTimeline(criticalHours),
				Hours:    criticalHours,
				Cost:     criticalHours * engineerRate,
				Priority: "IMMEDIATE",
			},
			{
				Name:     "Phase 2: High Priority",
				Duration: rca.estimateTimeline(highHours),
				Hours:    highHours,
				Cost:     highHours * engineerRate,
				Priority: "THIS WEEK",
			},
			{
				Name:     "Phase 3: Medium Priority",
				Duration: rca.estimateTimeline(mediumHours),
				Hours:    mediumHours,
				Cost:     mediumHours * engineerRate,
				Priority: "THIS MONTH",
			},
		},
	}
}

// estimateTimeline converts hours to calendar time
func (rca *RiskCostAnalyzer) estimateTimeline(hours float64) string {
	if hours <= 8 {
		return "1 day"
	} else if hours <= 16 {
		return "2 days"
	} else if hours <= 40 {
		return "1 week"
	} else if hours <= 80 {
		return "2 weeks"
	} else if hours <= 160 {
		return "1 month"
	}
	return fmt.Sprintf("%.0f weeks", hours/40)
}

// generatePriorityRecommendations creates actionable recommendations
func (rca *RiskCostAnalyzer) generatePriorityRecommendations(categories []models.RiskCategory) []string {
	var recommendations []string

	// Sort by risk exposure
	criticalCategories := []models.RiskCategory{}
	for _, cat := range categories {
		if cat.Severity == "critical" && cat.RiskExposure.Best > 1000 {
			criticalCategories = append(criticalCategories, cat)
		}
	}

	// Generate recommendations
	if len(criticalCategories) > 0 {
		recommendations = append(recommendations,
			fmt.Sprintf("🔴 IMMEDIATE: Fix %d critical security issues (exposure: $%.0fK)",
				len(criticalCategories),
				sumRiskExposure(criticalCategories)/1000))
	}

	recommendations = append(recommendations,
		"Implement Kubernetes Pod Security Standards (PSS) at namespace level",
		"Configure network policies to segment workloads and limit lateral movement",
		"Enable audit logging to detect exploitation attempts",
		"Add security scanning in CI/CD pipeline to prevent future issues",
	)

	return recommendations
}

// sumRiskExposure sums the best estimate risk across categories
func sumRiskExposure(categories []models.RiskCategory) float64 {
	var total float64
	for _, cat := range categories {
		total += cat.RiskExposure.Best
	}
	return total
}
