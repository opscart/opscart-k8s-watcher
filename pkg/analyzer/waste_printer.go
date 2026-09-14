package analyzer

import (
	"fmt"
	"strings"
)

// ================================================================
// Print Functions
// ================================================================

func PrintWasteAudit(audit *WasteAudit, minAgeDays int) {
	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║           CLUSTER WASTE & DRIFT ANALYSIS                  ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Age-gated checks: %2d days  │  Suggestions only - no changes made  ║\n", minAgeDays)
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	if audit == nil {
		printExecutiveSummary(nil)
		return
	}

	// Executive summary — high-level picture before the detail
	printExecutiveSummary(audit)

	// Scorecard
	printWasteScorecard(audit)

	// Details by category
	printAbandonedNamespaces(audit)
	printStalePods(audit)
	printOrphanedPVCs(audit)
	printStaleJobs(audit)
	printZeroReplicaWorkloads(audit)
	printOldReplicaSets(audit)
	printOrphanedServices(audit)
	printBrokenIngresses(audit)
	printMisconfiguredHPAs(audit)

	// Summary actions
	printWasteSummary(audit)
}

// splitMisconfiguredHPAs separates currently-active failures
// (ScalingActive=False) from age-gated tuning-review candidates
// (AlwaysAtMin), so callers classify waste findings without conflating a
// live break with a review suggestion.
func splitMisconfiguredHPAs(hpas []MisconfiguredHPA) (active, tuning int) {
	for _, hpa := range hpas {
		if hpa.IsActive {
			active++
		} else {
			tuning++
		}
	}
	return active, tuning
}

func printExecutiveSummary(audit *WasteAudit) {
	p := BuildWastePresentation(audit)
	fmt.Println("EXECUTIVE SUMMARY")
	fmt.Printf("Scanned: %s\n", p.ScanTimeLabel())
	fmt.Printf("Finding count: %d | Distinct resource count: %d\n", p.Counts.Findings, p.Counts.DistinctResources)
	fmt.Printf("Operational findings: %d | Housekeeping/retention findings: %d | Other review findings: %d\n",
		p.Counts.Operational, p.Counts.Retention, p.Counts.Review)
	fmt.Println("Operational counts are audit findings, not incident-store counts; historical and inferred evidence may be included.")
	fmt.Printf("Candidate PVC requests: %s (%d quantities unknown)\n", FormatWasteBytes(p.RequestedStorageBytes), p.UnknownStorageRequests)
	fmt.Printf("Coverage: %s\n", p.Coverage)
	for _, warning := range p.Warnings {
		fmt.Printf("Check warning: %s: %s\n", warning.Category, warning.Error)
	}
	if p.Counts.Findings == 0 {
		fmt.Println("No findings were reported by the available checks; this does not establish a clean cluster.")
	}
}

func printWasteScorecard(audit *WasteAudit) {
	fmt.Println("WASTE SCORECARD")
	fmt.Println("═══════════════════════════════════════════════════════════")

	printScoreRow("Namespace Activity Review", len(audit.AbandonedNamespaces), "🔴")
	zombieCount := 0
	bareCount := 0
	for _, p := range audit.StalePods {
		if p.Kind == StalePodZombie {
			zombieCount++
		} else {
			bareCount++
		}
	}
	printScoreRow("Pod Failure Evidence", zombieCount, "🔴")
	printScoreRow("Pod Ownership Review", bareCount, "🔴")
	printScoreRow("PVC State / Reference Review", len(audit.OrphanedPVCs), "🔴")
	printScoreRow("Service Selector Review", len(audit.OrphanedServices), "🟡")
	printScoreRow("Ingress Backend Evidence", len(audit.BrokenIngresses), "🔴")
	printScoreRow("Job / CronJob Retention Review", len(audit.StaleJobs), "🟡")
	printScoreRow("Zero-Replica Workloads", len(audit.ZeroReplicaWorkloads), "🟡")
	printScoreRow("ReplicaSet Retention Review", len(audit.OldReplicaSets), "🟢")
	hpaEmoji := "🟢" // tuning candidates only (AlwaysAtMin)
	if active, _ := splitMisconfiguredHPAs(audit.MisconfiguredHPAs); active > 0 {
		hpaEmoji = "🔴" // at least one HPA currently reporting ScalingActive=False
	}
	printScoreRow("HPA Configuration Review", len(audit.MisconfiguredHPAs), hpaEmoji)

	fmt.Println("───────────────────────────────────────────────────────────")
	fmt.Printf("  Finding count:  %d\n", BuildWastePresentation(audit).Counts.Findings)
	fmt.Println()
}

