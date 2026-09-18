package main

import (
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
)

// investigationAIPageData renders the Investigation page's AI Analysis tab.
// It is built entirely from the cached cluster scan, the incident store, and
// the AI runtime/cache — never from a live Kubernetes read. See
// handleInvestigationAIPage.
type investigationAIPageData struct {
	investigationSidebarFields
	investigationTabs

	BackURL     string
	ScannedAtMs int64

	Selector string
	Endpoint string

	// Status is one of: NOT_CONFIGURED, NOT_GENERATED, GENERATING, GENERATED, STALE, ERROR.
	// StatusClass is its lowercase, hyphenated CSS-class form.
	Status      string
	StatusClass string
	Message     string
	StaleReason string

	Identity        string
	Namespace       string
	PodName         string
	ContainerName   string
	NodeName        string
	IssueType       string
	WorkloadLabel   string
	NamespaceScoped bool
	NodeScoped      bool

	EvidenceCaptured string
	GeneratedAt      string
	Provider         string
	Model            string
	ConfidenceClass  string
	Result           *aianalysis.AnalysisResponse

	// GenerationEvidence is the exact sanitized evidence a cached result was
	// generated from (GENERATED/STALE) — never rebuilt from the current
	// scan. CurrentEvidence is the freshly captured sanitized evidence
	// available for a not-yet-generated analysis; the template only ever
	// renders one of the two, and clearly labels which it is showing.
	GenerationEvidence []aianalysis.EvidenceItem
	CurrentEvidence    []aianalysis.EvidenceItem

	CanGenerate bool
	Regenerate  bool
}

// buildInvestigationTabs computes the Evidence/AI tab-bar state for a given
// issue identity. It never captures or hashes evidence — only identity
// enrichment and the opaque selector, via collectWarRoomAISelections, the
// same cheap step the AI issue picker already performed before its removal
// from the War Room page.
func (srv *server) buildInvestigationTabs(cluster string, scan *clusterScan, issueType, namespace, podParam, nodeName, from string) investigationTabs {
	resource, container := podParam, ""
	if idx := strings.Index(podParam, "/"); idx != -1 {
		resource, container = podParam[:idx], podParam[idx+1:]
	}
	issue := warRoomIssue{Type: issueType, Namespace: namespace, Resource: resource, Container: container}
	switch {
	case nodeName != "":
		issue.IsNode = true
		issue.Resource = nodeName
	case isNamespaceFinding(issue):
		issue.Resource = "namespace"
		issue.Container = ""
	}

	configured := srv.aiProvider != nil && srv.aiRuntime != nil && srv.aiRuntime.cache != nil
	tabs := investigationTabs{
		ActiveTab:       "evidence",
		EvidenceTabHref: investigationEvidenceHref(issue, cluster, from),
		AIConfigured:    configured,
	}
	selector := ""
	for _, selection := range collectWarRoomAISelections(scan, cluster, srv.db) {
		candidate := selection.Issue
		// Legacy pod-only links may resolve only when the selection is unique.
		if issue.Container == "" {
			candidate.Container = ""
		}
		if warRoomAIIssueKey(candidate) != warRoomAIIssueKey(issue) {
			continue
		}
		if selector != "" {
			return tabs
		}
		selector = selection.Selector
	}
	if selector != "" {
		tabs.AIAvailable = true
		tabs.AITabHref = investigationAIHref(cluster, selector, from)
	}
	return tabs
}

// investigationEvidenceHref builds the canonical Evidence-tab URL for an
// issue identity — the same shape handleInvestigationPage's Evidence path
// already reads (pod/ns/node/type/cluster/from).
func investigationEvidenceHref(issue warRoomIssue, cluster, from string) string {
	if from == "" {
		from = "warroom"
	}
	values := url.Values{"type": {issue.Type}, "from": {from}}
	if cluster != "" {
		values.Set("cluster", cluster)
	}
	switch {
	case issue.IsNode:
		values.Set("node", issue.Resource)
	case isNamespaceFinding(issue):
		values.Set("ns", issue.Namespace)
	default:
		values.Set("ns", issue.Namespace)
		pod := issue.Resource
		if issue.Container != "" {
			pod += "/" + issue.Container
		}
		values.Set("pod", pod)
	}
	return "/investigate?" + values.Encode()
}

