package wsrelay

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

// 本文件验证票据绑定出口（X-Codex-Turn-State）在 WebSocket 侧的三条不变量：
//  1) 拨号出口进了连接池键：绑定出口的请求与无绑定出口的请求绝不共用连接与会话；
//  2) 发送失败后的重拨走本次尝试定稿的出口（票据绑定出口），而不是调用方入参出口；
//  3) 续链亲和（previous_response_id）完全不看出口：票据绑定出口之外的连接照样会被
//     交回给后续请求，绑定保证在这条路径上是漏的。

// ==================== 夹具 ====================

// turnStateTestTicketValue 造一个合法的非降级票据值（Fernet 信封 + 10 个密文块 = 292
// 字符，与真实 Pro 票据同形），好让注入侧的长度/格式校验放行。
func turnStateTestTicketValue() string {
	raw := make([]byte, 1+8+16+10*16+32)
	raw[0] = 0x80
	for i := 9; i < len(raw); i++ {
		raw[i] = byte(i % 251)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func newTurnStateTestWSServer(t *testing.T, received chan<- string) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if received != nil {
				select {
				case received <- string(payload):
				default:
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func dialTurnStateTestWSServer(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial test websocket server: %v", err)
	}
	return conn
}

// seedTurnStateConnection 按"某条出口"手工造一条已在池中的连接。池键的代理段就是这条
// 连接实际拨号用的出口，口径与 createConnection 一致（manager.go:1232）。
func seedTurnStateConnection(t *testing.T, manager *Manager, account *auth.Account, wsURL, sessionKey, egress string, conn *websocket.Conn) *WsConnection {
	t.Helper()
	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = manager.poolKey(account.ID(), wsURL, sessionKey, egress)
	manager.connections.Store(wc.PoolKey, wc)
	manager.sessions.Store(wc.PoolKey, session)
	return wc
}

// turnStateTestDialer 记录每次拨号请求的地址（= 本次实际选定的出口），并把连接落到本机
// WS 测试服务器：地址是代理时先扮演 HTTP CONNECT 代理，其余按直连。于是"这次到底从哪条
// 出口出去"在测试里可观测，且完全不碰真实网络。
type turnStateTestDialer struct {
	proxyAddr  string
	serverAddr string

	mu        sync.Mutex
	addresses []string
}

func (d *turnStateTestDialer) install(manager *Manager) {
	dialerCopy := *manager.dialer
	dialerCopy.NetDialContext = d.dial
	dialerCopy.HandshakeTimeout = 5 * time.Second
	manager.dialer = &dialerCopy
}

func (d *turnStateTestDialer) dial(ctx context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	d.addresses = append(d.addresses, address)
	viaProxy := address == d.proxyAddr
	d.mu.Unlock()

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", d.serverAddr)
	if err != nil {
		return nil, err
	}
	if !viaProxy {
		return conn, nil
	}
	// 代理路径：拨号器拿到的是"代理连接"，它会在上面写 CONNECT；真实的落点
	// （本机测试服务器）是另一条 socket，两者必须分开，否则就成了自己等自己。
	client, server := net.Pipe()
	go serveTurnStateConnectProxy(server, conn)
	return client, nil
}

func (d *turnStateTestDialer) recorded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addresses...)
}

// serveTurnStateConnectProxy 在 client 上扮演 HTTP CONNECT 代理：读掉 CONNECT 请求、回
// 200，之后把 client 与 target（本机测试服务器）双向直通。client 是拨号器拿到的连接。
func serveTurnStateConnectProxy(client net.Conn, target net.Conn) {
	defer client.Close()
	defer target.Close()

	reader := bufio.NewReader(client)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	go func() { _, _ = io.Copy(target, client) }()
	// bufio 可能把 CONNECT 之后的字节一起吞了（理论窗口），先按原序补给上游。
	if buffered := reader.Buffered(); buffered > 0 {
		if _, err := io.CopyN(target, reader, int64(buffered)); err != nil {
			return
		}
	}
	_, _ = io.Copy(client, target)
}

// turnStateTicketFixture 准备"账号上有一张绑定出口为 boundProxy 的托管票据"的环境：
// 采集开关打开 + PreserveExisting，账号票据表里放一张合法票据。
func turnStateTicketFixture(t *testing.T, boundProxy, state, model string) *auth.Account {
	t.Helper()
	previous := proxy.CurrentCodexTurnStateTicketConfig()
	t.Cleanup(func() { proxy.SetCodexTurnStateTicketConfig(previous) })
	proxy.SetCodexTurnStateTicketConfig(&proxy.CodexTurnStateTicketConfig{
		Enabled: true, PreserveExisting: true, TargetLength: len(state), TTLSeconds: 3600,
		RefreshBeforeSeconds: 600, Models: []string{model}, ProbeModels: []string{model},
	})
	account := &auth.Account{
		DBID:        42,
		AccountID:   "acct-42",
		AccessToken: "token-123",
		CodexTurnStateTickets: map[string]auth.CodexTurnStateTicket{
			model: {
				State: state, Length: len(state), CapturedAt: time.Now(),
				ExpiresAt: time.Now().Add(time.Hour), Source: "probe",
				ProxyURL: boundProxy, ProxySID: "sid-bound",
			},
		},
	}
	return account
}

// executeTurnStateRequest 走真实注入入口 proxy.ExecuteRequest：只有这条路能把票据绑定出口
// 写进 ctx（proxy 包对外只暴露 getter，setter 与 ctx key 都不导出）。WebsocketExecuteFunc
// 被换成直接调用本测试自己的 manager，避免落在全局池上。返回值 boundInCtx 是注入侧真正
// 定稿、WS 执行器随后据此覆盖出口的那个值。
func executeTurnStateRequest(t *testing.T, manager *Manager, account *auth.Account, body []byte, sessionID, callerProxy, apiKey string, headers http.Header) (*WsResponse, string, error) {
	t.Helper()
	exec := NewExecutorWithManager(manager)
	previous := proxy.WebsocketExecuteFunc
	t.Cleanup(func() { proxy.WebsocketExecuteFunc = previous })

	var captured *WsResponse
	boundInCtx := ""
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID, proxyOverride, apiKey string, deviceCfg *proxy.DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		boundInCtx = proxy.CodexTurnStateProxyFromContext(ctx)
		wsResp, err := exec.ExecuteRequestViaWebsocket(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, poolRouteKey)
		captured = wsResp
		return nil, err
	}
	_, err := proxy.ExecuteRequest(context.Background(), account, body, sessionID, callerProxy, apiKey, nil, headers, true)
	return captured, boundInCtx, err
}

