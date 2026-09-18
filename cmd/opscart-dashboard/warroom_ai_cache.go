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
	warRoomAIContractVersion = "warroom-ai-v3"
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
	ClusterKey    string
	Selector      string
	EvidenceHash  string
	RuntimeKey    string
	IssueIdentity string
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
