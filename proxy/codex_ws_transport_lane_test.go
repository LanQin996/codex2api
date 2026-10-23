package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestResolveCodexWebsocketTransportSessionKeySeparatesChildThread(t *testing.T) {
	const upstream = "isolated-upstream-session"
	parentHeaders := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"client-session"}}
	childHeaders := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"client-child-thread"}}

	if got := ResolveCodexWebsocketTransportSessionKey(upstream, parentHeaders); got != upstream {
		t.Fatalf("parent lane = %q, want shared upstream session", got)
	}
	child := ResolveCodexWebsocketTransportSessionKey(upstream, childHeaders)
	if child == upstream {
		t.Fatal("child thread collapsed onto shared upstream session")
	}
	if repeat := ResolveCodexWebsocketTransportSessionKey(upstream, childHeaders); repeat != child {
		t.Fatalf("child lane is not stable: first=%q repeat=%q", child, repeat)
	}
	for _, raw := range []string{upstream, "client-session", "client-child-thread"} {
		if strings.Contains(child, raw) {
			t.Fatalf("raw identity %q leaked into transport lane %q", raw, child)
		}
	}
}

func TestResolveCodexWebsocketTransportSessionKeyMetadataAndFallbacks(t *testing.T) {
	const upstream = "isolated-upstream-session"
	metadata := http.Header{}
	metadata.Set("X-Codex-Turn-Metadata", `{"session_id":"client-session","thread_id":"client-child-thread"}`)
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, metadata); got == upstream {
		t.Fatal("metadata child thread did not get a separate lane")
	}
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, http.Header{}); got != upstream {
		t.Fatalf("missing identity lane = %q, want legacy upstream session", got)
	}
	if got := ResolveCodexWebsocketTransportSessionKey("stateless-request", metadata); got != "stateless-request" {
		t.Fatalf("stateless lane = %q, want existing stateless routing", got)
	}

	equalMetadata := http.Header{}
	equalMetadata.Set("X-Codex-Turn-Metadata", `{"session_id":"same","thread_id":"same"}`)
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, equalMetadata); got != upstream {
		t.Fatalf("equal metadata identity lane = %q, want legacy upstream session", got)
	}
	malformed := http.Header{}
	malformed.Set("X-Codex-Turn-Metadata", `{not-json`)
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, malformed); got != upstream {
		t.Fatalf("malformed metadata lane = %q, want legacy upstream session", got)
	}
	explicitWins := metadata.Clone()
	explicitWins.Set("Thread-Id", "explicit-child")
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, explicitWins); got == ResolveCodexWebsocketTransportSessionKey(upstream, metadata) {
		t.Fatal("explicit Thread-Id did not take precedence over metadata thread_id")
	}
}

