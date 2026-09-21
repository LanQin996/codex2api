package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/internal/egressip"
)

// egressIPFakeProxy 是只回答回显请求的假出口：它不真的转发，而是自己把出口 IP echo
// 回来，所以整组测试完全不碰外网。每条请求的形态（绝对形式的目标主机、代理凭据）都被
// 记下来，用来证明"确实经这条代理取值"。
type egressIPFakeProxy struct {
	server   *httptest.Server
	respond  func(r *http.Request) (int, string)
	requests atomic.Int64

	mu   sync.Mutex
	host []string
	auth []string
}

func newEgressIPFakeProxy(t *testing.T, respond func(r *http.Request) (int, string)) *egressIPFakeProxy {
	t.Helper()
	p := &egressIPFakeProxy{respond: respond}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.requests.Add(1)
		p.mu.Lock()
		p.host = append(p.host, r.URL.Host)
		p.auth = append(p.auth, r.Header.Get("Proxy-Authorization"))
		p.mu.Unlock()
		if r.Method == http.MethodConnect {
			// 兜底端点是 https，经 HTTP 代理会先 CONNECT。假出口直接拒绝，测试既不
			// 需要真隧道也不会挂住。
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		status, body := p.respond(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func newEgressIPEchoProxy(t *testing.T, body string) *egressIPFakeProxy {
	t.Helper()
	return newEgressIPFakeProxy(t, func(*http.Request) (int, string) { return http.StatusOK, body })
}

// proxyURL 造出带凭据的代理地址；空凭据表示匿名出口。
func (p *egressIPFakeProxy) proxyURL(username, password string) string {
	host := strings.TrimPrefix(p.server.URL, "http://")
	if username == "" && password == "" {
		return "http://" + host
	}
	return "http://" + url.UserPassword(username, password).String() + "@" + host
}

func (p *egressIPFakeProxy) lastHost() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.host) == 0 {
		return ""
	}
	return p.host[len(p.host)-1]
}

func (p *egressIPFakeProxy) lastAuth() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.auth) == 0 {
		return ""
	}
	return p.auth[len(p.auth)-1]
}

// 探针收尾：验证通过的 winner 出口查到的出口 IP 必须落到票据上（admin 的 exit_ip 正是
// 运营者核对"是不是同一个 IP"的字段），并且只查一次、必须是经这条出口查的。
func TestCodexTurnStateProbeWinnerExitIPRecorded(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	echo := newEgressIPEchoProxy(t, `{"query":"203.0.113.7"}`)
	proxyURL := echo.proxyURL("ticket-user", "ticket-pass") + "/sid-abc123"
	state := testTurnStateValue(10)

	h.publishProbeWinner(context.Background(), account, "gpt-test", verifiedCodexTurnStateProbe{
		state: state, proxyURL: proxyURL, sid: "abc123", verifiedModel: "gpt-test",
	})

	ticket, ok := account.CodexTurnStateTickets["gpt-test"]
	if !ok {
		t.Fatal("probe winner was not stored")
	}
	if ticket.ExitIP != "203.0.113.7" {
		t.Fatalf("ticket exit ip = %q, want 203.0.113.7", ticket.ExitIP)
	}
	if ticket.State != state || ticket.Source != "probe" || ticket.ProxyURL != proxyURL || ticket.ProxySID != "abc123" {
		t.Fatalf("ticket lost its probe binding: %+v", ticket)
	}
	if got := echo.requests.Load(); got != 1 {
		t.Fatalf("echo endpoint requests = %d, want exactly 1 per winner", got)
	}
	if host := echo.lastHost(); host != "ip-api.com" {
		t.Fatalf("lookup did not go through the egress proxy: absolute target host = %q", host)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("ticket-user:ticket-pass"))
	if got := echo.lastAuth(); got != wantAuth {
		t.Fatalf("lookup did not authenticate on the egress: got %q, want %q", got, wantAuth)
	}
}

// 回显端点形态各异：{"query":...}、{"ip":...}、纯文本、ip= 行都要认；解析不出合法 IP 的
// 响应按失败处理（返回空），绝不当成出口 IP。
func TestCodexTurnStateExitIPParsesEchoResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"query json", `{"query":"203.0.113.7"}`, "203.0.113.7"},
		{"ip json", `{"ip":"198.51.100.9"}`, "198.51.100.9"},
		{"ip-api payload", `{"status":"success","country":"NL","query":"203.0.113.7"}`, "203.0.113.7"},
		{"ipv6 json", `{"ip":"2001:db8::1"}`, "2001:db8::1"},
		{"plain text", "203.0.113.7\n", "203.0.113.7"},
		{"ip= line", "echo notice\nip=203.0.113.7\n", "203.0.113.7"},
		{"padded json", "  {\"query\":\"203.0.113.7\"}\n", "203.0.113.7"},
		{"no ip in json", `{"status":"fail","message":"reserved range"}`, ""},
		{"plain garbage", "not an ip", ""},
		{"empty body", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			echo := newEgressIPEchoProxy(t, tc.body)
			got := codexTurnStateExitIP(context.Background(), echo.proxyURL("", ""))
			if got != tc.want {
				t.Fatalf("exit ip = %q, want %q", got, tc.want)
			}
			if echo.requests.Load() == 0 {
				t.Fatal("lookup never reached the egress proxy")
			}
		})
	}
}

