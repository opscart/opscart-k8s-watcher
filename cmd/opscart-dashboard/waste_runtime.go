package main

import (
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
)

// This file is docs/08 Phase 4D.5: Waste analysis migrated off direct
// Kubernetes acquisition onto the shared ClusterSnapshot pipeline.
// buildWasteAnalysis is called from analysis.go's buildClusterScan — the
// one analysis path per cluster (docs/08 Phase 5) — not from a separate
// Coordinator-facing entry point: Phase 5 removed the runWasteAnalysis/
// publishWasteAnalysis split (and the per-analyzer wasteAuditGeneration
// field it guarded) once there was no longer a second, independently-timed
// analysis path to reconcile against. See acquisition_runtime.go's
// runAnalysisPass and publishScan for the single ordering guard that
// replaced it.
//
// Time semantics: most Waste rules are clock-driven age gates, meaning an
// age threshold can be crossed by elapsed wall-clock time alone, with no
// new Kubernetes event to trigger a fresh ClusterState generation. now
// below is always the actual call-time clock, so this stays correct as
// long as SOMETHING re-invokes buildClusterScan periodically even on a
// static cluster — docs/08 Phase 5's Coordinator clock trigger
// (pkg/clusterstate/coordinator.go's clockInterval) is exactly that;
// nothing here performs or requires a periodic Kubernetes read.
func buildWasteAnalysis(resources clusterstate.ClusterResources) *analyzer.WasteAudit {
	input := analyzer.WasteSnapshot{
		Namespaces:               snapshotResourceCopy(resources.Namespaces),
		Pods:                     snapshotResourceCopy(resources.Pods),
		PersistentVolumeClaims:   snapshotResourceCopy(resources.PersistentVolumeClaims),
		Jobs:                     snapshotResourceCopy(resources.Jobs),
		CronJobs:                 snapshotResourceCopy(resources.CronJobs),
		Deployments:              snapshotResourceCopy(resources.Deployments),
		StatefulSets:             snapshotResourceCopy(resources.StatefulSets),
		ReplicaSets:              snapshotResourceCopy(resources.ReplicaSets),
		Services:                 snapshotResourceCopy(resources.Services),
		Ingresses:                snapshotResourceCopy(resources.Ingresses),
		HorizontalPodAutoscalers: snapshotResourceCopy(resources.HorizontalPodAutoscalers),
		EndpointSlices:           snapshotResourceCopy(resources.EndpointSlices),
		PodWarningEvents:         snapshotResourceCopy(resources.PodWarningEvents),
	}
	return analyzer.AnalyzeWaste(input, dashboardWasteMinAgeDays, time.Now())
}
