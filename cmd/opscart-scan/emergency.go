package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
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
	FirstDetected     string    // formatDuration(time since first seen); "" when no history
	ReopenCount       int       // retained for lifecycle consumers; Emergency does not render aggregates
	firstSeenAt       time.Time // raw form of FirstDetected, used only to pick a group's representative
	atHistoryBoundary bool
}

func runEmergencyScan(clusterContext string) error {
	fmt.Printf("\n🔍 Cluster: %s\n", clusterContext)
	if selectedDBPath != "" {
		fmt.Printf("OMA database: %s\n", selectedDBPath)
	} else {
		fmt.Println("OMA database: —")
	}
	earliest, _ := store.GetEarliestRetainedObservation(opStore, clusterContext)
	if earliest.IsZero() {
		fmt.Printf("Earliest retained observation for cluster %s: —\n", clusterContext)
	} else {
		fmt.Printf("Earliest retained observation for cluster %s: %s\n", clusterContext, earliest.Local().Format("2006-01-02 15:04:05 MST"))
	}
	s, err := scanner.NewScanner(clusterContext)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	rawIssues, err := s.FindEmergencyIssues(namespace)
	if err != nil {
		return fmt.Errorf("scanning cluster: %w", err)
	}

	probeFailures := detectProbeFailures(clusterContext, rawIssues)
	issues := classifyIssues(rawIssues, probeFailures)

	// Persist findings to operational memory (best-effort, never blocks
	// printing results — this is secondary to the CLI's primary job).
	persistFindings(opStore, clusterContext, newScanID(), issues)

	enriched := enrichIssues(opStore, clusterContext, issues)
	enriched = applyCriticalDebounce(opStore, clusterContext, enriched)
	enriched = suppressSecondaryRestartNoise(opStore, clusterContext, enriched)
	printEmergencyIssuesEnriched(os.Stdout, enriched)
	return nil
}

// classifiablePodReasons is the set of per-container "health" reasons
// analyzePodForIssues (pkg/scanner/cluster.go) can emit for a pod:
// CrashLoopBackOff, OOMKilled, ImagePullBackOff/ErrImagePull, and
// HighRestartCount are each their own independent `if`, not an if/else
// chain, so the SAME pod commonly produces two or more of these in a
// single scan (e.g. a container with >10 restarts that also happens to
// be Waiting in CrashLoopBackOff this instant satisfies both the
// CrashLoopBackOff check and the HighRestartCount check at once). A
// crash-looping pod's own brief post-backoff Running window can also
// make different single reasons from this set appear on consecutive
// scans, even with no real change in the pod's health. classifyPod
// collapses whichever of these reasons a pod has this run into exactly
// one verdict, so a physical pod problem never produces more than one
// fingerprint/incident. Reasons outside this set (PodFailed, Pending,
// PVC issues) are unaffected — classifyIssues passes them through
// untouched.
var classifiablePodReasons = map[string]bool{
	"CrashLoopBackOff": true,
	"OOMKilled":        true,
	"ImagePullBackOff": true,
	"ErrImagePull":     true,
	"HighRestartCount": true,
}

// classifyPod picks exactly one issue to represent a single pod's
// classifiable issues (see classifiablePodReasons), in this exact
// priority order, first match wins:
//
//  1. OOMKilled + CrashLoopBackOff -> "CrashLoopBackOff (OOMKilled)", CRITICAL
//  2. ProbeFailure signature + CrashLoopBackOff -> "CrashLoopBackOff (ProbeFailure)", CRITICAL
//  3. Plain CrashLoopBackOff (neither of the above) -> "CrashLoopBackOff", CRITICAL
//  4. ImagePullBackOff / ErrImagePull -> unchanged (HIGH, exactly as the scanner classifies it)
//  5. HighRestartCount, only if none of 1-4 matched -> "HighRestartCount", MEDIUM
//  6. Otherwise -> ok is false: podIssues held nothing classifiable
//
// One case sits outside that 6-step list but is required to preserve
// pre-existing behavior: OOMKilled with NO CrashLoopBackOff present.
// cs.LastTerminationState is sticky — it keeps reporting "OOMKilled"
// until the container's next restart, long after the container is back
// to Running — so a pod that OOM'd once and stabilized still carries a
// live OOMKilled signal indefinitely. That's already CRITICAL and no
// less real for lacking a current crash loop, so it's checked directly
// after case 3, before ImagePullBackOff/HighRestartCount, on the same
// "CRITICAL beats everything below it" logic as cases 1-3.
//
// probeFailure is detectProbeFailures' pre-computed result for this pod:
// checking recent events needs a clientset call per pod, done once up
// front (see detectProbeFailures) rather than inside this pure function.
//
// podIssues must all share the same namespace/name (one physical pod);
// callers (classifyIssues, tests) are responsible for that grouping.
func classifyPod(podIssues []models.EmergencyIssue, probeFailure bool) (models.EmergencyIssue, bool) {
	var crashLoop, oom, imagePull, highRestart *models.EmergencyIssue
	for i := range podIssues {
		switch podIssues[i].Reason {
		case "CrashLoopBackOff":
			crashLoop = &podIssues[i]
		case "OOMKilled":
			oom = &podIssues[i]
		case "ImagePullBackOff", "ErrImagePull":
			imagePull = &podIssues[i]
		case "HighRestartCount":
			highRestart = &podIssues[i]
		}
	}

	if crashLoop != nil && oom != nil {
		out := *crashLoop
		out.Reason = "CrashLoopBackOff (OOMKilled)"
		out.Message = "Container termination state reports OOMKilled; the pod is currently in CrashLoopBackOff."
		return out, true
	}
	if crashLoop != nil && probeFailure {
		out := *crashLoop
		out.Reason = "CrashLoopBackOff (ProbeFailure)"
		container := extractCrashLoopContainer(crashLoop.Message)
		out.Message = fmt.Sprintf("Container %s: Kubernetes events show repeated startup/liveness probe failures followed by container restarts. Investigate probe configuration and actual startup time", container)
		return out, true
	}
	if crashLoop != nil {
		return *crashLoop, true
	}
	if oom != nil {
		return *oom, true
	}
	if imagePull != nil {
		return *imagePull, true
	}
	if highRestart != nil {
		return *highRestart, true
	}
	return models.EmergencyIssue{}, false
}

