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
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// A call/result pair proves only that pair, not that the client replayed every
// ancestor. Observe the final native HTTP body rather than an execute hook.
func TestResponsesHTTPContextMergePreservesAncestorsAndRejectsAmbiguousReplay(t *testing.T) {
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

	parent := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"ANCESTOR_CONSTRAINT_MUST_SURVIVE"}`),
		json.RawMessage(`{"id":"fc_old_server_id","type":"function_call","call_id":"call_old","name":"lookup","arguments":"{\"q\":\"old\"}","namespace":"original"}`),
	}
	for _, stream := range []bool{false, true} {
		protocol := "JSON"
		if stream {
			protocol = "SSE"
		}
		for _, tc := range []struct {
			name        string
			input       string
			wantStatus  int
			foreign     bool
			userCount   int
			newCallWant int
		}{
			{name: "new call pair keeps parent", input: `[{"type":"function_call","call_id":"call_new","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_new","output":"new result"}]`, wantStatus: 200, userCount: 1, newCallWant: 1},
			{name: "parent output and a new pair keep ancestors", input: `[{"type":"function_call_output","call_id":"call_old","output":"old result"},{"type":"function_call","call_id":"call_new","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_new","output":"new result"}]`, wantStatus: 200, userCount: 1, newCallWant: 1},
			{name: "same message text can be a new turn", input: `[{"role":"user","content":"ANCESTOR_CONSTRAINT_MUST_SURVIVE"},{"type":"function_call","call_id":"call_new","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_new","output":"new result"}]`, wantStatus: 200, userCount: 2, newCallWant: 1},
			{name: "complete parent prefix is not duplicated", input: `[{"role":"user","content":"ANCESTOR_CONSTRAINT_MUST_SURVIVE"},{"id":"fc_client_replay_id","type":"function_call","call_id":"call_old","name":"lookup","arguments":"{\"q\":\"old\"}","namespace":"original"},{"type":"function_call_output","call_id":"call_old","output":"old result"}]`, wantStatus: 200, userCount: 1},
			{name: "partial old call replay is ambiguous", input: `[{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{\"q\":\"old\"}","namespace":"original"},{"type":"function_call_output","call_id":"call_old","output":"old result"}]`, wantStatus: 409},
			{name: "same id changed name is ambiguous", input: `[{"role":"user","content":"ANCESTOR_CONSTRAINT_MUST_SURVIVE"},{"type":"function_call","call_id":"call_old","name":"different","arguments":"{\"q\":\"old\"}","namespace":"original"},{"type":"function_call_output","call_id":"call_old","output":"old result"}]`, wantStatus: 409},
			{name: "same id changed arguments is ambiguous", input: `[{"role":"user","content":"ANCESTOR_CONSTRAINT_MUST_SURVIVE"},{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{\"q\":\"changed\"}","namespace":"original"},{"type":"function_call_output","call_id":"call_old","output":"old result"}]`, wantStatus: 409},
			{name: "same id changed namespace is ambiguous", input: `[{"role":"user","content":"ANCESTOR_CONSTRAINT_MUST_SURVIVE"},{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{\"q\":\"old\"}","namespace":"different"},{"type":"function_call_output","call_id":"call_old","output":"old result"}]`, wantStatus: 409},
			{name: "complete replay cannot bypass a foreign owner", input: `[{"role":"user","content":"ANCESTOR_CONSTRAINT_MUST_SURVIVE"},{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{\"q\":\"old\"}","namespace":"original"},{"type":"function_call_output","call_id":"call_old","output":"old result"}]`, wantStatus: 409, foreign: true},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				resetResponseCacheStateForTest(testResponseCacheConfig())
				setResponseCache(responseCacheOwner(1), "resp_merge_parent", parent)
				requests := make(chan []byte, 1)
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					requests <- readUpstreamRequestBody(r)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, wsContextTestSSE("resp_merge_child", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}`))
				}))
				defer upstream.Close()
				SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "context-merge-test"})
				store := newContinuationCodexStore()
				defer store.Stop()
				handler := NewHandler(store, nil, nil, nil)
				var input any
				if err := json.Unmarshal([]byte(tc.input), &input); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(map[string]any{"model": "gpt-5.5", "store": false, "stream": stream,
					"previous_response_id": "resp_merge_parent", "input": input})
				if err != nil {
					t.Fatal(err)
				}
				response := invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
					id := int64(1)
					if tc.foreign {
						id = 2
					}
					c.Set(contextAPIKeyID, id)
				}, handler.Responses, raw)
				if response.Code != tc.wantStatus {
					t.Fatalf("status=%d want=%d body=%s", response.Code, tc.wantStatus, response.Body.String())
				}
				if tc.wantStatus != http.StatusOK {
					if calls.Load() != 0 || gjson.GetBytes(response.Body.Bytes(), "error.code").String() != string(api.ErrCodeResponseContextUnavailable) {
						t.Fatalf("ambiguous/foreign history reached native upstream: calls=%d body=%s", calls.Load(), response.Body.String())
					}
					if strings.Contains(response.Body.String(), "resp_merge_parent") || strings.Contains(response.Body.String(), "ANCESTOR_CONSTRAINT_MUST_SURVIVE") {
						t.Fatal("context error echoed an identifier or ancestor")
					}
					return
				}
				if calls.Load() != 1 {
					t.Fatalf("native calls=%d want=1", calls.Load())
				}
				body := <-requests
				if gjson.GetBytes(body, "previous_response_id").Exists() {
					t.Fatal("native HTTP must expand the previous reference")
				}
				users, oldCalls, newCalls, newOutputs := 0, 0, 0, 0
				for _, item := range gjson.GetBytes(body, "input").Array() {
					if item.Get("role").String() == "user" && item.Get("content").String() == "ANCESTOR_CONSTRAINT_MUST_SURVIVE" {
						users++
					}
					if item.Get("type").String() == "function_call" && item.Get("call_id").String() == "call_old" {
						oldCalls++
					}
					if item.Get("call_id").String() == "call_new" {
						if item.Get("type").String() == "function_call" {
							newCalls++
						} else if item.Get("type").String() == "function_call_output" {
							newOutputs++
						}
					}
				}
				if users != tc.userCount || oldCalls != 1 || newCalls != tc.newCallWant || newOutputs != tc.newCallWant ||
					gjson.GetBytes(body, "input.0.content").String() != "ANCESTOR_CONSTRAINT_MUST_SURVIVE" {
					t.Fatalf("ancestor/call history was dropped or duplicated: users=%d old=%d new=%d/%d body=%s", users, oldCalls, newCalls, newOutputs, body)
				}
			})
		}
	}
}

