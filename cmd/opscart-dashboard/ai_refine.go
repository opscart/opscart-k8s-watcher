package main

import (
	"context"
	"errors"
	"log"
	"mime"
	"net/http"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
	"github.com/opscart/opscart-k8s-watcher/pkg/store"
)

// warRoomAIRefinedDisclosure is the clear, fixed disclosure text shown for
// a refined analysis result — never presented as an ordinary generated
// result. See templates/investigation_ai.html.
const warRoomAIRefinedDisclosure = "Analysis refined with locally derived diagnostic signals. Raw logs were not sent."

// handleAIRefine implements POST /api/investigation/ai/refine. The browser
// submits only the opaque issue selector and an opaque preview ID — never
// evidence, never a pod/container name, and never raw log content. It
// makes zero Kubernetes calls and at most one AI provider call, reusing
// the existing warRoomAICache get/begin/finish/put cache and singleflight
// guard so a concurrent or retried identical refinement shares one call.
func (srv *server) handleAIRefine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeWarRoomAIError(w, http.StatusMethodNotAllowed, "use POST to refine an AI analysis")
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
	if len(r.PostForm) != 2 || len(r.PostForm["issue"]) != 1 || len(r.PostForm["preview_id"]) != 1 {
		writeWarRoomAIError(w, http.StatusBadRequest, "invalid request")
		return
	}
	selector := r.PostForm.Get("issue")
	previewID := r.PostForm.Get("preview_id")

	previewEntry, found := srv.aiLogSignalsPreviewCache.get(previewID)
	if !found || previewEntry.ClusterKey != cluster || previewEntry.Selector != selector {
		log.Printf("[%s] ai refinement: rejected reason=%s", displayName(cluster), aiReasonPreviewExpired)
		writeWarRoomAIErrorWithReason(w, http.StatusGone, aiReasonPreviewExpired, "the evidence preview has expired; request a new preview")
		return
	}
	if len(previewEntry.Preview.Signals) == 0 {
		log.Printf("[%s] ai refinement: rejected reason=%s", displayName(cluster), aiReasonNoSignals)
		writeWarRoomAIErrorWithReason(w, http.StatusUnprocessableEntity, aiReasonNoSignals, "no supported diagnostic signals were detected; nothing to refine")
		return
	}
	// unknown_error_marker alone is not new, specific evidence — it is not
	// sufficient grounds for a paid refinement call. This check runs before
	// any cache lookup or provider work, so it structurally guarantees zero
	// provider calls for this case.
	if !hasActionableLogSignal(previewEntry.Preview.Signals) {
		log.Printf("[%s] ai refinement: rejected reason=%s", displayName(cluster), aiReasonNoActionableSignals)
		writeWarRoomAIErrorWithReason(w, http.StatusUnprocessableEntity, aiReasonNoActionableSignals, "no actionable diagnostic signal was detected; nothing to refine")
		return
	}

	state := srv.getState(cluster)
	state.mu.RLock()
	scan := state.scan
	state.mu.RUnlock()

	selection, err := findWarRoomAISelection(scan, cluster, srv.db, selector)
	gap := err != nil
	if gap {
		selection = previewEntry.Base.Selection
	}
	if selection.Selector != selector || selection.Issue.Namespace != previewEntry.Target.Namespace ||
		selection.Issue.Resource != previewEntry.Target.PodName ||
		store.CanonicalIssueType(selection.Issue.Type) != previewEntry.Target.IssueType {
		log.Printf("[%s] ai refinement: rejected reason=%s", displayName(cluster), aiReasonIssueChanged)
		writeWarRoomAIErrorWithReason(w, http.StatusConflict, aiReasonIssueChanged, "the issue changed since the preview was captured; request a new preview")
		return
	}
	episodeKey, err := aiLogSignalsIncidentEpisode(srv.db, cluster, selection)
	if err != nil || episodeKey != previewEntry.EpisodeKey {
		log.Printf("[%s] ai refinement: rejected reason=%s", displayName(cluster), aiReasonIssueChanged)
		writeWarRoomAIErrorWithReason(w, http.StatusConflict, aiReasonIssueChanged, "the issue changed since the preview was captured; request a new preview")
		return
	}

	capture := previewEntry.Base
	if gap {
		entry, ok := srv.aiRuntime.cache.latest(cluster, selector)
		if !ok || entry.RuntimeKey != srv.aiRuntime.key() || entry.BaseEvidenceHash != capture.Hash ||
			entry.IssueIdentity != selection.Identity || entry.ResolvedNamespace != previewEntry.Target.Namespace ||
			entry.ResolvedPodName != previewEntry.Target.PodName || entry.ResolvedContainerName != previewEntry.Target.ContainerName {
			writeWarRoomAIErrorWithReason(w, http.StatusGone, aiReasonAnalysisContextExpired, "This investigation context expired. Return to War Room and reopen the current issue.")
			return
		}
	} else {
		_, currentContainer, targetErr := resolveAILogSignalsTarget(scan, cluster, srv.db, selector)
		current, captureErr := captureWarRoomAIEvidence(scan, cluster, srv.db, selection)
		if targetErr != nil || currentContainer != previewEntry.Target.ContainerName || captureErr != nil || current.Hash != capture.Hash || current.Selection.Identity != capture.Selection.Identity {
			writeWarRoomAIErrorWithReason(w, http.StatusConflict, aiReasonIssueChanged, "the issue changed since the preview was captured; request a new preview")
			return
		}
	}
	refined, err := appendLogSignalsEvidence(capture, previewEntry.Preview)
	if err != nil {
		writeWarRoomAIError(w, http.StatusUnprocessableEntity, "refined evidence exceeds analysis limits")
		return
	}

	if _, found, current := srv.aiRuntime.cache.get(cluster, selector, refined.Hash, srv.aiRuntime.key()); found && current {
		log.Printf("[%s] ai refinement: completed cache_hit=true", displayName(cluster))
		writeWarRoomAIJSON(w, http.StatusOK, warRoomAIAPIResponse{Status: "cached", Message: "The current refined analysis is already available."})
		return
	}

	// Concurrent or closely-retried identical refinements (same selector,
	// same stable refined-evidence hash) must share one provider call
	// rather than each independently racing to call it. aiRefineGroup keys
	// in-flight work by exactly that pair; a different selector or a
	// different evidence hash never joins an unrelated call. See
	// performAIRefine for the shared unit of work itself — it never
	// touches r (this specific request), so any single waiter's context
	// canceling only stops that waiter from watching, never the shared
	// call or any other waiter.
	resolvedTarget := previewEntry.Target
	resultChan := srv.aiRefineGroup.DoChan(aiRefineSingleFlightKey(cluster, selector, refined.Hash), func() (any, error) {
		return srv.performAIRefine(cluster, selector, selection.Identity, capture, refined, resolvedTarget), nil
	})
	select {
	case <-r.Context().Done():
		// This waiter went away — the shared call (and every other waiter
		// still watching it) is completely unaffected.
		return
	case shared := <-resultChan:
		outcome := shared.Val.(aiRefineOutcome)
		writeWarRoomAIJSON(w, outcome.httpStatus, warRoomAIAPIResponse{Status: outcome.status, Message: outcome.message, Reason: outcome.reason})
	}
}

