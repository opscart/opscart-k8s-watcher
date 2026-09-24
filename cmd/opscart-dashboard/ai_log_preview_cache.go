package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"
)

const (
	// aiLogSignalsPreviewCacheCapacity/TTL bound the in-memory preview cache
	// — short-lived, sanitized evidence only, never persisted. See this
	// package's ai_log_signals.go for what a preview may and may not
	// contain.
	aiLogSignalsPreviewCacheCapacity = 64
	aiLogSignalsPreviewTTL           = 5 * time.Minute

	// aiOperationCooldownWindow bounds how often the same selector+operation
	// pair may proceed to the expensive step (a Kubernetes call or a
	// provider call) — a lightweight, additive protection alongside the
	// existing warRoomAICache begin/finish in-flight guard, not a
	// replacement for it.
	aiOperationCooldownWindow   = 60 * time.Second
	aiOperationCooldownCapacity = 1024
)

// aiLogSignalsPreviewEntry binds fixed log categories to a sanitized base
// analysis capture and server-resolved identity. It contains no log bytes
// or log-derived free-form strings.
type aiLogSignalsPreviewEntry struct {
	ClusterKey string
	Selector   string
	// EpisodeKey ties this preview to one incident occurrence (fingerprint
	// + reopen count) so a preview from a since-resolved-and-reopened
	// incident can never be redeemed at refine time.
	EpisodeKey string
	Container  string
	Target     aiRefineResolvedTarget
	Base       warRoomAICapture
	Preview    logSignalsPreview
	expiresAt  time.Time
	accessed   uint64
}

// aiLogSignalsPreviewCache is a small, bounded, short-lived, in-memory
// cache of sanitized evidence — safe for concurrent access. It is never
// backed by persistent storage.
type aiLogSignalsPreviewCache struct {
	mu       sync.Mutex
	entries  map[string]aiLogSignalsPreviewEntry
	capacity int
	ttl      time.Duration
	now      func() time.Time
	sequence uint64
	random   io.Reader
}

func newAILogSignalsPreviewCache(capacity int, ttl time.Duration, now func() time.Time) *aiLogSignalsPreviewCache {
	return &aiLogSignalsPreviewCache{
		entries:  make(map[string]aiLogSignalsPreviewEntry),
		capacity: capacity,
		ttl:      ttl,
		now:      now,
		random:   rand.Reader,
	}
}

// put stores entry under a fresh, cryptographically random opaque ID and
// returns that ID. A full cache evicts its least-recently-accessed entry
// first, mirroring warRoomAICache.put's eviction policy.
func (cache *aiLogSignalsPreviewCache) put(entry aiLogSignalsPreviewEntry) (string, error) {
	if cache == nil || cache.capacity <= 0 {
		return "", errors.New("preview cache unavailable")
	}
	id, err := newAILogSignalsPreviewID(cache.random)
	if err != nil {
		return "", err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.pruneExpired(now)
	entry.expiresAt = now.Add(cache.ttl)
	entry.Base.Request.Evidence = cloneEvidenceItems(entry.Base.Request.Evidence)
	entry.Base.StableEvidence = cloneEvidenceItems(entry.Base.StableEvidence)
	entry.Preview.Signals = append([]logSignalCount(nil), entry.Preview.Signals...)
	cache.sequence++
	entry.accessed = cache.sequence
	cache.entries[id] = entry
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
	return id, nil
}

// get returns the entry for id if it exists and has not expired. It never
// deletes the entry on a successful read — a legitimate browser retry of
// the same refine request must still find it, and dedup happens at the
// analysis-cache/singleflight layer (warRoomAICache), not here.
func (cache *aiLogSignalsPreviewCache) get(id string) (aiLogSignalsPreviewEntry, bool) {
	if cache == nil || id == "" {
		return aiLogSignalsPreviewEntry{}, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.pruneExpired(now)
	entry, ok := cache.entries[id]
	if !ok {
		return aiLogSignalsPreviewEntry{}, false
	}
	cache.sequence++
	entry.accessed = cache.sequence
	cache.entries[id] = entry
	entry.Base.Request.Evidence = cloneEvidenceItems(entry.Base.Request.Evidence)
	entry.Base.StableEvidence = cloneEvidenceItems(entry.Base.StableEvidence)
	entry.Preview.Signals = append([]logSignalCount(nil), entry.Preview.Signals...)
	return entry, true
}

func (cache *aiLogSignalsPreviewCache) pruneExpired(now time.Time) {
	for key, entry := range cache.entries {
		if now.After(entry.expiresAt) {
			delete(cache.entries, key)
		}
	}
}

// size reports the current entry count — test-only introspection.
func (cache *aiLogSignalsPreviewCache) size() int {
	if cache == nil {
		return 0
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.pruneExpired(cache.now())
	return len(cache.entries)
}

func newAILogSignalsPreviewID(random io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// aiOperationCooldown debounces rapid, sequential (non-concurrent) repeat
// requests for the same cluster/selector/operation — a browser double-click
// or an impatient retry a few seconds later must not each reach the
// expensive step. It is deliberately not a rate limiter or a generic
// throttling framework: one fixed window, one small map, nothing else.
type aiOperationCooldown struct {
	mu       sync.Mutex
	last     map[string]time.Time
	window   time.Duration
	now      func() time.Time
	capacity int
}

func newAIOperationCooldown(window time.Duration, now func() time.Time) *aiOperationCooldown {
	return &aiOperationCooldown{last: make(map[string]time.Time), window: window, now: now, capacity: aiOperationCooldownCapacity}
}

// allow reports whether cluster/selector/operation may proceed now, and if
// so, records this instant as the start of a new cooldown window.
func (c *aiOperationCooldown) allow(cluster, selector, operation string) bool {
	if c == nil {
		return true
	}
	key := cluster + "\x00" + selector + "\x00" + operation
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for candidate, last := range c.last {
		if now.Sub(last) >= c.window {
			delete(c.last, candidate)
		}
	}
	if last, ok := c.last[key]; ok && now.Sub(last) < c.window {
		return false
	}
	if c.capacity <= 0 {
		return true
	}
	if len(c.last) >= c.capacity {
		var oldest string
		var earliest time.Time
		for candidate, last := range c.last {
			if oldest == "" || last.Before(earliest) || (last.Equal(earliest) && candidate < oldest) {
				oldest, earliest = candidate, last
			}
		}
		delete(c.last, oldest)
	}
	c.last[key] = now
	return true
}
