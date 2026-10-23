package proxy

import (
	"container/list"
	"testing"
	"time"
)

func newTestRequestCache(maxBytes, maxEntryBytes int, ttl time.Duration) *requestCache {
	return &requestCache{
		order:         list.New(),
		items:         make(map[[32]byte]*list.Element),
		maxBytes:      maxBytes,
		maxEntryBytes: maxEntryBytes,
		ttl:           ttl,
	}
}

func TestRequestCacheEnforcesByteBudget(t *testing.T) {
	c := newTestRequestCache(10, 10, time.Minute)
	c.put([32]byte{1}, openAIRequest{}, 6)
	c.put([32]byte{2}, openAIRequest{}, 6)
	if len(c.items) != 1 {
		t.Fatalf("entries = %d, want 1 after byte-budget eviction", len(c.items))
	}
	if c.bytes > 10 {
		t.Fatalf("bytes = %d, want <= 10", c.bytes)
	}
	if _, ok := c.get([32]byte{1}); ok {
		t.Fatal("oldest entry should have been evicted")
	}
	if _, ok := c.get([32]byte{2}); !ok {
		t.Fatal("newest entry should remain")
	}
}

func TestRequestCacheRejectsOversizedEntry(t *testing.T) {
	c := newTestRequestCache(100, 8, time.Minute)
	c.put([32]byte{1}, openAIRequest{}, 9)
	if len(c.items) != 0 {
		t.Fatal("entry larger than maxEntryBytes must not be cached")
	}
}

func TestRequestCacheExpiresEntries(t *testing.T) {
	c := newTestRequestCache(100, 100, time.Nanosecond)
	c.put([32]byte{1}, openAIRequest{}, 1)
	time.Sleep(time.Millisecond)
	if _, ok := c.get([32]byte{1}); ok {
		t.Fatal("expired entry must not be returned")
	}
	if len(c.items) != 0 {
		t.Fatal("expired entry should be removed on access")
	}
}

func TestEstimateParsedRequestSizeCountsMessageSlices(t *testing.T) {
	raw := []byte(`{"messages":[{},{},{}]}`) // ~24 bytes
	// 20000 条短消息的原始 JSON 很小，但切片常驻内存远超单条上限：必须按不可缓存处理。
	sparse := openAIRequest{Messages: make([]openAIMessage, 20000)}
	if got := estimateParsedRequestSize(raw, &sparse); got != parsedRequestSizeCeiling {
		t.Fatalf("20k-message estimate = %d, want clamped to %d", got, parsedRequestSizeCeiling)
	}
	small := openAIRequest{Messages: make([]openAIMessage, 4)}
	want := len(raw)*3 + 4*parsedMessageOverhead
	if got := estimateParsedRequestSize(raw, &small); got != want {
		t.Fatalf("estimate = %d, want %d", got, want)
	}
	if got := estimateParsedRequestSize(raw, &openAIRequest{}); got != len(raw)*3 {
		t.Fatalf("empty request estimate = %d, want %d", got, len(raw)*3)
	}
}
