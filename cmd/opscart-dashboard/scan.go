package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/acquisition"
	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/clusterstate"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// clusterScan holds results from all analyzers for a single cluster scan.
// Fields other than report may be nil if the audit failed (RBAC, timeout, etc.).
type clusterScan struct {
	report *models.CloudCostReport

	// secAudit is displayed by pages.go/server.go's overview and is also an
	// input to this scan cycle's incident batch (collectWarRoomIssues'
	// critical "privileged_container" issues, refresh below) — like
	// nodeHealth/netAudit, one of clusterScan's coordinator-migrated fields
	// incident persistence reads. See secAuditGeneration and refresh's
	// legacyScan capture for why that persistence use deliberately does not
	// go through the coordinator-preserved value this field may hold.
	secAudit *models.SecurityAudit

	// secAuditGeneration is the ClusterSnapshot generation that produced
	// secAudit when it came from the coordinator-driven path
	// (security_runtime.go), or 0 for results from the legacy runFullScan
	// path. Same defense-in-depth provenance guard as
	// nodeOptimizationGeneration below — see its comment for why this is
	// never relied on to paper over a real ordering bug. This governs the
	// DISPLAY value only; see secAudit's comment for the incident-batch
	// distinction.
	secAuditGeneration uint64

	// cisResult is derived from secAudit and netAudit, but ONLY from
	// whichever values runFullScan itself fetched synchronously in this
	// same legacy scan cycle (server.go step 5, computed immediately after
	// steps 2 and 4) — never from the coordinator-published secAudit/netAudit
	// this clusterScan may otherwise be carrying by the time a reader sees
	// it. docs/08 Phase 4D.4 deliberately leaves this boundary as-is: CIS
	// scoring is a derived, correctness-sensitive combination of two
	// analyzers, and recomputing it from coordinator-driven Security+Network
	// on every coalesced generation would need its own generation-guarded
	// field and cross-analyzer sequencing inside runCoordinatedAnalysis —
	// out of proportion to a display-only migration slice. The accepted
	// consequence: once the coordinator has published a newer Security or
	// Network generation than the last legacy scan cycle, the displayed CIS
	// score is not guaranteed to be generation-consistent with the
	// independently newer secAudit/netAudit shown elsewhere on the
	// dashboard, until the next legacy scan cycle recomputes all three
	// together again. This is unchanged by migrating Security here; the
	// same latent gap already existed for netAudit alone since Network's
	// own migration (Phase 4D.3) and is only being made explicit now.
	cisResult *analyzer.CISResult

	wasteAudit *analyzer.WasteAudit

	// netAudit is displayed by pages.go/investigation.go/warroom.go and is
	// also an input to this scan cycle's incident batch
	// (collectWarRoomIssues' "unprotected_namespace" issues, refresh below)
	// — like nodeHealth, one of clusterScan's coordinator-migrated fields
	// incident persistence reads. See netAuditGeneration and refresh's
	// legacyScan capture for why that persistence use deliberately does not
	// go through the coordinator-preserved value this field may hold.
	netAudit *analyzer.NetworkPolicyAudit

	// netAuditGeneration is the ClusterSnapshot generation that produced
	// netAudit when it came from the coordinator-driven path
	// (network_runtime.go), or 0 for results from the legacy runFullScan
	// path. Same defense-in-depth provenance guard as
	// nodeOptimizationGeneration below — see its comment for why this is
	// never relied on to paper over a real ordering bug. This governs the
	// DISPLAY value only; see netAudit's comment for the incident-batch
	// distinction.
	netAuditGeneration uint64

	// nodeHealth is displayed by pages.go/investigation.go/warroom.go and is
	// also the DIRECT input to this scan cycle's incident batch
	// (completeIncidentBatch, refresh below) — the only one of clusterScan's
	// coordinator-migrated fields incident persistence reads. See
	// nodeHealthGeneration and refresh's legacyScan capture for why
	// that persistence use deliberately does NOT go through the
	// coordinator-preserved value this field may hold.
	nodeHealth []models.NodeConditionFinding

	// nodeHealthGeneration is the ClusterSnapshot generation that produced
	// nodeHealth when it came from the coordinator-driven path
	// (node_health_runtime.go), or 0 for results from the legacy runFullScan
	// path. Same defense-in-depth provenance guard as
	// nodeOptimizationGeneration below — see its comment for why this is
	// never relied on to paper over a real ordering bug. This governs the
	// DISPLAY value only; see nodeHealth's comment for the incident-batch
	// distinction.
	nodeHealthGeneration uint64

	// AllWorkloads is every Deployment/StatefulSet/DaemonSet the scan
	// observed, regardless of the --breakdown flag. Written by either the
	// legacy pod enumeration ResourceAnalyzer.AnalyzeClusterResources already
	// performs for cost allocation (not a second cluster fetch), or — since
	// docs/08 Phase 4D.1 — the coordinator-driven path
	// (resource_analysis_runtime.go), whichever last published a newer
	// generation; see resourceAnalysisGeneration.
	AllWorkloads []models.WorkloadRef

	// PodWorkloads is the confirmed pod -> owning workload map from the
	// same pod enumeration, keyed by "namespace/podName". This is the
	// single source of truth for pod ownership — used instead of
	// name-pattern matching (store.OwnerNameFromPod + prefix checks),
	// which cannot distinguish a real StatefulSet replica from an
	// unrelated pod sharing its naming pattern.
	PodWorkloads map[string]models.WorkloadRef

	// resourceAnalysisGeneration is the ClusterSnapshot generation that
	// produced AllWorkloads/PodWorkloads when they came from the
	// coordinator-driven path (resource_analysis_runtime.go), or 0 for
	// results from the legacy runFullScan path. Same defense-in-depth
	// provenance guard as nodeOptimizationGeneration below — see its
	// comment for why this is never relied on to paper over a real
	// ordering bug.
	resourceAnalysisGeneration uint64

	// nodeOptimization is the read-only consolidation-simulation
	// recommendation contract (see pkg/analyzer/node_optimization_recommendation.go),
	// built from the same NodeInfo/Pod snapshots already fetched above for
	// cost analysis — not a second cluster fetch. Nil/empty until the Node
	// Optimization page renders it.
	nodeOptimization []analyzer.NodeOptimizationRecommendation

	// nodeOptimizationSavings is aligned 1:1 by index with nodeOptimization.
	// Each entry reuses Cost Intelligence's already-computed provider pricing
	// (poolCosts from the cost-analysis scan step, not a second pricing call)
	// to project the monthly savings of that pool's recommendation, when an
	// exact price can be joined. See pkg/analyzer/node_optimization_savings.go.
	nodeOptimizationSavings []analyzer.NodeOptimizationSavingsProjection

	// nodeOptimizationGeneration is the ClusterSnapshot generation that
	// produced nodeOptimization/nodeOptimizationSavings when they came from
	// the coordinator-driven path (node_optimization_runtime.go), or 0 for
	// results from the legacy runFullScan path below. It exists purely as a
	// defensive, provenance-documenting guard: Coordinator's single-threaded
	// loop already makes an older generation overwriting a newer one
	// structurally impossible (see pkg/clusterstate/coordinator.go), so this
	// field is never relied on to paper over a real ordering bug.
	nodeOptimizationGeneration uint64
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
	// Kubernetes client could not be constructed at startup.
	//
	// Phase 4A only starts it; runFullScan below still acquires and analyzes
	// independently. Its ClusterState is read only via coordinator below.
	acquisition *acquisition.Runtime

	// coordinator is this cluster's Phase 4B coalescing coordinator, driving
	// every Phase 4C/4D-migrated analyzer (currently Node Optimization,
	// Resource Analyzer, and Node Health — see acquisition_runtime.go's
	// runCoordinatedAnalysis) off acquisition's ClusterState instead of a
	// direct Kubernetes call. Set
	// once at startup alongside acquisition, before any concurrent reader
	// could observe it, and never reassigned afterward — same no-lock
	// convention as acquisition above.
	coordinator *clusterstate.Coordinator
}

