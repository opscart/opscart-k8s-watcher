package main

import (
	"context"
	"log"

	"github.com/opscart/opscart-k8s-watcher/pkg/billing"
)

// startBillingRuntimes constructs and starts one Azure billing Runtime
// (pkg/billing) per configured cluster that has valid, enabled billing
// configuration. It is a no-op when srv.billingConfig is nil (Azure billing
// is disabled by default — see billingConfigFromEnv, main.go).
//
// This runtime is deliberately independent of startAcquisitionRuntimes and
// startAnalysisCoordinators (acquisition_runtime.go): it never reads
// Kubernetes, and nothing in the Kubernetes scan/analysis path ever calls
// it — see runtime.go's doc comment for why the two must stay separate.
func (srv *server) startBillingRuntimes(ctx context.Context) {
	if srv.billingConfig == nil {
		return
	}
	for _, clusterCtx := range srv.clusterList {
		srv.startBilling(ctx, clusterCtx)
	}
}

// startBilling constructs and starts one cluster's billing Runtime. It is
// best-effort and silent-by-default for the common case (no configuration
// for this cluster context): only a cluster explicitly enabled in the
// billing config, but whose credential cannot be acquired, is logged — an
// operator who enabled billing needs to see why it never started.
//
// The configured authMode's credential is used exactly as configured, with
// no fallback to any other credential type: if azure-cli or
// workload-identity fails here, billing for this cluster stays disabled
// until that's fixed, rather than silently trying some other ambient
// identity.
func (srv *server) startBilling(ctx context.Context, clusterCtx string) {
	cfg, ok := srv.billingConfig.ClusterByContext(clusterCtx)
	if !ok {
		return
	}
	credential, err := billing.NewCredential(cfg.EffectiveAuthMode())
	if err != nil {
		log.Printf("[%s] azure billing: credential unavailable, disabled: %v", displayName(clusterCtx), err)
		return
	}
	provider, err := billing.NewAzureProvider(cfg, credential)
	if err != nil {
		log.Printf("[%s] azure billing: invalid configuration, disabled: %v", displayName(clusterCtx), err)
		return
	}
	rt := billing.NewRuntime(cfg, provider)
	rt.Start(ctx)
	state := srv.getState(clusterCtx)
	state.billingRuntime = rt
	log.Printf("[%s] azure billing runtime started (refresh every %s)", displayName(clusterCtx), cfg.EffectiveRefreshInterval())
}
