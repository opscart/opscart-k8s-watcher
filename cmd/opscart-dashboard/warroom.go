package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/analyzer"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// handleWarRoom returns up to 5 critical issues: crash-looping pods, unexpected
// privileged containers, and high-risk unprotected namespaces.
//
//	GET /api/warroom[?cluster=<ctx>]
//	[{"severity":"critical","type":"crash_loop","namespace":"...","resource":"...","message":"..."}]
func (srv *server) handleWarRoom(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	issues := collectWarRoomIssues(scan, 5)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(issues)
}

// ── War Room helpers ──────────────────────────────────────────────────────────

type warRoomIssue struct {
	Severity       string `json:"severity"`
	Type           string `json:"type"`
	Namespace      string `json:"namespace"`
	Resource       string `json:"resource"`
	Message        string `json:"message"`
	AgeDays        int    `json:"age_days,omitempty"`
	KubectlCmd     string `json:"kubectl_cmd,omitempty"`
	RestartCount   int    `json:"restart_count,omitempty"`
	Classification string `json:"classification,omitempty"`
	Container      string `json:"container,omitempty"`
	WorkloadKind   string `json:"workload_kind,omitempty"`
	WorkloadName   string `json:"workload_name,omitempty"`
	Rank           int    `json:"rank,omitempty"`
}

type warRoomPageData struct {
	ClusterName        string
	ActiveCtx          string
	StatusDesc         string
	DashURL            string
	Critical           []warRoomIssue
	Warnings           []warRoomIssue
	ScannedAtMs        int64
	Sidebar            template.HTML
	TotalIssues        int
	IncidentsHref      string
	ResetURL           string
	Query              string
	Severity           string
	Type               string
	Limit              int
	FilterActive       bool
	Classifications    []warRoomFilterOption
	ShowingStatus      string
	Briefing           string
	ActiveCritical     int
	AffectedWorkloads  int
	OldestActive       int
	HasOldestActive    bool
	HighestRestarts    int
	HasHighestRestarts bool
	NamespaceFindings  int
}

type warRoomFilterOption struct {
	Value string
	Label string
}

type warRoomStats struct {
	activeCritical, affectedWorkloads, oldest, highestRestarts, namespaceFindings int
	hasOldest, hasHighestRestarts                                                 bool
}

func collectWarRoomIssues(scan *clusterScan, limit int) []warRoomIssue {
	var issues []warRoomIssue

	// 1. Crash-looping / zombie pods
	if scan.wasteAudit != nil {
		for _, pod := range scan.wasteAudit.StalePods {
			if pod.Kind == analyzer.StalePodZombie {
				itype := zombieTypeForStatus(pod.Status)
				issues = append(issues, warRoomIssue{
					Severity:       "critical",
					Type:           itype,
					Namespace:      pod.Namespace,
					Resource:       pod.Name,
					Message:        fmt.Sprintf("%s — %d restarts, %d days old", pod.Status, pod.RestartCount, pod.AgeDays),
					AgeDays:        pod.AgeDays,
					KubectlCmd:     kubectlCmdForIssue(itype, pod.Name, pod.Namespace),
					RestartCount:   int(pod.RestartCount),
					Classification: classificationForIssue(itype),
				})
			}
		}
	}

	// 2. Unexpected privileged containers (non-system or unexpected system ones)
	if scan.secAudit != nil {
		for _, issue := range scan.secAudit.Issues {
			if issue.Type == "privileged_container" && issue.Severity == "critical" {
				podName, containerName := issue.Name, ""
				if parts := strings.SplitN(issue.Name, "/", 2); len(parts) == 2 {
					podName, containerName = parts[0], parts[1]
				}
				issues = append(issues, warRoomIssue{
					Severity:       "critical",
					Type:           "privileged_container",
					Namespace:      issue.Namespace,
					Resource:       podName,
					Message:        issue.Description,
					KubectlCmd:     fmt.Sprintf("kubectl describe pod %s -n %s", podName, issue.Namespace),
					Classification: classificationForIssue("privileged_container"),
					Container:      containerName,
				})
			}
		}
	}

	// 3. High-risk unprotected namespaces
	if scan.netAudit != nil {
		for _, ns := range scan.netAudit.UnprotectedNamespaces {
			if ns.RiskLevel == "HIGH" {
				issues = append(issues, warRoomIssue{
					Severity:       "high",
					Type:           "unprotected_namespace",
					Namespace:      ns.Name,
					Resource:       "namespace",
					Message:        fmt.Sprintf("%d pods in namespace", ns.PodCount),
					KubectlCmd:     fmt.Sprintf("kubectl get networkpolicies -n %s", ns.Name),
					Classification: "Missing default-deny NetworkPolicy",
				})
			}
		}
	}
	// 4. Idle/abandoned namespaces are posture findings, not workloads.
	if scan.wasteAudit != nil {
		for _, ns := range scan.wasteAudit.AbandonedNamespaces {
			issues = append(issues, warRoomIssue{
				Severity: "high", Type: "idle_namespace", Namespace: ns.Name,
				Resource: "namespace", Message: ns.Reason, AgeDays: ns.AgeDays,
				KubectlCmd:     fmt.Sprintf("kubectl get pods -n %s", ns.Name),
				Classification: "Idle namespace",
			})
		}
	}
	// Sort: critical first, then by restart count descending.
	severityOrder := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}
	sort.SliceStable(issues, func(i, j int) bool {
		si := severityOrder[issues[i].Severity]
		sj := severityOrder[issues[j].Severity]
		if si != sj {
			return si < sj
		}
		// Within same severity: higher restart count = more urgent
		return issues[i].RestartCount > issues[j].RestartCount
	})

	if limit > 0 && len(issues) > limit {
		return issues[:limit]
	}
	return issues
}

