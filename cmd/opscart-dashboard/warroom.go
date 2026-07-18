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
	Severity     string `json:"severity"`
	Type         string `json:"type"`
	Namespace    string `json:"namespace"`
	Resource     string `json:"resource"`
	Message      string `json:"message"`
	AgeDays      int    `json:"age_days,omitempty"`
	KubectlCmd   string `json:"kubectl_cmd,omitempty"`
	RestartCount int    `json:"restart_count,omitempty"`
}

type warRoomPageData struct {
	ClusterName   string
	StatusDesc    string
	DashURL       string
	CritChipClass string
	WarnChipClass string
	Critical      []warRoomIssue
	Warnings      []warRoomIssue
	ScannedAtMs   int64
	Sidebar       template.HTML
	TotalIssues   int
	IncidentsHref string
}

func collectWarRoomIssues(scan *clusterScan, limit int) []warRoomIssue {
	var issues []warRoomIssue

	// 1. Crash-looping / zombie pods
	if scan.wasteAudit != nil {
		for _, pod := range scan.wasteAudit.StalePods {
			if pod.Kind == analyzer.StalePodZombie {
				itype := zombieTypeForStatus(pod.Status)
				issues = append(issues, warRoomIssue{
					Severity:     "critical",
					Type:         itype,
					Namespace:    pod.Namespace,
					Resource:     pod.Name,
					Message:      fmt.Sprintf("%s — %d restarts, %d days old", pod.Status, pod.RestartCount, pod.AgeDays),
					AgeDays:      pod.AgeDays,
					KubectlCmd:   kubectlCmdForIssue(itype, pod.Name, pod.Namespace),
					RestartCount: int(pod.RestartCount),
				})
			}
		}
	}

	// 2. Unexpected privileged containers (non-system or unexpected system ones)
	if scan.secAudit != nil {
		for _, issue := range scan.secAudit.Issues {
			if issue.Type == "privileged_container" && issue.Severity == "critical" {
				issues = append(issues, warRoomIssue{
					Severity:   "critical",
					Type:       "privileged_container",
					Namespace:  issue.Namespace,
					Resource:   issue.Name,
					Message:    issue.Description,
					KubectlCmd: fmt.Sprintf("kubectl describe pod %s -n %s", issue.Name, issue.Namespace),
				})
			}
		}
	}

	// 3. High-risk unprotected namespaces
	if scan.netAudit != nil {
		for _, ns := range scan.netAudit.UnprotectedNamespaces {
			if ns.RiskLevel == "HIGH" {
				issues = append(issues, warRoomIssue{
					Severity:   "high",
					Type:       "unprotected_namespace",
					Namespace:  ns.Name,
					Resource:   "namespace",
					Message:    fmt.Sprintf("No NetworkPolicy — %d pods exposed", ns.PodCount),
					KubectlCmd: fmt.Sprintf("kubectl get networkpolicies -n %s", ns.Name),
				})
			}
		}
	}
	// Sort: critical first, then by restart count descending (highest impact first)
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
	fmt.Fprint(w, renderWarRoomPage(scan, ctx, srv.clusterList))
}

