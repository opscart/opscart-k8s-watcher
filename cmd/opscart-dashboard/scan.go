package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/models"
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
		start := time.Now()

		incScore, _, _ := calcIncidentScore(scan)
		issues := collectWarRoomIssues(scan, 0)

		critical, warnings := 0, 0
		var incidents []store.IncidentData
		for _, is := range issues {
			if is.Severity == "critical" {
				critical++
			} else {
				warnings++
			}
			owner := store.OwnerNameFromPod(is.Resource)
			details, _ := json.Marshal(map[string]any{
				"age_days": is.AgeDays,
				"message":  is.Message,
			})
			incidents = append(incidents, store.IncidentData{
				Fingerprint:  store.Fingerprint(is.Namespace, "Workload", owner, is.Type),
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
		if err := s.db.UpsertIncidents(s.ctx, scanID, incidents); err != nil {
			log.Printf("[%s] store incidents: %v", displayName(s.ctx), err)
		}
		if resolved, err := s.db.ResolveMissing(s.ctx, scanID); err == nil && resolved > 0 {
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

// startBackgroundRefresh ticks every interval and re-scans every cluster that
// has been visited at least once. Uses the same refresh() pipeline as POST /refresh.
func (srv *server) startBackgroundRefresh(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		srv.mu.RLock()
		states := make([]*dashboardState, 0, len(srv.states))
		for _, s := range srv.states {
			states = append(states, s)
		}
		srv.mu.RUnlock()

		for _, state := range states {
			state.mu.RLock()
			hasData := state.scan != nil
			state.mu.RUnlock()
			if !hasData {
				continue
			}
			if err := state.refresh(srv.clusterList); err != nil {
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
