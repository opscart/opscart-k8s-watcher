package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/acquisition"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
)

// clusterScan holds results from all analyzers for one atomic analysis
// pass (docs/08 Phase 5: buildClusterScan, analysis.go). Fields other than
// report may be nil if the corresponding analyzer found nothing to report.
//
// Every field here is built from the SAME ClusterSnapshot generation in
// one buildClusterScan call — there is exactly one analysis path per
// cluster (Phase 5 removed the earlier coexistence of a separate
// Coordinator-driven path and a legacy path, each independently
// publishing/generation-guarding its own subset of these fields). generation
// is the single ordering key publishScan (acquisition_runtime.go) uses to
// guarantee a stale analysis pass can never overwrite a newer one.
type clusterScan struct {
	// generation is the ClusterSnapshot generation this entire clusterScan
	// was derived from (docs/08 §8: not a claim of one atomic Kubernetes
	// transaction, and never a proxy for elapsed time — see docs/08 §2.6).
	// publishScan's ordering guard compares this field; Coordinator's
	// single-threaded loop is what makes "second call for the same
	// generation is chronologically later" a safe basis for that guard
	// even when a clock-triggered pass reuses an unchanged generation
	// (docs/08 Phase 5's clock-driven re-analysis).
	generation uint64

	// report is the full Cost Intelligence composite for this pass (pool
	// costs, namespace allocation, optimization scenarios, provider
	// metadata) — see buildCostAnalysis, cost_runtime.go.
	report *models.CloudCostReport

	// secAudit is displayed by pages.go/server.go's overview and is also an
	// input to this pass's incident batch (collectWarRoomIssues' critical
	// "privileged_container" issues) and to cisResult below.
	secAudit *models.SecurityAudit

	// cisResult is derived from secAudit and netAudit computed in this SAME
	// buildClusterScan call (analysis.go) — never from independently-timed
	// values, so it is structurally impossible for this to combine
	// Security and Network evidence from two different generations
	// (docs/08 Phase 5's CIS/Security/Network generation-consistency
	// requirement).
	cisResult *analyzer.CISResult

	// wasteAudit is displayed by pages.go/investigation.go and is also an
	// input to this pass's incident batch (collectWarRoomIssues'
	// StalePods/AbandonedNamespaces issues and calcIncidentScore's
	// OrphanedPVCs penalty).
	wasteAudit *analyzer.WasteAudit

	// netAudit is displayed by pages.go/investigation.go/warroom.go and is
	// also an input to this pass's incident batch (collectWarRoomIssues'
	// "unprotected_namespace" issues).
	netAudit *analyzer.NetworkPolicyAudit

	// nodeHealth is displayed by pages.go/investigation.go/warroom.go and is
	// also the direct input to this pass's incident batch
	// (completeIncidentBatch, persistAnalysis in acquisition_runtime.go).
	nodeHealth []models.NodeConditionFinding

	// AllWorkloads is every Deployment/StatefulSet/DaemonSet this pass
	// observed, regardless of the --breakdown flag — see
	// buildResourceAnalysis, resource_analysis_runtime.go.
	AllWorkloads []models.WorkloadRef

	// PodWorkloads is the confirmed pod -> owning workload map from the
	// same pod enumeration, keyed by "namespace/podName". This is the
	// single source of truth for pod ownership — used instead of
	// name-pattern matching (store.OwnerNameFromPod + prefix checks),
	// which cannot distinguish a real StatefulSet replica from an
	// unrelated pod sharing its naming pattern.
	PodWorkloads map[string]models.WorkloadRef

	// namespaceCount is the authoritative Kubernetes namespace inventory
	// size for this pass — len(resources.Namespaces) from the same
	// ClusterSnapshot every other field here is built from (buildClusterScan,
	// analysis.go), not derived from Cost Intelligence's NamespaceCosts
	// (namespace *cost allocation* entries, which a namespace can legitimately
	// have zero of — e.g. no priced/allocated workloads — while still
	// existing). Overview's NamespaceCount must read this field, never
	// report.NamespaceCosts.
	namespaceCount int

	// namespaces is the authoritative, already-acquired Kubernetes
	// Namespace inventory for this pass (resources.Namespaces from the
	// same ClusterSnapshot every other field here is built from) — the
	// Namespaces page's primary table is built by iterating this slice,
	// never scan.report.NamespaceCosts (a *cost allocation* list a real
	// namespace can legitimately be absent from). len(namespaces) equals
	// namespaceCount above; this field exists because that page also
	// needs each namespace's Name, not just the count.
	namespaces []*corev1.Namespace

	// nodes is the authoritative, already-acquired Kubernetes Node inventory
	// for this pass (resources.Nodes from the same ClusterSnapshot every
	// other field here is built from) — the Infrastructure page's Nodes tab
	// reads real node conditions (Ready/MemoryPressure/DiskPressure/
	// PIDPressure/NetworkUnavailable), Spec.Unschedulable, CreationTimestamp,
	// and Status.NodeInfo.KubeletVersion directly from these objects. Never
	// derive node health or age from anything other than these fields.
	nodes []*corev1.Node

	// nodeInfos is result.NodeInfos from this same pass's Cost Intelligence
	// computation (buildCostAnalysis, cost_runtime.go) — per-node
	// NodePool/VMSize/Zone/CPUCapacity/MemGBCapacity/CPURequested/
	// MemGBRequested, keyed by Name. Reused rather than recomputed so the
	// Infrastructure page's request/allocatable figures can never drift
	// from Cost Intelligence's own per-node evidence.
	nodeInfos []models.NodeInfo

	// nodePodCounts maps Node name -> Running/Pending pod count for this
	// pass, computed once in buildClusterScan (countPodsByNode, analysis.go)
	// from this same snapshot's Pods.
	nodePodCounts map[string]int

	// namespacePodCounts maps Namespace name -> Pod count for this pass,
	// computed once in buildClusterScan from this same snapshot's Pods.
	namespacePodCounts map[string]int

	// aiPodEvidence is the small, sanitized pod/container/Warning Event view
	// derived once from this pass's ClusterSnapshot. It intentionally retains
	// no Kubernetes objects and is used only for manual War Room AI requests.
	aiPodEvidence *warRoomAIPodEvidenceIndex

	// nodeOptimization is the read-only consolidation-simulation
	// recommendation contract (see pkg/analyzer/node_optimization_recommendation.go),
	// built from this same pass's NodeInfo/Pod snapshots and report — not a
	// second cluster fetch. Nil/empty until the Node Optimization page
	// renders it.
	nodeOptimization []analyzer.NodeOptimizationRecommendation

	// nodeOptimizationSavings is aligned 1:1 by index with nodeOptimization.
	// Each entry reuses Cost Intelligence's already-computed provider pricing
	// (report.NodePoolCosts, not a second pricing call) to project the
	// monthly savings of that pool's recommendation, when an exact price
	// can be joined. See pkg/analyzer/node_optimization_savings.go.
	nodeOptimizationSavings []analyzer.NodeOptimizationSavingsProjection
}

