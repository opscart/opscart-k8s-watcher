package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// nodeResourceGroupOperation labels every error fetchNodeResourceGroup
// produces.
const nodeResourceGroupOperation = "resolving node resource group"

// aksAPIVersion pins the Microsoft.ContainerService managedClusters read
// contract this package depends on: only properties.nodeResourceGroup.
// Not user-configurable — this is an internal ARM read, not a billing
// scope choice.
const aksAPIVersion = "2024-05-01"

// nodeResourceGroupResolver resolves and caches an AKS cluster's
// auto-generated node resource group, so a Runtime's repeated refreshes
// (every DefaultRefreshInterval) issue this one-time lookup at most once
// per process lifetime per cluster, not on every refresh.
type nodeResourceGroupResolver struct {
	httpClient *http.Client
	credential azcore.TokenCredential
	endpoint   string

	mu       sync.Mutex
	resolved map[string]string // AKS resource ID -> node resource group
}

func newNodeResourceGroupResolver(httpClient *http.Client, credential azcore.TokenCredential, endpoint string) *nodeResourceGroupResolver {
	return &nodeResourceGroupResolver{
		httpClient: httpClient,
		credential: credential,
		endpoint:   strings.TrimRight(endpoint, "/"),
		resolved:   make(map[string]string),
	}
}

// Resolve returns cfg.NodeResourceGroup if explicitly configured (an
// explicit operator override always wins), otherwise resolves it once from
// the AKS resource's own properties.nodeResourceGroup and caches the
// result. budget is shared with every other HTTP attempt this refresh
// makes (see requestBudget).
func (r *nodeResourceGroupResolver) Resolve(ctx context.Context, cfg ClusterConfig, budget *requestBudget) (string, error) {
	if cfg.NodeResourceGroup != "" {
		return cfg.NodeResourceGroup, nil
	}

	r.mu.Lock()
	if cached, ok := r.resolved[cfg.AKSResourceID]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	nrg, err := r.fetchNodeResourceGroup(ctx, cfg.AKSResourceID, budget)
	if err != nil {
		return "", err
	}

	r.mu.Lock()
	r.resolved[cfg.AKSResourceID] = nrg
	r.mu.Unlock()
	return nrg, nil
}

type managedClusterResponse struct {
	Properties struct {
		NodeResourceGroup string `json:"nodeResourceGroup"`
	} `json:"properties"`
}

func (r *nodeResourceGroupResolver) fetchNodeResourceGroup(ctx context.Context, aksResourceID string, budget *requestBudget) (string, error) {
	requestURL := r.endpoint + aksResourceID + "?api-version=" + aksAPIVersion

	client := &queryClient{httpClient: r.httpClient, credential: r.credential, endpoint: r.endpoint, apiVersion: aksAPIVersion}
	body, statusCode, err := client.doWithRetry(ctx, http.MethodGet, requestURL, nil, nodeResourceGroupOperation, budget)
	if err != nil {
		return "", err
	}
	defer body.Close()
	if statusCode == http.StatusNoContent {
		return "", newSafeError(nodeResourceGroupOperation, safeReasonInvalidResponse, fmt.Errorf("empty response"))
	}

	raw, err := readBounded(body, nodeResourceGroupOperation)
	if err != nil {
		return "", err
	}
	var decoded managedClusterResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", newSafeError(nodeResourceGroupOperation, safeReasonInvalidResponse, err)
	}
	if decoded.Properties.NodeResourceGroup == "" {
		return "", newSafeError(nodeResourceGroupOperation, safeReasonInvalidResponse, fmt.Errorf("missing properties.nodeResourceGroup"))
	}
	return decoded.Properties.NodeResourceGroup, nil
}
