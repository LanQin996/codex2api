package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gorilla/websocket"
)

func TestAccountBoundEgressResponsesWebsocketPool(t *testing.T) {
	var handshakes, proxied atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		handshakes.Add(1)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"local-test","status":"completed"}}`)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	boundProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if r.Method != http.MethodConnect {
			t.Errorf("unexpected method %s", r.Method)
			return
		}
		target, err := net.Dial("tcp", strings.TrimPrefix(upstream.URL, "http://"))
		if err != nil {
			t.Error(err)
			return
		}
		defer target.Close()
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go io.Copy(target, client)
		io.Copy(client, target)
	}))
	defer boundProxy.Close()
	oldPool := openAIResponsesWebsocketPool
	pool := newOpenAIResponsesWSPool()
	openAIResponsesWebsocketPool = pool
	defer func() {
		pool.mu.Lock()
		conns := append([]*openAIResponsesWSConn(nil), pool.conns...)
		pool.mu.Unlock()
		for _, conn := range conns {
			pool.retire(conn)
		}
		openAIResponsesWebsocketPool = oldPool
	}()
	account := &auth.Account{DBID: 998130, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "local-key", ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket}
	for _, ctx := range []context.Context{context.Background(), WithAccountBoundEgress(context.Background(), ""), WithAccountBoundEgress(context.Background(), ""), WithAccountBoundEgress(context.Background(), boundProxy.URL)} {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := ExecuteOpenAIResponsesRequest(ctx, account, []byte(`{"model":"gpt-5.4","input":"local-test"}`), "", nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	if handshakes.Load() != 3 || proxied.Load() != 1 {
		t.Errorf("handshakes=%d proxied=%d; want 3/1", handshakes.Load(), proxied.Load())
	}
}