// 主用端点不可用时换兜底端点，两条都必须经同一条出口。
func TestCodexTurnStateExitIPFallsBackToSecondEndpoint(t *testing.T) {
	echo := newEgressIPFakeProxy(t, func(r *http.Request) (int, string) {
		if r.URL.Host == "primary.test" {
			return http.StatusOK, "not an ip"
		}
		return http.StatusOK, `{"ip":"198.51.100.9"}`
	})
	ip, err := egressip.LookupEndpoints(context.Background(), echo.proxyURL("ticket-user", "ticket-pass"), []string{
		"http://primary.test/json/?fields=query",
		"http://fallback.test/json/?fields=query",
	})
	if err != nil {
		t.Fatalf("fallback endpoint was not used: %v", err)
	}
	if ip != "198.51.100.9" {
		t.Fatalf("exit ip = %q, want the fallback answer 198.51.100.9", ip)
	}
	if got := echo.requests.Load(); got != 2 {
		t.Fatalf("echo requests = %d, want 2 (primary then fallback)", got)
	}
}

// 出口 IP 只是展示信息：查询失败（出口不可达、回显不解析、出口为空）一律忽略，
// 探针结果照常落库，且代理凭据绝不进日志。
func TestCodexTurnStateExitIPFailureKeepsProbeSuccess(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	offline := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	offlineHost := strings.TrimPrefix(offline.URL, "http://")
	offline.Close()

	unreachable := "http://ticket-user:ticket-pass@" + offlineHost
	garbage := newEgressIPEchoProxy(t, "not an ip").proxyURL("ticket-user", "ticket-pass")

	cases := []struct{ name, proxyURL string }{
		{"unreachable egress", unreachable},
		{"unparsable echo", garbage},
		{"empty egress", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureEgressIPLogs(t)
			state := testTurnStateValue(10)
			h.publishProbeWinner(context.Background(), account, "gpt-test", verifiedCodexTurnStateProbe{
				state: state, proxyURL: tc.proxyURL, sid: "abc123", verifiedModel: "gpt-test",
			})
			ticket, ok := account.CodexTurnStateTickets["gpt-test"]
			if !ok {
				t.Fatal("exit ip lookup failure broke the probe result: winner not stored")
			}
			if ticket.ExitIP != "" {
				t.Fatalf("ticket exit ip = %q, want empty on lookup failure", ticket.ExitIP)
			}
			if ticket.State != state || ticket.Source != "probe" {
				t.Fatalf("ticket lost the probe result: %+v", ticket)
			}
			if got := codexTurnStateExitIP(context.Background(), tc.proxyURL); got != "" {
				t.Fatalf("codexTurnStateExitIP = %q, want empty on lookup failure", got)
			}
			for _, secret := range []string{"ticket-user", "ticket-pass", tc.proxyURL} {
				if secret == "" {
					continue
				}
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("egress credentials leaked into logs: %q", secret)
				}
			}
		})
	}
}

// 失败时返回的错误文本同样不能带代理地址和凭据：上层任何一次 log 都不能把它们写出去。
func TestCodexTurnStateExitIPErrorsRedactEgressCredentials(t *testing.T) {
	offline := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	offlineHost := strings.TrimPrefix(offline.URL, "http://")
	offline.Close()

	proxyURL := "http://ticket-user:ticket-pass@" + offlineHost
	_, err := egressip.Lookup(context.Background(), proxyURL)
	if err == nil {
		t.Fatal("expected an error for an unreachable egress")
	}
	// 实测 Go 的代理拨号错误里带着出口的 host:port（"dial tcp 127.0.0.1:x: ..."），
	// 所以下面的断言确实在检查脱敏，而不是恰好没命中。
	for _, secret := range []string{"ticket-user", "ticket-pass", offlineHost} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error text leaked %q: %s", secret, err.Error())
		}
	}
}

