package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNodeResourceGroupResolverPrefersExplicitConfig(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	resolver := newNodeResourceGroupResolver(server.Client(), &fakeCredential{token: "t"}, server.URL)
	cfg := validClusterConfig()
	cfg.NodeResourceGroup = "MC_operator-supplied_rg"

	nrg, err := resolver.Resolve(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if nrg != "MC_operator-supplied_rg" {
		t.Errorf("nrg = %q, want operator override", nrg)
	}
	if calls != 0 {
		t.Errorf("ARM was called %d times despite an explicit override", calls)
	}
}

func TestNodeResourceGroupResolverFetchesAndCaches(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != validAKSResourceID {
			t.Errorf("request path = %q, want %q", r.URL.Path, validAKSResourceID)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"properties": map[string]any{
				"nodeResourceGroup": "MC_rxr-rxp-e2e-01-cus-rg_rxr-rxp-e2e-01-cus-aks_centralus",
			},
		})
	}))
	defer server.Close()

	resolver := newNodeResourceGroupResolver(server.Client(), &fakeCredential{token: "t"}, server.URL)
	cfg := validClusterConfig()

	for i := 0; i < 3; i++ {
		nrg, err := resolver.Resolve(context.Background(), cfg, nil)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if nrg != "MC_rxr-rxp-e2e-01-cus-rg_rxr-rxp-e2e-01-cus-aks_centralus" {
			t.Errorf("nrg = %q", nrg)
		}
	}
	if calls != 1 {
		t.Errorf("ARM was called %d times, want exactly 1 (cached after first resolve)", calls)
	}
}

func TestNodeResourceGroupResolverRejectsMissingProperty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"properties":{}}`))
	}))
	defer server.Close()

	resolver := newNodeResourceGroupResolver(server.Client(), &fakeCredential{token: "t"}, server.URL)
	if _, err := resolver.Resolve(context.Background(), validClusterConfig(), nil); err == nil {
		t.Fatal("expected an error when nodeResourceGroup is missing from the response")
	}
}

func TestNodeResourceGroupResolverPropagatesAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	resolver := newNodeResourceGroupResolver(server.Client(), &fakeCredential{token: "t"}, server.URL)
	if _, err := resolver.Resolve(context.Background(), validClusterConfig(), nil); err == nil {
		t.Fatal("expected an error for a 403 response, got nil")
	}
}
