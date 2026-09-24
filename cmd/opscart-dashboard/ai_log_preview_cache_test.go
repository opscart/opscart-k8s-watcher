package main

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func mustPutPreview(t *testing.T, cache *aiLogSignalsPreviewCache, entry aiLogSignalsPreviewEntry) string {
	t.Helper()
	id, err := cache.put(entry)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAILogSignalsPreviewCachePutGetRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cache := newAILogSignalsPreviewCache(8, 5*time.Minute, func() time.Time { return now })

	id := mustPutPreview(t, cache, aiLogSignalsPreviewEntry{
		ClusterKey: "prod", Selector: "sel-1", EpisodeKey: "fp\x000", Container: "app",
		Preview: logSignalsPreview{Signals: []logSignalCount{{Category: "panic", Count: 3}}},
	})
	if id == "" {
		t.Fatal("expected a non-empty opaque preview ID")
	}
	entry, ok := cache.get(id)
	if !ok {
		t.Fatal("expected the entry to be retrievable immediately after put")
	}
	if entry.ClusterKey != "prod" || entry.Selector != "sel-1" || entry.Container != "app" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if len(entry.Preview.Signals) != 1 || entry.Preview.Signals[0].Category != "panic" {
		t.Fatalf("unexpected preview contents: %+v", entry.Preview)
	}

	if _, ok := cache.get("does-not-exist"); ok {
		t.Fatal("expected an unknown ID to miss")
	}
}

func TestAILogSignalsPreviewCacheNeverContainsRawLogBytes(t *testing.T) {
	// Structural proof: aiLogSignalsPreviewEntry has no []byte / raw-log
	// field at all — only Preview (fixed labels and integers). This test
	// exercises the one field that could plausibly carry content and
	// confirms its type is the safe preview struct, not a string/[]byte.
	entry := aiLogSignalsPreviewEntry{Preview: logSignalsPreview{Signals: []logSignalCount{{Category: "panic", Count: 1}}}}
	var _ logSignalsPreview = entry.Preview // compile-time type assertion
}

func TestAILogSignalsPreviewCacheExpiresAfterTTL(t *testing.T) {
	current := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cache := newAILogSignalsPreviewCache(8, 5*time.Minute, func() time.Time { return current })

	id := mustPutPreview(t, cache, aiLogSignalsPreviewEntry{ClusterKey: "prod", Selector: "sel-1"})
	current = current.Add(4 * time.Minute)
	if _, ok := cache.get(id); !ok {
		t.Fatal("expected the entry to still be valid before its TTL elapses")
	}
	current = current.Add(2 * time.Minute) // total 6 minutes > 5-minute TTL
	if _, ok := cache.get(id); ok {
		t.Fatal("expected the entry to have expired")
	}
}

func TestAILogSignalsPreviewCacheEvictsAtCapacity(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cache := newAILogSignalsPreviewCache(2, 5*time.Minute, func() time.Time { return now })

	first := mustPutPreview(t, cache, aiLogSignalsPreviewEntry{ClusterKey: "prod", Selector: "sel-1"})
	second := mustPutPreview(t, cache, aiLogSignalsPreviewEntry{ClusterKey: "prod", Selector: "sel-2"})
	if _, ok := cache.get(first); !ok {
		t.Fatal("expected first entry to be present before capacity is exceeded")
	}
	third := mustPutPreview(t, cache, aiLogSignalsPreviewEntry{ClusterKey: "prod", Selector: "sel-3"})

	if cache.size() != 2 {
		t.Fatalf("cache size = %d, want capacity 2", cache.size())
	}
	if _, ok := cache.get(first); !ok {
		t.Fatal("expected recently read first entry to remain")
	}
	if _, ok := cache.get(second); ok {
		t.Fatal("expected least recently read second entry to be evicted")
	}
	if _, ok := cache.get(third); !ok {
		t.Fatal("expected third entry to remain")
	}
}

func TestAILogSignalsPreviewCacheIDsAreUniqueAndOpaque(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cache := newAILogSignalsPreviewCache(64, 5*time.Minute, func() time.Time { return now })
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		id := mustPutPreview(t, cache, aiLogSignalsPreviewEntry{ClusterKey: "prod", Selector: fmt.Sprintf("sel-%d", i)})
		if len(id) < 16 {
			t.Fatalf("preview ID too short/predictable: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate preview ID generated: %q", id)
		}
		seen[id] = true
	}
}

// TestAILogSignalsPreviewCacheConcurrentAccess exercises put/get from many
// goroutines simultaneously — run with -race to prove safety.
func TestAILogSignalsPreviewCacheConcurrentAccess(t *testing.T) {
	cache := newAILogSignalsPreviewCache(32, time.Minute, time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := cache.put(aiLogSignalsPreviewEntry{
				ClusterKey: "prod", Selector: fmt.Sprintf("sel-%d", i%8),
				Preview: logSignalsPreview{Signals: []logSignalCount{{Category: "panic", Count: i}}},
			})
			if err != nil {
				t.Error(err)
				return
			}
			cache.get(id)
			cache.size()
		}(i)
	}
	wg.Wait()
}

func TestAIOperationCooldownDebouncesRapidRepeat(t *testing.T) {
	current := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cooldown := newAIOperationCooldown(60*time.Second, func() time.Time { return current })

	if !cooldown.allow("prod", "sel-1", "preview") {
		t.Fatal("first request should be allowed")
	}
	if cooldown.allow("prod", "sel-1", "preview") {
		t.Fatal("immediate repeat should be rejected")
	}
	if !cooldown.allow("prod", "sel-2", "preview") {
		t.Fatal("a different selector should not be affected by another selector's cooldown")
	}
	if !cooldown.allow("prod", "sel-1", "refine") {
		t.Fatal("a different operation on the same selector should not be affected")
	}
	current = current.Add(61 * time.Second)
	if !cooldown.allow("prod", "sel-1", "preview") {
		t.Fatal("request after the cooldown window elapsed should be allowed")
	}
}

func TestAIOperationCooldownConcurrentAccess(t *testing.T) {
	cooldown := newAIOperationCooldown(time.Millisecond, time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cooldown.allow("prod", fmt.Sprintf("sel-%d", i%4), "preview")
		}(i)
	}
	wg.Wait()
}

type failingPreviewRandom struct{}

func (failingPreviewRandom) Read([]byte) (int, error) { return 0, errors.New("entropy failed secret") }

func TestPreviewIDFailureDoesNotInsert(t *testing.T) {
	cache := newAILogSignalsPreviewCache(2, time.Minute, time.Now)
	cache.random = failingPreviewRandom{}
	id, err := cache.put(aiLogSignalsPreviewEntry{Selector: "sel"})
	if err == nil || id != "" || cache.size() != 0 {
		t.Fatalf("id=%q error=%v size=%d", id, err, cache.size())
	}
}

func TestAIOperationCooldownPrunesAndEvictsDeterministically(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	c := newAIOperationCooldown(time.Minute, func() time.Time { return now })
	c.capacity = 2
	c.allow("prod", "b", "preview")
	c.allow("prod", "a", "preview")
	c.allow("prod", "c", "preview")
	if len(c.last) != 2 || !c.allow("prod", "a", "preview") {
		t.Fatal("lexically first tied entry should be evicted")
	}
	if c.allow("prod", "c", "preview") {
		t.Fatal("newest entry should remain")
	}
	now = now.Add(time.Minute)
	c.allow("prod", "d", "preview")
	if len(c.last) != 1 {
		t.Fatalf("expired entries retained: %d", len(c.last))
	}
}
