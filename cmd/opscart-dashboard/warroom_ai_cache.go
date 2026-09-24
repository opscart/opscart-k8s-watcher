package main

import (
	"sync"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
)

const (
	// The Phase 2 POC keeps at most 64 analyses for 15 minutes in this
	// process. Results are intentionally neither persisted nor refreshed in
	// the background.
	warRoomAICacheCapacity = 64
	warRoomAICacheTTL      = 15 * time.Minute
	// Bump this whenever the evidence mapping, provider instructions, or
	// structured response schema changes so older results cannot be reused.
	// v3: tightened analysisInstructions (pkg/aianalysis/openai.go) — a
	// result generated under the old instructions must not be presented as
	// current.
	// v4: added the log_signals evidence type and its provider instructions
	// (pkg/aianalysis/types.go, openai.go) — a result cached under the old
	// contract must never be presented as a refined result, and an old
	// cached result must not silently masquerade as current once refinement
	// evidence exists.
	// v5: prompt provenance now distinguishes Kubernetes events from
	// locally derived log signals and forbids raw-log transmission advice.
	// v6: lifecycle and structured-severity categories plus prompt rules
	// that correlate lifecycle with Kubernetes evidence without claiming health.
	warRoomAIContractVersion = "warroom-ai-v6"
)

type warRoomAIRuntime struct {
	providerName string
	model        string
	cache        *warRoomAICache
}

func newWarRoomAIRuntime(providerName, model string) *warRoomAIRuntime {
	return &warRoomAIRuntime{
		providerName: providerName,
		model:        model,
		cache:        newWarRoomAICache(warRoomAICacheCapacity, warRoomAICacheTTL, time.Now),
	}
}

func (runtime *warRoomAIRuntime) key() string {
	if runtime == nil {
		return ""
	}
	return runtime.providerName + "\x00" + runtime.model + "\x00" + warRoomAIContractVersion
}

type warRoomAICacheEntry struct {
	ClusterKey string
	Selector   string
	// EvidenceHash is the full analysis-identity hash: the stable evidence
	// this specific Response was generated from — base structured
	// Kubernetes evidence alone for a plain generation, or base plus the
	// stable (bucketed) log_signals item for a refined one (see
	// appendLogSignalsEvidence, ai_log_signal_preview.go). It is used for
	// provider-call caching/deduplication (warRoomAICache.get), never for
	// page-render staleness.
	EvidenceHash string
	// BaseEvidenceHash is always just the current structured Kubernetes
	// evidence's hash — the exact same kind of value
	// captureWarRoomAIEvidence's capture.Hash always is, regardless of
	// whether this entry is refined. Page rendering (resolveInvestigationAIState,
	// investigation_ai.go) compares against THIS field, never EvidenceHash,
	// so a refined result does not appear stale the instant it is rendered:
	// comparing a combined refined hash directly against a freshly
	// recomputed base-only hash would never match.
	BaseEvidenceHash string
	RuntimeKey       string
	IssueIdentity    string
	// Provider and Model record the runtime identity that actually produced
	// Response, independent of whatever provider/model is currently
	// configured — a stale entry must keep showing what generated it.
	Provider         string
	Model            string
	EvidenceCaptured time.Time
	GeneratedAt      time.Time
	Response         aianalysis.AnalysisResponse
	// Evidence is the exact sanitized evidence transmitted for this
	// generation (see aianalysis.AnalysisRequest.Evidence). Kept alongside
	// Response so a stale result can still show the evidence it was
	// actually generated from, never the current scan's evidence.
	Evidence []aianalysis.EvidenceItem
	// BaseCapture is sanitized server-owned context for a transient snapshot gap.
	BaseCapture warRoomAICapture
	// Refined is true when Evidence includes an operator-triggered
	// log_signals item (see ai_log_signals.go) — the page must show the
	// refined-result disclosure instead of the plain generated-result one
	// whenever this is true.
	Refined bool

	// ResolvedNamespace/ResolvedPodName/ResolvedContainerName/ResolvedIssueType
	// are the server-resolved focus pod, target container, and canonical
	// issue type this analysis was generated or refined against — best
	// effort, empty when the issue type doesn't support log-derived signals
	// (resolveAILogSignalsTarget, ai_log_signal_preview.go). They exist
	// solely so a later preview request whose selector has momentarily
	// dropped out of the current War Room snapshot (e.g. a transient
	// rescan gap) can still be validated against a live Kubernetes
	// Pods.Get for this exact, previously server-resolved target — never
	// an arbitrary pod/container, and never anything the browser supplies.
	// See resolveAILogSignalsTargetWithFallback.
	ResolvedNamespace     string
	ResolvedPodName       string
	ResolvedContainerName string
	ResolvedIssueType     string

	accessed uint64
}

type warRoomAICache struct {
	mu       sync.Mutex
	entries  map[string]warRoomAICacheEntry
	inFlight map[string]struct{}
	capacity int
	ttl      time.Duration
	now      func() time.Time
	sequence uint64
}