// aiRefineOutcome is the one safe, publishable result of a single refine
// provider call — every concurrent/waiting request receives an identical
// copy of this value. It never carries a raw provider error or response
// body; performAIRefine has already reduced any failure to one of the
// same fixed, safe messages handleAIRefine used before this call was
// shared.
type aiRefineOutcome struct {
	httpStatus int
	status     string
	message    string
	reason     string
}

// aiRefineSingleFlightKey identifies one unit of shareable refine work.
// Two requests join the same in-flight call only when both the selector
// and the stable refined-evidence hash match exactly.
func aiRefineSingleFlightKey(cluster, selector, evidenceHash string) string {
	return cluster + "\x00" + selector + "\x00" + evidenceHash
}

// performAIRefine is the shared unit of work behind aiRefineGroup: exactly
// one call runs per distinct (cluster, selector, evidenceHash) at a time,
// regardless of how many HTTP requests joined it. It deliberately never
// uses any single request's context: the provider call runs under a
// server-owned context bounded only by srv.aiTimeout
// (context.WithTimeout(context.Background(), srv.aiTimeout)), never
// context.Background() alone and never any one caller's r.Context(). This
// means one waiter disconnecting can never cancel the call other waiters
// are still depending on, while the call itself still cannot run forever
// if every waiter disconnects — it is bounded by the same configured AI
// timeout as any other provider call (see srv.aiTimeout's doc comment,
// server.go). http.Client.Timeout alone would not be enough here: Azure
// Foundry authentication/token acquisition (pkg/aianalysis/auth.go)
// happens before the outbound HTTP request is even built, so only a
// context deadline covers the whole operation. The existing
// warRoomAICache begin/finish guard is reused unchanged (preserving
// initial-generation's own behavior) so a concurrent initial
// generate/regenerate for the same selector still cannot race with this
// refinement's cache write, and its finish() reliably clears the
// in-flight entry on every exit path (success, failure, or timeout).
//
// The operation cooldown is checked here, not in handleAIRefine, precisely
// because this function runs at most once per shared key: a follower
// joining an already-in-flight identical refinement must never be
// rejected by the cooldown meant to debounce someone STARTING new work.
// aiRefineResolvedTarget carries the server-resolved focus pod/target
// container/issue type forward onto the refined cache entry (see
// warRoomAICacheEntry's Resolved* fields), exactly as the initial-generate
// path does via aiLogSignalsResolvedTarget — refining an analysis keeps
// its fallback-resolution context just as fresh as generating one does.
type aiRefineResolvedTarget struct {
	Namespace     string
	PodName       string
	ContainerName string
	IssueType     string
}