// The preserveNewerCoordinatorX family used by refresh below — one guard
// per Phase 4C/4D-migrated analyzer, protecting its coordinator-published
// DISPLAY result from refresh's wholesale *clusterScan replacement — lives
// in analysis_preservation.go.

func (s *dashboardState) refresh(clusterList []string) error {
	if !s.scanning.CompareAndSwap(false, true) {
		return nil
	}
	defer s.scanning.Store(false)

	// Timer covers the full cycle — scan, render, and persistence — so
	// DurationMS reflects what "scan duration" actually means to a reader.
	// Previously this started after runFullScan/renderHTML completed, so
	// the recorded duration measured only the persistence block below.
	start := time.Now()

	scanCounters := newAPICounters()
	scan, err := runFullScan(s.ctx, scanCounters)
	if err != nil {
		return err
	}
	// legacyScan is a shallow copy of this cycle's own synchronous scan
	// result, taken before any coordinator-preserve mutation below.
	// Incident persistence (calcIncidentScore/collectWarRoomIssues and
	// everything derived from them, further down) must always read every
	// coordinator-migrated field (nodeHealth, netAudit, secAudit, ...) from
	// this legacy scan cycle's own observation, never a coordinator-published
	// one — see preserveNewerCoordinatorNodeHealth's doc comment. A shallow
	// copy is enough: only the top-level *clusterScan fields the guards
	// below reassign (never mutated in place) need to diverge from scan.
	legacyScan := *scan

	page := renderHTML(scan, s.ctx, clusterList)

	s.mu.Lock()
	preserveNewerCoordinatorNodeOptimization(s.scan, scan)
	preserveNewerCoordinatorResourceAnalysis(s.scan, scan)
	preserveNewerCoordinatorNodeHealth(s.scan, scan)
	preserveNewerCoordinatorNetworkAnalysis(s.scan, scan)
	preserveNewerCoordinatorSecurityAnalysis(s.scan, scan)
	s.scan = scan
	s.htmlPage = page
	s.mu.Unlock()

	// Persist to operational memory (best-effort, never blocks scan)
	if s.db != nil {
		scanID := newScanID()

		incScore, _, _ := calcIncidentScore(&legacyScan)
		issues := collectWarRoomIssues(&legacyScan, 0)

		critical, warnings := tallySnapshotCounts(issues, legacyScan.nodeHealth)
		var incidents []store.IncidentData
		for _, is := range issues {
			details, _ := json.Marshal(map[string]any{
				"resource_age_days": is.ResourceAgeDays,
				"message":           is.Message,
			})
			incidents = append(incidents, store.IncidentData{
				Fingerprint:  store.WorkloadFingerprintForPod(is.Namespace, is.Resource, is.Type),
				Namespace:    is.Namespace,
				Resource:     is.Resource,
				IssueType:    is.Type,
				Severity:     is.Severity,
				DetailsJSON:  string(details),
				RestartCount: is.RestartCount,
			})
		}

		snap := store.SnapshotData{
			ScannedAt:     time.Now(),
			IncidentScore: incScore,
			CriticalCount: critical,
			WarningCount:  warnings,
			SecurityScore: legacyScan.securityScore(),
			WasteCount:    legacyScan.wasteTotal(),
			MonthlyCost:   legacyScan.report.TotalMonthlyCost,
			PodCount:      legacyScan.monthlyPodCount(),
		}

		if err := s.db.WriteSnapshot(s.ctx, scanID, snap); err != nil {
			log.Printf("[%s] store snapshot: %v", displayName(s.ctx), err)
		}
		incidents = completeIncidentBatch(incidents, legacyScan.nodeHealth)
		resolved, persistErr := persistCompleteIncidentBatch(s.db, s.ctx, scanID, incidents)
		if persistErr != nil {
			log.Printf("[%s] store incidents: %v", displayName(s.ctx), persistErr)
		} else if resolved > 0 {
			log.Printf("[%s] %d incident(s) resolved", displayName(s.ctx), resolved)
		}
		if cutoff, ok := store.RetentionCutoff(s.retentionDays, time.Now()); ok {
			if pruned, err := s.db.PruneOlderThan(s.ctx, cutoff); err != nil {
				log.Printf("[%s] store prune: %v", displayName(s.ctx), err)
			} else if pruned > 0 {
				log.Printf("[%s] retention: pruned %d incident(s) older than %d day(s)", displayName(s.ctx), pruned, s.retentionDays)
			}
		}
		_ = s.db.WriteScanHistory(s.ctx, scanID, store.ScanMeta{
			DurationMS: time.Since(start).Milliseconds(),
			Success:    true,
			Version:    Version,
		})
	}

	s.mu.Lock()
	s.observation = scanObservation{CompletedAt: time.Now(), Duration: time.Since(start), API: scanCounters.snapshot()}
	s.mu.Unlock()

	log.Printf("[%s] scan complete: %d namespaces, $%s/month, %d critical issues", displayName(s.ctx), len(scan.report.NamespaceCosts), formatMoney(scan.report.TotalMonthlyCost), countCriticalIssues(scan))
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

// startBackgroundRefresh ticks every interval and re-scans every cluster that
// has been visited at least once. The worker is owned by ctx and backgroundWG,
// so shutdown can stop scheduling work and wait for an in-flight scan before
// the SQLite store is closed.
func (srv *server) startBackgroundRefresh(ctx context.Context, interval time.Duration) {
	srv.backgroundWG.Add(1)
	go srv.runBackgroundRefresh(ctx, interval)
}

func (srv *server) runBackgroundRefresh(ctx context.Context, interval time.Duration) {
	defer srv.backgroundWG.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		srv.mu.RLock()
		states := make([]*dashboardState, 0, len(srv.states))
		for _, s := range srv.states {
			states = append(states, s)
		}
		srv.mu.RUnlock()

		for _, state := range states {
			if ctx.Err() != nil {
				return
			}
			state.mu.RLock()
			hasData := state.scan != nil
			state.mu.RUnlock()
			if !hasData {
				continue
			}
			if err := srv.refreshState(state, srv.clusterList); err != nil {
				log.Printf("[%s] background refresh error: %v", displayName(state.ctx), err)
			}
		}
	}
}

func newScanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
