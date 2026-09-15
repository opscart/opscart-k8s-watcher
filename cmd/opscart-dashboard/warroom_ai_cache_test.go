package main

import (
	"testing"
	"time"

	"github.com/opscart/opscart-k8s-watcher/pkg/aianalysis"
)

func TestWarRoomAICacheReuseAndStaleness(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cache := newWarRoomAICache(2, time.Minute, func() time.Time { return now })
	entry := warRoomAICacheEntry{
		ClusterKey: "prod", Selector: "issue-a", EvidenceHash: "evidence-a", RuntimeKey: "runtime-a",
		GeneratedAt: now, Response: aianalysis.AnalysisResponse{Summary: "current", EvidenceUsed: []string{"one"}},
	}
	cache.put(entry)

	got, found, current := cache.get("prod", "issue-a", "evidence-a", "runtime-a")
	if !found || !current || got.Response.Summary != "current" {
		t.Fatalf("expected current cache hit, got found=%t current=%t entry=%+v", found, current, got)
	}
	got.Response.EvidenceUsed[0] = "mutated"
	got, _, _ = cache.get("prod", "issue-a", "evidence-a", "runtime-a")
	if got.Response.EvidenceUsed[0] != "one" {
		t.Fatal("cache returned mutable response storage")
	}
	if _, found, current := cache.get("prod", "issue-a", "evidence-b", "runtime-a"); !found || current {
		t.Fatalf("evidence change should retain a stale result, got found=%t current=%t", found, current)
	}
	if _, found, current := cache.get("prod", "issue-a", "evidence-a", "runtime-b"); !found || current {
		t.Fatalf("runtime change should retain a stale result, got found=%t current=%t", found, current)
	}
	if _, found := cache.latest("other", "issue-a"); found {
		t.Fatal("cache entry crossed cluster boundary")
	}

	now = now.Add(2 * time.Minute)
	if _, found := cache.latest("prod", "issue-a"); found {
		t.Fatal("expired result remained in cache")
	}
}

func TestWarRoomAICacheCapacityAndDuplicateGeneration(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cache := newWarRoomAICache(2, time.Hour, func() time.Time { return now })
	put := func(selector string) {
		cache.put(warRoomAICacheEntry{ClusterKey: "prod", Selector: selector, GeneratedAt: now})
	}
	put("a")
	put("b")
	if _, ok := cache.latest("prod", "a"); !ok {
		t.Fatal("expected first entry before capacity eviction")
	}
	put("c")
	if _, ok := cache.latest("prod", "b"); ok {
		t.Fatal("least recently used entry was not evicted")
	}

	if !cache.begin("prod", "a") {
		t.Fatal("first generation should acquire in-flight slot")
	}
	if cache.begin("prod", "a") {
		t.Fatal("duplicate generation acquired the same in-flight slot")
	}
	if !cache.begin("other", "a") {
		t.Fatal("different cluster should have an independent in-flight slot")
	}
	cache.finish("prod", "a")
	if !cache.begin("prod", "a") {
		t.Fatal("finished generation did not release in-flight slot")
	}
}
