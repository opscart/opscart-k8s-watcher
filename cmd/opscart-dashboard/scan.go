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

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// clusterScan holds results from all analyzers for a single cluster scan.
// Fields other than report may be nil if the audit failed (RBAC, timeout, etc.).
type clusterScan struct {
	report     *models.CloudCostReport
	secAudit   *models.SecurityAudit
	cisResult  *analyzer.CISResult
	wasteAudit *analyzer.WasteAudit
	netAudit   *analyzer.NetworkPolicyAudit
	nodeHealth []models.NodeConditionFinding

	// AllWorkloads is every Deployment/StatefulSet/DaemonSet the scan
	// observed, regardless of the --breakdown flag — retained from the pod
	// enumeration ResourceAnalyzer.AnalyzeClusterResources already performs
	// for cost allocation, not a second cluster fetch.
	AllWorkloads []models.WorkloadRef

	// PodWorkloads is the confirmed pod -> owning workload map from the
	// same pod enumeration, keyed by "namespace/podName". This is the
	// single source of truth for pod ownership — used instead of
	// name-pattern matching (store.OwnerNameFromPod + prefix checks),
	// which cannot distinguish a real StatefulSet replica from an
	// unrelated pod sharing its naming pattern.
	PodWorkloads map[string]models.WorkloadRef
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
}

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

	scan, err := runFullScan(s.ctx)
	if err != nil {
		return err
	}
	page := renderHTML(scan, s.ctx, clusterList)

	s.mu.Lock()
	s.scan = scan
	s.htmlPage = page
	s.mu.Unlock()

	// Persist to operational memory (best-effort, never blocks scan)
	if s.db != nil {
		scanID := newScanID()

		incScore, _, _ := calcIncidentScore(scan)
		issues := collectWarRoomIssues(scan, 0)

		critical, warnings := tallySnapshotCounts(issues, scan.nodeHealth)
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
			SecurityScore: scan.securityScore(),
			WasteCount:    scan.wasteTotal(),
			MonthlyCost:   scan.report.TotalMonthlyCost,
			PodCount:      scan.monthlyPodCount(),
		}

		if err := s.db.WriteSnapshot(s.ctx, scanID, snap); err != nil {
			log.Printf("[%s] store snapshot: %v", displayName(s.ctx), err)
		}
		incidents = completeIncidentBatch(incidents, scan.nodeHealth)
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
