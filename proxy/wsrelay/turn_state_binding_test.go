package wsrelay

import (
	"bufio"
	"context"
	"crypto/tls"
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
//  3) 续链亲和（previous_response_id）让位于出口绑定：有票据绑定时只接受池键出口与
//     绑定出口一致的连接，不符就回落到常规 acquire 在绑定出口新建连接（宁可丢续链）；
//     没有票据绑定时照旧复用原连接，亲和行为一字不变。

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

// newTurnStateTestWSServer 起一个 TLS 化的 WS 测试服务器。必须 TLS：执行器按
// CodexBaseURL 生成 wss:// 地址，夹具的拨号器只是把落点改到本服务器，握手仍是 TLS ——
// 明文服务器会以 "first record does not look like a TLS handshake" 拒绝，让"拨到哪条出口"
// 之外的断言全部无法进行。
func newTurnStateTestWSServer(t *testing.T, received chan<- string) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	wsURL := "wss" + strings.TrimPrefix(server.URL, "https")
	dialer := &websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial test websocket server: %v", err)
	}
	return conn
}

// turnStateTestServerAddr 返回测试 WS 服务器的 host:port，即夹具拨号器的落点。
func turnStateTestServerAddr(server *httptest.Server) string {
	return strings.TrimPrefix(server.URL, "https://")
}

