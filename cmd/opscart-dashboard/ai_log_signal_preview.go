package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// istioProxyContainerName is excluded from log-derived signals entirely —
// see this file's package doc and the frozen scope this implements.
const istioProxyContainerName = "istio-proxy"

var (
	errAILogSignalsSelectionNotFound  = errors.New("selected issue is not active")
	errAILogSignalsUnsupportedIssue   = errors.New("log-derived signals are not supported for this issue")
	errAILogSignalsTargetUnknown      = errors.New("the target container could not be determined")
	errAILogSignalsIstioProxyRejected = errors.New("the sidecar proxy container is not eligible for log-derived signals")
	errAILogSignalsNoPreviousLogs     = errors.New("previous logs are unavailable for this container")
	errAILogSignalsHistoryUnavailable = errors.New("incident history is unavailable")
	// errAILogSignalsContextExpired is returned only by
	// resolveAILogSignalsTargetWithFallback, when the selector is absent
	// from the current War Room snapshot AND no still-valid GENERATED
	// analysis cache entry exists to fall back to.
	errAILogSignalsContextExpired = errors.New("this investigation context expired")
)

// Machine-readable reason codes shared by the JSON response's "reason"
// field and the safe operational log lines (see aiLogSignalsErrorResponse
// below and ai_refine.go). Each is a fixed, non-sensitive string — never a
// selector, pod/container name, or any other identifier — so the browser
// can key UI behavior off it and a log line can safely include it.
const (
	aiReasonIssueNotActive         = "issue_not_active"
	aiReasonUnsupportedIssue       = "unsupported_issue"
	aiReasonTargetUnknown          = "target_unknown"
	aiReasonIstioProxyRejected     = "istio_proxy_rejected"
	aiReasonNoPreviousLogs         = "no_previous_logs"
	aiReasonHistoryUnavailable     = "history_unavailable"
	aiReasonCooldown               = "cooldown"
	aiReasonPreviewExpired         = "preview_expired"
	aiReasonIssueChanged           = "issue_changed"
	aiReasonNoSignals              = "no_signals"
	aiReasonNoActionableSignals    = "no_actionable_signals"
	aiReasonInProgress             = "in_progress"
	aiReasonTimeout                = "timeout"
	aiReasonCanceled               = "canceled"
	aiReasonProviderError          = "provider_error"
	aiReasonAnalysisContextExpired = "analysis_context_expired"
	aiReasonTargetUnavailable      = "target_unavailable"
)

// Resolution-source labels for safe operational logging only (never
// returned to the browser) — see resolveAILogSignalsTargetWithFallback.
const (
	aiLogSignalsSourceCurrentSnapshot = "current_snapshot"
	aiLogSignalsSourceCachedContext   = "cached_context"
)

// logSignalsSupportedIssueType reports whether issue is the kind of
// pod/container failure previous-container logs can meaningfully speak to.
// Namespace and node findings, and privileged_container (never a restart
// signal), are excluded — see the frozen design's Included/Excluded scope.
func logSignalsSupportedIssueType(issue warRoomIssue) bool {
	if issue.IsNode || isNamespaceFinding(issue) {
		return false
	}
	switch store.CanonicalIssueType(issue.Type) {
	case store.IssueCrashLoop, store.IssueProbeFailure, store.IssueOOMKilled,
		store.IssueImagePullBackOff, store.IssueHighRestartCount:
		return true
	default:
		return false
	}
}

