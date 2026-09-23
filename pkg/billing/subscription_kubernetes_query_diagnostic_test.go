package billing

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestManualSubscriptionKubernetesQueryDiagnostic is a manually invoked,
// opt-in diagnostic — it is NEVER run by `go test ./...` (including CI)
// and is skipped unless explicitly enabled. It exists to empirically check
// whether the request shape this package's synthetic spike tests validate
// (subscription_kubernetes_query_spike_test.go) is actually accepted by
// the real Azure Cost Management API, using a real subscription an
// operator has access to.
//
// Run it explicitly:
//
//	OPSCART_BILLING_SPIKE_MANUAL=1 \
//	OPSCART_BILLING_SPIKE_SUBSCRIPTION_ID=<subscription-id> \
//	OPSCART_BILLING_SPIKE_CLUSTER_RESOURCE_ID=<aks-cluster-arm-resource-id> \
//	go test ./pkg/billing/ -run TestManualSubscriptionKubernetesQueryDiagnostic -v
//
// Safety properties (all deliberate, none configurable):
//   - Credential: exactly AuthModeAzureCLI via the existing, already-
//     reviewed NewCredential — no other credential mode, no fallback
//     chain, and no new credential construction path.
//   - Permissions: requests exactly armTokenScope, the same ARM scope
//     production billing already uses — nothing broader.
//   - Requests: at most one HTTP call (runSubscriptionKubernetesQuery
//     never retries or follows pagination — see
//     TestSubscriptionKubernetesQuerySpikeMakesExactlyOneRequestOnFailure).
//   - Timeout: bounded to 15s via ctx, tighter than newARMHTTPClient's own
//     30s client-level timeout.
//   - Redirects: disabled (newARMHTTPClient, shared with production).
//   - Output: prints only the total, currency, queried period, and a
//     fixed, safe status classification — see AzureAPIError/SafeError,
//     which every failure from runSubscriptionKubernetesQuery already
//     routes through. Never a token, subscription ID, cluster resource
//     ID, raw response body, or request URL.
//   - No automatic refresh: this is one manual invocation, not wired into
//     Runtime.Start or any ticker/schedule.
//
// It never touches state used by any other test and makes no change to
// the production billing path.
func TestManualSubscriptionKubernetesQueryDiagnostic(t *testing.T) {
	if os.Getenv("OPSCART_BILLING_SPIKE_MANUAL") != "1" {
		t.Skip("manual diagnostic — set OPSCART_BILLING_SPIKE_MANUAL=1 (see this test's doc comment) to run it against a real subscription")
	}
	subscriptionID := os.Getenv("OPSCART_BILLING_SPIKE_SUBSCRIPTION_ID")
	clusterResourceID := os.Getenv("OPSCART_BILLING_SPIKE_CLUSTER_RESOURCE_ID")
	if subscriptionID == "" || clusterResourceID == "" {
		t.Fatal("OPSCART_BILLING_SPIKE_SUBSCRIPTION_ID and OPSCART_BILLING_SPIKE_CLUSTER_RESOURCE_ID are both required")
	}

	credential, err := NewCredential(AuthModeAzureCLI)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}

	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC) // month-to-date, matching ClusterConfig's default period mode
	end := now
	periodLabel := start.Format("2006-01-02") + " to " + end.Format("2006-01-02")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	total, currency, queryErr := runSubscriptionKubernetesQuery(ctx, newARMHTTPClient(), credential, defaultManagementEndpoint, subscriptionID, clusterResourceID, start, end)

	// queryErr, when non-nil, is always an *AzureAPIError or *SafeError —
	// both types are already safe to print verbatim (see their doc
	// comments in azure_error.go): fixed operation label, fixed status
	// classification, Azure's own request ID. Never the raw response,
	// subscription ID, cluster resource ID, or request URL.
	if queryErr != nil {
		fmt.Printf("status: error: %s\nperiod: %s\n", queryErr.Error(), periodLabel)
		t.Fatalf("diagnostic query failed: %v", queryErr)
	}
	fmt.Printf("status: ok\ntotal: %.2f\ncurrency: %s\nperiod: %s\n", total, currency, periodLabel)
}