// seedTurnStateConnection 按"某条出口"手工造一条已在池中的连接。池键的代理段（以及
// WsConnection 记录的池键出口）就是这条连接所属的出口，口径与 createConnection 一致
// （manager.go:1262 一带：poolKey 第 4 段与 wc.poolKeyProxyURL 同源同值）。
func seedTurnStateConnection(t *testing.T, manager *Manager, account *auth.Account, wsURL, sessionKey, egress string, conn *websocket.Conn) *WsConnection {
	t.Helper()
	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = manager.poolKey(account.ID(), wsURL, sessionKey, egress)
	wc.poolKeyProxyURL = strings.TrimSpace(egress)
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
	// 落点是自签证书的 TLS 测试服务器，握手必须放行它。只影响本夹具装出来的 dialer。
	dialerCopy.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
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
		RefreshBeforeSeconds: 600, Models: []string{model}, ProbeModels: []string{model}, TicketProxySticky: true,
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
		serverAddr: turnStateTestServerAddr(server),
	}
	manager := NewManager()
	t.Cleanup(manager.Stop)
	dialer.install(manager)
	manager.probeFunc = func(*WsConnection) bool { return true }

	account := &auth.Account{DBID: 42, AccessToken: "token-123"}
	// wss:// 而非 ws://：落点是 TLS 测试服务器，明文握手会被它按 "HTTP request to an HTTPS
	// server" 拒掉；拨号地址（代理/直连目标）与池键不因此改变。
	wsURL := "wss://upstream.test/responses"
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
// boundProxy 时，续链亲和取回的连接首发失败后的重拨落在 boundProxy 上（重拨拨的就是这条
// 出口），而不是调用方入参出口。（出口与绑定不符的续链连接根本不会被取回，见 (d) 的用例。）
//
// 夹具里续链亲和绑定的连接建在 boundProxy 上、socket 已死 → 首发必失败，池里没有可复用
// 的活连接。代码若在重拨处用错出口变量，拨号就会落在 caller-egress 而不是 bound-egress，
// 断言直接失败。
func TestCodexTurnStateSendFailureReconnectKeepsTicketBoundEgress(t *testing.T) {
	received := make(chan string, 4)
	server := newTurnStateTestWSServer(t, received)
	dialer := &turnStateTestDialer{
		proxyAddr:  "bound-egress.test:3128",
		serverAddr: turnStateTestServerAddr(server),
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

	// 续链亲和绑定的连接：建在票据绑定出口上（出口一致才会被取回），socket 已死。session.ID
	// 与生产一致等于自己的池槽号，重拨会以"实际槽位 + 本次定稿出口"重新取连。
	deadConn := seedTurnStateConnection(t, manager, account, wsURL, "base#0", boundProxy, newClosedTestWebsocketConn(t))
	deadConn.session.ID = "base#0"
	manager.BindResponseConn("resp_chain", deadConn, "base#0", account.ID(), "key-A")

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
	// 重拨必须拨本次尝试定稿的绑定出口：拨到 caller-egress 时记录到的地址就是另一个。
	if got := dialer.recorded(); len(got) != 1 || got[0] != dialer.proxyAddr {
		t.Fatalf("重拨 dials = %v, want exactly [%s]: 重拨必须走票据绑定出口", got, dialer.proxyAddr)
	}
	if wsResp.conn == deadConn {
		t.Fatal("重拨复用了那条已死的续链连接")
	}
	select {
	case payload := <-received:
		if !strings.Contains(payload, "resp_chain") {
			t.Fatalf("帧体 = %s, want previous_response_id 续链请求", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("重拨出来的连接没有收到任何帧")
	}
	if current, ok := manager.connections.Load(deadConn.PoolKey); ok && current == deadConn {
		t.Fatal("首发失败的续链连接应当已被丢弃")
	}
}

// ==================== (d) 续链亲和必须让位于票据绑定出口 ====================

// TestCodexTurnStatePreferredConnectionRequiresBoundEgress 验证出口不符时续链亲和让位：
// 本次尝试被票据绑定到 boundProxy，但 previous_response_id 指向的连接建在 egress-old 上，
// AcquirePreferredConnection 因出口不符拒回该连接 —— 请求回落到常规 acquire，在绑定出口
// 池键上新建连接（记录到恰好一次到 boundProxy 的拨号），帧体仍带 previous_response_id
// （让位丢的是"命中原连接"的机会，不是续链语义本身）。
func TestCodexTurnStatePreferredConnectionRequiresBoundEgress(t *testing.T) {
	received := make(chan string, 4)
	server := newTurnStateTestWSServer(t, received)
	dialer := &turnStateTestDialer{
		proxyAddr:  "bound-egress.test:3128",
		serverAddr: turnStateTestServerAddr(server),
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
	// 出口不符 → 偏好连接被拒 → 必须在绑定出口上新建连接，且拨的就是绑定出口。
	if got := dialer.recorded(); len(got) != 1 || got[0] != dialer.proxyAddr {
		t.Fatalf("dials = %v, want exactly [%s]: 出口不符必须丢偏好连接、改建在绑定出口上的连接", got, dialer.proxyAddr)
	}
	if wsResp.conn == nil || wsResp.conn == oldConn {
		t.Fatalf("请求仍落在 egress-old 那条连接上：出口绑定被偏好连接绕过")
	}
	if !strings.Contains(wsResp.conn.PoolKey, dialer.proxyAddr) {
		t.Fatalf("请求落到的池键 = %q, want 含绑定出口 %s", wsResp.conn.PoolKey, dialer.proxyAddr)
	}
	if wsResp.conn.poolKeyProxyURL != boundProxy {
		t.Fatalf("连接记录的池键出口 = %q, want %q", wsResp.conn.poolKeyProxyURL, boundProxy)
	}
	// 被拒的偏好连接原样留在池里：它仍能服务与它同出口的请求。
	if current, ok := manager.connections.Load(oldConn.PoolKey); !ok || current != oldConn {
		t.Fatal("出口不符被拒的偏好连接被连带销毁了")
	}
	// 续链 ID 照旧上送：让位的只是复用原连接，不是续链语义。
	select {
	case payload := <-received:
		if !strings.Contains(payload, "resp_chain") {
			t.Fatalf("帧体 = %s, want previous_response_id 续链请求", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("绑定出口上的新连接没有收到任何帧")
	}
}

// TestCodexTurnStatePreferredConnectionReusedWithoutTicketBinding 是本组断言的反证：没有
// 票据绑定（requiredProxyURL 为空）时续链亲和一字不变 —— 原连接照旧被复用，拨号数为 0，
// 请求仍从 egress-old 出去。出口判断不得成为常规续链的回归。
func TestCodexTurnStatePreferredConnectionReusedWithoutTicketBinding(t *testing.T) {
	received := make(chan string, 4)
	server := newTurnStateTestWSServer(t, received)
	dialer := &turnStateTestDialer{
		proxyAddr:  "bound-egress.test:3128",
		serverAddr: turnStateTestServerAddr(server),
	}

	manager := NewManager()
	t.Cleanup(manager.Stop)
	dialer.install(manager)
	manager.probeFunc = func(*WsConnection) bool { return true }

	// 显式关掉票据注入：本用例走的是"没有绑定出口"的路径。
	previous := proxy.CurrentCodexTurnStateTicketConfig()
	t.Cleanup(func() { proxy.SetCodexTurnStateTicketConfig(previous) })
	proxy.SetCodexTurnStateTicketConfig(&proxy.CodexTurnStateTicketConfig{
		Enabled: false, PreserveExisting: false, RefreshBeforeSeconds: 600,
	})

	account := &auth.Account{DBID: 42, AccountID: "acct-42", AccessToken: "token-123"}

	wsURL, err := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	if err != nil {
		t.Fatalf("buildWebsocketURL: %v", err)
	}

	// 产出 resp_chain 的连接仍然活着，且它记录的池键出口与入参出口不同。
	oldEgress := "http://egress-old.test:8080"
	oldConn := seedTurnStateConnection(t, manager, account, wsURL, "base#0", oldEgress, dialTurnStateTestWSServer(t, server))
	manager.BindResponseConn("resp_chain", oldConn, "base#0", account.ID(), "key-A")

	body := []byte(`{"model":"gpt-5.4","input":"hi","previous_response_id":"resp_chain"}`)

	wsResp, boundInCtx, err := executeTurnStateRequest(t, manager, account, body, "session-1", "http://caller-egress.test:8080", "key-A", http.Header{})
	if wsResp != nil {
		defer wsResp.Close()
	}
	if boundInCtx != "" {
		t.Fatalf("夹具没做到无票据绑定：注入侧定稿的绑定出口 = %q", boundInCtx)
	}
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if got := dialer.recorded(); len(got) != 0 {
		t.Fatalf("dials = %v, want none: 无票据绑定时续链亲和必须照旧复用原连接", got)
	}
	if wsResp.conn != oldConn {
		t.Fatal("无票据绑定的续链请求没有复用产出连接")
	}
	select {
	case payload := <-received:
		if !strings.Contains(payload, "resp_chain") {
			t.Fatalf("帧体 = %s, want previous_response_id 续链请求", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("egress-old 上的续链连接没有收到帧")
	}
}
