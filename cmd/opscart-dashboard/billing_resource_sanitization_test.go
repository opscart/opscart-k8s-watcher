package main

import (
	"strings"
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/billing"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// Tests in this file are the focused regression coverage for resource-ID
// redaction: a full Azure resource ID (which always embeds the
// subscription ID and resource group — sensitive scope information) must
// never reach a rendered billing page, only the sanitized resource type
// and name. See sanitizeResourceID and billingResourceRow/billingPageData
// in billing_page.go. testSubscriptionID/testARMResourceID/
// testARMClusterResourceID are shared fixtures defined in
// billing_page_test.go.

func TestSanitizeResourceIDExtractsTypeAndNameNeverSubscriptionOrResourceGroup(t *testing.T) {
	tests := []struct {
		name         string
		id           string
		wantType     string
		wantResource string
	}{
		{
			name:         "AKS managed cluster",
			id:           testARMClusterResourceID("my-rg", "my-aks"),
			wantType:     "Microsoft.ContainerService/managedClusters",
			wantResource: "my-aks",
		},
		{
			name:         "VM scale set",
			id:           "/subscriptions/" + testSubscriptionID + "/resourceGroups/MC_my-rg_my-aks_eastus2/providers/Microsoft.Compute/virtualMachineScaleSets/aks-nodepool1-vmss",
			wantType:     "Microsoft.Compute/virtualMachineScaleSets",
			wantResource: "aks-nodepool1-vmss",
		},
		{
			name:         "disk",
			id:           testARMResourceID("MC_my-rg_my-aks_eastus2", "my-disk"),
			wantType:     "Microsoft.Compute/disks",
			wantResource: "my-disk",
		},
		{name: "empty string", id: "", wantType: "unknown", wantResource: "unknown"},
		{name: "not an ARM ID", id: "not-an-azure-resource-id", wantType: "unknown", wantResource: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotName := sanitizeResourceID(tt.id)
			if gotType != tt.wantType || gotName != tt.wantResource {
				t.Errorf("sanitizeResourceID(%q) = (%q, %q), want (%q, %q)", tt.id, gotType, gotName, tt.wantType, tt.wantResource)
			}
			if strings.Contains(gotType, testSubscriptionID) || strings.Contains(gotName, testSubscriptionID) {
				t.Error("sanitized output must never contain the subscription ID")
			}
			if strings.Contains(gotType, "/subscriptions/") || strings.Contains(gotName, "/subscriptions/") {
				t.Error(`sanitized output must never contain "/subscriptions/"`)
			}
		})
	}
}

// TestRenderCostPageNeverRendersFullResourceIDOrSubscriptionUUID is the
// focused regression test for resource-ID redaction: it builds a Snapshot
// whose ClusterResourceID and resource-row IDs are built from a
// recognizable, obviously-synthetic subscription UUID (testSubscriptionID)
// and asserts that UUID, and the literal "/subscriptions/" prefix, never
// occur anywhere in the rendered page — scanning the complete HTML
// document (head, body, inline <script>, and every attribute), not just
// visible text, so this single check covers visible text, title
// attributes, data attributes, and JavaScript in one pass. It also proves
// the redaction didn't just delete the rows: the sanitized resource type
// and name must still be present.
func TestRenderCostPageNeverRendersFullResourceIDOrSubscriptionUUID(t *testing.T) {
	scan := &clusterScan{report: &models.CloudCostReport{Timestamp: time.Now(), ClusterName: "aks", Currency: "USD"}}
	clusterResourceID := testARMClusterResourceID("my-rg", "my-aks-cluster")
	nodeResourceID := testARMResourceID("MC_my-rg_my-aks-cluster_eastus2", "my-node-disk")
	snap := billing.Snapshot{
		Status: billing.StatusAvailable, Total: 150, Currency: "USD",
		ClusterResourceID: clusterResourceID,
		AttributedTotal:   50,
		UnattributedTotal: 100,
		Lines: []billing.ResourceCost{
			{ResourceID: clusterResourceID, ResourceGroup: "my-rg", Cost: 50, Currency: "USD", Attributed: true},
			{ResourceID: nodeResourceID, ResourceGroup: "MC_my-rg_my-aks-cluster_eastus2", Cost: 100, Currency: "USD"},
		},
	}
	html := renderCostPage(scan, "", []string{""}, snap, true, 0, 0)

	if strings.Contains(html, testSubscriptionID) {
		t.Error("rendered HTML contains the subscription UUID")
	}
	if strings.Contains(html, "/subscriptions/") {
		t.Error(`rendered HTML contains the literal "/subscriptions/" prefix`)
	}
	if strings.Contains(html, clusterResourceID) {
		t.Error("rendered HTML contains the full AKS cluster resource ID")
	}
	if strings.Contains(html, nodeResourceID) {
		t.Error("rendered HTML contains the full node resource ID")
	}

	// Positive control: redaction must not have just deleted the rows —
	// the sanitized resource type and name are still shown.
	if !strings.Contains(html, "Microsoft.ContainerService/managedClusters") {
		t.Error("sanitized AKS resource type not rendered")
	}
	if !strings.Contains(html, "my-aks-cluster") {
		t.Error("sanitized AKS resource name not rendered")
	}
	if !strings.Contains(html, "Microsoft.Compute/disks") {
		t.Error("sanitized node resource type not rendered")
	}
	if !strings.Contains(html, "my-node-disk") {
		t.Error("sanitized node resource name not rendered")
	}
}