// resolveAILogSignalsTarget re-derives the focus pod and target container
// from the opaque selector, entirely from the cached scan — zero
// Kubernetes calls. It is the single source of truth for both the
// Investigate-deeper button's visibility (investigation_ai.go) and the
// preview endpoint's server-side re-resolution: the browser never supplies
// a pod or container name for this feature.
func resolveAILogSignalsTarget(scan *clusterScan, cluster string, db store.Store, selector string) (warRoomAISelection, string, error) {
	selection, err := findWarRoomAISelection(scan, cluster, db, selector)
	if err != nil {
		return warRoomAISelection{}, "", errAILogSignalsSelectionNotFound
	}
	if !logSignalsSupportedIssueType(selection.Issue) {
		return selection, "", errAILogSignalsUnsupportedIssue
	}
	if scan == nil || scan.aiPodEvidence == nil || !scan.aiPodEvidence.podsAvailable {
		return selection, "", errAILogSignalsTargetUnknown
	}
	pod, ok := scan.aiPodEvidence.pods[selection.Issue.Namespace+"/"+selection.Issue.Resource]
	if !ok {
		return selection, "", errAILogSignalsTargetUnknown
	}
	target, targetKnown, _ := warRoomAITargetContainer(pod.containers, selection.Issue)
	if !targetKnown {
		return selection, "", errAILogSignalsTargetUnknown
	}
	if target.name == istioProxyContainerName {
		return selection, "", errAILogSignalsIstioProxyRejected
	}
	restarts, err := strconv.ParseInt(target.restartCount, 10, 32)
	if err != nil || restarts <= 0 {
		return selection, "", errAILogSignalsNoPreviousLogs
	}
	return selection, target.name, nil
}

// resolveAILogSignalsTargetWithFallback first attempts
// resolveAILogSignalsTarget against the current War Room snapshot. If the
// selector cannot be found there — e.g. a transient rescan gap where a
// crash-looping pod momentarily drops out of one scan cycle — it falls
// back to the server's own existing GENERATED-analysis cache entry for
// this exact (cluster, selector), which retains the ORIGINAL
// server-resolved focus pod, target container, and issue type from when
// the analysis was generated or refined (see warRoomAICacheEntry's
// Resolved* fields). The browser supplies only the opaque selector; this
// fallback identity comes entirely from server-held state, never from
// anything the browser could supply.
//
// The fallback is bounded by the AI cache's own TTL (warRoomAICacheTTL):
// cache.latest already prunes entries older than that, so an entry this
// function cannot find is indistinguishable from one that never existed —
// both correctly report errAILogSignalsContextExpired here. In practice a
// preview is only ever attempted from a page that required a GENERATED
// analysis to render its control, so "no cache entry" at this point
// overwhelmingly means "it aged out," not "it never existed" — entries are
// never silently re-extended by a failed preview.
//
// This function never makes a Kubernetes call itself; live validation
// against the resolved target happens only in handleAILogSignalsPreview,
// which requests logs for exactly this bound container and never
// substitutes another pod, container, or replica.
func resolveAILogSignalsTargetWithFallback(scan *clusterScan, cluster string, db store.Store, selector string, cache *warRoomAICache) (selection warRoomAISelection, containerName, source string, err error) {
	selection, containerName, err = resolveAILogSignalsTarget(scan, cluster, db, selector)
	if err == nil {
		return selection, containerName, aiLogSignalsSourceCurrentSnapshot, nil
	}
	if !errors.Is(err, errAILogSignalsSelectionNotFound) {
		return selection, containerName, aiLogSignalsSourceCurrentSnapshot, err
	}

	entry, hasEntry := cache.latest(cluster, selector)
	if !hasEntry || entry.ResolvedNamespace == "" || entry.ResolvedPodName == "" || entry.ResolvedContainerName == "" {
		return warRoomAISelection{}, "", aiLogSignalsSourceCachedContext, errAILogSignalsContextExpired
	}
	fallbackIssue := warRoomIssue{
		Namespace: entry.ResolvedNamespace,
		Resource:  entry.ResolvedPodName,
		Type:      entry.ResolvedIssueType,
	}
	fallbackSelection := warRoomAISelection{
		Selector:    selector,
		Fingerprint: warRoomAIFingerprint(fallbackIssue),
		Identity:    entry.IssueIdentity,
		Issue:       fallbackIssue,
	}
	return fallbackSelection, entry.ResolvedContainerName, aiLogSignalsSourceCachedContext, nil
}

// aiLogSignalsResolvedTarget best-effort resolves the same server-side
// target resolveAILogSignalsTarget would, for retention on a new AI cache
// entry (warRoomAICacheEntry's Resolved* fields). It never fails the
// caller: an unsupported issue type or an unresolvable target simply
// leaves every field empty, since this is optional context for a later
// preview's fallback path, never a requirement for generation or
// refinement itself.
func aiLogSignalsResolvedTarget(scan *clusterScan, cluster string, db store.Store, selector string) (namespace, podName, containerName, issueType string) {
	selection, container, err := resolveAILogSignalsTarget(scan, cluster, db, selector)
	if err != nil {
		return "", "", "", ""
	}
	return selection.Issue.Namespace, selection.Issue.Resource, container, store.CanonicalIssueType(selection.Issue.Type)
}