func (srv *server) performAIRefine(cluster, selector, issueIdentity string, base warRoomAICapture, refined warRoomAICapture, resolvedTarget aiRefineResolvedTarget) aiRefineOutcome {
	if !srv.aiOperationCooldown.allow(cluster, selector, "refine") {
		log.Printf("[%s] ai refinement: rejected reason=%s", displayName(cluster), aiReasonCooldown)
		return aiRefineOutcome{httpStatus: http.StatusTooManyRequests, status: "error", message: "please wait before requesting another refinement", reason: aiReasonCooldown}
	}
	if !srv.aiRuntime.cache.begin(cluster, selector) {
		log.Printf("[%s] ai refinement: rejected reason=%s", displayName(cluster), aiReasonInProgress)
		return aiRefineOutcome{httpStatus: http.StatusConflict, status: "error", message: "analysis is already in progress for this issue", reason: aiReasonInProgress}
	}
	defer srv.aiRuntime.cache.finish(cluster, selector)

	log.Printf("[%s] ai refinement: started", displayName(cluster))
	sharedCtx, cancel := context.WithTimeout(context.Background(), srv.aiTimeout)
	defer cancel()
	response, err := srv.aiProvider.Analyze(sharedCtx, refined.Request)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			log.Printf("[%s] ai refinement: failed reason=%s", displayName(cluster), aiReasonCanceled)
			return aiRefineOutcome{httpStatus: http.StatusRequestTimeout, status: "error", message: "AI analysis was canceled", reason: aiReasonCanceled}
		case errors.Is(err, context.DeadlineExceeded):
			log.Printf("[%s] ai refinement: failed reason=%s", displayName(cluster), aiReasonTimeout)
			return aiRefineOutcome{httpStatus: http.StatusGatewayTimeout, status: "error", message: "AI analysis timed out", reason: aiReasonTimeout}
		default:
			log.Printf("[%s] ai refinement: failed reason=%s", displayName(cluster), aiReasonProviderError)
			return aiRefineOutcome{httpStatus: http.StatusBadGateway, status: "error", message: "AI analysis could not be generated", reason: aiReasonProviderError}
		}
	}
	if response == nil || aianalysis.ValidateResponse(response) != nil {
		log.Printf("[%s] ai refinement: failed reason=%s", displayName(cluster), aiReasonProviderError)
		return aiRefineOutcome{httpStatus: http.StatusBadGateway, status: "error", message: "AI analysis could not be generated", reason: aiReasonProviderError}
	}

	entry := warRoomAICacheEntry{
		ClusterKey: cluster, Selector: selector, EvidenceHash: refined.Hash, BaseEvidenceHash: base.Hash,
		RuntimeKey: srv.aiRuntime.key(), IssueIdentity: issueIdentity,
		Provider: srv.aiRuntime.providerName, Model: srv.aiRuntime.model,
		EvidenceCaptured: refined.CapturedAt, GeneratedAt: srv.aiRuntime.cache.now(), Response: *response,
		Evidence: refined.Request.Evidence, Refined: true,
		BaseCapture:       base,
		ResolvedNamespace: resolvedTarget.Namespace, ResolvedPodName: resolvedTarget.PodName,
		ResolvedContainerName: resolvedTarget.ContainerName, ResolvedIssueType: resolvedTarget.IssueType,
	}
	srv.aiRuntime.cache.put(entry)

	log.Printf("[%s] ai refinement: completed cache_hit=false", displayName(cluster))
	return aiRefineOutcome{httpStatus: http.StatusOK, status: "refined", message: warRoomAIRefinedDisclosure}
}
