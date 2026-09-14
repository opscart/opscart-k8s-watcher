package main

import (
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
)

// This file is docs/08 Phase 4D.2: Node Health migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline.
// buildNodeHealth is called from analysis.go's buildClusterScan — the one
// analysis path per cluster (docs/08 Phase 5) — not from a separate
// Coordinator-facing entry point: Phase 5 removed the runNodeHealth/
// publishNodeHealth split (and the per-analyzer nodeHealthGeneration field
// it guarded) once there was no longer a second, independently-timed
// analysis path to reconcile against. See acquisition_runtime.go's
// runAnalysisPass and publishScan for the single ordering guard that
// replaced it.
//
// nodeHealth is also the direct input to the incident batch
// (persistAnalysis, acquisition_runtime.go) — buildNodeHealth's result is
// used for that unmodified, exactly as it is for display.

// buildNodeHealth runs the same detection+correlation algorithm
// FindNodeHealthConditions uses (scanner.AnalyzeNodeHealth), sourced from
// one ClusterSnapshot generation's Nodes/Pods/Jobs instead of direct
// Kubernetes calls. Jobs supply Job -> CronJob owner resolution purely from
// each Job's own OwnerReferences — no separate CronJob read is needed.
func buildNodeHealth(resources clusterstate.ClusterResources) []models.NodeConditionFinding {
	nodes := snapshotResourceCopy(resources.Nodes)
	pods := snapshotResourceCopy(resources.Pods)
	jobs := snapshotResourceCopy(resources.Jobs)
	return scanner.AnalyzeNodeHealth(nodes, pods, jobs)
}
