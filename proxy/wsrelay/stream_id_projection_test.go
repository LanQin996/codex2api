package wsrelay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestPrepareWebsocketBodyProjectsOnlyGatewayStreamEnvelope(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.5","stream_id":"alpha","previous_response_id":"resp_parent","metadata":{"stream_id":"nested-metadata"},"input":[{"stream_id":"nested-input"}],"tools":[{"parameters":{"properties":{"stream_id":{"type":"string"}}}}]}`)
	got := NewExecutor().prepareWebsocketBody(body, "test-session")
	if gjson.GetBytes(got, "stream_id").Exists() {
		t.Fatalf("gateway stream_id leaked into the outbound body: %s", got)
	}
	if gjson.GetBytes(got, "previous_response_id").String() != "resp_parent" || gjson.GetBytes(got, "type").String() != "response.create" {
		t.Fatalf("projection changed the response/continuation envelope: %s", got)
	}
	for field, expected := range map[string]string{"metadata.stream_id": "nested-metadata", "input.0.stream_id": "nested-input", "tools.0.parameters.properties.stream_id.type": "string"} {
		if gjson.GetBytes(got, field).String() != expected {
			t.Fatalf("projection removed nested %s: %s", field, got)
		}
	}
	if gjson.GetBytes(body, "stream_id").String() != "alpha" {
		t.Fatal("projection mutated the gateway routing envelope")
	}
}

func TestWebsocketNamedLaneProjectionPreservesLocalIsolation(t *testing.T) {
	previous := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(previous) })
	frames := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames <- frame
			// Keep each response in flight. The next named lane must acquire a
			// different transport rather than waiting for this lease to finish.
		}
	}))
	t.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "stream-projection-test"})
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	account := &auth.Account{DBID: 42, AccessToken: "synthetic-token", DynamicConcurrencyLimit: 4}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const session = "shared-downstream-session"
	var connections []*WsConnection
	for _, lane := range []string{"alpha", "beta"} {
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","stream_id":%q,"input":[{"role":"user","content":"fixture","stream_id":"nested-input"}],"tools":[{"type":"function","name":"test","parameters":{"type":"object","properties":{"stream_id":{"type":"string"}}}}]}`, lane))
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, "", "synthetic-key", nil, http.Header{}, "")
		if err != nil {
			t.Fatalf("named lane %s could not acquire independently: %v", lane, err)
		}
		t.Cleanup(func() { _ = response.Close() })
		connections = append(connections, response.conn)
		if gjson.GetBytes(body, "stream_id").String() != lane {
			t.Fatal("outbound preparation mutated the original routing envelope")
		}
		select {
		case frame := <-frames:
			if gjson.GetBytes(frame, "stream_id").Exists() {
				t.Fatalf("gateway stream_id leaked onto the Codex socket: %s", frame)
			}
			if gjson.GetBytes(frame, "input.0.stream_id").String() != "nested-input" || gjson.GetBytes(frame, "tools.0.parameters.properties.stream_id.type").String() != "string" {
				t.Fatalf("nested application/schema stream_id was removed: %s", frame)
			}
			if gjson.GetBytes(frame, "type").String() != "response.create" || !gjson.GetBytes(frame, "stream").Bool() || gjson.GetBytes(frame, "prompt_cache_key").String() != session {
				t.Fatalf("projection changed the response envelope or session cache: %s", frame)
			}
		case <-ctx.Done():
			t.Fatal("loopback upstream did not receive its response.create")
		}
	}
	if connections[0] == connections[1] || connections[0].PoolKey == connections[1].PoolKey {
		t.Fatal("stripping the upstream field collapsed independent local lanes")
	}
}
