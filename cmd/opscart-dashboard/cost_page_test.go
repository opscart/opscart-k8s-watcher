package main

import (
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

func TestAWSCostPageIsProviderHonest(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{
		Timestamp: time.Now(), ClusterName: "eks-prod", Provider: "aws", Region: "us-east-1",
		PricingSource:   "AWS Price List Query API public EC2 On-Demand pricing",
		PricingCoverage: "0 of 1 nodes priced", Currency: "USD",
		PricingWarnings: []string{"spot: EC2 Spot pricing is not integrated"},
		NodePoolCosts:   []models.NodePoolCost{{Name: "spot", Provider: "aws", Region: "us-east-1", VMSize: "m7i.large", NodeCount: 1, Priority: "SPOT"}},
		NamespaceCosts:  []models.NamespaceCostInfo{{Name: "default", PodCount: 1}},
	}}
	html := renderCostPage(scan, "named/context", []string{"named/context"})
	for _, forbidden := range []string{"Azure retail", "Azure RI savings</th>", "grid-template-columns:1fr 340px", `class="wr-mini-card"`, "Critical Issues", "Security Score", "Waste Resources"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("AWS cost page contains %q", forbidden)
		}
	}
	if !strings.Contains(html, "Not calculated") || !strings.Contains(html, "Spot") {
		t.Fatalf("AWS unavailable Spot state missing from page")
	}
	if !strings.Contains(html, "cluster=named%2Fcontext") {
		t.Fatalf("named cluster parameter was not preserved")
	}
}

func TestCostPageNeedsOnlyCostReportData(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{
		Timestamp: time.Now(), ClusterName: "aks", Provider: "azure", Region: "eastus2",
		PricingSource: "Embedded Azure public retail pricing catalog", PricingCoverage: "1 of 1 nodes priced", Currency: "USD",
		NodePoolCosts:    []models.NodePoolCost{{Name: "system", Provider: "azure", Region: "eastus2", NodeCount: 1, PricingAvailable: true, PricePerNodeMonth: 100, TotalMonthly: 100}},
		TotalMonthlyCost: 100,
	}}
	html := renderCostPage(scan, "", []string{""})
	if html == "" || !strings.Contains(html, "Cost Situation Briefing") {
		t.Fatal("cost-only report did not render")
	}
	if strings.Contains(html, `<div class="section-title">War Room</div>`) {
		t.Fatal("War Room panel remains")
	}
}

func TestManualAzureOverrideIsProminent(t *testing.T) {
	report := &models.CloudCostReport{
		Timestamp: time.Now(), ClusterName: "minikube", Provider: "azure",
		DetectedProvider: "unknown", EffectiveProvider: "azure",
		ProviderDetectionMode: "manual",
		ProviderWarning:       "Azure pricing is enabled by manual provider override; the cluster provider was not detected as Azure.",
		Region:                "eastus2", Currency: "USD",
	}
	html := renderCostPage(&clusterScan{report: report}, "", []string{""})
	if !strings.Contains(html, report.ProviderWarning) || !strings.Contains(html, "Manual provider override") {
		t.Fatalf("manual override warning is not prominent")
	}
}
