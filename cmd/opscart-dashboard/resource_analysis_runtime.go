package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
)

// This file is docs/08 Phase 4D.1: Resource Analyzer migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline.
// buildResourceAnalysis is called from analysis.go's buildClusterScan — the
// one analysis path per cluster (docs/08 Phase 5) — not from a separate
// Coordinator-facing entry point: Phase 5 removed the runResourceAnalysis/
// publishResourceAnalysis split (and the per-analyzer
// resourceAnalysisGeneration field it guarded) once there was no longer a
// second, independently-timed analysis path to reconcile against. See
// acquisition_runtime.go's runAnalysisPass and publishScan for the single
// ordering guard that replaced it.

// buildResourceAnalysis runs analyzer.AnalyzeResources — the same resource
// analysis algorithm AnalyzeClusterResources itself now delegates to
// (pkg/analyzer/resources.go) — sourced from one ClusterSnapshot
// generation's Pods/Nodes instead of direct Kubernetes calls.
func buildResourceAnalysis(resources clusterstate.ClusterResources, namespace string) *models.ClusterResourceAnalysis {
	pods := snapshotResourceCopy(resources.Pods)
	nodes := snapshotResourceCopy(resources.Nodes)
	return analyzer.AnalyzeResources(pods, nodes, namespace)
}