func TestResolveCodexWebsocketTransportSessionKeyRequestKindLanes(t *testing.T) {
	const upstream = "isolated-upstream-session"
	withKind := func(kind string) http.Header {
		h := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"client-session"}}
		h.Set("X-Codex-Turn-Metadata", `{"session_id":"client-session","thread_id":"client-session","request_kind":"`+kind+`"}`)
		return h
	}
	// 主道：turn / prewarm / compaction / 未声明都落回共享上游会话键。
	for _, kind := range []string{"turn", "prewarm", "compaction", ""} {
		if got := ResolveCodexWebsocketTransportSessionKey(upstream, withKind(kind)); got != upstream {
			t.Fatalf("request_kind=%q lane = %q, want shared upstream session", kind, got)
		}
	}
	memory := ResolveCodexWebsocketTransportSessionKey(upstream, withKind("memory"))
	if memory == upstream {
		t.Fatal("request_kind=memory did not get its own lane")
	}
	if repeat := ResolveCodexWebsocketTransportSessionKey(upstream, withKind("memory")); repeat != memory {
		t.Fatalf("memory lane is not stable: first=%q repeat=%q", memory, repeat)
	}
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, withKind("MEMORY ")); got != memory {
		t.Fatalf("request_kind normalization mismatch: %q vs %q", got, memory)
	}
	if other := ResolveCodexWebsocketTransportSessionKey(upstream, withKind("review")); other == memory || other == upstream {
		t.Fatalf("unknown request_kind should form its own lane, got %q", other)
	}
	if strings.Contains(memory, "memory") {
		t.Fatalf("raw lane label leaked into transport lane %q", memory)
	}

	// 子线程 + memory：与子线程 turn 也不同道。
	childTurn := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"client-child"}}
	childMemory := childTurn.Clone()
	childMemory.Set("X-Codex-Turn-Metadata", `{"thread_id":"client-child","request_kind":"memory"}`)
	if ResolveCodexWebsocketTransportSessionKey(upstream, childTurn) == ResolveCodexWebsocketTransportSessionKey(upstream, childMemory) {
		t.Fatal("child-thread memory turn collapsed onto the child-thread user turn lane")
	}

	// 头里没有 turn 元数据时回退请求体 client_metadata 内嵌 JSON（Desktop 走 HTTP）。
	headerOnly := http.Header{"Session-Id": []string{"client-session"}}
	body := []byte(`{"model":"gpt-5.5","input":"x","client_metadata":{"session_id":"client-session","x-codex-turn-metadata":"{\"thread_id\":\"client-session\",\"request_kind\":\"memory\"}"}}`)
	if got := ResolveCodexWebsocketTransportSessionKeyWithBody(upstream, headerOnly, body); got == upstream {
		t.Fatal("body-embedded request_kind=memory did not get its own lane")
	}
	turnBody := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"turn\"}"}}`)
	if got := ResolveCodexWebsocketTransportSessionKeyWithBody(upstream, headerOnly, turnBody); got != upstream {
		t.Fatalf("body-embedded request_kind=turn lane = %q, want shared upstream session", got)
	}
	// 头存在且合法时以头为准，不再看请求体。
	if got := ResolveCodexWebsocketTransportSessionKeyWithBody(upstream, withKind("turn"), body); got != upstream {
		t.Fatalf("valid header must win over body metadata, got %q", got)
	}
	if got := ResolveCodexWebsocketTransportSessionKeyWithBody("stateless-request", withKind("memory"), body); got != "stateless-request" {
		t.Fatalf("stateless lane = %q, want existing stateless routing", got)
	}
}

func TestResolveCodexWebsocketTransportSessionKeySubagentLane(t *testing.T) {
	const upstream = "isolated-upstream-session"
	base := http.Header{"Session-Id": []string{"client-session"}}
	guardian := base.Clone()
	guardian.Set("X-Openai-Subagent", "guardian_classifier")
	lane := ResolveCodexWebsocketTransportSessionKey(upstream, guardian)
	if lane == upstream {
		t.Fatal("subagent request without thread metadata did not get its own lane")
	}
	if repeat := ResolveCodexWebsocketTransportSessionKey(upstream, guardian); repeat != lane {
		t.Fatalf("subagent lane is not stable: first=%q repeat=%q", lane, repeat)
	}
	upper := base.Clone()
	upper.Set("X-Openai-Subagent", "Guardian_Classifier")
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, upper); got != lane {
		t.Fatalf("subagent value should be case-insensitive: %q vs %q", got, lane)
	}
	// 带线程标识的子代理请求走线程分道，不再按子代理值分道。
	threaded := guardian.Clone()
	threaded.Set("Thread-Id", "client-session")
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, threaded); got != upstream {
		t.Fatalf("subagent with root thread metadata lane = %q, want shared upstream session", got)
	}
	bodyOnly := []byte(`{"client_metadata":{"x-openai-subagent":"guardian_classifier"}}`)
	if got := ResolveCodexWebsocketTransportSessionKeyWithBody(upstream, base, bodyOnly); got != lane {
		t.Fatalf("body-embedded subagent lane = %q, want header-equivalent lane %q", got, lane)
	}
}

func TestCodexTransportSeparatesResponseStreams(t *testing.T) {
	headers := http.Header{"Session-Id": []string{"same"}, "Thread-Id": []string{"same"}}
	a := []byte(`{"stream_id":"planner"}`)
	b := []byte(`{"stream_id":"review"}`)
	first := ResolveCodexWebsocketTransportSessionKeyWithBody("upstream", headers, a)
	if first == "upstream" || first == ResolveCodexWebsocketTransportSessionKeyWithBody("upstream", headers, b) {
		t.Fatal("independent streams collapsed onto one transport lease")
	}
	if first != ResolveCodexWebsocketTransportSessionKeyWithBody("upstream", headers, a) {
		t.Fatal("same stream transport key was unstable")
	}
	if strings.Contains(first, "planner") {
		t.Fatal("raw stream identity leaked into pool key")
	}
}
