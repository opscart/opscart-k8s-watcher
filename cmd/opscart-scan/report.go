package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/report"
)

func runReportGeneration(clusterContext string, clusterName string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterName)
	fmt.Println("📊 Generating comprehensive report...")

	// Get Kubernetes client
	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	// Run resource analysis for real ResourceScore and CostScore
	fmt.Println("  📦 Running resource analysis...")
	ra := analyzer.NewResourceAnalyzer(clientset)
	resourceAnalysis, raErr := ra.AnalyzeClusterResources(namespace)
	if raErr != nil {
		fmt.Printf("  ⚠️  Resource analysis skipped: %v\n", raErr)
		resourceAnalysis = nil
	}

	// Run REAL security audit
	fmt.Println("  🛡️  Running security audit...")
	sa := analyzer.NewSecurityAuditor(clientset)
	audit, err := sa.AuditClusterSecurity(namespace)
	if err != nil {
		return fmt.Errorf("security audit failed: %w", err)
	}

	// Run network policy audit for CIS 5.7.3 — real coverage data
	fmt.Println("  🌐 Running network policy audit (CIS 5.7.3)...")
	npa := analyzer.NewNetworkPolicyAuditor(clientset)
	netAudit, netErr := npa.AuditNetworkPolicies(namespace)
	if netErr != nil {
		fmt.Printf("  ⚠️  Network policy audit skipped: %v\n", netErr)
		netAudit = nil
	}
	cisResult := analyzer.CalculateCISScore(audit, netAudit)

	// Build report data with REAL security findings
	reportData := &report.ReportData{
		ClusterName:    clusterName,
		GeneratedAt:    time.Now(),
		CISScore:       cisResult.Score,
		SecurityScore:  cisResult.Score,
		ControlsPassed: cisResult.PassedChecks,
		ControlsFailed: cisResult.FailedChecks,
		PodCount:       audit.TotalPodsAudited,
		NamespaceCount: len(audit.Issues),
		MonthlyCost:    monthlyCost,
	}

	namespaceCostBest := map[string]float64{}
	namespaceCostLow := map[string]float64{}
	namespaceCostHigh := map[string]float64{}

	// Calculate savings if cost provided
	if monthlyCost > 0 {
		if resourceAnalysis != nil {
			ca := analyzer.NewCostAnalyzer(resourceAnalysis)
			costEstimate, costErr := ca.AnalyzeCosts(monthlyCost)
			if costErr != nil {
				fmt.Printf("  ⚠️  Cost analysis fallback used: %v\n", costErr)
				reportData.PotentialSavings = report.SavingsRange{
					Min: monthlyCost * 0.24,
					Max: monthlyCost * 0.36,
				}
			} else {
				reportData.PotentialSavings = report.SavingsRange{
					Min: costEstimate.TotalSavingsPotential.Low,
					Max: costEstimate.TotalSavingsPotential.High,
				}
				reportData.CostBreakdown = buildCostBreakdownFromScenarios(costEstimate.OptimizationScenarios, monthlyCost)

				for _, namespaceCost := range costEstimate.NamespaceCosts {
					namespaceCostBest[namespaceCost.Name] = namespaceCost.EstimatedCost.Best
					namespaceCostLow[namespaceCost.Name] = namespaceCost.EstimatedCost.Low
					namespaceCostHigh[namespaceCost.Name] = namespaceCost.EstimatedCost.High
				}
			}
		} else {
			reportData.PotentialSavings = report.SavingsRange{
				Min: monthlyCost * 0.24,
				Max: monthlyCost * 0.36,
			}
		}
	}

	// Extract security risks
	risks := audit.Risks

	// Add critical issues
	if risks.PrivilegedContainers > 0 {
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d privileged containers detected", risks.PrivilegedContainers),
			Description: "Containers with elevated privileges can escape containment",
			Count:       risks.PrivilegedContainers,
		})
	}

	if risks.HostPathVolumes > 0 {
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d pods mounting host paths", risks.HostPathVolumes),
			Description: "Host path volumes provide direct access to host filesystem",
			Count:       risks.HostPathVolumes,
		})
	}

	if risks.HostPID > 0 {
		reportData.CriticalIssues = append(reportData.CriticalIssues, report.IssueItem{
			Severity:    "critical",
			Title:       fmt.Sprintf("🔴 %d containers sharing host PID namespace", risks.HostPID),
			Description: "Host PID namespace sharing allows container processes to see all processes",
			Count:       risks.HostPID,
		})
	}

	// Add warnings
	if risks.RunningAsRoot > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers running as root", risks.RunningAsRoot),
			Description: "Running as root increases attack surface",
			Count:       risks.RunningAsRoot,
		})
	}

	if risks.MissingResourceLimits > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers missing resource limits", risks.MissingResourceLimits),
			Description: "Missing resource limits can lead to resource exhaustion",
			Count:       risks.MissingResourceLimits,
		})
	}

	if risks.HostNetwork > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers using host network", risks.HostNetwork),
			Description: "Host network access bypasses network policies",
			Count:       risks.HostNetwork,
		})
	}

	if risks.HostIPC > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers sharing host IPC namespace", risks.HostIPC),
			Description: "Host IPC namespace sharing can leak sensitive information",
			Count:       risks.HostIPC,
		})
	}

	if risks.PrivilegeEscalation > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers allowing privilege escalation", risks.PrivilegeEscalation),
			Description: "Privilege escalation can lead to container breakout",
			Count:       risks.PrivilegeEscalation,
		})
	}

	if risks.DefaultServiceAccount > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d pods using default service account", risks.DefaultServiceAccount),
			Description: "Default service account may have excessive permissions",
			Count:       risks.DefaultServiceAccount,
		})
	}

	if risks.AddedCapabilities > 0 {
		reportData.WarningIssues = append(reportData.WarningIssues, report.IssueItem{
			Severity:    "warning",
			Title:       fmt.Sprintf("🟡 %d containers with added capabilities", risks.AddedCapabilities),
			Description: "Unnecessary capabilities increase attack surface",
			Count:       risks.AddedCapabilities,
		})
	}

	// Calculate real resource and cost scores from actual cluster data
	resourceScore := 75 // fallback if resource analysis was skipped
	costScore := 60     // fallback if resource analysis was skipped
	if resourceAnalysis != nil {
		avgUtilization := (resourceAnalysis.CPUUtilization + resourceAnalysis.MemoryUtilization) / 2
		resourceScore = report.CalculateResourceScore(avgUtilization)

		totalPods, idlePods, spotEligible := 0, 0, 0
		for _, ns := range resourceAnalysis.Namespaces {
			totalPods += ns.PodCount
			idlePods += ns.IdlePods
			spotEligible += ns.SpotEligiblePods
		}
		if totalPods > 0 {
			costScore = report.CalculateCostScore(idlePods, spotEligible, totalPods)
		}

		// Populate resource fields in report data
		reportData.TotalCPU = resourceAnalysis.TotalCPUCores
		reportData.TotalMemory = resourceAnalysis.TotalMemoryGB
		reportData.UsedCPU = resourceAnalysis.TotalCPURequested
		reportData.UsedMemory = resourceAnalysis.TotalMemoryRequested

		for _, namespaceUsage := range resourceAnalysis.Namespaces {
			flags := append([]string{}, namespaceUsage.Flags...)
			if namespaceUsage.IdlePods > 0 {
				flags = append(flags, fmt.Sprintf("IDLE-%dp", namespaceUsage.IdlePods))
			}
			if namespaceUsage.SpotEligiblePods > 0 {
				flags = append(flags, fmt.Sprintf("SPOT-OK (%d)", namespaceUsage.SpotEligiblePods))
			}

			weightedSharePct := namespaceUsage.WeightedShare() * 100
			bestCost := namespaceCostBest[namespaceUsage.Name]
			lowCost := namespaceCostLow[namespaceUsage.Name]
			highCost := namespaceCostHigh[namespaceUsage.Name]

			if monthlyCost > 0 && bestCost == 0 {
				bestCost = monthlyCost * namespaceUsage.WeightedShare()
				lowCost = bestCost * 0.8
				highCost = bestCost * 1.2
			}

			reportData.Namespaces = append(reportData.Namespaces, report.NamespaceItem{
				Name:             namespaceUsage.Name,
				CPUPercent:       namespaceUsage.CPUPercent,
				MemPercent:       namespaceUsage.MemoryPercent,
				PodCount:         namespaceUsage.PodCount,
				Cost:             bestCost,
				CostLow:          lowCost,
				CostHigh:         highCost,
				WeightedShare:    weightedSharePct,
				Confidence:       deriveCostConfidence(weightedSharePct),
				IdlePods:         namespaceUsage.IdlePods,
				SpotEligiblePods: namespaceUsage.SpotEligiblePods,
				Flags:            flags,
			})
		}

		if monthlyCost > 0 {
			sort.Slice(reportData.Namespaces, func(i, j int) bool {
				return reportData.Namespaces[i].Cost > reportData.Namespaces[j].Cost
			})
		}
	}
	reportData.ResourceScore = resourceScore
	reportData.CostScore = costScore
	reportData.OverallScore = report.CalculateOverallScore(reportData.SecurityScore, resourceScore, costScore)

	// Default to html if not specified
	if reportFormat == "" {
		reportFormat = "html"
	}

	// Determine format
	var reportFmt report.ReportFormat
	switch reportFormat {
	case "html":
		reportFmt = report.FormatHTML
	case "json":
		reportFmt = report.FormatJSON
	case "csv":
		reportFmt = report.FormatCSV
	default:
		return fmt.Errorf("unsupported format: %s", reportFormat)
	}

	// Generate report
	generator := report.NewGenerator(reportFmt, "")
	outputPath, err := generator.Generate(reportData)
	if err != nil {
		return fmt.Errorf("generating report: %w", err)
	}

	// Show success
	fmt.Printf("\n✅ Report generated: %s\n", outputPath)
	if reportFmt == report.FormatHTML {
		fmt.Printf("🌐 Open in browser: file://%s\n", outputPath)
	}
	fmt.Printf("📊 Summary: CIS Score %d/100 | %d Critical | %d Warnings | %d Total Issues\n",
		cisResult.Score, len(reportData.CriticalIssues), len(reportData.WarningIssues), len(audit.Issues))

	return nil
}