// classifyIssues is this task's root fix applied to a full scan: every
// pod-resource issue whose Reason is in classifiablePodReasons is grouped
// by pod and reduced to classifyPod's single verdict. Every other issue —
// a non-pod resource (e.g. PVC) or a pod issue classifyPod doesn't cover
// (PodFailed, Pending) — passes through untouched, exactly once. A pod's
// classified verdict takes the position of that pod's first classifiable
// raw issue, so scan order is preserved as closely as collapsing allows.
func classifyIssues(issues []models.EmergencyIssue, probeFailureByPod map[string]bool) []models.EmergencyIssue {
	groups := make(map[string][]models.EmergencyIssue)
	slot := make(map[string]int)
	var podOrder []string

	out := make([]models.EmergencyIssue, 0, len(issues))
	for _, iss := range issues {
		if iss.Resource != "pod" || !classifiablePodReasons[iss.Reason] {
			out = append(out, iss)
			continue
		}
		key := podKey(iss.Namespace, iss.Name)
		if _, seen := groups[key]; !seen {
			podOrder = append(podOrder, key)
			slot[key] = len(out)
			out = append(out, models.EmergencyIssue{})
		}
		groups[key] = append(groups[key], iss)
	}

	for _, key := range podOrder {
		if classified, ok := classifyPod(groups[key], probeFailureByPod[key]); ok {
			out[slot[key]] = classified
		}
	}

	// Drop the zero-value placeholder for any pod whose classifiable
	// issues, against all expectation, still failed to classify (see
	// classifyPod's step 6) — every reason in classifiablePodReasons is
	// handled explicitly above, so this is defensive, not reachable today.
	filtered := out[:0]
	for _, iss := range out {
		if iss.Reason == "" && iss.Name == "" && iss.Namespace == "" {
			continue
		}
		filtered = append(filtered, iss)
	}
	return filtered
}

// detectProbeFailures checks, for every distinct pod with a raw
// CrashLoopBackOff issue in issues, whether its recent events show a
// probe-failure signature (see hasProbeFailureSignature) — the input
// classifyPod's priority 2 needs. Best-effort: a scan with no
// CrashLoopBackOff issues never triggers a clientset connection at all,
// and any lookup failure (can't connect, can't list events for one pod)
// just leaves that pod out of the returned map, which classifyPod treats
// identically to "no signature found".
func detectProbeFailures(clusterContext string, issues []models.EmergencyIssue) map[string]bool {
	hasCrashLoop := false
	for _, issue := range issues {
		if issue.Reason == "CrashLoopBackOff" {
			hasCrashLoop = true
			break
		}
	}
	if !hasCrashLoop {
		return nil
	}

	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		log.Printf("opscart-scan: could not check probe-failure events: %v", err)
		return nil
	}

	result := make(map[string]bool)
	for _, issue := range issues {
		if issue.Reason != "CrashLoopBackOff" {
			continue
		}
		key := podKey(issue.Namespace, issue.Name)
		if _, done := result[key]; done {
			continue // already checked this pod (multi-container case)
		}
		messages, err := recentPodEventMessages(clientset, issue.Namespace, issue.Name)
		if err != nil {
			log.Printf("opscart-scan: could not list events for %s/%s: %v", issue.Namespace, issue.Name, err)
			continue
		}
		result[key] = hasProbeFailureSignature(messages)
	}
	return result
}