// 出口 IP 只对验证通过的 winner 查一次：并发 fan-out 本身一个回显请求都不许发，
// 查一次也只有一次（64 并发各自去查会打爆回显端点）。
func TestCodexTurnStateExitIPQueriedOnlyForWinner(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	cfg := *CurrentCodexTurnStateTicketConfig()
	cfg.Concurrency = 64
	SetCodexTurnStateTicketConfig(&cfg)

	echo := newEgressIPEchoProxy(t, `{"query":"203.0.113.7"}`)
	proxyURL := echo.proxyURL("ticket-user", "ticket-pass") + "/sid-abc123"
	state := testTurnStateValue(10)

	var attempts atomic.Int64
	winner, err := h.raceVerifiedProbes(context.Background(), 64, func(context.Context) (verifiedCodexTurnStateProbe, error) {
		attempts.Add(1)
		return verifiedCodexTurnStateProbe{state: state, proxyURL: proxyURL, sid: "abc123", verifiedModel: "gpt-test"}, nil
	}, account.ID())
	if err != nil {
		t.Fatalf("race: %v", err)
	}
	if attempts.Load() == 0 {
		t.Fatal("no attempt ran")
	}
	if got := echo.requests.Load(); got != 0 {
		t.Fatalf("echo endpoint queried %d times during the fan-out, want 0", got)
	}
	h.publishProbeWinner(context.Background(), account, "gpt-test", winner)
	if got := echo.requests.Load(); got != 1 {
		t.Fatalf("echo endpoint queried %d times for one winner, want 1", got)
	}
	if ticket := account.CodexTurnStateTickets["gpt-test"]; ticket.ExitIP != "203.0.113.7" {
		t.Fatalf("ticket exit ip = %q, want 203.0.113.7", ticket.ExitIP)
	}
}

// SOCKS5 出口（标准库没有 SOCKS5 客户端，包内自带最小实现）同样必须查到出口 IP，
// 并且用上代理凭据。
func TestCodexTurnStateExitIPThroughSOCKS5Egress(t *testing.T) {
	socks := newEgressIPSOCKS5Proxy(t, "ticket-user", "ticket-pass", `{"query":"203.0.113.7"}`)
	got := codexTurnStateExitIP(context.Background(), socks.proxyURL())
	if got != "203.0.113.7" {
		t.Fatalf("exit ip = %q, want 203.0.113.7", got)
	}
	if !socks.authed.Load() {
		t.Fatal("socks5 egress credentials were not used")
	}
	if target := socks.target.Load(); target != "ip-api.com:80" {
		t.Fatalf("socks5 target = %v, want ip-api.com:80", target)
	}
}

func captureEgressIPLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buffer := &bytes.Buffer{}
	previous := log.Writer()
	log.SetOutput(buffer)
	t.Cleanup(func() { log.SetOutput(previous) })
	return buffer
}

// egressIPSOCKS5Proxy 是最小 SOCKS5 假出口：握手与凭据按 RFC 1928/1929 校验，握手成功后
// 直接扮演回显端点把 JSON 写回隧道（不做真转发，因此测试不需要外网）。
type egressIPSOCKS5Proxy struct {
	listener net.Listener
	username string
	password string
	body     string

	authed   atomic.Bool
	target   atomic.Value
	requests atomic.Int64
}

func newEgressIPSOCKS5Proxy(t *testing.T, username, password, body string) *egressIPSOCKS5Proxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &egressIPSOCKS5Proxy{listener: listener, username: username, password: password, body: body}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return p
}

func (p *egressIPSOCKS5Proxy) proxyURL() string {
	host := p.listener.Addr().String()
	if p.username == "" && p.password == "" {
		return "socks5://" + host
	}
	return "socks5://" + url.UserPassword(p.username, p.password).String() + "@" + host
}

func (p *egressIPSOCKS5Proxy) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)

	greeting := make([]byte, 2)
	if _, err := io.ReadFull(reader, greeting); err != nil {
		return
	}
	methods := make([]byte, greeting[1])
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}
	if p.username != "" || p.password != "" {
		if !egressIPHasMethod(methods, 0x02) {
			_, _ = conn.Write([]byte{0x05, 0xff})
			return
		}
		if _, err := conn.Write([]byte{0x05, 0x02}); err != nil {
			return
		}
		header := make([]byte, 2)
		if _, err := io.ReadFull(reader, header); err != nil {
			return
		}
		username := make([]byte, header[1])
		if _, err := io.ReadFull(reader, username); err != nil {
			return
		}
		length := make([]byte, 1)
		if _, err := io.ReadFull(reader, length); err != nil {
			return
		}
		password := make([]byte, length[0])
		if _, err := io.ReadFull(reader, password); err != nil {
			return
		}
		if string(username) != p.username || string(password) != p.password {
			_, _ = conn.Write([]byte{0x01, 0x01})
			return
		}
		p.authed.Store(true)
		if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	} else if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return
	}
	host := ""
	switch header[3] {
	case 0x01:
		address := make([]byte, 4)
		if _, err := io.ReadFull(reader, address); err != nil {
			return
		}
		host = net.IP(address).String()
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(reader, length); err != nil {
			return
		}
		address := make([]byte, length[0])
		if _, err := io.ReadFull(reader, address); err != nil {
			return
		}
		host = string(address)
	case 0x04:
		address := make([]byte, 16)
		if _, err := io.ReadFull(reader, address); err != nil {
			return
		}
		host = net.IP(address).String()
	default:
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(reader, port); err != nil {
		return
	}
	p.target.Store(net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))))
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}); err != nil {
		return
	}

	// 隧道已建立：读掉这条 HTTP 请求的头部，然后扮演回显端点回一条响应。
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	p.requests.Add(1)
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(p.body), p.body)
}

func egressIPHasMethod(methods []byte, want byte) bool {
	for _, method := range methods {
		if method == want {
			return true
		}
	}
	return false
}