// investigationAIHref builds the AI-tab deep link. Per the routing
// contract, it carries only the cluster, tab=ai, the opaque issue selector,
// and an optional navigation origin — never namespace/pod/container/node.
func investigationAIHref(cluster, selector, from string) string {
	values := url.Values{"tab": {"ai"}, "issue": {selector}}
	if cluster != "" {
		values.Set("cluster", cluster)
	}
	if from != "" {
		values.Set("from", from)
	}
	return "/investigate?" + values.Encode()
}

// handleInvestigationAIPage serves the Investigation page's AI Analysis tab.
// It resolves the opaque issue selector against the currently cached scan
// exactly as POST /api/warroom/ai-analysis already does, and never calls
// kubeClientFor, refreshes the cluster, or otherwise performs a live
// Kubernetes read — a cached-scan-absent cluster renders a safe
// not-generated state instead.
func (srv *server) handleInvestigationAIPage(w http.ResponseWriter, r *http.Request) {
	cluster, ok := srv.warRoomCluster(r)
	if !ok {
		http.Error(w, "unknown cluster", http.StatusBadRequest)
		return
	}
	selector := r.URL.Query().Get("issue")
	if selector == "" {
		http.Error(w, "missing issue query parameter", http.StatusBadRequest)
		return
	}
	from := r.URL.Query().Get("from")

	state := srv.getState(cluster)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	data := srv.buildInvestigationAIPageData(scan, cluster, selector, from)
	renderInvestigationAI(w, data)
}

func (srv *server) buildInvestigationAIPageData(scan *clusterScan, cluster, selector, from string) investigationAIPageData {
	enabled := srv.aiProvider != nil && srv.aiRuntime != nil && srv.aiRuntime.cache != nil
	tabs := investigationTabs{
		ActiveTab:    "ai",
		AIAvailable:  true,
		AIConfigured: enabled,
		AITabHref:    investigationAIHref(cluster, selector, from),
	}
	data := investigationAIPageData{
		investigationSidebarFields: srv.buildInvestigationSidebar(cluster, scan, from),
		investigationTabs:          tabs,
		Selector:                   selector,
		Endpoint:                   warRoomAIEndpoint(cluster),
		ScannedAtMs:                time.Now().UnixMilli(),
	}
	data.BackURL = "/warroom?cluster=" + url.QueryEscape(cluster)
	if from == "incidents" {
		data.BackURL = "/incidents?cluster=" + url.QueryEscape(cluster)
	}

	data = srv.resolveInvestigationAIState(data, scan, cluster, selector, from)
	if data.EvidenceTabHref == "" {
		data.EvidenceTabHref = data.BackURL
	}

	if enabled && srv.aiRuntime.cache.isInFlight(cluster, selector) {
		data.Status = "GENERATING"
		data.CanGenerate = false
		data.Message = "Generating an analysis from the captured evidence. No cluster action will be performed."
	}
	data.StatusClass = strings.ToLower(strings.ReplaceAll(data.Status, "_", "-"))
	return data
}