// ==================== (c) 池键按出口分池 ====================

// TestCodexTurnStateBoundEgressSeparatesWsPoolConnections 验证绑定出口与无绑定出口的请求
// 各自从对应出口拨号，且互不复用连接/会话。
func TestCodexTurnStateBoundEgressSeparatesWsPoolConnections(t *testing.T) {
	server := newTurnStateTestWSServer(t, nil)
	dialer := &turnStateTestDialer{
		proxyAddr:  "bound-egress.test:3128",
		serverAddr: strings.TrimPrefix(server.URL, "http://"),
	}
	manager := NewManager()
	t.Cleanup(manager.Stop)
	dialer.install(manager)
	manager.probeFunc = func(*WsConnection) bool { return true }

	account := &auth.Account{DBID: 42, AccessToken: "token-123"}
	wsURL := "ws://upstream.test/responses"
	boundProxy := "http://" + dialer.proxyAddr
	ctx := context.Background()

	boundConn, boundPending, err := manager.AcquireConnection(ctx, account, wsURL, "session-1", http.Header{}, boundProxy)
	if err != nil {
		t.Fatalf("bound acquire: %v", err)
	}
	boundKey := manager.poolKey(account.ID(), wsURL, "session-1", boundProxy)
	if boundConn.PoolKey != boundKey {
		t.Fatalf("bound PoolKey = %q, want %q", boundConn.PoolKey, boundKey)
	}
	if got := dialer.recorded(); len(got) != 1 || got[0] != dialer.proxyAddr {
		t.Fatalf("bound egress dials = %v, want exactly [%s]: 票据绑定出口必须真的参与拨号", got, dialer.proxyAddr)
	}
	boundConn.session.RemovePendingRequest(boundPending.RequestID)

	plainConn, plainPending, err := manager.AcquireConnection(ctx, account, wsURL, "session-1", http.Header{}, "")
	if err != nil {
		t.Fatalf("unbound acquire: %v", err)
	}
	if plainConn == boundConn {
		t.Fatal("无绑定出口的请求复用了票据绑定出口上的连接：出口绑定被跳过")
	}
	plainKey := manager.poolKey(account.ID(), wsURL, "session-1", "")
	if plainKey == boundKey {
		t.Fatalf("bound/unbound pool keys must differ, both = %q", boundKey)
	}
	if plainConn.PoolKey != plainKey {
		t.Fatalf("unbound PoolKey = %q, want %q", plainConn.PoolKey, plainKey)
	}
	got := dialer.recorded()
	if len(got) != 2 {
		t.Fatalf("dials = %v, want 2 (bound then direct)", got)
	}
	if got[1] == dialer.proxyAddr {
		t.Fatalf("无绑定出口的请求拨到了票据绑定出口 %s：两者绝不能共池", dialer.proxyAddr)
	}
	if !strings.Contains(got[1], "upstream.test") {
		t.Fatalf("unbound dial = %q, want the direct upstream target", got[1])
	}
	for _, key := range []string{boundKey, plainKey} {
		if _, ok := manager.connections.Load(key); !ok {
			t.Fatalf("connection under %q missing from pool", key)
		}
	}
	plainConn.session.RemovePendingRequest(plainPending.RequestID)
}

