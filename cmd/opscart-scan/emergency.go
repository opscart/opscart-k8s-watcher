package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/models"
	"github.com/opscart/opscart-k8s-watcher/pkg/scanner"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// enrichedIssue pairs a scan finding with operational memory context, if
// any was found for its fingerprint.
type enrichedIssue struct {
	models.EmergencyIssue
	FirstDetected string // formatDuration(time since first seen); "" when no history
	ReopenCount   int
}

func runEmergencyScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	s, err := scanner.NewScanner(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	issues, err := s.FindEmergencyIssues(namespace)
	if err != nil {
		return fmt.Errorf("scanning cluster: %w", err)
	}

	// Persist findings to operational memory (best-effort, never blocks
	// printing results — this is secondary to the CLI's primary job).
	persistFindings(opStore, clusterContext, newScanID(), issues)

	printEmergencyIssuesEnriched(os.Stdout, enrichIssues(opStore, clusterContext, issues))
	return nil
}

// persistFindings writes this scan's findings to operational memory and
// resolves incidents absent from it. Errors are logged, not propagated —
// printing results is the primary job; persistence is best-effort.
func persistFindings(db store.Store, clusterContext, scanID string, issues []models.EmergencyIssue) {
	if err := db.UpsertIncidents(clusterContext, scanID, mapIssuesToIncidents(issues)); err != nil {
		log.Printf("opscart-scan: could not write operational memory: %v", err)
	}
	if _, err := db.ResolveMissing(clusterContext, scanID); err != nil {
		log.Printf("opscart-scan: could not resolve missing incidents: %v", err)
	}
}

// incidentFingerprint builds the same fingerprint identity the dashboard's
// scan loop uses (cmd/opscart-dashboard/scan.go), so history lines up
// across both tools when pointed at the same --db-path.
func incidentFingerprint(issue models.EmergencyIssue) string {
	return store.Fingerprint(issue.Namespace, "Workload", store.OwnerNameFromPod(issue.Name), issue.Reason)
}

// mapIssuesToIncidents converts scan findings to the store's write format.
func mapIssuesToIncidents(issues []models.EmergencyIssue) []store.IncidentData {
	var incidents []store.IncidentData
	for _, issue := range issues {
		details, _ := json.Marshal(map[string]any{
			"age_days": int(issue.Age.Hours() / 24),
			"message":  issue.Message,
		})
		incidents = append(incidents, store.IncidentData{
			Fingerprint:  incidentFingerprint(issue),
			Namespace:    issue.Namespace,
			Resource:     issue.Name,
			IssueType:    issue.Reason,
			Severity:     issue.Severity,
			DetailsJSON:  string(details),
			RestartCount: issue.Restarts,
		})
	}
	return incidents
}

// enrichIssues looks up every issue's operational memory in two batched
// calls covering all fingerprints at once, rather than a pair of queries
// per issue. Missing history, a lookup error, or a NullStore (stateless
// mode) all degrade silently to no enrichment — never an error shown to
// the user.
func enrichIssues(db store.Store, clusterContext string, issues []models.EmergencyIssue) []enrichedIssue {
	enriched := make([]enrichedIssue, len(issues))
	fingerprints := make([]string, len(issues))
	for i, issue := range issues {
		enriched[i] = enrichedIssue{EmergencyIssue: issue}
		fingerprints[i] = incidentFingerprint(issue)
	}

	history, err := db.BatchGetIncidentHistory(clusterContext, fingerprints)
	if err != nil {
		return enriched
	}

	ids := make([]int64, 0, len(history))
	for _, rec := range history {
		ids = append(ids, rec.ID)
	}
	// A failed reopen-count lookup still leaves FirstDetected enriched
	// below; reopenCounts stays nil, and indexing a nil map yields 0.
	reopenCounts, _ := db.BatchGetReopenCounts(ids)

	for i, fingerprint := range fingerprints {
		rec, ok := history[fingerprint]
		if !ok || rec == nil {
			continue
		}
		enriched[i].FirstDetected = formatDuration(time.Since(rec.FirstSeen))
		enriched[i].ReopenCount = reopenCounts[rec.ID]
	}
	return enriched
}

// printEmergencyIssuesEnriched mirrors scanner.PrintEmergencyIssues'
// grouping and box-drawing style exactly, with memory-context lines
// appended to each issue block when available.
func printEmergencyIssuesEnriched(w io.Writer, issues []enrichedIssue) {
	if len(issues) == 0 {
		fmt.Fprintln(w, "✅ No critical issues found!")
		return
	}

	var critical, high, medium []enrichedIssue
	for _, issue := range issues {
		switch issue.Severity {
		case "critical":
			critical = append(critical, issue)
		case "high":
			high = append(high, issue)
		case "medium":
			medium = append(medium, issue)
		}
	}

	fmt.Fprintln(w, "╔════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║             WAR ROOM - EMERGENCY ISSUES                    ║")
	fmt.Fprintln(w, "╚════════════════════════════════════════════════════════════╝")
	fmt.Fprintf(w, "\n🔴 CRITICAL: %d    🟡 HIGH: %d    🟠 MEDIUM: %d\n\n", len(critical), len(high), len(medium))

	if len(critical) > 0 {
		fmt.Fprintln(w, "🔴 CRITICAL ISSUES:")
		fmt.Fprintln(w, strings.Repeat("═", 80))
		for _, issue := range critical {
			printEnrichedIssue(w, issue)
		}
		fmt.Fprintln(w)
	}

	if len(high) > 0 {
		fmt.Fprintln(w, "🟡 HIGH PRIORITY:")
		fmt.Fprintln(w, strings.Repeat("═", 80))
		for _, issue := range high {
			printEnrichedIssue(w, issue)
		}
		fmt.Fprintln(w)
	}

	if len(medium) > 0 {
		fmt.Fprintln(w, "🟠 MEDIUM PRIORITY:")
		fmt.Fprintln(w, strings.Repeat("═", 80))
		for _, issue := range medium {
			printEnrichedIssue(w, issue)
		}
	}
}

func printEnrichedIssue(w io.Writer, issue enrichedIssue) {
	fmt.Fprintf(w, "  %s/%s\n", issue.Namespace, issue.Name)
	fmt.Fprintf(w, "  └─ Status: %s", issue.Reason)
	if issue.Restarts > 0 {
		fmt.Fprintf(w, " | Restarts: %d", issue.Restarts)
	}
	fmt.Fprintf(w, " | Age: %s\n", formatDuration(issue.Age))
	fmt.Fprintf(w, "  └─ %s\n", issue.Message)
	if issue.FirstDetected != "" {
		fmt.Fprintf(w, "  └─ First detected: %s ago\n", issue.FirstDetected)
	}
	if issue.ReopenCount > 0 {
		fmt.Fprintf(w, "  └─ Reopened: %dx\n", issue.ReopenCount)
	}
	fmt.Fprintln(w)
}

// formatDuration matches pkg/scanner/printer.go's formatDuration exactly.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// newScanID matches cmd/opscart-dashboard/scan.go's newScanID exactly.
func newScanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
