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

type warRoomAIAPIResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	// Reason is a fixed, machine-readable code (see the aiReason* constants,
	// ai_log_signal_preview.go) the browser can key UI behavior off of —
	// never a selector, identifier, or anything derived from raw content.
	// Empty for responses with no more specific reason than Message itself.
	Reason string `json:"reason,omitempty"`
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
	if err := aianalysis.ValidateResponse(response); err != nil {
		writeWarRoomAIError(w, http.StatusBadGateway, "AI analysis could not be generated")
		return
	}

	generatedAt := srv.aiRuntime.cache.now()
	// Best-effort: retains the server-resolved focus pod/target
	// container/issue type (when this issue type supports log-derived
	// signals) so a later preview can still validate against this exact,
	// previously-resolved target if the selector momentarily drops out of
	// a future scan cycle — see resolveAILogSignalsTargetWithFallback.
	resolvedNamespace, resolvedPod, resolvedContainer, resolvedIssueType := aiLogSignalsResolvedTarget(scan, cluster, srv.db, selector)
	entry := warRoomAICacheEntry{
		ClusterKey: cluster, Selector: selector, EvidenceHash: capture.Hash, BaseEvidenceHash: capture.Hash,
		RuntimeKey: srv.aiRuntime.key(), IssueIdentity: selection.Identity,
		Provider: srv.aiRuntime.providerName, Model: srv.aiRuntime.model,
		EvidenceCaptured: capture.CapturedAt, GeneratedAt: generatedAt, Response: *response,
		Evidence:          capture.Request.Evidence,
		BaseCapture:       capture,
		ResolvedNamespace: resolvedNamespace, ResolvedPodName: resolvedPod,
		ResolvedContainerName: resolvedContainer, ResolvedIssueType: resolvedIssueType,
	}
	srv.aiRuntime.cache.put(entry)

	status := "generated"
	message := "Analysis generated from the supplied read-only evidence."
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
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] == "" || origins[0] != strings.TrimSpace(origins[0]) {
		return false
	}
	parsed, err := url.Parse(origins[0])
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	} else if forwardedProto := r.Header.Values("X-Forwarded-Proto"); len(forwardedProto) != 0 {
		if len(forwardedProto) != 1 || (forwardedProto[0] != "http" && forwardedProto[0] != "https") {
			return false
		}
		requestScheme = forwardedProto[0]
	}
	return parsed.Scheme == requestScheme && strings.EqualFold(parsed.Host, r.Host)
}

func writeWarRoomAIError(w http.ResponseWriter, status int, message string) {
	writeWarRoomAIJSON(w, status, warRoomAIAPIResponse{Status: "error", Message: message})
}

// writeWarRoomAIErrorWithReason is writeWarRoomAIError plus a fixed
// machine-readable reason code — used for the subset of rejections the
// browser needs to distinguish (e.g. "issue_not_active" vs a generic
// failure) without parsing the human-readable message.
func writeWarRoomAIErrorWithReason(w http.ResponseWriter, status int, reason, message string) {
	writeWarRoomAIJSON(w, status, warRoomAIAPIResponse{Status: "error", Message: message, Reason: reason})
}

func writeWarRoomAIJSON(w http.ResponseWriter, status int, response warRoomAIAPIResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
