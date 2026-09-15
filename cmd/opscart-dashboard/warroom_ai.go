package main

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
)

const warRoomAIMaxFormBytes = 2 << 10

type warRoomAIOption struct {
	Selector string
	Label    string
}

type warRoomAIPageData struct {
	Enabled          bool
	State            string
	Message          string
	Endpoint         string
	Options          []warRoomAIOption
	SelectedSelector string
	SelectedIdentity string
	EvidenceCaptured string
	GeneratedAt      string
	ConfidenceClass  string
	CanGenerate      bool
	Regenerate       bool
	Result           *aianalysis.AnalysisResponse
}

type warRoomAIAPIResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func (srv *server) warRoomCluster(r *http.Request) (string, bool) {
	if len(srv.clusterList) == 0 {
		return "", false
	}
	requested := r.URL.Query().Get("cluster")
	if requested == "" || requested == displayName("") {
		requested = srv.clusterList[0]
	}
	for _, configured := range srv.clusterList {
		if configured == requested {
			return configured, true
		}
	}
	return "", false
}

func (srv *server) buildWarRoomAIPageData(scan *clusterScan, cluster, selector string) warRoomAIPageData {
	data := warRoomAIPageData{State: "unavailable", Message: "AI analysis is not configured for this dashboard."}
	if srv.aiProvider == nil || srv.aiRuntime == nil || srv.aiRuntime.cache == nil {
		return data
	}

	data.Enabled = true
	data.Endpoint = warRoomAIEndpoint(cluster)
	selections := collectWarRoomAISelections(scan, cluster, srv.db)
	for _, selection := range selections {
		data.Options = append(data.Options, warRoomAIOption{Selector: selection.Selector, Label: selection.Identity})
	}
	if selector == "" {
		if len(selections) == 0 {
			data.Message = "No supported active issue is available for analysis."
			return data
		}
		data.State = "ready"
		data.Message = "Select an active issue to generate an evidence-bound analysis."
		return data
	}

	data.SelectedSelector = selector
	selection, err := findWarRoomAISelection(scan, cluster, srv.db, selector)
	if err != nil {
		if entry, ok := srv.aiRuntime.cache.latest(cluster, selector); ok {
			data.Options = append(data.Options, warRoomAIOption{Selector: selector, Label: entry.IssueIdentity + " (no longer active)"})
			applyWarRoomAIEntry(&data, entry, "stale", "This result is stale because the selected issue is no longer active.")
			return data
		}
		data.Message = "The selected issue is no longer active or is not supported."
		return data
	}
	data.SelectedIdentity = selection.Identity

	capture, err := captureWarRoomAIEvidence(scan, cluster, srv.db, selection)
	if err != nil {
		if entry, ok := srv.aiRuntime.cache.latest(cluster, selector); ok {
			applyWarRoomAIEntry(&data, entry, "stale", "This result is stale because current evidence is unavailable.")
			return data
		}
		data.Message = warRoomAIUnavailableMessage(err)
		return data
	}
	data.EvidenceCaptured = formatWarRoomAITime(capture.CapturedAt)

	entry, found, current := srv.aiRuntime.cache.get(cluster, selector, capture.Hash, srv.aiRuntime.key())
	if found && current {
		applyWarRoomAIEntry(&data, entry, "complete", "Analysis generated from the current sanitized evidence.")
		data.CanGenerate = true
		data.Regenerate = true
		return data
	}
	if found {
		applyWarRoomAIEntry(&data, entry, "stale", "New evidence is available. Regenerate to analyze the latest observations.")
		data.CanGenerate = true
		data.Regenerate = true
		return data
	}

	data.State = "ready"
	data.Message = "Ready to analyze the selected issue using the current sanitized evidence."
	data.CanGenerate = true
	return data
}

func applyWarRoomAIEntry(data *warRoomAIPageData, entry warRoomAICacheEntry, state, message string) {
	response := cloneAnalysisResponse(entry.Response)
	data.State = state
	data.Message = message
	data.SelectedIdentity = entry.IssueIdentity
	data.EvidenceCaptured = formatWarRoomAITime(entry.EvidenceCaptured)
	data.GeneratedAt = formatWarRoomAITime(entry.GeneratedAt)
	data.ConfidenceClass = warRoomAIConfidenceClass(response.Confidence)
	data.Result = &response
}

func warRoomAIUnavailableMessage(err error) string {
	switch {
	case errors.Is(err, errWarRoomAIHistoryUnavailable):
		return "Analysis is unavailable because incident history is incomplete."
	case errors.Is(err, errWarRoomAIEvidenceTooLarge):
		return "Analysis is unavailable because the sanitized evidence exceeds the POC limits."
	default:
		return "Current evidence for this issue is unavailable."
	}
}

func warRoomAIConfidenceClass(confidence aianalysis.Confidence) string {
	switch confidence {
	case aianalysis.ConfidenceHigh:
		return "high"
	case aianalysis.ConfidenceMedium:
		return "medium"
	default:
		return "low"
	}
}

func formatWarRoomAITime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Local().Format("Jan 2, 2006 3:04 PM MST")
}

