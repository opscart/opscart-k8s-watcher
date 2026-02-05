package report

import (
	"time"
)

// BuildOptions holds optional scan results for report building
type BuildOptions struct {
	// Add your actual types here as you integrate
	// For now, using interface{} as placeholder
	SecurityAudit    interface{}
	ResourceAnalysis interface{}
	EmergencyIssues  interface{}
	MonthlyCost      float64
}

// BuildFromScans creates ReportData from multiple scan results
// This is a simplified version - you'll wire in your actual analyzer types
func BuildFromScans(clusterName string, opts *BuildOptions) *ReportData {
	data := &ReportData{
		ClusterName: clusterName,
		GeneratedAt: time.Now(),
	}

	// Placeholder scores - replace with actual calculations
	data.SecurityScore = 67
	data.ResourceScore = 85
	data.CostScore = 45

	// Add sample data (replace with actual scan results)
	data.CISScore = 67
	data.ControlsPassed = 4
	data.ControlsFailed = 3

	// Sample critical issues
	data.CriticalIssues = []IssueItem{
		{
			Severity:    "critical",
			Title:       "🔴 3 pods in CrashLoopBackOff",
			Description: "Pods are restarting continuously",
			Count:       3,
			Details:     []string{"backend-api", "worker-queue", "metrics-exporter"},
		},
	}

	data.WarningIssues = []IssueItem{
		{
			Severity:    "warning",
			Title:       "🟡 31 containers running as root",
			Description: "Running as root increases attack surface",
			Count:       31,
		},
	}

	// Sample resource data
	data.TotalCPU = 24.0
	data.TotalMemory = 29.1
	data.UsedCPU = 10.2
	data.UsedMemory = 13.0
	data.PodCount = 89
	data.NamespaceCount = 12

	// Sample cost data
	if opts.MonthlyCost > 0 {
		data.MonthlyCost = opts.MonthlyCost
		data.PotentialSavings = SavingsRange{
			Min: opts.MonthlyCost * 0.24,
			Max: opts.MonthlyCost * 0.36,
		}

		data.CostBreakdown = []CostItem{
			{
				Name:    "Delete idle namespaces",
				Impact:  "High",
				Savings: 850,
				Action:  "5 namespaces identified",
			},
			{
				Name:    "Move to spot instances",
				Impact:  "Medium",
				Savings: 350,
				Action:  "38 pods eligible",
			},
			{
				Name:    "Right-size pods",
				Impact:  "Low",
				Savings: 200,
				Action:  "12 over-provisioned pods",
			},
		}
	}

	// Sample namespace data
	data.Namespaces = []NamespaceItem{
		{
			Name:       "cost-test",
			CPUPercent: 28.3,
			MemPercent: 30.9,
			PodCount:   18,
			Cost:       1750,
			Flags:      []string{"SPOT-OK"},
		},
		{
			Name:       "prod-api",
			CPUPercent: 5.0,
			MemPercent: 4.3,
			PodCount:   3,
			Cost:       750,
			Flags:      []string{"SPOT-OK"},
		},
		{
			Name:       "staging",
			CPUPercent: 1.3,
			MemPercent: 1.3,
			PodCount:   3,
			Cost:       300,
			Flags:      []string{"IDLE-21d"},
		},
	}

	// Calculate overall score
	data.OverallScore = CalculateOverallScore(data.SecurityScore, data.ResourceScore, data.CostScore)

	return data
}

// BuildFromRealScans - TEMPLATE for when you wire in actual types
// Uncomment and modify this once you integrate with your analyzer package
/*
func BuildFromRealScans(clusterName string, opts *BuildOptions) *ReportData {
	data := &ReportData{
		ClusterName: clusterName,
		GeneratedAt: time.Now(),
	}

	// Security data
	if secAudit, ok := opts.SecurityAudit.(*analyzer.SecurityAudit); ok {
		data.CISScore = secAudit.CISScore
		data.SecurityScore = secAudit.CISScore
		data.ControlsPassed = secAudit.ControlsPassed
		data.ControlsFailed = secAudit.ControlsFailed

		if secAudit.PrivilegedContainers > 0 {
			data.CriticalIssues = append(data.CriticalIssues, IssueItem{
				Severity:    "critical",
				Title:       fmt.Sprintf("🔴 %d privileged containers", secAudit.PrivilegedContainers),
				Description: "Containers with elevated privileges detected",
				Count:       secAudit.PrivilegedContainers,
			})
		}

		if secAudit.RootContainers > 0 {
			data.WarningIssues = append(data.WarningIssues, IssueItem{
				Severity:    "warning",
				Title:       fmt.Sprintf("🟡 %d containers running as root", secAudit.RootContainers),
				Description: "Running as root increases attack surface",
				Count:       secAudit.RootContainers,
			})
		}
	}

	// Resource data
	if resAnalysis, ok := opts.ResourceAnalysis.(*analyzer.ResourceAnalysis); ok {
		data.TotalCPU = resAnalysis.TotalCPU
		data.TotalMemory = resAnalysis.TotalMemory
		data.UsedCPU = resAnalysis.UsedCPU
		data.UsedMemory = resAnalysis.UsedMemory
		data.PodCount = resAnalysis.TotalPods
		data.NamespaceCount = len(resAnalysis.Namespaces)

		usedPercent := (data.UsedCPU / data.TotalCPU) * 100
		data.ResourceScore = CalculateResourceScore(usedPercent)

		// Namespace breakdown
		for _, ns := range resAnalysis.Namespaces {
			item := NamespaceItem{
				Name:       ns.Name,
				CPUPercent: (ns.CPURequested / data.TotalCPU) * 100,
				MemPercent: (ns.MemoryRequested / data.TotalMemory) * 100,
				PodCount:   ns.PodCount,
				Flags:      []string{},
			}

			if ns.IdleDays > 21 {
				item.Flags = append(item.Flags, fmt.Sprintf("IDLE-%dd", ns.IdleDays))
			}
			if ns.SpotEligible {
				item.Flags = append(item.Flags, "SPOT-OK")
			}

			if opts.MonthlyCost > 0 {
				cpuCost := opts.MonthlyCost / data.TotalCPU
				item.Cost = ns.CPURequested * cpuCost
			}

			data.Namespaces = append(data.Namespaces, item)
		}
	}

	// Emergency issues
	if emergency, ok := opts.EmergencyIssues.(*scanner.EmergencyIssues); ok {
		if emergency.CriticalCount > 0 {
			details := []string{}
			for _, pod := range emergency.CriticalPods {
				details = append(details, pod.Name)
			}

			data.CriticalIssues = append(data.CriticalIssues, IssueItem{
				Severity:    "critical",
				Title:       fmt.Sprintf("🔴 %d pods in critical state", emergency.CriticalCount),
				Description: "CrashLoopBackOff, ImagePullBackOff, or pending",
				Count:       emergency.CriticalCount,
				Details:     details,
			})
		}
	}

	// Calculate overall score
	data.OverallScore = CalculateOverallScore(data.SecurityScore, data.ResourceScore, data.CostScore)

	return data
}
*/
