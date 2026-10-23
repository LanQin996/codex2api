package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// These requests traverse the actual HTTP Handler and native HTTP executor.
// The loopback server observes the final wire body; no execute hook is replaced.
func TestResponsesHTTPContinuationFailsClosedWithoutParentSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })
	previousResin := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(previousResin) })
	t.Cleanup(resetResponseCacheForTest)

	for _, tc := range []struct {
		name       string
		stream     bool
		toolParent bool
		input      any
	}{
		{name: "plain string JSON", input: "return the previous marker"},
		{name: "plain array SSE", stream: true, input: []any{map[string]any{"role": "user", "content": "return the previous marker"}}},
		{name: "complete tool input cannot prove a missing parent", input: []any{
			map[string]any{"type": "function_call", "call_id": "call_client", "name": "lookup", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_client", "output": "client-result"},
		}},
		{name: "existing cached tool continuation", toolParent: true, input: []any{
			map[string]any{"type": "function_call_output", "call_id": "call_parent", "output": "tool-result"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetResponseCacheStateForTest(testResponseCacheConfig())
			requests := make(chan []byte, 2)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- readUpstreamRequestBody(r)
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"SAVED"}]}`
					if tc.toolParent {
						item = `{"type":"function_call","call_id":"call_parent","name":"lookup","arguments":"{}"}`
					}
					_, _ = io.WriteString(w, wsContextTestSSE("resp_http_parent", item))
					return
				}
				_, _ = io.WriteString(w, wsContextTestSSE("resp_http_child"))
			}))
			defer upstream.Close()
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "http-continuation-test"})
			store := newContinuationCodexStore()
			defer store.Stop()
			handler := NewHandler(store, nil, nil, nil)
			parent := invokeResponsesHandler(t, handler.Responses, []byte(`{"model":"gpt-5.5","store":false,"input":"Remember HISTORY_MUST_NOT_DISAPPEAR. Reply SAVED."}`))
			if parent.Code != http.StatusOK || gjson.GetBytes(parent.Body.Bytes(), "id").String() != "resp_http_parent" {
				t.Fatalf("parent status=%d body=%s", parent.Code, parent.Body.String())
			}
			<-requests
			if !tc.toolParent && len(getResponseCache("anon", "resp_http_parent")) != 0 {
				t.Fatal("plain text store:false unexpectedly acquired a persistent local snapshot")
			}
			childRaw, err := json.Marshal(map[string]any{"model": "gpt-5.5", "store": false, "stream": tc.stream,
				"previous_response_id": "resp_http_parent", "input": tc.input})
			if err != nil {
				t.Fatal(err)
			}
			child := invokeResponsesHandler(t, handler.Responses, childRaw)
			if tc.toolParent {
				if child.Code != http.StatusOK || calls.Load() != 2 {
					t.Fatalf("cached tool continuation failed: status=%d calls=%d body=%s", child.Code, calls.Load(), child.Body.String())
				}
				body := <-requests
				if gjson.GetBytes(body, "previous_response_id").Exists() || gjson.GetBytes(body, "input.#").Int() != 3 ||
					gjson.GetBytes(body, "input.1.call_id").String() != "call_parent" || gjson.GetBytes(body, "input.2.output").String() != "tool-result" {
					t.Fatalf("native HTTP replay lost parent/tool pairing: %s", body)
				}
			} else {
				if child.Code != http.StatusConflict || gjson.GetBytes(child.Body.Bytes(), "error.code").String() != string(api.ErrCodeResponseContextUnavailable) || calls.Load() != 1 {
					t.Fatalf("missing parent became a new upstream turn: status=%d calls=%d body=%s", child.Code, calls.Load(), child.Body.String())
				}
				if strings.Contains(child.Body.String(), "resp_http_parent") {
					t.Fatal("continuation error echoed a response identifier")
				}
			}
		})
	}
}

func TestResponsesHTTPOrdinaryContinuationBackendErrorAndRelayPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, relay := range []bool{false, true} {
		name := "Codex 503"
		if relay {
			name = "native Responses relay preserves previous id"
		}
		t.Run(name, func(t *testing.T) {
			resetResponseCacheStateForTest(testResponseCacheConfig())
			t.Cleanup(resetResponseCacheForTest)
			backend := newRecordingResponseContextBackend(true)
			backend.boundedErr = errSyntheticBackend
			SetResponseContextCache(backend)
			t.Cleanup(func() { SetResponseContextCache(nil); _ = backend.TokenCache.Close() })
			var seenBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenBody = readUpstreamRequestBody(r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_native_relay","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"RELAY_CHAIN_OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			t.Cleanup(upstream.Close)
			store := newContinuationCodexStore()
			if relay {
				store.Stop()
				store = newContinuationRelayStore(upstream.URL)
			}
			defer store.Stop()
			handler := NewHandler(store, nil, nil, nil)
			raw := []byte(`{"model":"gpt-5.5","store":false,"previous_response_id":"resp_missing","input":"Return the prior marker only."}`)
			response := invokeResponsesHandler(t, handler.Responses, raw)
			if relay {
				if response.Code != http.StatusOK || gjson.GetBytes(seenBody, "previous_response_id").String() != "resp_missing" ||
					gjson.GetBytes(seenBody, "store").Type != gjson.False || gjson.GetBytes(seenBody, "input").String() != "Return the prior marker only." {
					t.Fatalf("native relay lost upstream continuation: status=%d sent=%s response=%s", response.Code, seenBody, response.Body.String())
				}
			} else if response.Code != http.StatusServiceUnavailable || len(seenBody) != 0 || gjson.GetBytes(response.Body.Bytes(), "error.code").String() != string(api.ErrCodeServiceUnavailable) {
				t.Fatalf("backend fault was silently accepted: status=%d sent=%s response=%s", response.Code, seenBody, response.Body.String())
			}
		})
	}
}

func TestResponsesContinuationRelayFilterExcludesAdaptersWithoutNativeStorage(t *testing.T) {
	filter := responsesContinuationRelayFilter(nil)
	for _, upstream := range []string{"", auth.UpstreamGrok, auth.UpstreamAntigravity} {
		account := &auth.Account{UpstreamType: upstream, AccessToken: "synthetic", APIKey: "synthetic", BaseURL: "https://example.invalid"}
		if filter(account) {
			t.Fatalf("adapter %q cannot resolve a missing native Responses parent", upstream)
		}
	}
	account := &auth.Account{UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "synthetic", BaseURL: "https://example.invalid"}
	if !filter(account) || responsesContinuationRelayFilter(func(*auth.Account) bool { return false })(account) {
		t.Fatal("native Responses relay fallback ignored its existing account filter")
	}
}