// resolveInvestigationAIState fills in the Evidence/AI status fields of an
// otherwise-initialized investigationAIPageData. It mirrors the POST
// handler's own resolution order: resolve the selector against current
// server-computed issues, capture current sanitized evidence, and compare
// against the cached generation (if any) to decide GENERATED vs. STALE.
func (srv *server) resolveInvestigationAIState(data investigationAIPageData, scan *clusterScan, cluster, selector, from string) investigationAIPageData {
	if srv.aiProvider == nil || srv.aiRuntime == nil || srv.aiRuntime.cache == nil {
		data.Status = "NOT_CONFIGURED"
		data.Message = "AI analysis is disabled for this OpsCart installation."
		selection, err := findWarRoomAISelection(scan, cluster, srv.db, selector)
		if err == nil {
			data.EvidenceTabHref = investigationEvidenceHref(selection.Issue, cluster, from)
			populateInvestigationAIIdentity(&data, selection.Issue)
		}
		return data
	}

	entry, hasEntry := srv.aiRuntime.cache.latest(cluster, selector)
	selection, err := findWarRoomAISelection(scan, cluster, srv.db, selector)
	if err != nil {
		if hasEntry {
			populateInvestigationAIFromEntry(&data, entry)
			data.Status = "STALE"
			data.StaleReason = "This result is stale because the selected issue is no longer active."
			return data
		}
		data.Status = "NOT_GENERATED"
		data.Message = "The selected issue is no longer active or is not supported."
		return data
	}
	data.EvidenceTabHref = investigationEvidenceHref(selection.Issue, cluster, from)
	populateInvestigationAIIdentity(&data, selection.Issue)

	capture, err := captureWarRoomAIEvidence(scan, cluster, srv.db, selection)
	if err != nil {
		if hasEntry {
			populateInvestigationAIFromEntry(&data, entry)
			data.Status = "STALE"
			data.StaleReason = "This result is stale because current evidence is unavailable."
			return data
		}
		data.Status = "NOT_GENERATED"
		data.Message = warRoomAIUnavailableMessage(err)
		return data
	}
	data.CurrentEvidence = capture.Request.Evidence
	data.EvidenceCaptured = formatWarRoomAITime(capture.CapturedAt)

	if hasEntry && entry.RuntimeKey == srv.aiRuntime.key() && entry.EvidenceHash == capture.Hash {
		populateInvestigationAIFromEntry(&data, entry)
		data.Status = "GENERATED"
		data.Message = "Analysis generated from the current sanitized evidence."
		data.CanGenerate = true
		data.Regenerate = true
		return data
	}
	if hasEntry {
		populateInvestigationAIFromEntry(&data, entry)
		data.Status = "STALE"
		data.StaleReason = "New evidence is available. Regenerate to analyze the latest observations."
		data.CanGenerate = true
		data.Regenerate = true
		return data
	}

	data.Status = "NOT_GENERATED"
	data.Message = "Ready to analyze the selected issue using the current sanitized evidence."
	data.CanGenerate = true
	return data
}

// populateInvestigationAIIdentity renders the issue's generic identity
// (workload/pod/container, namespace, or node/condition) so the template can
// render each conditionally without leaving empty pod/container fields.
func populateInvestigationAIIdentity(data *investigationAIPageData, issue warRoomIssue) {
	data.IssueType = issue.Type
	data.Namespace = issue.Namespace
	switch {
	case issue.IsNode:
		data.NodeScoped = true
		data.NodeName = issue.Resource
	case isNamespaceFinding(issue):
		data.NamespaceScoped = true
	default:
		data.PodName = issue.Resource
		data.ContainerName = issue.Container
		data.WorkloadLabel = issue.WorkloadKind + "/" + issue.WorkloadName
	}
	data.Identity = warRoomAIIdentity(issue)
}

// populateInvestigationAIFromEntry renders a cached generation exactly as it
// was produced: its own provider/model, its own generation-time evidence,
// and its own result — never the current scan's evidence. entry is already
// a defensively cloned copy (see warRoomAICache.latest), so this never
// mutates cache-owned storage.
func populateInvestigationAIFromEntry(data *investigationAIPageData, entry warRoomAICacheEntry) {
	if data.Identity == "" {
		data.Identity = entry.IssueIdentity
	}
	data.Provider = entry.Provider
	data.Model = entry.Model
	data.EvidenceCaptured = formatWarRoomAITime(entry.EvidenceCaptured)
	data.GeneratedAt = formatWarRoomAITime(entry.GeneratedAt)
	data.GenerationEvidence = entry.Evidence
	data.ConfidenceClass = warRoomAIConfidenceClass(entry.Response.Confidence)
	response := entry.Response
	data.Result = &response
}

var getInvestigationAITmpl = sync.OnceValue(func() *template.Template {
	return template.Must(
		template.New("investigation_ai.html").
			Funcs(template.FuncMap{
				"issueTypeLabel": issueTypeLabel,
				// inc turns a template range's 0-based index into a 1-based
				// display number for causes/checks — html/template has no
				// arithmetic operators of its own.
				"inc": func(i int) int { return i + 1 },
			}).
			ParseFS(templateFS,
				"templates/base.html",
				"templates/sidebar.html",
				"templates/investigation_tabs.html",
				"templates/investigation_ai.html"),
	)
})

func renderInvestigationAI(w http.ResponseWriter, data investigationAIPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	var buf strings.Builder
	if err := getInvestigationAITmpl().Execute(&buf, data); err != nil {
		log.Printf("investigation ai template: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Write([]byte(buf.String()))
}
