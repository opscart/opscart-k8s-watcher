package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

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
// result.
func (r *nodeResourceGroupResolver) Resolve(ctx context.Context, cfg ClusterConfig) (string, error) {
	if cfg.NodeResourceGroup != "" {
		return cfg.NodeResourceGroup, nil
	}

	r.mu.Lock()
	if cached, ok := r.resolved[cfg.AKSResourceID]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	nrg, err := r.fetchNodeResourceGroup(ctx, cfg.AKSResourceID)
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

func (r *nodeResourceGroupResolver) fetchNodeResourceGroup(ctx context.Context, aksResourceID string) (string, error) {
	requestURL := r.endpoint + aksResourceID + "?api-version=" + aksAPIVersion

	client := &queryClient{httpClient: r.httpClient, credential: r.credential, endpoint: r.endpoint, apiVersion: aksAPIVersion}
	body, statusCode, err := client.doWithRetry(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", fmt.Errorf("resolving node resource group for %q: %w", aksResourceID, err)
	}
	defer body.Close()
	if statusCode == http.StatusNoContent {
		return "", fmt.Errorf("resolving node resource group for %q: empty response", aksResourceID)
	}

	limited := io.LimitReader(body, 1<<20)
	var decoded managedClusterResponse
	if err := json.NewDecoder(limited).Decode(&decoded); err != nil {
		return "", fmt.Errorf("decoding managed cluster response for %q: %w", aksResourceID, err)
	}
	if decoded.Properties.NodeResourceGroup == "" {
		return "", fmt.Errorf("managed cluster %q response did not include properties.nodeResourceGroup", aksResourceID)
	}
	return decoded.Properties.NodeResourceGroup, nil
}
