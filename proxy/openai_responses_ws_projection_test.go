package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesWebsocketProjectsOnlyGatewayStreamEnvelope(t *testing.T) {
	resetOpenAIResponsesWebsocketPool()
	frames := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, frame, err := conn.ReadMessage()
		if err != nil {
			return
		}
		frames <- frame
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_projected","status":"completed","output":[]}}`))
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(server.Close)
	t.Cleanup(resetOpenAIResponsesWebsocketPool)
	account := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL,
		APIKey: "synthetic-key", ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket}
	body := []byte(`{"model":"gpt-5.5","stream_id":"alpha","previous_response_id":"resp_parent","metadata":{"retained":"native"},"input":[{"role":"user","content":"fixture","stream_id":"nested-input"}],"tools":[{"type":"function","name":"test","parameters":{"type":"object","properties":{"stream_id":{"type":"string"}}}}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := ExecuteOpenAIResponsesRequest(ctx, account, body, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-frames:
		if gjson.GetBytes(frame, "stream_id").Exists() {
			t.Fatalf("gateway stream_id leaked onto the Responses relay socket: %s", frame)
		}
		if gjson.GetBytes(frame, "type").String() != "response.create" || !gjson.GetBytes(frame, "stream").Bool() || gjson.GetBytes(frame, "previous_response_id").String() != "resp_parent" {
			t.Fatalf("projection changed the native response/continuation envelope: %s", frame)
		}
		if gjson.GetBytes(frame, "input.0.stream_id").String() != "nested-input" || gjson.GetBytes(frame, "tools.0.parameters.properties.stream_id.type").String() != "string" || gjson.GetBytes(frame, "metadata.retained").String() != "native" {
			t.Fatalf("projection changed native fields or nested stream_id: %s", frame)
		}
	case <-ctx.Done():
		t.Fatal("loopback relay did not observe a request")
	}
	if gjson.GetBytes(body, "stream_id").String() != "alpha" {
		t.Fatal("projection mutated the original gateway routing envelope")
	}
}