func printScoreRow(label string, count int, emoji string) {
	if count == 0 {
		fmt.Printf("  ✅ %-28s 0\n", label+":")
	} else {
		fmt.Printf("  %s %-28s %d\n", emoji, label+":", count)
	}
}

func printAbandonedNamespaces(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Namespace activity review")
	if len(audit.AbandonedNamespaces) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🔴 NAMESPACE ACTIVITY REVIEW (%d)\n", len(audit.AbandonedNamespaces))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, ns := range audit.AbandonedNamespaces {
		fmt.Printf("\n  📁 %s\n", ns.Name)
		fmt.Printf("     Age:      %d days\n", ns.AgeDays)
		fmt.Printf("     Pods:     %s\n", podLabel(ns.PodCount))
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printStalePods(audit *WasteAudit) {
	if len(audit.StalePods) == 0 {
		return
	}

	// Separate zombies from idle
	zombies := []StalePod{}
	idle := []StalePod{}
	for _, p := range audit.StalePods {
		if p.Kind == StalePodZombie {
			zombies = append(zombies, p)
		} else {
			idle = append(idle, p)
		}
	}

	if len(zombies) > 0 {
		evidence := BuildWastePresentation(audit).FindingsInCategory("Pod failure evidence")
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Printf("🔴 POD FAILURE EVIDENCE (%d)\n", len(zombies))
		fmt.Println("═══════════════════════════════════════════════════════════")
		for i, p := range zombies {
			fmt.Printf("\n  💀 %s (namespace: %s)\n", p.Name, p.Namespace)
			fmt.Printf("     Classification: %s\n", p.Status)
			fmt.Printf("     Age:      %d days\n", p.AgeDays)
			fmt.Printf("     Restarts: %d\n", p.RestartCount)
			printWasteEvidence(evidence[i])
		}
		fmt.Println()
	}

	if len(idle) > 0 {
		evidence := BuildWastePresentation(audit).FindingsInCategory("Pod ownership review")
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Printf("🔴 POD OWNERSHIP REVIEW (%d)\n", len(idle))
		fmt.Println("═══════════════════════════════════════════════════════════")
		for i, p := range idle {
			fmt.Printf("\n  [BARE] %s (namespace: %s)\n", p.Name, p.Namespace)
			fmt.Printf("     Age:           %d days\n", p.AgeDays)
			fmt.Printf("     Restarts:      %d (total)\n", p.RestartCount)
			printWasteEvidence(evidence[i])
		}
		fmt.Println()
	}
}

func printOrphanedPVCs(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("PVC state / reference review")
	if len(audit.OrphanedPVCs) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🔴 PVC STATE / REFERENCE REVIEW (%d)\n", len(audit.OrphanedPVCs))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, pvc := range audit.OrphanedPVCs {
		sizeStr := evidence[i].Storage
		fmt.Printf("\n  💾 %s (namespace: %s)\n", pvc.Name, pvc.Namespace)
		fmt.Printf("     State evidence: %s\n", evidence[i].Observed)
		fmt.Printf("     Size:    %s\n", sizeStr)
		fmt.Printf("     Age:     %d days\n", pvc.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printStaleJobs(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Job / CronJob retention review")
	if len(audit.StaleJobs) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 JOB / CRONJOB RETENTION REVIEW (%d)\n", len(audit.StaleJobs))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, job := range audit.StaleJobs {
		kind := "Job"
		if job.IsCronJob {
			kind = "CronJob"
		}
		fmt.Printf("\n  ⏰ %s [%s] (namespace: %s)\n", job.Name, kind, job.Namespace)
		fmt.Printf("     Review category: %s\n", evidence[i].Category)
		fmt.Printf("     Age:     %d days\n", job.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printZeroReplicaWorkloads(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Zero-replica workload")
	if len(audit.ZeroReplicaWorkloads) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 ZERO-REPLICA WORKLOADS (%d)\n", len(audit.ZeroReplicaWorkloads))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, w := range audit.ZeroReplicaWorkloads {
		fmt.Printf("\n  📦 %s [%s] (namespace: %s)\n", w.Name, w.Kind, w.Namespace)
		fmt.Printf("     Age:     %d days\n", w.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printOldReplicaSets(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("ReplicaSet retention review")
	if len(audit.OldReplicaSets) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟢 REPLICASET RETENTION REVIEW (%d)\n", len(audit.OldReplicaSets))
	fmt.Println("═══════════════════════════════════════════════════════════")
	// Show top 10 only - these can be numerous
	shown := audit.OldReplicaSets
	remaining := 0
	if len(shown) > 10 {
		remaining = len(shown) - 10
		shown = shown[:10]
	}
	for i, rs := range shown {
		fmt.Printf("  📋 %s (owner: %s, age: %d days)\n", rs.Name, rs.OwnerDeployment, rs.AgeDays)
		printWasteEvidence(evidence[i])
	}
	if remaining > 0 {
		fmt.Printf("  ... and %d more old ReplicaSets\n", remaining)
		fmt.Println("  Inspect: kubectl get rs -A -o yaml; confirm rollback and retention requirements with owners.")
	}
	fmt.Println()
}

func printOrphanedServices(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Service selector review")
	if len(audit.OrphanedServices) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 SERVICE SELECTOR REVIEW (%d)\n", len(audit.OrphanedServices))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, svc := range audit.OrphanedServices {
		lbNote := ""
		if svc.IsLB {
			lbNote = " LoadBalancer — billing not checked"
		}
		fmt.Printf("\n  🔌 %s [%s]%s (namespace: %s)\n", svc.Name, svc.Type, lbNote, svc.Namespace)
		fmt.Printf("     Age:     %d days\n", svc.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printBrokenIngresses(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("Ingress backend evidence")
	if len(audit.BrokenIngresses) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟡 INGRESS BACKEND EVIDENCE (%d)\n", len(audit.BrokenIngresses))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, ing := range audit.BrokenIngresses {
		activeNote := ""
		if ing.IsActive {
			activeNote = " condition reported"
		}
		fmt.Printf("\n  🌐 %s (namespace: %s)%s\n", ing.Name, ing.Namespace, activeNote)
		fmt.Printf("     Hosts:   %s\n", strings.Join(ing.Hosts, ", "))
		fmt.Printf("     Age:     %d days\n", ing.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printMisconfiguredHPAs(audit *WasteAudit) {
	evidence := BuildWastePresentation(audit).FindingsInCategory("HPA configuration review")
	if len(audit.MisconfiguredHPAs) == 0 {
		return
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("🟢 HPA CONFIGURATION REVIEW (%d)\n", len(audit.MisconfiguredHPAs))
	fmt.Println("═══════════════════════════════════════════════════════════")
	for i, hpa := range audit.MisconfiguredHPAs {
		activeNote := ""
		if hpa.IsActive {
			activeNote = " condition reported"
		}
		fmt.Printf("\n  📈 %s → %s (namespace: %s)%s\n", hpa.Name, hpa.TargetName, hpa.Namespace, activeNote)
		fmt.Printf("     Replicas: min=%d max=%d  Condition: %s\n", hpa.MinReplicas, hpa.MaxReplicas, hpa.Condition)
		fmt.Printf("     Age:      %d days\n", hpa.AgeDays)
		printWasteEvidence(evidence[i])
	}
	fmt.Println()
}

func printWasteSummary(audit *WasteAudit) {
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("📋 NEXT STEPS")
	fmt.Println("Review observations, workload intent, and retention requirements with the owning team.")
	fmt.Println("Priority is the legacy heuristic, not confidence, financial impact, or cleanup safety.")
	fmt.Println("Use --min-age-days to adjust age-gated checks and --namespace to focus the scan.")
}

func printWasteEvidence(f WasteFinding) {
	fmt.Printf("     Observed: %s\n     Inference: %s\n     Limitations: %s\n     Review: %s\n", f.Observed, f.Inference, f.Limitations, f.Review)
	fmt.Printf("     Priority score: %g (legacy heuristic) | Evidence confidence: %s — %s\n     %s\n", f.Priority, f.Confidence, f.ConfidenceReason, f.Command)
}