func renderWarRoomPage(scan *clusterScan, activeCtx string, clusterList []string) string {
	allIssues := collectWarRoomIssues(scan, 0)
	var critical, warnings []warRoomIssue
	for _, issue := range allIssues {
		if issue.Severity == "critical" {
			critical = append(critical, issue)
		} else {
			warnings = append(warnings, issue)
		}
	}

	scannedAt := time.Now()
	clusterName := displayName(activeCtx)
	if scan != nil && scan.report != nil {
		scannedAt = scan.report.Timestamp
		clusterName = scan.report.ClusterName
	}

	q := ""
	if activeCtx != "" {
		q = "?cluster=" + url.QueryEscape(activeCtx)
	}
	// Cap War Room at top 20 — full registry is in Incidents
	if len(critical) > 20 {
		critical = critical[:20]
	}
	critChipClass := "ok"
	if len(critical) > 0 {
		critChipClass = "c"
	}
	warnChipClass := "ok"
	if len(warnings) > 0 {
		warnChipClass = "w"
	}

	data := warRoomPageData{
		ClusterName:   clusterName,
		StatusDesc:    fmt.Sprintf("%d issue(s) detected", len(critical)+len(warnings)),
		DashURL:       "/" + q,
		CritChipClass: critChipClass,
		WarnChipClass: warnChipClass,
		Critical:      critical,
		Warnings:      warnings,
		ScannedAtMs:   scannedAt.UnixMilli(),
		Sidebar:       template.HTML(buildSidebar("warroom", activeCtx, clusterName, clusterList, countCriticalIssues(scan))),
		TotalIssues:   len(critical) + len(warnings),
		IncidentsHref: "/incidents" + q,
	}

	var buf strings.Builder
	if err := getWarRoomTmpl().Execute(&buf, data); err != nil {
		log.Printf("warroom template: %v", err)
		return ""
	}
	return buf.String()
}

var warRoomTmplOnce sync.Once
var warRoomTmpl *template.Template

func getWarRoomTmpl() *template.Template {
	warRoomTmplOnce.Do(func() {
		warRoomTmpl = template.Must(
			template.New("warroom.html").Funcs(template.FuncMap{
				"renderCard": func(issue warRoomIssue) template.HTML {
					return template.HTML(renderWarRoomCard(issue))
				},
			}).ParseFS(templateFS, "templates/base.html", "templates/warroom.html"),
		)
	})
	return warRoomTmpl
}

func renderWarRoomCard(issue warRoomIssue) string {
	sc := "c"
	bl := "CRITICAL"
	if issue.Severity == "warning" {
		sc = "w"
		bl = "WARNING"
	}

	icon, label := warRoomTypeLabel(issue.Type)

	age := ""
	if issue.AgeDays > 0 {
		age = fmt.Sprintf(`<span class="age-lbl">%dd</span>`, issue.AgeDays)
	}

	name := issue.Resource
	if len(name) > 38 {
		name = name[:37] + "…"
	}

	reason := issue.Message
	if len(reason) > 320 {
		reason = reason[:317] + "…"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`<div class="wr-card %s %s">`, sc, "wr-type-"+strings.ReplaceAll(issue.Type, "_", "-")))
	sb.WriteString(fmt.Sprintf(`<div class="wr-top"><span class="badge %s">%s</span><span class="type-lbl"><span class="wr-type-icon %s"></span>%s</span>%s</div>`,
		sc, bl, icon, label, age))
	sb.WriteString(fmt.Sprintf(`<div class="wr-name">%s</div>`, name))
	sb.WriteString(fmt.Sprintf(`<div class="wr-ns">ns: %s</div>`, issue.Namespace))
	sb.WriteString(fmt.Sprintf(`<div class="wr-reason">%s</div>`, reason))
	sb.WriteString(`<div class="wr-cmd">`)
	sb.WriteString(fmt.Sprintf(`<code>%s</code>`, issue.KubectlCmd))
	sb.WriteString(`<button class="copy-btn" onclick="var b=this,c=this.previousElementSibling.textContent;navigator.clipboard.writeText(c).then(function(){b.textContent='✓';setTimeout(function(){b.textContent='Copy'},1500)})">Copy</button>`)
	sb.WriteString(`</div>`)
	// Investigate button — links to investigation page
	if issue.Resource != "" && issue.Namespace != "" {
		investigateURL := fmt.Sprintf("/investigate?pod=%s&ns=%s&type=%s",
			url.QueryEscape(issue.Resource),
			url.QueryEscape(issue.Namespace),
			url.QueryEscape(issue.Type))
		sb.WriteString(fmt.Sprintf(
			`<a class="investigate-btn" href="%s">Investigate →</a>`,
			investigateURL))
	}
	sb.WriteString(`</div>`)
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