// hasProbeFailureSignature reports whether any event message shows a
// pod's liveness/startup probe repeatedly failing, followed by container
// restarts and a CrashLoopBackOff observation.
//
// Matching is deliberately a tolerant, case-insensitive substring check
// against the two phrasings Kubernetes actually emits (confirmed against
// a representative probe-failure event history: "Startup probe failed: HTTP probe
// failed with statuscode: 500" and "failed startup probe, will be
// restarted"), not a rigid exact-string match — kubelet's exact wording
// isn't a documented API contract, and a brittle match would silently
// stop working the moment it shifts slightly.
func hasProbeFailureSignature(messages []string) bool {
	for _, msg := range messages {
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "probe failed") || strings.Contains(lower, "probe, will be restarted") {
			return true
		}
	}
	return false
}

// criticalDebounceReasons are every Reason classifyPod can produce at
// CRITICAL severity — the crash-loop family whose underlying symptom (a
// crash loop, an OOM kill) is one Kubernetes' own exponential backoff can
// make transiently invisible to a single point-in-time scan. A
// crash-looping pod cycles crash -> wait -> briefly Running -> crash
// again, and a scan landing in that "briefly Running" window sees only
// a high restart count (HighRestartCount, MEDIUM) instead of the
// CrashLoopBackOff/OOMKilled that's still, moments later, exactly what
// it is.
var criticalDebounceReasons = []string{
	"CrashLoopBackOff (OOMKilled)",
	"CrashLoopBackOff (ProbeFailure)",
	"CrashLoopBackOff",
	"OOMKilled",
}

// applyCriticalDebounce is this task's fix: consult operational memory
// before letting a pod's severity drop below CRITICAL for this run's
// display. It mirrors what ResolveMissing's 3-consecutive-scan
// missing_scans counter already does for the dashboard (pkg/store/
// sqlite.go) — treat a single contradicting live snapshot as noise, not
// truth, when memory disagrees — applied here to the CLI's classification
// step instead of the store's resolve step.
//
// For every issue about to display below CRITICAL, it checks whether an
// active CRITICAL incident already exists in memory for the same pod
// under one of criticalDebounceReasons. If so, this run's display keeps
// the issue at CRITICAL under that reason, on the theory that a pod
// which was crash-looping moments ago and has no confirmed resolution
// yet is still crash-looping now, not fixed. A brand-new issue with no
// prior CRITICAL history is left exactly as classified — this never
// inflates a genuinely new, milder problem. A RESOLVED incident is never
// resurrected, since only Status == "active" qualifies.
//
// This affects only what THIS run prints/counts. persistFindings (see
// above) already wrote and resolved this scan's classified findings
// before this function runs, and the store's own ResolveMissing debounce
// remains the sole authority on when an incident is actually resolved —
// this function never touches that pipeline.
//
// In --stateless mode, this smoothing has no effect — a pod's severity
// can still flicker scan-to-scan, since there is no memory to defend the
// previous classification. This is an inherent tradeoff of choosing no
// persistence, not a bug. No explicit stateless check is needed: on
// NullStore, BatchGetIncidentHistory always returns an empty map (see
// pkg/store/null.go), so the lookup below naturally finds nothing and
// today's exact live-classification behavior falls through unchanged.
//
// classifyIssues (see above) already guarantees at most one issue per pod
// by the time issues reaches this function, so the alreadyCritical guard
// below only ever needs to skip a pod whose single issue is already
// CRITICAL under one of criticalDebounceReasons — it can no longer also
// be defending against a second, coexisting non-critical entry for that
// same pod.
func applyCriticalDebounce(db store.Store, clusterContext string, issues []enrichedIssue) []enrichedIssue {
	alreadyCritical := make(map[string]bool)
	var candidates []string
	for _, issue := range issues {
		if issue.Resource != "pod" {
			continue
		}
		if issue.Severity == "critical" {
			if isCriticalDebounceReason(issue.Reason) {
				alreadyCritical[podKey(issue.Namespace, issue.Name)] = true
			}
			continue
		}
		owner := store.OwnerNameFromPod(issue.Name)
		for _, reason := range criticalDebounceReasons {
			candidates = append(candidates, store.Fingerprint(issue.Namespace, "Workload", owner, canonicalIssueType(reason)))
		}
	}
	if len(candidates) == 0 {
		return issues
	}

	history, err := db.BatchGetIncidentHistory(clusterContext, candidates)
	if err != nil || len(history) == 0 {
		return issues
	}

	for i, issue := range issues {
		if issue.Resource != "pod" || issue.Severity == "critical" {
			continue
		}
		if alreadyCritical[podKey(issue.Namespace, issue.Name)] {
			continue
		}
		owner := store.OwnerNameFromPod(issue.Name)
		for _, reason := range criticalDebounceReasons {
			rec, ok := history[store.Fingerprint(issue.Namespace, "Workload", owner, canonicalIssueType(reason))]
			if !ok || rec == nil || rec.Status != "active" {
				continue
			}
			issues[i].Severity = "critical"
			issues[i].Reason = reason
			issues[i].Message = criticalDebounceMessage(reason)
			break
		}
	}
	return issues
}