// aiLogSignalsIncidentEpisode ties a preview to the specific incident
// occurrence it was captured for — a fingerprint plus its reopen count —
// so a preview from a since-resolved-and-reopened incident (a new episode
// of the same fingerprint) can never be redeemed at refine time.
func aiLogSignalsIncidentEpisode(db store.Store, cluster string, selection warRoomAISelection) (string, error) {
	incident, err := warRoomAIIncident(db, cluster, selection.Fingerprint, selection.Issue)
	if err != nil {
		return "", errAILogSignalsHistoryUnavailable
	}
	return fmt.Sprintf("%s\x00%d", selection.Fingerprint, incident.ReopenCount), nil
}

// aiLogSignalsErrorResponse maps a resolution error to an HTTP status, a
// fixed machine-readable reason code, and a safe display message — never
// derived from the error's own text, so it can never carry a selector,
// pod/container name, or any other identifier.
func aiLogSignalsErrorResponse(err error) (status int, reason, message string) {
	switch {
	case errors.Is(err, errAILogSignalsSelectionNotFound):
		return http.StatusNotFound, aiReasonIssueNotActive, "selected issue is not active"
	case errors.Is(err, errAILogSignalsUnsupportedIssue):
		return http.StatusUnprocessableEntity, aiReasonUnsupportedIssue, "log-derived signals are not supported for this issue"
	case errors.Is(err, errAILogSignalsIstioProxyRejected):
		return http.StatusUnprocessableEntity, aiReasonIstioProxyRejected, "the sidecar proxy container is not eligible for log-derived signals"
	case errors.Is(err, errAILogSignalsTargetUnknown):
		return http.StatusUnprocessableEntity, aiReasonTargetUnknown, "the target container could not be determined"
	case errors.Is(err, errAILogSignalsNoPreviousLogs):
		return http.StatusConflict, aiReasonNoPreviousLogs, "previous logs are unavailable for this container"
	case errors.Is(err, errAILogSignalsHistoryUnavailable):
		return http.StatusUnprocessableEntity, aiReasonHistoryUnavailable, "incident history is unavailable"
	default:
		return http.StatusInternalServerError, "", "request failed"
	}
}

// aiLogSignalsPreviewResponse is the exact wire shape returned to the
// browser: the opaque preview ID plus the safe preview fields only.
// ActionableSignalCount and CanRefine are computed here, server-side, from
// the fixed actionable-category policy (logSignalIsActionable,
// ai_log_signals.go) — the browser must treat them as authoritative and
// never independently re-derive whether refinement is allowed.
type aiLogSignalsPreviewResponse struct {
	Status                string           `json:"status"`
	PreviewID             string           `json:"preview_id"`
	Source                string           `json:"source"`
	ContainerRole         string           `json:"container_role"`
	LinesRequested        int64            `json:"lines_requested"`
	ByteLimit             int64            `json:"byte_limit"`
	BytesReceived         int              `json:"bytes_received"`
	RawLinesIncluded      int              `json:"raw_lines_included"`
	Signals               []logSignalCount `json:"signals"`
	ActionableSignalCount int              `json:"actionable_signal_count"`
	CanRefine             bool             `json:"can_refine"`
}