func newWarRoomAICache(capacity int, ttl time.Duration, now func() time.Time) *warRoomAICache {
	return &warRoomAICache{
		entries:  make(map[string]warRoomAICacheEntry),
		inFlight: make(map[string]struct{}),
		capacity: capacity,
		ttl:      ttl,
		now:      now,
	}
}

func (cache *warRoomAICache) get(cluster, selector, evidenceHash, runtimeKey string) (warRoomAICacheEntry, bool, bool) {
	entry, ok := cache.latest(cluster, selector)
	if !ok {
		return warRoomAICacheEntry{}, false, false
	}
	current := entry.EvidenceHash == evidenceHash && entry.RuntimeKey == runtimeKey
	return entry, true, current
}

func (cache *warRoomAICache) latest(cluster, selector string) (warRoomAICacheEntry, bool) {
	if cache == nil {
		return warRoomAICacheEntry{}, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.pruneExpired(cache.now())
	entry, ok := cache.entries[selector]
	if !ok || entry.ClusterKey != cluster {
		return warRoomAICacheEntry{}, false
	}
	cache.sequence++
	entry.accessed = cache.sequence
	cache.entries[selector] = entry
	entry.Response = cloneAnalysisResponse(entry.Response)
	entry.Evidence = cloneEvidenceItems(entry.Evidence)
	entry.BaseCapture.Request.Evidence = cloneEvidenceItems(entry.BaseCapture.Request.Evidence)
	entry.BaseCapture.StableEvidence = cloneEvidenceItems(entry.BaseCapture.StableEvidence)
	return entry, true
}

func (cache *warRoomAICache) put(entry warRoomAICacheEntry) {
	if cache == nil || cache.capacity <= 0 {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.pruneExpired(cache.now())
	cache.sequence++
	entry.accessed = cache.sequence
	entry.Response = cloneAnalysisResponse(entry.Response)
	entry.Evidence = cloneEvidenceItems(entry.Evidence)
	entry.BaseCapture.Request.Evidence = cloneEvidenceItems(entry.BaseCapture.Request.Evidence)
	entry.BaseCapture.StableEvidence = cloneEvidenceItems(entry.BaseCapture.StableEvidence)
	cache.entries[entry.Selector] = entry
	for len(cache.entries) > cache.capacity {
		var oldestKey string
		var oldestAccess uint64
		for key, candidate := range cache.entries {
			if oldestKey == "" || candidate.accessed < oldestAccess {
				oldestKey, oldestAccess = key, candidate.accessed
			}
		}
		delete(cache.entries, oldestKey)
	}
}

func (cache *warRoomAICache) begin(cluster, selector string) bool {
	if cache == nil {
		return false
	}
	key := cluster + "\x00" + selector
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if _, exists := cache.inFlight[key]; exists {
		return false
	}
	cache.inFlight[key] = struct{}{}
	return true
}

func (cache *warRoomAICache) finish(cluster, selector string) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	delete(cache.inFlight, cluster+"\x00"+selector)
	cache.mu.Unlock()
}

// isInFlight reports whether a generation is currently running for this
// cluster/selector, so a second AI-tab request can render a GENERATING
// status instead of a stale or not-generated one.
func (cache *warRoomAICache) isInFlight(cluster, selector string) bool {
	if cache == nil {
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	_, exists := cache.inFlight[cluster+"\x00"+selector]
	return exists
}

func (cache *warRoomAICache) pruneExpired(now time.Time) {
	for key, entry := range cache.entries {
		if now.Sub(entry.GeneratedAt) > cache.ttl {
			delete(cache.entries, key)
		}
	}
}

func cloneAnalysisResponse(response aianalysis.AnalysisResponse) aianalysis.AnalysisResponse {
	if response.LikelyCauses != nil {
		cloned := make([]aianalysis.LikelyCause, len(response.LikelyCauses))
		copy(cloned, response.LikelyCauses)
		response.LikelyCauses = cloned
	}
	if response.Recommendations != nil {
		cloned := make([]aianalysis.Recommendation, len(response.Recommendations))
		copy(cloned, response.Recommendations)
		response.Recommendations = cloned
	}
	if response.EvidenceUsed != nil {
		response.EvidenceUsed = append([]string{}, response.EvidenceUsed...)
	}
	if response.MissingEvidence != nil {
		response.MissingEvidence = append([]string{}, response.MissingEvidence...)
	}
	return response
}

// cloneEvidenceItems returns an independent copy of items so a caller can
// never mutate cache-owned evidence through the returned slice. EvidenceItem
// holds only scalar string fields, so a shallow per-element copy is a full
// clone. A non-nil empty slice is preserved as non-nil.
func cloneEvidenceItems(items []aianalysis.EvidenceItem) []aianalysis.EvidenceItem {
	if items == nil {
		return nil
	}
	cloned := make([]aianalysis.EvidenceItem, len(items))
	copy(cloned, items)
	return cloned
}