// suppressSecondaryRestartNoise removes only the rendered HighRestartCount
// symptom when the same canonical workload has a stronger active incident.
func suppressSecondaryRestartNoise(db store.Store, clusterContext string, issues []enrichedIssue) []enrichedIssue {
	active := make(map[string]bool)
	for _, issue := range issues {
		t := canonicalIssueType(issue.Reason)
		if t == "crash_loop" || t == "probe_failure" || t == "oomkilled" || t == "image_pull_backoff" {
			active[issue.Namespace+"/"+store.OwnerNameFromPod(issue.Name)] = true
		}
	}
	var fingerprints []string
	fingerprintWorkload := make(map[string]string)
	for _, issue := range issues {
		if issue.Reason != "HighRestartCount" {
			continue
		}
		owner := store.OwnerNameFromPod(issue.Name)
		for _, typ := range []string{"crash_loop", "probe_failure", "oomkilled", "image_pull_backoff"} {
			fp := store.Fingerprint(issue.Namespace, "Workload", owner, typ)
			fingerprints = append(fingerprints, fp)
			fingerprintWorkload[fp] = issue.Namespace + "/" + owner
		}
	}
	if history, err := db.BatchGetIncidentHistory(clusterContext, fingerprints); err == nil {
		for fp, rec := range history {
			if rec != nil && rec.Status == "active" {
				active[fingerprintWorkload[fp]] = true
			}
		}
	}
	out := issues[:0]
	for _, issue := range issues {
		if issue.Reason == "HighRestartCount" && active[issue.Namespace+"/"+store.OwnerNameFromPod(issue.Name)] {
			continue
		}
		out = append(out, issue)
	}
	return out
}

// isCriticalDebounceReason reports whether reason is one of
// criticalDebounceReasons.
func isCriticalDebounceReason(reason string) bool {
	for _, r := range criticalDebounceReasons {
		if reason == r {
			return true
		}
	}
	return false
}

// criticalDebounceMessage produces the display message for a pod
// relabeled by applyCriticalDebounce — deliberately distinct from the
// scanner's own CrashLoopBackOff/OOMKilled messages (cluster.go), which
// name the specific container from a live probe this run didn't take, so
// as not to imply detail this scan didn't actually observe.
func criticalDebounceMessage(reason string) string {
	if reason == "OOMKilled" {
		return "Pod's container was OOMKilled and has not been confirmed resolved; this scan caught it between kills"
	}
	return "Pod is in an active CrashLoopBackOff cycle; this scan caught it mid-backoff, during its brief Running window"
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
	return store.Fingerprint(issue.Namespace, "Workload", store.OwnerNameFromPod(issue.Name), store.CanonicalIssueType(issue.Reason))
}

// canonicalIssueType remains as a package-local compatibility shim for the
// scanner's classification helpers; the shared store function is authoritative.
func canonicalIssueType(reason string) string {
	return store.CanonicalIssueType(reason)
}

// semanticIssueFamily normalizes only explicitly equivalent historical
// aliases. It intentionally has no fuzzy matching: unrelated issue classes
// must remain separate even when they share a namespace and workload.
func semanticIssueFamily(issueType string) string {
	switch issueType {
	case "crash_loop", "CrashLoopBackOff":
		return "crash_loop"
	case "probe_failure", "ProbeFailure", "CrashLoopBackOff (ProbeFailure)":
		return "probe_failure"
	case "oomkilled", "oom_killed", "OOMKilled", "CrashLoopBackOff (OOMKilled)":
		return "oomkilled"
	case "image_pull_backoff", "ImagePullBackOff":
		return "image_pull_backoff"
	case "high_restart_count", "HighRestartCount":
		return "high_restart_count"
	default:
		return issueType
	}
}

type memoryIdentity struct {
	namespace string
	owner     string
	family    string
}

func findingMemoryIdentity(issue models.EmergencyIssue) memoryIdentity {
	return memoryIdentity{
		namespace: issue.Namespace,
		owner:     store.OwnerNameFromPod(issue.Name),
		family:    semanticIssueFamily(store.CanonicalIssueType(issue.Reason)),
	}
}

func storedMemoryIdentity(incident store.IncidentSummary) memoryIdentity {
	return memoryIdentity{
		namespace: incident.Namespace,
		owner:     store.OwnerNameFromPod(incident.Resource),
		family:    semanticIssueFamily(incident.IssueType),
	}
}

func reconcileHistoricalFirstSeen(issues []models.EmergencyIssue, incidents []store.IncidentSummary) map[memoryIdentity]time.Time {
	wanted := make(map[memoryIdentity]bool, len(issues))
	for _, issue := range issues {
		wanted[findingMemoryIdentity(issue)] = true
	}
	earliest := make(map[memoryIdentity]time.Time)
	for _, incident := range incidents {
		identity := storedMemoryIdentity(incident)
		if !wanted[identity] || incident.FirstSeen.IsZero() {
			continue
		}
		if prior := earliest[identity]; prior.IsZero() || incident.FirstSeen.Before(prior) {
			earliest[identity] = incident.FirstSeen
		}
	}
	return earliest
}