// handleAILogSignalsPreview implements POST /api/investigation/ai/log-signals/preview.
// It makes at most one Pods.Get and one Pods.GetLogs call, classifies the
// received bytes locally (see classifyLogSignals, ai_log_signals.go),
// discards them immediately, and returns only the safe preview plus an
// opaque preview ID. See this file's package doc for the full frozen
// contract.
func (srv *server) handleAILogSignalsPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeWarRoomAIError(w, http.StatusMethodNotAllowed, "use POST to preview diagnostic signals")
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
	if !srv.logsEnabled {
		writeWarRoomAIError(w, http.StatusNotFound, "container log preview is disabled")
		return
	}
	cluster, ok := srv.warRoomCluster(r)
	if !ok {
		writeWarRoomAIError(w, http.StatusBadRequest, "unknown cluster")
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

	if !srv.aiOperationCooldown.allow(cluster, selector, "log_signals_preview") {
		log.Printf("[%s] ai log signals: rejected reason=%s", displayName(cluster), aiReasonCooldown)
		writeWarRoomAIErrorWithReason(w, http.StatusTooManyRequests, aiReasonCooldown, "please wait before requesting another preview")
		return
	}

	state := srv.getState(cluster)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	selection, containerName, source, err := resolveAILogSignalsTargetWithFallback(scan, cluster, srv.db, selector, srv.aiRuntime.cache)
	if err != nil {
		if errors.Is(err, errAILogSignalsContextExpired) {
			log.Printf("[%s] ai log signals: rejected reason=%s", displayName(cluster), aiReasonAnalysisContextExpired)
			writeWarRoomAIErrorWithReason(w, http.StatusGone, aiReasonAnalysisContextExpired,
				"This investigation context expired. Return to War Room and reopen the current issue.")
			return
		}
		status, reason, message := aiLogSignalsErrorResponse(err)
		log.Printf("[%s] ai log signals: rejected reason=%s", displayName(cluster), reason)
		writeWarRoomAIErrorWithReason(w, status, reason, message)
		return
	}
	log.Printf("[%s] ai log signals: resolved source=%s", displayName(cluster), source)

	episodeKey, err := aiLogSignalsIncidentEpisode(srv.db, cluster, selection)
	if err != nil {
		status, reason, message := aiLogSignalsErrorResponse(err)
		log.Printf("[%s] ai log signals: rejected reason=%s", displayName(cluster), reason)
		writeWarRoomAIErrorWithReason(w, status, reason, message)
		return
	}

	clientset, err := srv.kubeClientFor(cluster, nil)
	if err != nil {
		log.Printf("[%s] ai log signals: kube client unavailable", displayName(cluster))
		writeWarRoomAIError(w, http.StatusBadGateway, "cluster connection failed")
		return
	}
	requestCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Exactly one Pods.Get, for exactly the server-resolved namespace/pod —
	// never a name the browser supplied, and never another replica.
	pod, err := clientset.CoreV1().Pods(selection.Issue.Namespace).Get(requestCtx, selection.Issue.Resource, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Printf("[%s] ai log signals: rejected reason=%s", displayName(cluster), aiReasonTargetUnavailable)
			writeWarRoomAIErrorWithReason(w, http.StatusNotFound, aiReasonTargetUnavailable, "the target pod no longer exists")
			return
		}
		if requestCtx.Err() == context.DeadlineExceeded {
			writeWarRoomAIError(w, http.StatusGatewayTimeout, "pod lookup timed out")
			return
		}
		log.Printf("[%s] ai log signals: pod lookup failed", displayName(cluster))
		writeWarRoomAIError(w, http.StatusBadGateway, "pod lookup failed")
		return
	}

	// containerName came only from resolveAILogSignalsTargetWithFallback
	// (either the current snapshot or the cached generated-analysis
	// context) — never from the browser — and the search below never
	// substitutes a different container if it's missing; that is reported
	// as target_unavailable, not silently redirected to another container.
	liveRestartCount := int32(-1)
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			liveRestartCount = status.RestartCount
			break
		}
	}
	if containerName == istioProxyContainerName {
		log.Printf("[%s] ai log signals: rejected reason=%s", displayName(cluster), aiReasonIstioProxyRejected)
		writeWarRoomAIErrorWithReason(w, http.StatusUnprocessableEntity, aiReasonIstioProxyRejected, "the sidecar proxy container is not eligible for log-derived signals")
		return
	}
	if liveRestartCount < 0 {
		log.Printf("[%s] ai log signals: rejected reason=%s", displayName(cluster), aiReasonTargetUnavailable)
		writeWarRoomAIErrorWithReason(w, http.StatusNotFound, aiReasonTargetUnavailable, "the target container no longer exists")
		return
	}
	if liveRestartCount == 0 {
		log.Printf("[%s] ai log signals: rejected reason=%s", displayName(cluster), aiReasonNoPreviousLogs)
		writeWarRoomAIErrorWithReason(w, http.StatusConflict, aiReasonNoPreviousLogs, "previous logs are unavailable for this container")
		return
	}

	options := &corev1.PodLogOptions{
		Container:  containerName,
		Previous:   true,
		TailLines:  int64Pointer(investigationLogTailLines),
		LimitBytes: int64Pointer(investigationLogMaxBytes),
		Timestamps: true,
	}
	logBytes, err := srv.podLogReader(requestCtx, clientset, selection.Issue.Namespace, selection.Issue.Resource, options)
	if err != nil {
		if requestCtx.Err() == context.DeadlineExceeded {
			writeWarRoomAIError(w, http.StatusGatewayTimeout, "log request timed out")
			return
		}
		log.Printf("[%s] ai log signals: reading previous logs: request failed", displayName(cluster))
		writeWarRoomAIError(w, http.StatusBadGateway, "logs could not be read")
		return
	}

	counts := classifyLogSignals(logBytes)
	bytesReceived := len(logBytes)
	logBytes = nil // discard immediately — never retained, logged, or returned

	preview := buildLogSignalsPreview(counts, bytesReceived)
	actionableCount := actionableLogSignalCount(preview.Signals)
	previewID := ""
	if actionableCount > 0 {
		var base warRoomAICapture
		if source == aiLogSignalsSourceCachedContext {
			entry, ok := srv.aiRuntime.cache.latest(cluster, selector)
			if !ok || entry.BaseCapture.Hash == "" || entry.BaseCapture.Hash != entry.BaseEvidenceHash || entry.RuntimeKey != srv.aiRuntime.key() {
				writeWarRoomAIErrorWithReason(w, http.StatusGone, aiReasonAnalysisContextExpired, "This investigation context expired. Return to War Room and reopen the current issue.")
				return
			}
			incident, episodeErr := warRoomAIIncident(srv.db, cluster, selection.Fingerprint, selection.Issue)
			if episodeErr != nil || incident.ReopenCount != entry.BaseCapture.Request.ReopenCount || !incident.FirstSeen.Equal(entry.BaseCapture.Request.FirstDetected) {
				writeWarRoomAIErrorWithReason(w, http.StatusConflict, aiReasonIssueChanged, "the issue changed since the analysis was captured; return to War Room")
				return
			}
			base = entry.BaseCapture
		} else {
			base, err = captureWarRoomAIEvidence(scan, cluster, srv.db, selection)
			if err != nil {
				writeWarRoomAIError(w, http.StatusUnprocessableEntity, warRoomAIUnavailableMessage(err))
				return
			}
		}
		previewID, err = srv.aiLogSignalsPreviewCache.put(aiLogSignalsPreviewEntry{
			ClusterKey: cluster, Selector: selector, EpisodeKey: episodeKey,
			Container: containerName, Preview: preview, Base: base,
			Target: aiRefineResolvedTarget{Namespace: selection.Issue.Namespace, PodName: selection.Issue.Resource,
				ContainerName: containerName, IssueType: store.CanonicalIssueType(selection.Issue.Type)},
		})
		if err != nil {
			log.Printf("[%s] ai log signals: rejected reason=preview_id_unavailable", displayName(cluster))
			writeWarRoomAIError(w, http.StatusInternalServerError, "preview could not be created")
			return
		}
	}

	log.Printf("[%s] ai log signals: completed categories=%d actionable=%d bytes=%d",
		displayName(cluster), len(preview.Signals), actionableCount, bytesReceived)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(aiLogSignalsPreviewResponse{
		Status: "ok", PreviewID: previewID,
		Source: preview.Source, ContainerRole: preview.ContainerRole,
		LinesRequested: preview.LinesRequested, ByteLimit: preview.ByteLimit,
		BytesReceived: preview.BytesReceived, RawLinesIncluded: preview.RawLinesIncluded,
		Signals:               preview.Signals,
		ActionableSignalCount: actionableCount,
		CanRefine:             actionableCount > 0,
	})
}
