package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
)

// This file is docs/08 Phase 4D.3: Network Policy analysis migrated off
// direct Kubernetes acquisition onto the shared ClusterSnapshot pipeline.
// buildNetworkAnalysis is called from analysis.go's buildClusterScan — the
// one analysis path per cluster (docs/08 Phase 5) — not from a separate
// Coordinator-facing entry point: Phase 5 removed the runNetworkAnalysis/
// publishNetworkAnalysis split (and the per-analyzer netAuditGeneration
// field it guarded) once there was no longer a second, independently-timed
// analysis path to reconcile against. See acquisition_runtime.go's
// runAnalysisPass and publishScan for the single ordering guard that
// replaced it.
//
// netAudit also feeds CIS scoring and the incident batch — both derived,
// in buildClusterScan, from this same call's result, never from a value
// read back from state.scan (see clusterScan.cisResult's doc comment in
// scan.go).
//
// filterNamespace is always "" here, matching the pre-Phase-4E legacy call
// site's historical behavior exactly: server.go used to pass "" to both
// AuditNetworkPolicies and AuditNetworkPoliciesWithPods regardless of the
// --namespace flag — that flag only ever decided which Pod-acquisition
// strategy the legacy pass used internally, never namespace-scoped the
// Network audit's own result. skipNamespaces is nil, matching that no
// dashboard call site configures NetworkPolicyAuditor.WithSkipNamespaces —
// shouldSkipNamespace's built-in infra-pattern/label strategies are the
// only skip behavior in play.
func buildNetworkAnalysis(resources clusterstate.ClusterResources) *analyzer.NetworkPolicyAudit {
	namespaces := snapshotResourceCopy(resources.Namespaces)
	pods := snapshotResourceCopy(resources.Pods)
	policies := snapshotResourceCopy(resources.NetworkPolicies)
	return analyzer.AnalyzeNetworkPolicies(namespaces, pods, policies, "", nil)
}