func queryAllIncidentHistory(db store.Store, cluster string) ([]store.IncidentSummary, error) {
	const pageSize = 200
	var all []store.IncidentSummary
	for page := 1; ; page++ {
		items, total, err := db.QueryIncidents(store.IncidentFilter{Cluster: cluster, Page: page, PerPage: pageSize})
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(all) >= total || len(items) == 0 {
			return all, nil
		}
	}
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
			IssueType:    store.CanonicalIssueType(issue.Reason),
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
	allIncidents, aliasErr := queryAllIncidentHistory(db, clusterContext)
	aliases := map[memoryIdentity]time.Time{}
	if aliasErr == nil {
		aliases = reconcileHistoricalFirstSeen(issues, allIncidents)
	}
	ids := make([]int64, 0, len(history))
	for _, rec := range history {
		ids = append(ids, rec.ID)
	}
	reopenCounts, _ := db.BatchGetReopenCounts(ids)
	earliest, _ := store.GetEarliestRetainedObservation(db, clusterContext)

	for i, fingerprint := range fingerprints {
		rec, ok := history[fingerprint]
		if !ok || rec == nil {
			continue
		}
		enriched[i].FirstDetected = formatDuration(time.Since(rec.FirstSeen))
		enriched[i].ReopenCount = reopenCounts[rec.ID]
		enriched[i].firstSeenAt = rec.FirstSeen
		enriched[i].atHistoryBoundary = store.AtObservationBoundary(rec.FirstSeen, earliest)
		if legacyFirstSeen := aliases[findingMemoryIdentity(issues[i])]; !legacyFirstSeen.IsZero() && legacyFirstSeen.Before(rec.FirstSeen) {
			enriched[i].FirstDetected = formatDuration(time.Since(legacyFirstSeen))
			enriched[i].firstSeenAt = legacyFirstSeen
			enriched[i].atHistoryBoundary = store.AtObservationBoundary(legacyFirstSeen, earliest)
		}
	}
	return enriched
}

// printEmergencyIssuesEnriched mirrors scanner.PrintEmergencyIssues'
// grouping and box-drawing style exactly, with memory-context lines
// appended to each issue block when available.
//
// Before printing, HIGH and MEDIUM tiers are lightly grouped (see
// dedupeHighTier, groupIssues below) to collapse redundant entries that
// describe the same underlying workload problem across DIFFERENT pods.
// CRITICAL stays one-line-per-pod, unchanged, as it's the tier operators
// act on first and its detail shouldn't be compressed. Unlike HIGH/MEDIUM,
// CRITICAL needs no same-pod dedup here: classifyIssues (see above)
// already guarantees at most one issue per pod before issues ever reaches
// this function.
func printEmergencyIssuesEnriched(w io.Writer, issues []enrichedIssue) {
	issues = aggregateFailedJobPods(issues)
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

	high = dedupeHighTier(high)
	highGroups := groupIssues(high)
	mediumGroups := groupIssues(medium)

	fmt.Fprintln(w, "╔════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║             WAR ROOM - EMERGENCY ISSUES                    ║")
	fmt.Fprintln(w, "╚════════════════════════════════════════════════════════════╝")
	// These counts are printed-entry counts (a 3-pod group counts as 1),
	// not underlying pod counts — the header answers "how many things do
	// I need to read," and must match what a reader can literally count
	// below it. Underlying pod counts still show up inside a grouped
	// entry's own title/body (e.g. "3 pods cannot pull image from ...").
	fmt.Fprintf(w, "\n🔴 CRITICAL: %d    🟡 HIGH: %d    🟠 MEDIUM: %d\n\n", len(critical), len(highGroups), len(mediumGroups))

	if len(critical) > 0 {
		fmt.Fprintln(w, "🔴 CRITICAL ISSUES:")
		fmt.Fprintln(w, strings.Repeat("═", 80))
		for _, issue := range critical {
			printEnrichedIssue(w, issue)
		}
		fmt.Fprintln(w)
	}

	if len(highGroups) > 0 {
		fmt.Fprintln(w, "🟡 HIGH PRIORITY:")
		fmt.Fprintln(w, strings.Repeat("═", 80))
		printIssueGroups(w, highGroups)
		fmt.Fprintln(w)
	}

	if len(mediumGroups) > 0 {
		fmt.Fprintln(w, "🟠 MEDIUM PRIORITY:")
		fmt.Fprintln(w, strings.Repeat("═", 80))
		printIssueGroups(w, mediumGroups)
	}
}