func warRoomAIEndpoint(cluster string) string {
	values := url.Values{}
	if cluster != "" {
		values.Set("cluster", cluster)
	}
	if encoded := values.Encode(); encoded != "" {
		return "/api/warroom/ai-analysis?" + encoded
	}
	return "/api/warroom/ai-analysis"
}

func (srv *server) handleWarRoomAIAnalysis(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeWarRoomAIError(w, http.StatusMethodNotAllowed, "use POST to generate an AI analysis")
		return
	}
	if !sameOriginWarRoomAIRequest(r) {
		writeWarRoomAIError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}
	if srv.aiProvider == nil || srv.aiRuntime == nil || srv.aiRuntime.cache == nil {
		writeWarRoomAIError(w, http.StatusServiceUnavailable, "AI analysis is disabled")
		return
	}
	cluster, ok := srv.warRoomCluster(r)
	if !ok {
		writeWarRoomAIError(w, http.StatusBadRequest, "unknown cluster")
		return
	}
	regenerate, ok := warRoomAIRegenerate(r.URL.Query())
	if !ok {
		writeWarRoomAIError(w, http.StatusBadRequest, "invalid request")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeWarRoomAIError(w, http.StatusUnsupportedMediaType, "use application/x-www-form-urlencoded")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, warRoomAIMaxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeWarRoomAIError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if len(r.PostForm) != 1 || len(r.PostForm["issue"]) != 1 {
		writeWarRoomAIError(w, http.StatusBadRequest, "invalid request")
		return
	}
	selector := r.PostForm.Get("issue")

	state := srv.getState(cluster)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()
	selection, err := findWarRoomAISelection(scan, cluster, srv.db, selector)
	if err != nil {
		writeWarRoomAIError(w, http.StatusNotFound, "selected issue is not active")
		return
	}
	capture, err := captureWarRoomAIEvidence(scan, cluster, srv.db, selection)
	if err != nil {
		writeWarRoomAIError(w, http.StatusUnprocessableEntity, warRoomAIUnavailableMessage(err))
		return
	}
	if _, found, current := srv.aiRuntime.cache.get(cluster, selector, capture.Hash, srv.aiRuntime.key()); found && !regenerate {
		status := "stale"
		message := "New evidence is available; regenerate to analyze it."
		if current {
			status = "cached"
			message = "The current analysis is already available."
		}
		writeWarRoomAIJSON(w, http.StatusOK, warRoomAIAPIResponse{Status: status, Message: message})
		return
	}
	if !srv.aiRuntime.cache.begin(cluster, selector) {
		writeWarRoomAIError(w, http.StatusConflict, "analysis is already in progress for this issue")
		return
	}
	defer srv.aiRuntime.cache.finish(cluster, selector)

	response, err := srv.aiProvider.Analyze(r.Context(), capture.Request)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			writeWarRoomAIError(w, http.StatusRequestTimeout, "AI analysis was canceled")
		case errors.Is(err, context.DeadlineExceeded):
			writeWarRoomAIError(w, http.StatusGatewayTimeout, "AI analysis timed out")
		default:
			writeWarRoomAIError(w, http.StatusBadGateway, "AI analysis could not be generated")
		}
		return
	}
	if response == nil {
		writeWarRoomAIError(w, http.StatusBadGateway, "AI analysis could not be generated")
		return
	}

	generatedAt := srv.aiRuntime.cache.now()
	entry := warRoomAICacheEntry{
		ClusterKey: cluster, Selector: selector, EvidenceHash: capture.Hash,
		RuntimeKey: srv.aiRuntime.key(), IssueIdentity: selection.Identity,
		EvidenceCaptured: capture.CapturedAt, GeneratedAt: generatedAt, Response: *response,
	}
	srv.aiRuntime.cache.put(entry)

	status := "generated"
	message := "Analysis generated from the current sanitized evidence."
	if !srv.warRoomAICaptureStillCurrent(cluster, selector, capture.Hash) {
		status = "stale"
		message = "Analysis completed, but the issue or its evidence changed while generation was in progress."
	}
	writeWarRoomAIJSON(w, http.StatusOK, warRoomAIAPIResponse{Status: status, Message: message})
}

func (srv *server) warRoomAICaptureStillCurrent(cluster, selector, evidenceHash string) bool {
	state := srv.getState(cluster)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()
	selection, err := findWarRoomAISelection(scan, cluster, srv.db, selector)
	if err != nil {
		return false
	}
	capture, err := captureWarRoomAIEvidence(scan, cluster, srv.db, selection)
	return err == nil && capture.Hash == evidenceHash
}

func warRoomAIRegenerate(query url.Values) (bool, bool) {
	for key := range query {
		if key != "cluster" && key != "regenerate" {
			return false, false
		}
	}
	values, exists := query["regenerate"]
	if !exists {
		return false, true
	}
	valid := len(values) == 1 && values[0] == "1"
	return valid, valid
}

func sameOriginWarRoomAIRequest(r *http.Request) bool {
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" && site != "same-origin" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	}
	return parsed.Scheme == requestScheme && strings.EqualFold(parsed.Host, r.Host)
}

func writeWarRoomAIError(w http.ResponseWriter, status int, message string) {
	writeWarRoomAIJSON(w, status, warRoomAIAPIResponse{Status: "error", Message: message})
}

func writeWarRoomAIJSON(w http.ResponseWriter, status int, response warRoomAIAPIResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
