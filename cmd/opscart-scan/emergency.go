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
	FirstDetected string // formatDuration(time since first seen); "" when no history
	ReopenCount   int
	firstSeenAt   time.Time // raw form of FirstDetected, used only to pick a group's representative
	probeFailure  bool      // true when a CrashLoopBackOff pod's recent events show a probe-failure signature (see annotateProbeFailures)
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

	enriched := enrichIssues(opStore, clusterContext, issues)
	enriched = annotateProbeFailures(clusterContext, enriched)
	printEmergencyIssuesEnriched(os.Stdout, enriched)
	return nil
}

// annotateProbeFailures checks each CrashLoopBackOff issue's recent pod
// events for a probe-failure signature (see hasProbeFailureSignature) and
// records the result on the issue for mergeCrashLoopOOM/printCriticalItem
// to act on at print time. Best-effort: a pod with no CrashLoopBackOff
// issues never triggers a clientset connection at all, and any lookup
// failure (can't connect, can't list events for one pod) just leaves that
// issue's classification as plain CrashLoopBackOff — exactly like today.
func annotateProbeFailures(clusterContext string, issues []enrichedIssue) []enrichedIssue {
	hasCrashLoop := false
	for _, issue := range issues {
		if issue.Reason == "CrashLoopBackOff" {
			hasCrashLoop = true
			break
		}
	}
	if !hasCrashLoop {
		return issues
	}

	clientset, err := getKubernetesClient(clusterContext)
	if err != nil {
		log.Printf("opscart-scan: could not check probe-failure events: %v", err)
		return issues
	}

	for i, issue := range issues {
		if issue.Reason != "CrashLoopBackOff" {
			continue
		}
		messages, err := recentPodEventMessages(clientset, issue.Namespace, issue.Name)
		if err != nil {
			log.Printf("opscart-scan: could not list events for %s/%s: %v", issue.Namespace, issue.Name, err)
			continue
		}
		issues[i].probeFailure = hasProbeFailureSignature(messages)
	}
	return issues
}

// hasProbeFailureSignature reports whether any event message shows a
// pod's liveness/startup probe repeatedly failing — the well-understood
// Kubernetes pattern where a struggling-but-viable container gets killed
// by its own probe before it can stabilize, and eventually degrades into
// a plain CrashLoopBackOff with no other visible cause.
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
		enriched[i].firstSeenAt = rec.FirstSeen
	}
	return enriched
}