// aggregateFailedJobPods turns explicitly owned failed pods into one review
// finding per Job/CronJob. Unowned failed pods remain independent findings.
func aggregateFailedJobPods(issues []enrichedIssue) []enrichedIssue {
	type ownerKey struct{ namespace, kind, name string }
	groups := make(map[ownerKey][]enrichedIssue)
	var order []ownerKey
	var out []enrichedIssue
	for _, issue := range issues {
		if issue.Reason != "PodFailed" || issue.OwnerName == "" || (issue.OwnerKind != "Job" && issue.OwnerKind != "CronJob") {
			out = append(out, issue)
			continue
		}
		key := ownerKey{issue.Namespace, issue.OwnerKind, issue.OwnerName}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], issue)
	}
	for _, key := range order {
		items := groups[key]
		executions := make(map[string]bool)
		recent := items[0]
		for _, item := range items {
			if item.OwnerExecution != "" {
				executions[item.OwnerExecution] = true
			}
			if item.FailureObservedAt.After(recent.FailureObservedAt) || (recent.FailureObservedAt.IsZero() && item.Age < recent.Age) {
				recent = item
			}
		}
		when := "unknown"
		if !recent.FailureObservedAt.IsZero() {
			when = recent.FailureObservedAt.Format(time.RFC3339)
		}
		out = append(out, enrichedIssue{EmergencyIssue: models.EmergencyIssue{
			Severity: "medium", Resource: strings.ToLower(key.kind), Namespace: key.namespace,
			Name: key.name, Reason: key.kind + "FailedExecutions",
			Message: fmt.Sprintf("Owner: %s/%s | Failed executions: %d | Failed pods: %d | Most recent failure: %s | Namespace: %s", key.kind, key.name, len(executions), len(items), when, key.namespace),
		}})
	}
	return out
}

// podKey identifies a pod for dedup purposes. EmergencyIssue has no
// separate container field (the container name only ever appears inside
// Message's free text), so namespace+pod name is the finest-grained key
// available; in practice every case this file's classification and
// dedup logic handle is a whole pod reported twice, not two different
// containers in it.
func podKey(namespace, name string) string {
	return namespace + "/" + name
}

// dedupeHighTier: a pod stuck Pending because its image can't be pulled
// shows up once as Pending (from analyzePodForIssues' pod-phase check)
// and once as ImagePullBackOff/ErrImagePull (from its per-container
// check) for the same root cause — these are two different reasons, so
// classifyPod's single-pod collapse (scoped to classifiablePodReasons)
// doesn't touch Pending at all. Keep only the more actionable
// ImagePullBackOff/ErrImagePull entry. A Pending pod with no matching
// image-pull entry is a distinct, real scenario and is left alone.
func dedupeHighTier(high []enrichedIssue) []enrichedIssue {
	imagePullKeys := make(map[string]bool, len(high))
	for _, issue := range high {
		if issue.Reason == "ImagePullBackOff" || issue.Reason == "ErrImagePull" {
			imagePullKeys[podKey(issue.Namespace, issue.Name)] = true
		}
	}

	var out []enrichedIssue
	for _, issue := range high {
		if issue.Reason == "Pending" && imagePullKeys[podKey(issue.Namespace, issue.Name)] {
			continue
		}
		out = append(out, issue)
	}
	return out
}

// issueGroup holds one or more issues collapsed under the same
// fingerprint (see groupKeyFor) within a single severity tier. len(issues)
// == 1 is the common case and prints exactly as before; 2+ prints as one
// grouped entry.
type issueGroup struct {
	issues []enrichedIssue
}

// groupKey is the fingerprint issues are grouped on within a tier.
// namespace is deliberately part of the key only for reasons where the
// task calls for it (HighRestartCount) — see groupKeyFor.
type groupKey struct {
	reason    string
	namespace string
	signature string
}

// groupIssues collapses same-tier, same-pod-resource issues that share a
// groupKeyFor fingerprint into one issueGroup, in first-seen order.
// Non-pod resources (e.g. PVCs) are never grouped — this is specifically
// about multiple pods sharing a root cause.
func groupIssues(tier []enrichedIssue) []issueGroup {
	indexOf := make(map[groupKey]int)
	var groups []issueGroup

	for _, issue := range tier {
		if issue.Resource != "pod" {
			groups = append(groups, issueGroup{issues: []enrichedIssue{issue}})
			continue
		}
		k := groupKeyFor(issue)
		if idx, ok := indexOf[k]; ok {
			groups[idx].issues = append(groups[idx].issues, issue)
			continue
		}
		indexOf[k] = len(groups)
		groups = append(groups, issueGroup{issues: []enrichedIssue{issue}})
	}
	return groups
}

