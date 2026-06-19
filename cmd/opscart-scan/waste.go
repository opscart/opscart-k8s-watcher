package main

import (
	"fmt"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/report"
)

func runWasteScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	fmt.Printf("   Minimum age: %d days\n", minAgeDays)
	if wasteFormat == "html" {
		fmt.Printf("   Format: HTML report\n")
	}

	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	wa, cancel := analyzer.NewWasteAuditor(clientset, minAgeDays)
	defer cancel() // Ensure context cleanup even if we exit early

	audit, err := wa.AuditWaste(namespace)
	if err != nil {
		return fmt.Errorf("auditing waste: %w", err)
	}

	// Output based on format
	switch wasteFormat {
	case "html":
		if err := report.GenerateWasteHTML(audit, clusterContext, minAgeDays); err != nil {
			return fmt.Errorf("generating HTML report: %w", err)
		}
	default:
		analyzer.PrintWasteAudit(audit, minAgeDays)
	}

	return nil
}