// ==================== (a) 发送失败后的重拨出口 ====================

// TestCodexTurnStateSendFailureReconnectKeepsTicketBoundEgress 验证：本次尝试被票据绑定到
// boundProxy 时，首条连接发送失败后的重拨落在 boundProxy 的连接池键上（重拨拨的就是这条
// 出口），而不是调用方入参出口，也不是续链亲和取回的那条旧出口连接。
//
// 夹具体现两个出口：续链亲和绑定的连接建在 egress-old 上（socket 已死 → 首发必失败），
// 绑定出口池键下预置一条活连接。代码若在重拨处用错出口变量，就会去别的池键找连接 → 必然
// 触发一次真实拨号（被 dialer 记录）→ 断言失败。
func TestCodexTurnStateSendFailureReconnectKeepsTicketBoundEgress(t *testing.T) {
	received := make(chan string, 4)
	server := newTurnStateTestWSServer(t, received)
	dialer := &turnStateTestDialer{
		proxyAddr:  "bound-egress.test:3128",
		serverAddr: strings.TrimPrefix(server.URL, "http://"),
	}

	manager := NewManager()
	t.Cleanup(manager.Stop)
	dialer.install(manager)
	manager.probeFunc = func(*WsConnection) bool { return true }

	state := turnStateTestTicketValue()
	boundProxy := "http://" + dialer.proxyAddr
	account := turnStateTicketFixture(t, boundProxy, state, "gpt-5.4")

	wsURL, err := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	if err != nil {
		t.Fatalf("buildWebsocketURL: %v", err)
	}

	// 续链亲和绑定的连接：建在 egress-old 出口上，socket 已死。session.ID 与生产一致
	// 等于自己的池槽号，重拨会以"实际槽位 + 本次定稿出口"重新取连。
	oldEgress := "http://egress-old.test:8080"
	deadConn := seedTurnStateConnection(t, manager, account, wsURL, "base#0", oldEgress, newClosedTestWebsocketConn(t))
	deadConn.session.ID = "base#0"
	if deadConn.PoolKey == manager.poolKey(account.ID(), wsURL, "base#0", boundProxy) {
		t.Fatal("fixture: 续链连接必须在与绑定出口不同的池键下")
	}
	manager.BindResponseConn("resp_chain", deadConn, "base#0", account.ID(), "key-A")

	// 绑定出口池键（同一槽位 + 票据绑定出口）下的活连接：重拨应当落到这里复用。
	liveConn := seedTurnStateConnection(t, manager, account, wsURL, "base#0", boundProxy, dialTurnStateTestWSServer(t, server))
	liveKey := liveConn.PoolKey

	headers := http.Header{}
	headers.Set("X-Codex-Turn-State", state)
	body := []byte(`{"model":"gpt-5.4","input":"hi","previous_response_id":"resp_chain"}`)

	wsResp, boundInCtx, err := executeTurnStateRequest(t, manager, account, body, "session-1", "http://caller-egress.test:8080", "key-A", headers)
	if wsResp != nil {
		defer wsResp.Close()
	}
	if boundInCtx != boundProxy {
		t.Fatalf("注入侧定稿的绑定出口 = %q, want %q（夹具没走到绑定注入）", boundInCtx, boundProxy)
	}
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if got := dialer.recorded(); len(got) != 0 {
		t.Fatalf("重拨没有复用票据绑定出口池键下的连接，而是拨号到了 %v", got)
	}
	select {
	case payload := <-received:
		if !strings.Contains(payload, "resp_chain") {
			t.Fatalf("帧体 = %s, want previous_response_id 续链请求", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("绑定出口池键下的连接没有收到任何帧")
	}
	if _, ok := manager.connections.Load(deadConn.PoolKey); ok {
		t.Fatal("首发失败的续链连接应当已被丢弃")
	}
	if _, ok := manager.connections.Load(liveKey); !ok {
		t.Fatal("绑定出口池键下的连接应当仍在池中")
	}
}

// ==================== (d) 续链亲和不看出口 ====================

// TestCodexTurnStatePreferredConnectionIgnoresTicketBoundEgress 验证续链亲和会绕过出口绑定：
// 本次尝试被票据绑定到 boundProxy，但 previous_response_id 指向的连接建在 egress-old 上，
// AcquirePreferredConnection 按 (responseID, accountID, apiKey) 命中后直接返回它 —— 请求从
// egress-old 出去，一个字节都没走 boundProxy（记录到的拨号数为 0）。
func TestCodexTurnStatePreferredConnectionIgnoresTicketBoundEgress(t *testing.T) {
	received := make(chan string, 4)
	server := newTurnStateTestWSServer(t, received)
	dialer := &turnStateTestDialer{
		proxyAddr:  "bound-egress.test:3128",
		serverAddr: strings.TrimPrefix(server.URL, "http://"),
	}

	manager := NewManager()
	t.Cleanup(manager.Stop)
	dialer.install(manager)
	manager.probeFunc = func(*WsConnection) bool { return true }

	state := turnStateTestTicketValue()
	boundProxy := "http://" + dialer.proxyAddr
	account := turnStateTicketFixture(t, boundProxy, state, "gpt-5.4")

	wsURL, err := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	if err != nil {
		t.Fatalf("buildWebsocketURL: %v", err)
	}

	// 产出 resp_chain 的那条连接建在 egress-old 上，且仍然活着。
	oldEgress := "http://egress-old.test:8080"
	oldConn := seedTurnStateConnection(t, manager, account, wsURL, "base#0", oldEgress, dialTurnStateTestWSServer(t, server))
	if strings.Contains(oldConn.PoolKey, dialer.proxyAddr) {
		t.Fatal("fixture: 续链连接的池键不该记录票据绑定出口")
	}
	manager.BindResponseConn("resp_chain", oldConn, "base#0", account.ID(), "key-A")

	headers := http.Header{}
	headers.Set("X-Codex-Turn-State", state)
	body := []byte(`{"model":"gpt-5.4","input":"hi","previous_response_id":"resp_chain"}`)

	wsResp, boundInCtx, err := executeTurnStateRequest(t, manager, account, body, "session-1", "http://caller-egress.test:8080", "key-A", headers)
	if wsResp != nil {
		defer wsResp.Close()
	}
	if boundInCtx != boundProxy {
		t.Fatalf("注入侧定稿的绑定出口 = %q, want %q（夹具没走到绑定注入）", boundInCtx, boundProxy)
	}
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if got := dialer.recorded(); len(got) != 0 {
		t.Fatalf("dials = %v, want none: 续链亲和不该为绑定出口新建连接", got)
	}
	select {
	case payload := <-received:
		if !strings.Contains(payload, "resp_chain") {
			t.Fatalf("帧体 = %s, want previous_response_id 续链请求", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("egress-old 上的续链连接没有收到帧")
	}
	boundKey := manager.poolKey(account.ID(), wsURL, "session-1", boundProxy)
	if _, ok := manager.connections.Load(boundKey); ok {
		t.Fatalf("绑定出口 %s 的池键下不该出现连接", boundProxy)
	}
}