// groupKeyFor computes a stable grouping fingerprint for an issue,
// replacing literal cause-text comparison (which breaks the moment two
// pods' messages differ by so much as an image tag or digest).
//
//   - ImagePullBackOff/ErrImagePull group by (reason, registry hostname)
//     — extractRegistryHost pulls the actual unreachable host out of the
//     message, so pods failing against the same registry group together
//     regardless of per-pod image tag/digest text. Namespace is
//     deliberately NOT part of this key: the same broken registry
//     commonly affects pods across several namespaces at once (that's
//     the real incident), and splitting the group by namespace would
//     recreate the exact noise this fix removes.
//   - HighRestartCount groups by (reason, namespace, container name) —
//     extractRestartContainer pulls the container name out of the
//     message. The restart count itself is intentionally excluded from
//     the key: it varies pod-to-pod even when the underlying pattern
//     (e.g. "node-exporter restarting a lot in monitoring") is
//     identical. Namespace IS part of this key, since "the same
//     container name restarting a lot" is only the same pattern when
//     it's the same workload/namespace — two unrelated namespaces
//     happening to run a container with the same name is a coincidence,
//     not one incident.
//   - Anything else falls back to the previous heuristic: the message
//     text after its first ": ", un-namespaced.
func groupKeyFor(issue enrichedIssue) groupKey {
	switch issue.Reason {
	case "ImagePullBackOff", "ErrImagePull":
		if host := extractRegistryHost(issue.Message); host != "" {
			return groupKey{reason: issue.Reason, signature: host}
		}
	case "HighRestartCount":
		if container := extractRestartContainer(issue.Message); container != "" {
			return groupKey{reason: issue.Reason, namespace: issue.Namespace, signature: container}
		}
	}
	return groupKey{reason: issue.Reason, signature: issueCause(issue.Message)}
}

// issueCause extracts the pod/container-independent part of an issue
// Message for grouping comparison, used as a fallback for reasons with
// no dedicated fingerprint extractor. Messages built by
// pkg/scanner/cluster.go follow "<action> for container <name>: <cause>"
// or "<action> container <name> ...: <cause>" — a single literal ": "
// separates the container-specific prefix from the shared underlying
// cause, so a simple split on the first occurrence is enough to line up
// different pods failing for the identical reason without a fragile
// full-string comparison.
func issueCause(message string) string {
	if idx := strings.Index(message, ": "); idx != -1 {
		if cause := strings.TrimSpace(message[idx+2:]); cause != "" {
			return cause
		}
	}
	return strings.TrimSpace(message)
}

var (
	// registryLookupRe matches a DNS-resolution failure embedded in an
	// ImagePullBackOff/ErrImagePull message, e.g. "...dial tcp: lookup
	// company-registry.internal on 192.168.65.254:53: no such host" —
	// the literal hostname the pod tried (and failed) to resolve.
	registryLookupRe = regexp.MustCompile(`lookup\s+(\S+)\s+on`)
	// registryImageRe matches a quoted image reference in a generic
	// pull-failure message, e.g. `Back-off pulling image
	// "company-registry.internal/foo:v1.2.3"` — the registry host is the
	// first path segment before the "/", ahead of the image name/tag/
	// digest, which is exactly the part that differs pod-to-pod.
	registryImageRe = regexp.MustCompile(`image\s+"([^"/]+)(?:/[^"]*)?"`)
	// restartContainerRe matches the container name out of a
	// HighRestartCount message, e.g. "Container node-exporter has
	// restarted 88 times".
	restartContainerRe = regexp.MustCompile(`^Container (\S+) has restarted \d+ times`)
	// crashLoopContainerRe matches the container name out of a
	// CrashLoopBackOff message, e.g. "Container stress is crash
	// looping: ...".
	crashLoopContainerRe = regexp.MustCompile(`^Container (\S+) is crash looping`)
)

// extractRegistryHost pulls the registry hostname out of an
// ImagePullBackOff/ErrImagePull message so pods can be grouped by the
// actual root cause (an unreachable registry) rather than by raw message
// text, which differs pod-to-pod (image tags/digests, container names).
// Two message shapes are tried in order; "" means neither matched, and
// the caller falls back to issueCause-based grouping. This is exactly
// the kind of extraction that breaks quietly if kubelet's wording ever
// shifts — keep it to these two well-known shapes rather than growing a
// general-purpose message parser.
func extractRegistryHost(message string) string {
	if m := registryLookupRe.FindStringSubmatch(message); len(m) == 2 {
		return m[1]
	}
	if m := registryImageRe.FindStringSubmatch(message); len(m) == 2 {
		return m[1]
	}
	return ""
}

// extractRestartContainer pulls the container name out of a
// HighRestartCount message (see cluster.go's "Container %s has restarted
// %d times" format). "" means the message didn't match the expected
// shape, and the caller falls back to issueCause-based grouping.
func extractRestartContainer(message string) string {
	if m := restartContainerRe.FindStringSubmatch(message); len(m) == 2 {
		return m[1]
	}
	return ""
}

// extractCrashLoopContainer pulls the container name out of a
// CrashLoopBackOff message (see cluster.go's "Container %s is crash
// looping: %s" format), for classifyPod's merged CrashLoopBackOff+
// OOMKilled/ProbeFailure message. Falls back to a generic noun if the
// message doesn't match the expected shape.
func extractCrashLoopContainer(message string) string {
	if m := crashLoopContainerRe.FindStringSubmatch(message); len(m) == 2 {
		return m[1]
	}
	return "container"
}