func TestResponsesHTTPContextMergeDoesNotConstrainNativeRelayHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheStateForTest(testResponseCacheConfig())
	t.Cleanup(resetResponseCacheForTest)
	setResponseCache("anon", "resp_relay_parent", []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"local snapshot"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{}"}`),
	})
	var seen []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = readUpstreamRequestBody(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_native_relay","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	store := newContinuationRelayStore(upstream.URL)
	defer store.Stop()
	handler := NewHandler(store, nil, nil, nil)
	raw := []byte(`{"model":"gpt-5.5","store":false,"previous_response_id":"resp_relay_parent","input":[{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_old","output":"upstream owns the parent"}]}`)
	response := invokeResponsesHandler(t, handler.Responses, raw)
	if response.Code != http.StatusOK || gjson.GetBytes(seen, "previous_response_id").String() != "resp_relay_parent" ||
		gjson.GetBytes(seen, "input.#").Int() != 2 || strings.Contains(string(seen), "local snapshot") {
		t.Fatalf("native relay lost its original upstream-owned history: status=%d sent=%s body=%s", response.Code, seen, response.Body.String())
	}
}

// Keep the raw duplicate keys intact: marshaling a Go map would erase the
// first-value/last-value disagreement this boundary is intended to reject.
func TestResponsesHTTPContextMergeRejectsDuplicateTopLevelFields(t *testing.T) {
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
	const prefix = `[{"role":"user","content":"ANCESTOR_MUST_SURVIVE"},{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_old","output":"old"}]`
	const newPair = `[{"type":"function_call","call_id":"call_new","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_new","output":"new"}]`
	for _, relay := range []bool{false, true} {
		channel := "Codex"
		if relay {
			channel = "native relay"
		}
		for _, compact := range []bool{false, true} {
			endpoint := "Responses"
			if compact {
				endpoint = "Compact"
			}
			for _, tc := range []struct{ name, fields string }{
				{"first input proves prefix but last drops it", `"previous_response_id":"resp_dup_parent","input":` + prefix + `,"input":` + newPair},
				{"last input proves prefix but first does not", `"previous_response_id":"resp_dup_parent","input":` + newPair + `,"input":` + prefix},
				{"escaped input key is the same field", `"previous_response_id":"resp_dup_parent","input":` + prefix + `,"in\u0070ut":` + newPair},
				{"first known parent then missing parent", `"previous_response_id":"resp_dup_parent","previous_response_id":"resp_other","input":` + newPair},
				{"empty parent then hidden missing parent", `"previous_response_id":"","previous_response_id":"resp_other","input":` + newPair},
				{"known parent then null changes expansion", `"previous_response_id":"resp_dup_parent","previous_response_id":null,"input":` + newPair},
				{"escaped parent key is the same field", `"previous_response_id":"resp_dup_parent","previous_response_\u0069d":"resp_other","input":` + newPair},
				{"identical duplicate fields are still ambiguous", `"input":` + newPair + `,"input":` + newPair},
			} {
				t.Run(channel+"/"+endpoint+"/"+tc.name, func(t *testing.T) {
					resetResponseCacheStateForTest(testResponseCacheConfig())
					setResponseCache("anon", "resp_dup_parent", []json.RawMessage{
						json.RawMessage(`{"role":"user","content":"ANCESTOR_MUST_SURVIVE"}`),
						json.RawMessage(`{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{}"}`),
					})
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						if !relay && !compact {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, wsContextTestSSE("resp_duplicate_accepted", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}`))
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"resp_duplicate_accepted","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
					}))
					defer upstream.Close()
					SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "duplicate-context-test"})
					store := newContinuationCodexStore()
					if relay {
						store.Stop()
						store = newContinuationRelayStore(upstream.URL)
					}
					defer store.Stop()
					handler := NewHandler(store, nil, nil, nil)
					invoke := handler.Responses
					if compact {
						invoke = handler.ResponsesCompact
					}
					raw := []byte(`{"model":"gpt-5.5","store":false,` + tc.fields + `}`)
					if !gjson.ValidBytes(raw) {
						t.Fatal("duplicate-key fixture is not syntactically valid JSON")
					}
					response := invokeResponsesHandler(t, invoke, raw)
					if response.Code != http.StatusBadRequest || gjson.GetBytes(response.Body.Bytes(), "error.code").String() != string(api.ErrCodeInvalidRequest) || calls.Load() != 0 {
						t.Fatalf("ambiguous context envelope was not rejected before upstream: status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
					}
					if strings.Contains(response.Body.String(), "resp_dup_parent") || strings.Contains(response.Body.String(), "ANCESTOR_MUST_SURVIVE") {
						t.Fatal("duplicate-key error leaked a parent identifier or history")
					}
				})
			}
		}
	}
}

func TestResponsesHTTPContextEnvelopeKeepsNestedNamesAndNativeRelayNumbers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheStateForTest(testResponseCacheConfig())
	t.Cleanup(resetResponseCacheForTest)
	for _, compact := range []bool{false, true} {
		endpoint := "Responses"
		if compact {
			endpoint = "Compact"
		}
		t.Run(endpoint, func(t *testing.T) {
			requests := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- readUpstreamRequestBody(r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_native_fields","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer upstream.Close()
			store := newContinuationRelayStore(upstream.URL)
			defer store.Stop()
			handler := NewHandler(store, nil, nil, nil)
			invoke := handler.Responses
			if compact {
				invoke = handler.ResponsesCompact
			}
			raw := []byte(`{"model":"gpt-5.5","store":false,"previous_response_id":"resp_native_parent","input":[{"role":"user","content":"preserve native fields","metadata":{"input":"nested input","previous_response_id":"nested parent"}}],"metadata":{"input":"native metadata","previous_response_id":"native nested parent","large":9007199254740993},"text":{"format":{"type":"json_schema","name":"native","schema":{"type":"object","properties":{"input":{"type":"integer","minimum":9007199254740993},"previous_response_id":{"type":"string"}}}}}}`)
			response := invokeResponsesHandler(t, invoke, raw)
			if response.Code != http.StatusOK {
				t.Fatalf("unique native envelope rejected: status=%d body=%s", response.Code, response.Body.String())
			}
			body := <-requests
			for path, want := range map[string]string{
				"previous_response_id":                  "resp_native_parent",
				"input.0.metadata.input":                "nested input",
				"input.0.metadata.previous_response_id": "nested parent",
				"metadata.input":                        "native metadata",
				"metadata.previous_response_id":         "native nested parent",
			} {
				if got := gjson.GetBytes(body, path).String(); got != want {
					t.Errorf("native field changed at %s: got=%q want=%q", path, got, want)
				}
			}
			for _, path := range []string{"metadata.large", "text.format.schema.properties.input.minimum"} {
				if got := gjson.GetBytes(body, path).Raw; got != "9007199254740993" {
					t.Errorf("native integer precision changed at %s: %s", path, got)
				}
			}
		})
	}
}