// ── War Room page ─────────────────────────────────────────────────────────────

func (srv *server) handleWarRoomPage(w http.ResponseWriter, r *http.Request) {
	if canonical, changed := canonicalWarRoomQuery(r.URL.Query()); changed {
		target := "/warroom"
		if encoded := canonical.Encode(); encoded != "" {
			target += "?" + encoded
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	ctx := srv.activeCtx(r)
	state := srv.getState(ctx)

	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	if scan == nil {
		if err := state.refresh(srv.clusterList); err != nil {
			http.Error(w, "scan failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		state.mu.RLock()
		scan = state.scan
		state.mu.RUnlock()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, renderWarRoomPageWithFilters(scan, ctx, srv.clusterList, r.URL.Query()))
}

func renderWarRoomPage(scan *clusterScan, activeCtx string, clusterList []string) string {
	return renderWarRoomPageWithFilters(scan, activeCtx, clusterList, nil)
}

func renderWarRoomPageWithFilters(scan *clusterScan, activeCtx string, clusterList []string, query url.Values) string {
	allIssues := collectWarRoomIssues(scan, 0)
	for i := range allIssues {
		enrichWarRoomIdentity(&allIssues[i], scan)
	}
	stats := warRoomStatsFor(allIssues)
	qText := strings.TrimSpace(query.Get("q"))
	severity := strings.ToLower(strings.TrimSpace(query.Get("severity")))
	issueType := strings.TrimSpace(query.Get("type"))
	limit := parseWarRoomLimit(query.Get("limit"))
	filtered := filterWarRoomIssues(allIssues, qText, severity, issueType)
	var incidents, warnings []warRoomIssue
	for _, issue := range filtered {
		if isNamespaceFinding(issue) {
			warnings = append(warnings, issue)
		} else {
			incidents = append(incidents, issue)
		}
	}
	if len(incidents) > limit {
		incidents = incidents[:limit]
	}
	for i := range incidents {
		incidents[i].Rank = i + 1
	}

	scannedAt := time.Now()
	clusterName := displayName(activeCtx)
	if scan != nil && scan.report != nil {
		scannedAt = scan.report.Timestamp
		clusterName = scan.report.ClusterName
	}

	clusterQ := ""
	if activeCtx != "" {
		clusterQ = "?cluster=" + url.QueryEscape(activeCtx)
	}
	filterActive := qText != "" || severity != "" || issueType != "" || limit != 12
	classifications := uniqueClassifications(allIssues)
	status := fmt.Sprintf("Showing %d prioritized incident%s", len(incidents), plural(len(incidents)))
	if len(warnings) > 0 {
		status += fmt.Sprintf(" · %d namespace finding%s", len(warnings), plural(len(warnings)))
	}
	briefing := buildWarRoomBriefing(stats)

	data := warRoomPageData{
		ClusterName:   clusterName,
		ActiveCtx:     activeCtx,
		StatusDesc:    fmt.Sprintf("%d issue(s) detected", len(allIssues)),
		DashURL:       "/" + clusterQ,
		Critical:      incidents,
		Warnings:      warnings,
		ScannedAtMs:   scannedAt.UnixMilli(),
		Sidebar:       template.HTML(buildSidebar("warroom", activeCtx, clusterName, clusterList, countCriticalIssues(scan))),
		TotalIssues:   len(allIssues),
		IncidentsHref: withWarRoomQuery("/incidents", activeCtx, qText, severity, issueType, limit),
		ResetURL:      "/warroom" + clusterQ,
		Query:         qText, Severity: severity, Type: issueType, Limit: limit,
		FilterActive: filterActive, Classifications: classifications,
		ShowingStatus: status, Briefing: briefing,
		ActiveCritical: stats.activeCritical, AffectedWorkloads: stats.affectedWorkloads,
		OldestActive: stats.oldest, HasOldestActive: stats.hasOldest,
		HighestRestarts: stats.highestRestarts, HasHighestRestarts: stats.hasHighestRestarts,
		NamespaceFindings: stats.namespaceFindings,
	}

	var buf strings.Builder
	if err := getWarRoomTmpl().Execute(&buf, data); err != nil {
		log.Printf("warroom template: %v", err)
		return ""
	}
	return buf.String()
}

func canonicalWarRoomQuery(query url.Values) (url.Values, bool) {
	canonical := make(url.Values, len(query))
	for key, values := range query {
		canonical[key] = append([]string(nil), values...)
	}
	changed := false
	for _, key := range []string{"q", "severity", "type"} {
		if values, exists := canonical[key]; exists && strings.TrimSpace(firstValue(values)) == "" {
			canonical.Del(key)
			changed = true
		}
	}
	if values, exists := canonical["limit"]; exists {
		raw := firstValue(values)
		if raw == "12" || (raw != "25" && raw != "50") {
			canonical.Del("limit")
			changed = true
		}
	}
	return canonical, changed
}

func firstValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func parseWarRoomLimit(raw string) int {
	switch raw {
	case "25":
		return 25
	case "50":
		return 50
	default:
		return 12
	}
}

func isNamespaceFinding(issue warRoomIssue) bool {
	return issue.Type == "unprotected_namespace" || issue.Type == "idle_namespace"
}

func classificationForIssue(issueType string) string {
	switch issueType {
	case "crash_loop":
		return "CrashLoopBackOff"
	case "oomkilled":
		return "OOMKilled"
	case "image_pull_backoff":
		return "ImagePullBackOff"
	case "probe_failure":
		return "Probe failure"
	case "privileged_container":
		return "Privileged container"
	case "unprotected_namespace":
		return "Missing default-deny NetworkPolicy"
	case "idle_namespace":
		return "Idle namespace"
	default:
		return issueType
	}
}

func enrichWarRoomIdentity(issue *warRoomIssue, scan *clusterScan) {
	if issue.Classification == "" {
		issue.Classification = classificationForIssue(issue.Type)
	}
	if isNamespaceFinding(*issue) {
		issue.WorkloadKind, issue.WorkloadName = "Namespace", issue.Namespace
		return
	}
	owner := store.OwnerNameFromPod(issue.Resource)
	issue.WorkloadKind, issue.WorkloadName = "Pod", issue.Resource
	if scan == nil {
		if owner != issue.Resource {
			issue.WorkloadKind, issue.WorkloadName = "Deployment", owner
		}
		return
	}
	for _, workload := range scan.AllWorkloads {
		if workload.Namespace != issue.Namespace {
			continue
		}
		if workload.Name == owner {
			issue.WorkloadKind, issue.WorkloadName = workload.Kind, owner
			return
		}
		if strings.HasPrefix(issue.Resource, workload.Name+"-") {
			switch workload.Kind {
			case "StatefulSet":
				// OwnerNameFromPod deliberately retains StatefulSet ordinals.
				issue.WorkloadKind, issue.WorkloadName = workload.Kind, owner
			case "DaemonSet":
				issue.WorkloadKind, issue.WorkloadName = workload.Kind, workload.Name
			}
			return
		}
	}
	if owner != issue.Resource {
		issue.WorkloadKind, issue.WorkloadName = "Deployment", owner
	}
}

func filterWarRoomIssues(issues []warRoomIssue, query, severity, issueType string) []warRoomIssue {
	needle := strings.ToLower(query)
	var filtered []warRoomIssue
	for _, issue := range issues {
		if severity != "" && issue.Severity != severity {
			continue
		}
		if issueType != "" && issue.Type != issueType {
			continue
		}
		if needle != "" {
			haystack := strings.ToLower(strings.Join([]string{
				issue.WorkloadKind, issue.WorkloadName, issue.Resource, issue.Namespace,
				issue.Classification, issue.Container,
			}, " "))
			if !strings.Contains(haystack, needle) {
				continue
			}
		}
		filtered = append(filtered, issue)
	}
	return filtered
}

func uniqueClassifications(issues []warRoomIssue) []warRoomFilterOption {
	labels := map[string]string{}
	for _, issue := range issues {
		labels[issue.Type] = issue.Classification
	}
	var options []warRoomFilterOption
	for value, label := range labels {
		options = append(options, warRoomFilterOption{Value: value, Label: label})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Label < options[j].Label })
	return options
}

func warRoomStatsFor(issues []warRoomIssue) warRoomStats {
	stats := warRoomStats{}
	workloads := map[string]bool{}
	for _, issue := range issues {
		if issue.AgeDays > 0 && (!stats.hasOldest || issue.AgeDays > stats.oldest) {
			stats.oldest, stats.hasOldest = issue.AgeDays, true
		}
		if isNamespaceFinding(issue) {
			stats.namespaceFindings++
			continue
		}
		if issue.Severity == "critical" {
			stats.activeCritical++
		}
		if issue.WorkloadName != "" {
			workloads[issue.Namespace+"\x00"+issue.WorkloadKind+"\x00"+issue.WorkloadName] = true
		}
		if issue.RestartCount > 0 && (!stats.hasHighestRestarts || issue.RestartCount > stats.highestRestarts) {
			stats.highestRestarts, stats.hasHighestRestarts = issue.RestartCount, true
		}
	}
	stats.affectedWorkloads = len(workloads)
	return stats
}

func buildWarRoomBriefing(stats warRoomStats) string {
	briefing := fmt.Sprintf("%d critical incident%s require attention across %d workload%s.",
		stats.activeCritical, plural(stats.activeCritical), stats.affectedWorkloads, plural(stats.affectedWorkloads))
	var facts []string
	if stats.hasOldest {
		facts = append(facts, fmt.Sprintf("the oldest active incident is %d day%s old", stats.oldest, plural(stats.oldest)))
	}
	if stats.hasHighestRestarts {
		facts = append(facts, fmt.Sprintf("the highest observed restart count is %s", formatCount(stats.highestRestarts)))
	}
	if len(facts) > 0 {
		briefing += " " + strings.ToUpper(facts[0][:1]) + facts[0][1:]
		if len(facts) > 1 {
			briefing += "; " + facts[1]
		}
		briefing += "."
	}
	return briefing
}

func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func formatCount(count int) string {
	s := fmt.Sprintf("%d", count)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func withWarRoomQuery(path, activeCtx, query, severity, issueType string, limit int) string {
	values := url.Values{}
	if activeCtx != "" {
		values.Set("cluster", activeCtx)
	}
	if query != "" {
		values.Set("q", query)
	}
	if severity != "" {
		values.Set("severity", severity)
	}
	if issueType != "" {
		values.Set("type", issueType)
	}
	values.Set("limit", fmt.Sprintf("%d", limit))
	return path + "?" + values.Encode()
}

var warRoomTmplOnce sync.Once
var warRoomTmpl *template.Template

func getWarRoomTmpl() *template.Template {
	warRoomTmplOnce.Do(func() {
		warRoomTmpl = template.Must(
			template.New("warroom.html").Funcs(template.FuncMap{
				"renderCard": func(issue warRoomIssue, activeCtx string) template.HTML {
					return template.HTML(renderWarRoomCard(issue, activeCtx))
				},
			}).ParseFS(templateFS, "templates/base.html", "templates/warroom.html"),
		)
	})
	return warRoomTmpl
}

func renderWarRoomCard(issue warRoomIssue, activeCtx string) string {
	if issue.Severity == "" {
		issue.Severity = "critical"
	}
	if issue.Classification == "" {
		issue.Classification = classificationForIssue(issue.Type)
	}
	sc := "c"
	bl := strings.ToUpper(issue.Severity)
	if issue.Severity != "critical" {
		sc = "w"
	}

	cardClass := ""
	if issue.Rank == 1 {
		cardClass = " featured"
	}
	if isNamespaceFinding(issue) {
		cardClass += " posture-card"
	}
	identity := issue.WorkloadKind + "/" + issue.WorkloadName
	if isNamespaceFinding(issue) {
		identity = "Namespace/" + issue.Namespace
	} else if issue.WorkloadKind == "" {
		owner := store.OwnerNameFromPod(issue.Resource)
		if owner != issue.Resource {
			identity = "Deployment/" + owner
		} else {
			identity = "Pod/" + issue.Resource
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`<article class="wr-card %s %s%s">`, sc, "wr-type-"+strings.ReplaceAll(issue.Type, "_", "-"), cardClass))
	sb.WriteString(`<div class="wr-top">`)
	if issue.Rank > 0 {
		sb.WriteString(fmt.Sprintf(`<span class="rank-badge">#%d</span>`, issue.Rank))
	}
	sb.WriteString(fmt.Sprintf(`<span class="badge %s">%s</span></div>`, sc, template.HTMLEscapeString(bl)))
	sb.WriteString(fmt.Sprintf(`<div class="wr-name" title="%s">%s</div>`, template.HTMLEscapeString(identity), template.HTMLEscapeString(identity)))
	if !isNamespaceFinding(issue) {
		sb.WriteString(fmt.Sprintf(`<div class="wr-focus" title="Focus Pod: %s">Focus Pod: %s</div>`, template.HTMLEscapeString(issue.Resource), template.HTMLEscapeString(issue.Resource)))
		if issue.Container != "" {
			sb.WriteString(fmt.Sprintf(`<div class="wr-focus" title="Container: %s">Container: %s</div>`, template.HTMLEscapeString(issue.Container), template.HTMLEscapeString(issue.Container)))
		}
	}
	sb.WriteString(fmt.Sprintf(`<div class="wr-ns" title="Namespace: %s">Namespace: %s</div>`, template.HTMLEscapeString(issue.Namespace), template.HTMLEscapeString(issue.Namespace)))
	activeFor, restarts := "—", "—"
	if issue.AgeDays > 0 {
		activeFor = fmt.Sprintf("%dd", issue.AgeDays)
	}
	if issue.RestartCount > 0 {
		restarts = formatCount(issue.RestartCount)
	}
	sb.WriteString(`<div class="wr-evidence">`)
	sb.WriteString(fmt.Sprintf(`<div><span>Classification</span><strong title="%s">%s</strong></div>`, template.HTMLEscapeString(issue.Classification), template.HTMLEscapeString(issue.Classification)))
	sb.WriteString(fmt.Sprintf(`<div><span>Active For</span><strong>%s</strong></div>`, activeFor))
	sb.WriteString(fmt.Sprintf(`<div><span>Restarts</span><strong>%s</strong></div>`, restarts))
	sb.WriteString(`</div>`)
	if isNamespaceFinding(issue) && issue.Message != "" {
		sb.WriteString(fmt.Sprintf(`<div class="wr-reason">%s</div>`, template.HTMLEscapeString(issue.Message)))
	}
	sb.WriteString(`<footer class="wr-actions">`)
	sb.WriteString(`<div class="wr-cmd">`)
	sb.WriteString(fmt.Sprintf(`<code title="%s">%s</code>`, template.HTMLEscapeString(issue.KubectlCmd), template.HTMLEscapeString(issue.KubectlCmd)))
	sb.WriteString(`<button class="copy-btn" onclick="var b=this,c=this.previousElementSibling.textContent;navigator.clipboard.writeText(c).then(function(){b.textContent='Copied';setTimeout(function(){b.textContent='Copy'},1500)})">Copy</button>`)
	sb.WriteString(`</div>`)
	// Investigate button — links to investigation page
	if investigationHref := investigateURL(issue.Namespace, issue.Resource, issue.Type, activeCtx); investigationHref != "/warroom" {
		sb.WriteString(fmt.Sprintf(
			`<a class="investigate-btn" href="%s">Investigate →</a>`,
			investigationHref))
	}
	sb.WriteString(`</footer></article>`)
	return sb.String()
}

// ── War Room page helpers ──────────────────────────────────────────────────────

func zombieTypeForStatus(status string) string {
	switch status {
	case "OOMKilled":
		return "oomkilled"
	case "ImagePullBackOff":
		return "image_pull_backoff"
	case "ProbeFailure":
		return "probe_failure"
	default:
		return "crash_loop"
	}
}

func kubectlCmdForIssue(issueType, resource, namespace string) string {
	switch issueType {
	case "crash_loop":
		return fmt.Sprintf("kubectl logs %s -n %s --previous", resource, namespace)
	case "probe_failure":
		return fmt.Sprintf("kubectl describe pod %s -n %s", resource, namespace)
	default:
		return fmt.Sprintf("kubectl describe pod %s -n %s", resource, namespace)
	}
}
