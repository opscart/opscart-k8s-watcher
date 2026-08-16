package main

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

type incidentsPageData struct {
	// Sidebar
	DashHref      string
	WrHref        string
	CostsHref     string
	InfraHref     string
	WasteHref     string
	SecurityHref  string
	IncidentsHref string
	ActivePage    string
	ClusterName   string
	ClusterParam  string
	CriticalCount int
	Clusters      []sidebarCluster

	// Filter state
	Filter store.IncidentFilter

	// Results
	Incidents  []store.IncidentSummary
	Total      int
	Page       int
	PerPage    int
	TotalPages int
	Namespaces []string // distinct namespaces for filter dropdown

	// Summary counts (across all matching, not just this page)
	ActiveCritical int
	ActiveHigh     int
	ActiveMedium   int
	ActiveLow      int
	ReopenedCount  int
	ResolvedCount  int
}

func (srv *server) handleIncidentsPage(w http.ResponseWriter, r *http.Request) {
	ctx := srv.activeCtx(r)
	q := r.URL.Query()

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}

	status := q.Get("status")
	if status == "" && q.Get("page") == "" && q.Get("q") == "" &&
		q.Get("ns") == "" && q.Get("type") == "" && q.Get("severity") == "" {
		status = "active" // default: active only on first load
	}

	sortBy := q.Get("sort")
	if sortBy == "" {
		sortBy = "priority"
	}

	f := store.IncidentFilter{
		Cluster:   ctx,
		Text:      q.Get("q"),
		Namespace: q.Get("ns"),
		IssueType: q.Get("type"),
		Severity:  q.Get("severity"),
		Status:    status,
		SortBy:    sortBy,
		SortDesc:  true,
		Page:      page,
		PerPage:   50,
	}

	state := srv.getState(ctx)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	clusterQ := "?cluster=" + url.QueryEscape(ctx)
	data := incidentsPageData{
		DashHref:      "/" + clusterQ,
		WrHref:        "/warroom" + clusterQ,
		CostsHref:     "/costs" + clusterQ,
		InfraHref:     "/infrastructure" + clusterQ,
		WasteHref:     "/waste" + clusterQ,
		SecurityHref:  "/security" + clusterQ,
		IncidentsHref: "/incidents" + clusterQ,
		ActivePage:    "incidents",
		ClusterName:   displayName(ctx),
		ClusterParam:  ctx,
		CriticalCount: countCriticalIssues(scan),
		Filter:        f,
		Page:          page,
		PerPage:       50,
	}

	// Query store
	items, total, err := srv.db.QueryIncidents(f)
	if err == nil {
		data.Incidents = items
		data.Total = total
		data.TotalPages = int(math.Ceil(float64(total) / 50))
	}

	// Summary counts use the current non-status filters across active and
	// resolved results, rather than labeling every active row critical.
	if all, e := queryAllIncidentSummaries(srv.db, store.IncidentFilter{
		Cluster: ctx, Text: f.Text, Namespace: f.Namespace,
		IssueType: f.IssueType, Severity: f.Severity,
		Status: "",
	}); e == nil {
		for _, inc := range all {
			switch {
			case inc.Status == "resolved":
				data.ResolvedCount++
			default:
				switch inc.Severity {
				case "critical":
					data.ActiveCritical++
				case "high":
					data.ActiveHigh++
				case "medium":
					data.ActiveMedium++
				case "low":
					data.ActiveLow++
				}
				if inc.ReopenCount > 0 {
					data.ReopenedCount++
				}
			}
		}
	}

	// Distinct namespaces for dropdown
	if ns, _, e := srv.db.QueryIncidents(store.IncidentFilter{
		Cluster: ctx, Status: "", PerPage: 200,
	}); e == nil {
		seen := map[string]bool{}
		for _, inc := range ns {
			if !seen[inc.Namespace] {
				seen[inc.Namespace] = true
				data.Namespaces = append(data.Namespaces, inc.Namespace)
			}
		}
	}

	renderIncidents(w, data)
}

var getIncidentsTmpl = sync.OnceValue(func() *template.Template {
	humanAge := func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 24*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		case d < 48*time.Hour:
			return "yesterday"
		default:
			days := int(d.Hours() / 24)
			return fmt.Sprintf("%d days ago", days)
		}
	}
	recoveredIn := func(first, last time.Time) string {
		if first.IsZero() || last.IsZero() {
			return "—"
		}
		d := last.Sub(first)
		days := int(d.Hours() / 24)
		if days < 1 {
			return fmt.Sprintf("%dh", int(d.Hours()))
		}
		return fmt.Sprintf("%dd", days)
	}
	pageStart := func(page, perPage int) int {
		return (page-1)*perPage + 1
	}
	pageEnd := func(page, perPage, total int) int {
		end := page * perPage
		if end > total {
			return total
		}
		return end
	}
	prevPage := func(page int) int {
		if page > 1 {
			return page - 1
		}
		return 1
	}
	nextPage := func(page, total int) int {
		if page < total {
			return page + 1
		}
		return total
	}
	pageRange := func(page, totalPages int) []int {
		var pages []int
		start := page - 2
		if start < 1 {
			start = 1
		}
		end := start + 4
		if end > totalPages {
			end = totalPages
		}
		for i := start; i <= end; i++ {
			pages = append(pages, i)
		}
		return pages
	}

	return template.Must(
		template.New("incidents.html").
			Funcs(template.FuncMap{
				"firstDetected": humanAge,
				"lastSeen":      humanAge,
				"recoveredIn":   recoveredIn,
				"pageStart":     pageStart,
				"pageEnd":       pageEnd,
				"prevPage":      prevPage,
				"nextPage":      nextPage,
				"pageRange":     pageRange,
				"ownerName":     store.OwnerNameFromPod,
				"isNamespaceScoped": func(issueType string) bool {
					return issueType == "unprotected_namespace" || issueType == "idle_namespace"
				},
				"isNodeIncident": func(fingerprint string) bool {
					return isNodeIncidentFingerprint(fingerprint)
				},
				"trendApplies": store.RestartTrendApplies,
			}).
			ParseFS(templateFS,
				"templates/base.html",
				"templates/sidebar.html",
				"templates/incidents.html"),
	)
})

func renderIncidents(w http.ResponseWriter, data incidentsPageData) {
	var buf bytes.Buffer
	if err := getIncidentsTmpl().Execute(&buf, data); err != nil {
		log.Printf("incidents template: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write(buf.Bytes())
}

func queryAllIncidentSummaries(db store.Store, filter store.IncidentFilter) ([]store.IncidentSummary, error) {
	filter.PerPage = 200
	filter.Page = 1
	var all []store.IncidentSummary
	for {
		items, total, err := db.QueryIncidents(filter)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(all) >= total || len(items) == 0 {
			return all, nil
		}
		filter.Page++
	}
}
