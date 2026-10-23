package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesWSUpstreamAuthenticationFailureIsLaneScoped(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			started, release := make(chan string, 1), make(chan struct{})
			client := newResponsesWSMultiplexClient(t, func(ctx context.Context, body []byte) (*http.Response, error) {
				if strings.Contains(string(body), "reject-upstream") {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_api_key","message":"synthetic upstream credential rejected"}}`))}, nil
				}
				started <- "healthy"
				select {
				case <-release:
					return responsesWSMultiplexResponse("resp_healthy"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			})
			responsesWSMultiplexSend(t, client, "healthy", "healthy")
			responsesWSMultiplexWaitStart(t, started, "healthy")
			responsesWSMultiplexSend(t, client, "rejected", "reject-upstream")
			frame := responsesWSMultiplexRead(t, client)
			if gjson.GetBytes(frame, "type").String() != "error" || gjson.GetBytes(frame, "stream_id").String() != "rejected" {
				t.Fatalf("upstream authentication error lost its lane: %s", frame)
			}
			close(release)
			responsesWSMultiplexReadTerminal(t, client, "healthy", "resp_healthy")
		})
	}
}

func TestResponsesWSNativeRelayRecoversLostContinuationWithoutDroppingNativeFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldRuntime := CurrentRuntimeSettings()
	settings := oldRuntime
	settings.CodexWSSilentRetry = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	resetResponseCacheForTest()
	resetOpenAIResponsesWebsocketPool()
	t.Cleanup(func() {
		ApplyRuntimeSettings(oldRuntime)
		resetResponseCacheForTest()
	})
	var calls atomic.Int32
	requests := make(chan []byte, 3)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			requests <- body
			switch calls.Add(1) {
			case 1:
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"parent-answer"}]}}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_relay_parent","status":"completed","output":[]}}`))
			case 2:
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"previous_response_not_found","message":"Previous response with id resp_relay_parent not found."}}}`))
			default:
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_relay_child","status":"completed","output":[]}}`))
			}
		}
	}))
	t.Cleanup(upstream.Close)
	// Close the pool before the loopback upstream, including assertion failures.
	t.Cleanup(resetOpenAIResponsesWebsocketPool)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL,
		APIKey: "synthetic-relay-key", PlanType: "api", Models: []string{"gpt-5.5"}, ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{strictResponsesCompatibilityHeader: {"true"}, "Session-Id": {"native-relay-recovery"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.5","stream_id":"main","store":false,"input":"parent-question"}`)); err != nil {
		t.Fatal(err)
	}
	responsesWSMultiplexReadTerminal(t, client, "main", "resp_relay_parent")
	payload := []byte(`{"type":"response.create","model":"gpt-5.5","stream_id":"main","store":false,"previous_response_id":"resp_relay_parent","input":"child-question","metadata":{"marker":"retained"},"max_output_tokens":123,"background":false,"text":{"format":{"type":"json_schema","name":"result","strict":true,"schema":{"type":"object","properties":{"n":{"type":"integer","minimum":9007199254740993}},"required":["n"],"additionalProperties":false}}}}`)
	if err := client.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatal(err)
	}
	responsesWSMultiplexReadTerminal(t, client, "main", "resp_relay_child")
	if calls.Load() != 3 {
		t.Fatalf("upstream calls=%d, want parent, lost continuation, recovered continuation", calls.Load())
	}
	<-requests // Parent creation.
	rejected, recovered := <-requests, <-requests
	if gjson.GetBytes(rejected, "previous_response_id").String() != "resp_relay_parent" || gjson.GetBytes(recovered, "previous_response_id").Exists() {
		t.Fatalf("recovery did not replace the lost ID: rejected=%s recovered=%s", rejected, recovered)
	}
	for _, ancestor := range []string{"parent-question", "parent-answer", "child-question"} {
		if !strings.Contains(gjson.GetBytes(recovered, "input").Raw, ancestor) {
			t.Errorf("recovered native input lost %q: %s", ancestor, recovered)
		}
	}
	canonical := func(raw string) string {
		t.Helper()
		decoder := json.NewDecoder(bytes.NewBufferString(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("invalid native field JSON %s: %v", raw, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	for _, field := range []string{"metadata", "max_output_tokens", "background", "text", "store"} {
		if got, want := canonical(gjson.GetBytes(recovered, field).Raw), canonical(gjson.GetBytes(payload, field).Raw); got != want {
			t.Errorf("recovered native field %s changed: got=%s want=%s", field, got, want)
		}
	}
}
