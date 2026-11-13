package usagehistory

import "testing"

func TestCacheHitRateUsesAggregatedInputTokens(t *testing.T) {
	var stats Stats
	stats.add(Event{Tokens: Tokens{Input: 100, CacheRead: 90}})
	stats.add(Event{Tokens: Tokens{Input: 900, CacheRead: 0}})
	stats.finish()
	if stats.CacheHitRate == nil || *stats.CacheHitRate != 9 {
		t.Fatalf("rate must be weighted by input tokens: %v", stats.CacheHitRate)
	}
	var empty Stats
	empty.finish()
	if empty.CacheHitRate != nil {
		t.Fatal("missing input must have unknown cache rate")
	}
}