// printEmergencyIssuesEnriched mirrors scanner.PrintEmergencyIssues'
// grouping and box-drawing style exactly, with memory-context lines
// appended to each issue block when available.
//
// Before printing, HIGH and MEDIUM tiers are deduplicated and lightly
// grouped (see dedupeHighTier, dedupeMediumTier, groupIssues below) to
// collapse redundant entries that describe the same underlying workload
// problem. CRITICAL stays one-line-per-pod, unchanged, as it's the tier
// operators act on first and its detail shouldn't be compressed.
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

	medium = dedupeMediumTier(medium, critical)
	high = dedupeHighTier(high)
	criticalItems := mergeCrashLoopOOM(critical)
	highGroups := groupIssues(high)
	mediumGroups := groupIssues(medium)

	fmt.Fprintln(w, "╔════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║             WAR ROOM - EMERGENCY ISSUES                    ║")
	fmt.Fprintln(w, "╚════════════════════════════════════════════════════════════╝")
	// These counts are printed-entry counts (a 3-pod group or a merged
	// pair counts as 1), not underlying pod counts — the header answers
	// "how many things do I need to read," and must match what a reader
	// can literally count below it. Underlying pod counts still show up
	// inside a grouped entry's own title/body (e.g. "3 pods cannot pull
	// image from ...").
	fmt.Fprintf(w, "\n🔴 CRITICAL: %d    🟡 HIGH: %d    🟠 MEDIUM: %d\n\n", len(criticalItems), len(highGroups), len(mediumGroups))

	if len(criticalItems) > 0 {
		fmt.Fprintln(w, "🔴 CRITICAL ISSUES:")
		fmt.Fprintln(w, strings.Repeat("═", 80))
		for _, item := range criticalItems {
			printCriticalItem(w, item)
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

// podKey identifies a pod for dedup purposes. EmergencyIssue has no
// separate container field (the container name only ever appears inside
// Message's free text), so namespace+pod name is the finest-grained key
// available; in practice every duplicate case this file's Fix 1/2 handle
// is a whole pod reported twice, not two different containers in it.
func podKey(namespace, name string) string {
	return namespace + "/" + name
}

// dedupeMediumTier implements Fix 1: a pod already shown under CRITICAL
// as CrashLoopBackOff or OOMKilled has its restart count printed there
// already, so a MEDIUM HighRestartCount entry for the same pod is pure
// noise. A pod that reaches MEDIUM on its own (no CRITICAL entry) keeps
// its HighRestartCount line.
func dedupeMediumTier(medium, critical []enrichedIssue) []enrichedIssue {
	criticalKeys := make(map[string]bool, len(critical))
	for _, issue := range critical {
		if issue.Reason == "CrashLoopBackOff" || issue.Reason == "OOMKilled" {
			criticalKeys[podKey(issue.Namespace, issue.Name)] = true
		}
	}

	var out []enrichedIssue
	for _, issue := range medium {
		if issue.Reason == "HighRestartCount" && criticalKeys[podKey(issue.Namespace, issue.Name)] {
			continue
		}
		out = append(out, issue)
	}
	return out
}

// dedupeHighTier implements Fix 2: a pod stuck Pending because its image
// can't be pulled shows up once as Pending and once as ImagePullBackOff/
// ErrImagePull for the same root cause. Keep only the more actionable
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
// fingerprint (see groupKeyFor) within a single severity tier.
// len(issues) == 1 is the common case and prints exactly as before; 2+
// prints as one grouped entry (Fix 3 of the prior task / this task's
// Fix 1).
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
//     text after its first ": ", un-namespaced, same as before this
//     task's fingerprinting was added.
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
// looping: %s" format), for the merged CrashLoopBackOff+OOMKilled
// message (Fix 3). Falls back to a generic noun if the message doesn't
// match the expected shape.
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
		switch {
		case issue.ReopenCount > rep.ReopenCount:
			rep = issue
		case issue.ReopenCount == rep.ReopenCount &&
			!issue.firstSeenAt.IsZero() &&
			(rep.firstSeenAt.IsZero() || issue.firstSeenAt.Before(rep.firstSeenAt)):
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
		fmt.Fprintf(w, "  └─ First detected: %s ago (%s)\n", rep.FirstDetected, rep.Name)
	}
	if rep.ReopenCount > 0 {
		fmt.Fprintf(w, "  └─ Reopened: %dx (%s)\n", rep.ReopenCount, rep.Name)
	}
	fmt.Fprintln(w)
}

// criticalItem is a printable CRITICAL-tier entry: either a single issue
// (the common case, unchanged from before), a CrashLoopBackOff merged
// with the same-pod OOMKilled that caused it (Fix 3 of the prior task),
// or a CrashLoopBackOff relabeled with a probe-failure root cause found
// in its recent events (this task's fix).
type criticalItem struct {
	issue        enrichedIssue
	oomCause     *enrichedIssue // non-nil only when issue is a merged CrashLoopBackOff+OOMKilled
	probeFailure bool           // true only when issue is CrashLoopBackOff, has no OOMKilled cause, and its events show a probe-failure signature
}

// mergeCrashLoopOOM implements Fix 3 of the prior task (CrashLoopBackOff+
// OOMKilled merge) and this task's probe-failure relabeling, narrowly
// scoped in the same way: when the SAME pod (see podKey's container-
// granularity caveat) appears as both CrashLoopBackOff and OOMKilled at
// CRITICAL, the OOMKilled is the cause of the crash loop, not a second,
// independent problem — merge the pair into one entry. When a
// CrashLoopBackOff pod has no OOMKilled cause but annotateProbeFailures
// found a probe-failure signature in its recent events, relabel it as
// caused by its probe instead. Every other CRITICAL issue (including a
// pod that's ONLY CrashLoopBackOff with neither signal, or ONLY
// OOMKilled with no crash loop) is left exactly as-is, one line per pod,
// per the existing CRITICAL format.
//
// Precedence when a pod has BOTH an OOMKilled cause and a probe-failure
// event signature: OOMKilled wins, and the probe signal is dropped
// silently. An observed OOM is a direct, first-hand cause; a
// probe-failure event only correlates with the crash loop (the pod could
// still be crashing from the same memory pressure that trips its probe).
// Showing both would just be noise once we already have the more certain
// cause.
//
// This is intentionally narrow: it does not attempt to detect or merge
// any other status pair. PodFailed following an OOMKilled, for instance,
// looks like it could be a similarly causal pair, but there's no
// evidence yet that it needs the same treatment — flagged in the task
// summary as a possible follow-up rather than guessed at here.
func mergeCrashLoopOOM(critical []enrichedIssue) []criticalItem {
	oomIndexByPod := make(map[string]int, len(critical))
	for i, issue := range critical {
		if issue.Reason == "OOMKilled" {
			oomIndexByPod[podKey(issue.Namespace, issue.Name)] = i
		}
	}

	skip := make(map[int]bool, len(critical))
	var out []criticalItem
	for i, issue := range critical {
		if skip[i] {
			continue
		}
		if issue.Reason == "CrashLoopBackOff" {
			if oomIdx, ok := oomIndexByPod[podKey(issue.Namespace, issue.Name)]; ok {
				oom := critical[oomIdx]
				out = append(out, criticalItem{issue: issue, oomCause: &oom})
				skip[oomIdx] = true
				continue
			}
			if issue.probeFailure {
				out = append(out, criticalItem{issue: issue, probeFailure: true})
				continue
			}
		}
		out = append(out, criticalItem{issue: issue})
	}
	return out
}

func printCriticalItem(w io.Writer, item criticalItem) {
	switch {
	case item.oomCause != nil:
		rep := groupRepresentative([]enrichedIssue{item.issue, *item.oomCause})
		container := extractCrashLoopContainer(item.issue.Message)
		printMergedCriticalItem(w, item.issue, rep, "OOMKilled",
			fmt.Sprintf("Container %s is being OOM-killed, causing the crash loop — check resources.limits.memory", container))
	case item.probeFailure:
		container := extractCrashLoopContainer(item.issue.Message)
		printMergedCriticalItem(w, item.issue, item.issue, "ProbeFailure",
			fmt.Sprintf("Container %s is being killed by its startup/liveness probe before it can stabilize — check probe initialDelaySeconds/periodSeconds/failureThreshold against actual container startup time", container))
	default:
		printEnrichedIssue(w, item.issue)
	}
}

// printMergedCriticalItem prints a CrashLoopBackOff issue relabeled with
// a merged root cause (OOMKilled or ProbeFailure). rep supplies the
// First detected/Reopened enrichment shown — for an OOMKilled merge
// that's groupRepresentative's pick between the two constituent issues;
// for a probe-failure relabel there's only ever the one issue, so rep is
// always item.issue itself.
func printMergedCriticalItem(w io.Writer, issue, rep enrichedIssue, causeLabel, causeMessage string) {
	fmt.Fprintf(w, "  %s/%s\n", issue.Namespace, issue.Name)
	fmt.Fprintf(w, "  └─ Status: CrashLoopBackOff (%s)", causeLabel)
	if issue.Restarts > 0 {
		fmt.Fprintf(w, " | Restarts: %d", issue.Restarts)
	}
	fmt.Fprintf(w, " | Age: %s\n", formatDuration(issue.Age))
	fmt.Fprintf(w, "  └─ %s\n", causeMessage)
	if rep.FirstDetected != "" {
		fmt.Fprintf(w, "  └─ First detected: %s ago\n", rep.FirstDetected)
	}
	if rep.ReopenCount > 0 {
		fmt.Fprintf(w, "  └─ Reopened: %dx\n", rep.ReopenCount)
	}
	fmt.Fprintln(w)
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