// ── Per-cluster state ─────────────────────────────────────────────────────────

type dashboardState struct {
	ctx           string
	mu            sync.RWMutex
	scan          *clusterScan
	htmlPage      string
	scanning      atomic.Bool
	db            store.Store
	retentionDays int
	observation   scanObservation

	// acquisition is this cluster's informer-backed acquisition runtime
	// (docs/08 Phase 3/4A). It is set once during dashboard startup (see
	// acquisition_runtime.go) before any concurrent reader could observe
	// it, and never reassigned afterward, so — like ctx/db/retentionDays
	// above — reading it needs no lock. It is nil if that cluster's
	// Kubernetes client could not be constructed at startup, in which case
	// refresh below can never produce a scan for this cluster.
	//
	// Since docs/08 Phase 4E, refresh below reads this runtime's
	// ClusterState directly (via runAnalysisPass, acquisition_runtime.go)
	// — the same ClusterState the coordinator below also reads.
	acquisition *acquisition.Runtime

	// coordinator is this cluster's Phase 4B coalescing coordinator (Phase 5:
	// also clock-triggered — see NewCoordinator's clockInterval), driving
	// every analyzer through the one analysis path (acquisition_runtime.go's
	// runAnalysisPass) off acquisition's ClusterState instead of a direct
	// Kubernetes call. Set once at startup alongside acquisition, before any
	// concurrent reader could observe it, and never reassigned afterward —
	// same no-lock convention as acquisition above.
	coordinator *clusterstate.Coordinator

	// costAnalyzer is this cluster's persistent Cost/pricing runtime (docs/08
	// Phase 4D.6) — one *analyzer.NodePoolCostAnalyzer shared by every
	// analysis pass (on-demand and Coordinator-triggered alike, since Phase
	// 5 there is only one analysis path), so a pricing provider's own cache
	// (e.g. the Azure Retail Prices provider's 24h TTL) survives across
	// generations instead of being discarded with a freshly-constructed
	// analyzer every cycle. Constructed once in getState, before this
	// dashboardState is published to srv.states, and never reassigned
	// afterward — same no-lock convention as acquisition/coordinator above.
	// The type's own mutex protects it against the concurrent access this
	// sharing introduces (see NodePoolCostAnalyzer's doc comment).
	costAnalyzer *analyzer.NodePoolCostAnalyzer

	// lastPersistedAt is the wall-clock time persistAnalysis
	// (acquisition_runtime.go) last actually wrote incidents/history for
	// this cluster, guarded by mu. It is the throttle that bounds
	// persistence to roughly persistenceInterval regardless of how often
	// runAnalysisPass itself runs (docs/08 Phase 5: an event-coalesced pass
	// can fire every ~2s during a burst, and that must not become ~2s-
	// cadence scan_history/incident writes). Its zero value means "never
	// persisted yet," so the first pass for a cluster always persists.
	lastPersistedAt time.Time
}

