package proxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/codex2api/cache"
	"github.com/tidwall/gjson"
)

func TestPrepareResponsesBodyDetailedCacheHitAndDependentMiss(t *testing.T) {
	config := testResponseCacheConfig()
	resetResponseCacheStateForTest(config)
	setResponseCache("key:1", "resp_hit", []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}`),
	})

	hitRaw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_hit","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	hit := prepareResponsesBodyForOwnerDetailed(hitRaw, "key:1")
	if hit.CacheLookup.Kind != responseCacheLookupHit || !hit.RequiresLocalContext {
		t.Fatalf("hit preparation = %+v, want required cache hit", hit.CacheLookup)
	}
	if gjson.GetBytes(hit.Body, "previous_response_id").Exists() {
		t.Fatalf("Codex body retained previous_response_id: %s", hit.Body)
	}
	if got := gjson.GetBytes(hit.Body, "input.0.type").String(); got != "function_call" {
		t.Fatalf("first injected item type = %q, want function_call; body=%s", got, hit.Body)
	}

	missRaw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"function_call_output","call_id":"call_missing","output":"ok"}]}`)
	miss := prepareResponsesBodyForOwnerDetailed(missRaw, "key:1")
	if miss.CacheLookup.Kind != responseCacheLookupMiss || !miss.RequiresLocalContext {
		t.Fatalf("dependent miss = kind:%v required:%v, want ordinary required miss", miss.CacheLookup.Kind, miss.RequiresLocalContext)
	}
}

func TestPrepareResponsesBodyDetailedCompleteContextStillRequiresParent(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	backend := newRecordingResponseContextBackend(true)
	backend.bounded = cache.ResponseContextReadResult{Status: cache.ResponseContextReadCorrupt}
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})

	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_remote","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	got := prepareResponsesBodyForOwnerDetailed(raw, "key:1")
	if got.Bypassed || !got.RequiresLocalContext {
		t.Fatalf("preparation bypass=%v required=%v, want required parent proof before any replay bypass", got.Bypassed, got.RequiresLocalContext)
	}
	if _, gets := backend.counts(); gets != 1 {
		t.Fatalf("backend gets = %d, want parent lookup before bypassing expansion", gets)
	}
	if status, _, unavailable := responseCachePreparationFailure(got); !unavailable || status != http.StatusConflict {
		t.Fatalf("corrupt parent proof must reject complete tool input: status=%d unavailable=%v", status, unavailable)
	}
}

func TestPrepareResponsesBodyDetailedOrdinaryMissFailsClosed(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_unknown","input":[{"role":"user","content":"continue"}]}`)
	got := prepareResponsesBodyForOwnerDetailed(raw, "key:1")
	if got.CacheLookup.Kind != responseCacheLookupMiss || !got.RequiresLocalContext {
		t.Fatalf("preparation = kind:%v required:%v, want required parent miss", got.CacheLookup.Kind, got.RequiresLocalContext)
	}
	if status, _, unavailable := responseCachePreparationFailure(got); !unavailable || status != http.StatusConflict {
		t.Fatalf("ordinary continuation miss must return 409: status=%d unavailable=%v", status, unavailable)
	}
}

func TestPrepareResponsesBodyDetailedOrdinaryBackendErrorFailsClosed(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	backend := newRecordingResponseContextBackend(true)
	backend.boundedErr = errSyntheticBackend
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})
	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_unknown","input":[{"role":"user","content":"continue"}]}`)
	got := prepareResponsesBodyForOwnerDetailed(raw, "key:1")
	if got.CacheLookup.Kind != responseCacheLookupBackendError || !got.RequiresLocalContext {
		t.Fatalf("preparation = kind:%v required:%v, want required parent backend error", got.CacheLookup.Kind, got.RequiresLocalContext)
	}
	if status, _, unavailable := responseCachePreparationFailure(got); !unavailable || status != http.StatusServiceUnavailable {
		t.Fatalf("ordinary continuation backend error must return 503: status=%d unavailable=%v", status, unavailable)
	}
}

func TestPrepareCompactResponsesBodyDetailedMatchesResponseSemantics(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"mcp_tool_call_output","call_id":"call_1","output":"ok"}]}`)
	got := prepareCompactResponsesBodyForOwnerDetailed(raw, "key:1")
	if got.CacheLookup.Kind != responseCacheLookupMiss || !got.RequiresLocalContext {
		t.Fatalf("compact preparation = kind:%v required:%v, want required miss", got.CacheLookup.Kind, got.RequiresLocalContext)
	}
}

func TestPrepareResponsesWebSocketBodyDoesNotLookupResponseCache(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	backend := newRecordingResponseContextBackend(true)
	backend.boundedErr = errSyntheticBackend
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})

	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_ws","input":[{"type":"function_call_output","call_id":"call_ws","output":"ok"}]}`)
	got, _ := PrepareResponsesWebSocketBody(raw)
	if prev := gjson.GetBytes(got, "previous_response_id").String(); prev != "resp_ws" {
		t.Fatalf("WS previous_response_id = %q, want preserved", prev)
	}
	if _, gets := backend.counts(); gets != 0 {
		t.Fatalf("WS preparation performed %d backend lookups, want zero", gets)
	}
}

var errSyntheticBackend = &syntheticBackendError{}

type syntheticBackendError struct{}

func (*syntheticBackendError) Error() string { return "synthetic backend error" }