func printIssueGroups(w io.Writer, groups []issueGroup) {
	for _, g := range groups {
		if len(g.issues) == 1 {
			printEnrichedIssue(w, g.issues[0])
			continue
		}
		printGroupedIssue(w, g.issues)
	}
}

// groupTitle produces the human summary line for a collapsed group,
// e.g. "3 pods cannot pull image from company-registry.internal".
func groupTitle(key groupKey, count int) string {
	switch key.reason {
	case "ImagePullBackOff", "ErrImagePull":
		return fmt.Sprintf("%d pods cannot pull image from %s", count, key.signature)
	case "HighRestartCount":
		return fmt.Sprintf("%d pods restarting frequently (container %s)", count, key.signature)
	case "Pending":
		return fmt.Sprintf("%d pods stuck Pending", count)
	default:
		return fmt.Sprintf("%d pods failing with %s", count, key.reason)
	}
}

// groupDetailLine produces the grouped entry's third detail line. For
// HighRestartCount, the grouping key deliberately excludes the restart
// count (see groupKeyFor), so the range across the group's pods is
// surfaced here instead of hiding it entirely. Every other reason shows
// the shared cause text, taken from the representative issue.
func groupDetailLine(key groupKey, issues []enrichedIssue, rep enrichedIssue) string {
	if key.reason == "HighRestartCount" {
		min, max := issues[0].Restarts, issues[0].Restarts
		for _, issue := range issues[1:] {
			if issue.Restarts < min {
				min = issue.Restarts
			}
			if issue.Restarts > max {
				max = issue.Restarts
			}
		}
		if min == max {
			return fmt.Sprintf("Restarts: %d across %d pods", min, len(issues))
		}
		return fmt.Sprintf("Restarts: %d-%d across %d pods", min, max, len(issues))
	}
	return fmt.Sprintf("Cause: %s", issueCause(rep.Message))
}

// groupRepresentative picks which pod's enrichment (First detected/
// Reopened) is shown for a collapsed group: the one with the most
// reopens (the most historically significant instance of this problem),
// tie-broken by the oldest first-seen time, tie-broken by scan order.
func groupRepresentative(issues []enrichedIssue) enrichedIssue {
	rep := issues[0]
	for _, issue := range issues[1:] {
		if !issue.firstSeenAt.IsZero() &&
			(rep.firstSeenAt.IsZero() || issue.firstSeenAt.Before(rep.firstSeenAt)) {
			rep = issue
		}
	}
	return rep
}

func printGroupedIssue(w io.Writer, issues []enrichedIssue) {
	key := groupKeyFor(issues[0])
	rep := groupRepresentative(issues)

	namespaces := make([]string, 0, len(issues))
	seenNS := make(map[string]bool, len(issues))
	pods := make([]string, len(issues))
	for i, issue := range issues {
		pods[i] = issue.Name
		if !seenNS[issue.Namespace] {
			seenNS[issue.Namespace] = true
			namespaces = append(namespaces, issue.Namespace)
		}
	}

	fmt.Fprintf(w, "  %s\n", groupTitle(key, len(issues)))
	fmt.Fprintf(w, "  └─ Namespaces: %s\n", strings.Join(namespaces, ", "))
	fmt.Fprintf(w, "  └─ Pods: %s\n", strings.Join(pods, ", "))
	fmt.Fprintf(w, "  └─ %s\n", groupDetailLine(key, issues, rep))
	if rep.FirstDetected != "" {
		fmt.Fprintf(w, "  └─ First observed by this OMA: %s ago (%s)\n", rep.FirstDetected, rep.Name)
		if rep.atHistoryBoundary {
			fmt.Fprintln(w, "  └─ Present when this OMA history began; earlier duration is unknown.")
		}
	} else {
		fmt.Fprintln(w, "  └─ First observed by this OMA: —")
	}
	fmt.Fprintln(w)
}

func printEnrichedIssue(w io.Writer, issue enrichedIssue) {
	fmt.Fprintf(w, "  %s/%s\n", issue.Namespace, issue.Name)
	fmt.Fprintf(w, "  └─ Status: %s", issue.Reason)
	if issue.Restarts > 0 {
		fmt.Fprintf(w, " | Restarts: %d", issue.Restarts)
	}
	if issue.Age > 0 {
		label := "Pod Age"
		switch issue.Resource {
		case "job":
			label = "Job Age"
		case "cronjob":
			label = "CronJob Age"
		case "pvc":
			label = "PVC Age"
		}
		fmt.Fprintf(w, " | %s: %s", label, formatDuration(issue.Age))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  └─ %s\n", issue.Message)
	if issue.FirstDetected != "" {
		fmt.Fprintf(w, "  └─ First observed by this OMA: %s ago\n", issue.FirstDetected)
		if issue.atHistoryBoundary {
			fmt.Fprintln(w, "  └─ Present when this OMA history began; earlier duration is unknown.")
		}
	} else {
		fmt.Fprintln(w, "  └─ First observed by this OMA: —")
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