func (s *dashboardState) refresh(clusterList []string) error {
	if !s.scanning.CompareAndSwap(false, true) {
		return nil
	}
	defer s.scanning.Store(false)

	// docs/08 Phase 4E/5: this pass's evidence comes from the same
	// ClusterSnapshot the Coordinator reads (runAnalysisPass,
	// acquisition_runtime.go), not a direct Kubernetes call. A snapshot
	// that isn't Trustworthy (RESYNCING/DEGRADED/STALE, or none published
	// yet) must not be analyzed — the acquisition-health invariant every
	// analysis pass enforces (docs/08 §4). main.go's startup sequence waits
	// for initial sync before the first call, so this only actually
	// triggers for a cluster whose acquisition never started or is still
	// recovering.
	if s.acquisition == nil {
		return fmt.Errorf("acquisition unavailable for %s", displayName(s.ctx))
	}
	snapshot := s.acquisition.ClusterState().Latest()
	if snapshot == nil || !snapshot.Trustworthy() {
		return fmt.Errorf("cluster state not yet trustworthy for %s", displayName(s.ctx))
	}

	runAnalysisPass(s, snapshot, clusterList)
	return nil
}

// tallySnapshotCounts computes the critical/warning counts for a scan
// snapshot. Pod-scoped issues come from collectWarRoomIssues; node findings
// are counted separately from nodeHealth because collectWarRoomIssues has
// no DB access to correlate node incidents (see collectActiveNodeWarRoomIssues,
// which does but requires a persisted incident to already exist — wrong
// timing for this call site, which runs before this scan's node incidents
// are persisted).
func tallySnapshotCounts(issues []warRoomIssue, nodeHealth []models.NodeConditionFinding) (critical, warnings int) {
	for _, is := range issues {
		if is.Severity == "critical" {
			critical++
		} else {
			warnings++
		}
	}
	for _, finding := range nodeHealth {
		switch sev, _ := models.NodeConditionSeverity(finding.ConditionType); sev {
		case "critical":
			critical++
		case "high":
			warnings++
		}
	}
	return critical, warnings
}

func completeIncidentBatch(workloads []store.IncidentData, nodeFindings []models.NodeConditionFinding) []store.IncidentData {
	complete := append([]store.IncidentData(nil), workloads...)
	return append(complete, scanner.NodeConditionIncidents(nodeFindings)...)
}

func persistCompleteIncidentBatch(db store.Store, cluster, scanID string, incidents []store.IncidentData) (int, error) {
	if err := db.UpsertIncidents(cluster, scanID, incidents); err != nil {
		return 0, err
	}
	return db.ResolveMissing(cluster, scanID)
}

func newScanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
