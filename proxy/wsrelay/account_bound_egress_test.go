package wsrelay

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

func TestAccountBoundEgressWebsocketRoutesAndPoolIsolation(t *testing.T) {
	old := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(old) })
	var direct, bound atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		direct.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	// The CONNECT proxy only tunnels to this test's local server.
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bound.Add(1)
		if r.Method != http.MethodConnect {
			t.Errorf("expected CONNECT, got %s", r.Method)
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
	defer proxyServer.Close()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: "http://127.0.0.1:1", PlatformName: "test"})
	m := NewManager()
	defer m.Stop()
	// Simulate an inherited dialer/environment proxy; marked direct must clear it.
	proxyURL, _ := url.Parse(proxyServer.URL)
	m.dialer.Proxy = http.ProxyURL(proxyURL)
	account := &auth.Account{DBID: 998201, ProxyURL: proxyServer.URL}
	wsURL := "ws" + strings.TrimPrefix(upstream.URL, "http")
	acquire := func(ctx context.Context) *WsConnection {
		t.Helper()
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		wc, pr, err := m.AcquireConnection(ctx, account, wsURL, "shared-session", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		wc.cancelUnsentReadLease(pr.RequestID)
		wc.session.RemovePendingRequest(pr.RequestID)
		return wc
	}
	business := acquire(context.Background()) // Existing behavior: inherited dialer, Resin ignores account proxy.
	directConn := acquire(proxy.WithAccountBoundEgress(context.Background(), ""))
	boundConn := acquire(proxy.WithAccountBoundEgress(context.Background(), proxyServer.URL))
	if business == directConn || business == boundConn || directConn == boundConn {
		t.Error("cross-route WebSocket connection reused")
	}
	if got := acquire(proxy.WithAccountBoundEgress(context.Background(), "")); got != directConn {
		t.Error("same explicit route did not reuse its connection")
	}
	if direct.Load() != 3 || bound.Load() != 2 {
		t.Errorf("captures upstream=%d proxy=%d, want 3/2", direct.Load(), bound.Load())
	}
}

func TestAccountBoundEgressWebsocketProxyFailureNeverDialsDirect(t *testing.T) {
	old := proxy.GetResinConfig()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: "http://127.0.0.1:1", PlatformName: "test"})
	t.Cleanup(func() { proxy.SetResinConfig(old) })
	var direct atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1); w.WriteHeader(502) }))
	defer upstream.Close()
	m := NewManager()
	defer m.Stop()
	for _, route := range []string{"socks5h://", "http://127.0.0.1:1"} {
		ctx, cancel := context.WithTimeout(proxy.WithAccountBoundEgress(context.Background(), route), time.Second)
		_, _, err := m.AcquireConnection(ctx, &auth.Account{DBID: 998202}, "ws"+strings.TrimPrefix(upstream.URL, "http"), route, nil, route)
		cancel()
		if err == nil {
			t.Error("bound proxy failure should return an error")
		}
	}
	if direct.Load() != 0 {
		t.Errorf("direct received %d failed-proxy requests", direct.Load())
	}
}
