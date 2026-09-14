package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// This file is docs/08 Phase 4D.4: Security analysis migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline.
// buildSecurityAnalysis is called from analysis.go's buildClusterScan — the
// one analysis path per cluster (docs/08 Phase 5) — not from a separate
// Coordinator-facing entry point: Phase 5 removed the runSecurityAnalysis/
// publishSecurityAnalysis split (and the per-analyzer secAuditGeneration
// field it guarded) once there was no longer a second, independently-timed
// analysis path to reconcile against. See acquisition_runtime.go's
// runAnalysisPass and publishScan for the single ordering guard that
// replaced it.
//
// secAudit also feeds CIS scoring and the incident batch — both derived,
// in buildClusterScan, from this same call's result, never from a value
// read back from state.scan (see clusterScan.cisResult's doc comment in
// scan.go).
func buildSecurityAnalysis(resources clusterstate.ClusterResources) *models.SecurityAudit {
	pods := snapshotResourceCopy(resources.Pods)
	return analyzer.AnalyzeSecurity(pods)
}
