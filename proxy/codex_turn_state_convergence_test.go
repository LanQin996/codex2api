package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 运营报告的故障收敛测试：上游按铸造出口校验 X-Codex-Turn-State，所以"铸造票据的
// 出口"必须是"后续所有携带该票据的请求"的出口。dc9c3245 之后的未提交改动声称把
// legacy 票据（无 ProxyURL）→ 探针铸造绑定票据 → 出站注入 → 响应回购 → 下一次出站
// 这条链路接起来了；本文件按运营现场逐步复现整条链路：
//
//   - 原生 Codex OAuth 账号，受管模型上存着一条 legacy 票据（无 ProxyURL）；
//   - PreserveExisting=true；
//   - 采集代理配成粘性模板 http://sid-<值>:pass@<host:port>（探针逐次替换 sid 片段）；
//   - 下游客户端每轮都回带自己的合法 X-Codex-Turn-State。
//
// 出站落点用"只记录 CONNECT、随后 502"的假代理观察：真正的出站头在 TLS 隧道里，
// 假代理看不见，注入值改从 upstream trace 快照读（与线上用量日志同源）。

const (
	convergenceModel = "gpt-5.5"
	// 票据 TTL 用一个较短的值：探针铸造的绑定票据会被收窄到 90s 有效期（仍然有效，
	// 但明显早于 TTL），这样"回购后续期"是可观测的。
	convergenceTTLSeconds = 300
	// 出站目标是常量 CodexBaseURL（https://chatgpt.com/backend-api/codex），
	// 经 HTTP 代理时表现为一次 CONNECT chatgpt.com:443。
	convergenceUpstreamTarget = "chatgpt.com:443"
)

// convergenceTurnStateValue 造一个结构合法、内容可区分的 Fernet 信封取值。
// 网关只校验信封形态（版本字节、块数、长度、base64 形态），不校验 HMAC，
// 所以换掉密文段的填充字节就能得到"结构合法但不是同一个 blob"的票据。
func convergenceTurnStateValue(salt byte, blocks int) string {
	if blocks <= 0 {
		blocks = 10
	}
	raw := make([]byte, 1+8+16+blocks*16+32)
	raw[0] = 0x80
	for i := 9; i < len(raw); i++ {
		raw[i] = byte((i + int(salt)*7) % 251)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

// convergenceEgressProxy 是只做"记录 + 拒绝"的假 HTTP 出口：每条 CONNECT 记下隧道
// 目标与代理凭据，然后 502 结束。测试只关心这次拨号落在哪条出口，不需要真隧道。
type convergenceEgressProxy struct {
	server *httptest.Server

	mu       sync.Mutex
	connects []convergenceConnect
}

type convergenceConnect struct {
	target string
	user   string
}

func newConvergenceEgressProxy(t *testing.T) *convergenceEgressProxy {
	t.Helper()
	p := &convergenceEgressProxy{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "test egress proxy only tunnels CONNECT", http.StatusMethodNotAllowed)
			return
		}
		p.mu.Lock()
		p.connects = append(p.connects, convergenceConnect{
			target: firstNonEmptyString(r.URL.Host, r.Host),
			user:   proxyAuthorizationUser(r.Header.Get("Proxy-Authorization")),
		})
		p.mu.Unlock()
		http.Error(w, "tunnel rejected by convergence test egress", http.StatusBadGateway)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *convergenceEgressProxy) url() string { return p.server.URL }

// hostPort 供拼粘性采集代理模板（sid-<值>:pass@host:port）。
func (p *convergenceEgressProxy) hostPort() string {
	return strings.TrimPrefix(p.server.URL, "http://")
}

func (p *convergenceEgressProxy) snapshot() []convergenceConnect {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]convergenceConnect(nil), p.connects...)
}

func (p *convergenceEgressProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.connects)
}

// proxyAuthorizationUser 取 Proxy-Authorization 里的用户名。粘性出口把 sid 放在代理
// URL 的 userinfo 里（sid-<值>:pass），假代理据此看到这次拨号用的是哪条粘性会话。
func proxyAuthorizationUser(header string) string {
	header = strings.TrimSpace(header)
	const prefix = "Basic "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return ""
	}
	user, _, _ := strings.Cut(string(decoded), ":")
	return user
}

// convergenceTraceContext 复刻 handler 的请求级 trace 容器：本次出站注入了哪个
// turn state 只能从这里读（真正的出站头在 TLS 隧道里）。
func convergenceTraceContext(requestID string) context.Context {
	return context.WithValue(context.Background(), upstreamTraceContextKey{}, &upstreamTraceAudit{requestID: requestID})
}

// convergenceHarvesterFixture 搭出运营环境的票据采集配置：原生 Codex OAuth 账号、
// 单个受管模型、PreserveExisting=true、粘性采集代理模板。
func convergenceHarvesterFixture(t *testing.T, dbid int64, harvestProxyTemplate string) (*CodexTurnStateHarvester, *auth.Account) {
	t.Helper()
	oldConfig := CurrentCodexTurnStateTicketConfig()
	oldHarvester := activeCodexTurnStateHarvester.Load()
	t.Cleanup(func() {
		SetCodexTurnStateTicketConfig(oldConfig)
		activeCodexTurnStateHarvester.Store(oldHarvester)
	})
	SetCodexTurnStateTicketConfig(&CodexTurnStateTicketConfig{
		Enabled: true, Models: []string{convergenceModel}, ProbeModels: []string{convergenceModel},
		TargetLength: len(testTurnStateValue(10)), TTLSeconds: convergenceTTLSeconds,
		RefreshBeforeSeconds: 60, HarvestProxyURL: harvestProxyTemplate, PreserveExisting: true,
		TicketProxySticky: true,
	})
	account := &auth.Account{DBID: dbid, AccessToken: "convergence-access-token", CodexTurnStateTickets: map[string]auth.CodexTurnStateTicket{}}
	store := &auth.Store{}
	store.SetAccountsForTest([]*auth.Account{account})
	h := NewCodexTurnStateHarvester(store, &database.DB{})
	activeCodexTurnStateHarvester.Store(h)
	return h, account
}

// convergenceScenario 是运营现场：一条 legacy 票据 + 三条可观测的假出口
// （票据绑定 / 账号 proxy_url / 分组代理池 override）+ 会回带自己票据的下游客户端。
type convergenceScenario struct {
	t             *testing.T
	h             *CodexTurnStateHarvester
	account       *auth.Account
	boundEgress   *convergenceEgressProxy
	accountEgress *convergenceEgressProxy
	poolEgress    *convergenceEgressProxy
	affinityKey   string
	legacy        string
	echo          string
	downstream    http.Header
	body          []byte
}

func newConvergenceScenario(t *testing.T, dbid int64, affinityKey string) *convergenceScenario {
	t.Helper()
	boundEgress := newConvergenceEgressProxy(t)
	accountEgress := newConvergenceEgressProxy(t)
	poolEgress := newConvergenceEgressProxy(t)
	// 操作者配的粘性采集代理模板：探针每轮替换 sid- 片段。
	harvestTemplate := fmt.Sprintf("http://sid-aaaa:pass@%s", boundEgress.hostPort())

	h, account := convergenceHarvesterFixture(t, dbid, harvestTemplate)
	account.ProxyURL = accountEgress.url()

	// legacy 票据：正是故障起点——有值、有效，但没有绑定出口。
	legacy := convergenceTurnStateValue(1, 10)
	account.CodexTurnStateTickets[convergenceModel] = auth.CodexTurnStateTicket{
		State: legacy, Length: len(legacy), CapturedAt: time.Now(), ExpiresAt: time.Now().Add(30 * time.Minute), Source: "response",
	}
	// 下游客户端回带自己的合法票据（与 legacy 不同，便于区分"注入的是哪一个"）。
	echo := convergenceTurnStateValue(2, 10)
	downstream := http.Header{}
	downstream.Set(codexTurnStateHeader, echo)

	t.Cleanup(func() {
		codexTurnStateOrigins.Delete(affinityKey)
		for _, proxyURL := range []string{account.ProxyURL, poolEgress.url(), boundEgress.url(), ""} {
			clientPool.Delete(clientPoolKey(account, proxyURL, codexTransportModeFromEnv()))
		}
	})
	return &convergenceScenario{
		t: t, h: h, account: account,
		boundEgress: boundEgress, accountEgress: accountEgress, poolEgress: poolEgress,
		affinityKey: affinityKey, legacy: legacy, echo: echo,
		downstream: downstream,
		body:       []byte(fmt.Sprintf(`{"model":%q,"input":"hi"}`, convergenceModel)),
	}
}

// attempt 复刻 handler 逐 attempt 的 ctx 形状（handler.go:4912-4920）：
// 下游客户端模型 + 票据采集绑定 + 逐 attempt 出口决策记录器 + 下游会话亲和键。
func (s *convergenceScenario) attempt() context.Context {
	s.t.Helper()
	ctx := convergenceTraceContext(NewUpstreamSessionUUID())
	ctx = WithCodexClientModel(ctx, convergenceModel)
	ctx = BindCodexTurnStateRequest(ctx, s.account, convergenceModel)
	ctx = WithCodexTurnStateBinding(ctx)
	return WithCodexAffinityKey(ctx, s.affinityKey)
}

// mintBoundTicket 走探针的铸造收尾：按 sticky 模板生成带新 sid 的出口 URL，把票据与
// 绑定出口一起记到账号上（harvester.probe() 的收尾就是 recordTicketWithBinding）。
func (s *convergenceScenario) mintBoundTicket(t *testing.T) (state, proxyURL, sid string) {
	t.Helper()
	proxyURL, sid, err := codexTurnStateStickyProxy(CurrentCodexTurnStateTicketConfig().HarvestProxyURL)
	if err != nil {
		t.Fatalf("sticky harvest proxy: %v", err)
	}
	if !strings.Contains(proxyURL, "sid-"+sid) {
		t.Fatalf("minted sticky proxy %q does not carry the fresh sid-%s", proxyURL, sid)
	}
	state = convergenceTurnStateValue(3, 10)
	s.h.recordTicketWithBinding(s.account, convergenceModel, state, "probe", true, proxyURL, sid, convergenceModel, "")
	ticket, ok := s.account.CodexTurnStateTickets[convergenceModel]
	if !ok || ticket.State != state || ticket.ProxyURL != proxyURL || ticket.ProxySID != sid {
		t.Fatalf("probe ticket not stored with its binding: %+v ok=%v", ticket, ok)
	}
	// 收窄有效期（此时仍有效）：让响应回购后的续期可观测。
	ticket.ExpiresAt = time.Now().Add(90 * time.Second)
	s.account.CodexTurnStateTickets[convergenceModel] = ticket
	s.h.store.ApplyAccountCodexTurnStateTicket(s.account.DBID, convergenceModel, ticket)
	return state, proxyURL, sid
}

// request 发一次真实出站。假出口拒绝隧道，所以这里必然失败——失败本身不参与断言，
// "这次拨号落在哪条出口"由假出口记录证明；成功反而说明出站没经过被测出口。
func (s *convergenceScenario) request(t *testing.T, label string, ctx context.Context, proxyOverride string) {
	t.Helper()
	if _, err := ExecuteRequest(ctx, s.account, s.body, "", proxyOverride, "api-key-1", nil, s.downstream, false); err == nil {
		t.Fatalf("%s: 假出口拒绝隧道后请求仍然成功，说明出站没有经过被测出口", label)
	}
}

// harvestOn 走响应回购的真实入口：上游响应头 → Stage → Commit。
func harvestOn(ctx context.Context, value string) {
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, value)
	headers.Set("openai-model", convergenceModel)
	StageCodexTurnStateResponse(ctx, headers)
	CommitCodexTurnStateRequest(ctx)
}

func storedTicket(t *testing.T, account *auth.Account) auth.CodexTurnStateTicket {
	t.Helper()
	ticket, ok := account.CodexTurnStateTickets[convergenceModel]
	if !ok {
		t.Fatal("managed ticket disappeared from the account")
	}
	return ticket
}

// assertConvergenceDial 断言该假出口新增了恰好一次到上游的 CONNECT，且带上期望的
// 粘性凭据（无 userinfo 的出口期望空用户名）。
func assertConvergenceDial(t *testing.T, label string, proxy *convergenceEgressProxy, before int, wantUser string) {
	t.Helper()
	connects := proxy.snapshot()
	if len(connects) != before+1 {
		t.Fatalf("%s: 出口 %s 累计 %d 次 CONNECT，期望 %d 次", label, proxy.server.URL, len(connects), before+1)
	}
	last := connects[len(connects)-1]
	if last.target != convergenceUpstreamTarget {
		t.Fatalf("%s: CONNECT 目标 = %q，期望 %q", label, last.target, convergenceUpstreamTarget)
	}
	if last.user != wantUser {
		t.Fatalf("%s: 出口 %s 收到的代理凭据用户 = %q，期望 %q", label, proxy.server.URL, last.user, wantUser)
	}
}

func assertNoConvergenceDial(t *testing.T, label string, proxy *convergenceEgressProxy, before int) {
	t.Helper()
	if got := proxy.count(); got != before {
		t.Fatalf("%s: 出口 %s 新增 %d 次 CONNECT，期望 0 次", label, proxy.server.URL, got-before)
	}
}

// TestCodexTurnStateConvergenceLegacyTicketToBoundEgress 复现运营故障的完整收敛过程：
// legacy 无绑定票据 → 探针铸造绑定票据 → 出站回到绑定出口 → 回购继承绑定并续期 →
// 后续请求（含回带自己的票据）仍走同一条出口。
func TestCodexTurnStateConvergenceLegacyTicketToBoundEgress(t *testing.T) {
	s := newConvergenceScenario(t, 9001, "turn-state-convergence::api-key:1")

	// 阶段 1：legacy 票据 + 客户端回带 → 绑定出口尚不存在，出站落在分组代理池出口，
	// 注入的是回带值本身。绑定尚未建立时这是正确行为，也是修复前的常态。
	legacyCtx := s.attempt()
	poolBefore, boundBefore, accountBefore := s.poolEgress.count(), s.boundEgress.count(), s.accountEgress.count()
	s.request(t, "legacy attempt", legacyCtx, s.poolEgress.url())
	if proxyURL, decided := CodexTurnStateBoundEgress(legacyCtx); !decided || proxyURL != "" {
		t.Fatalf("legacy attempt 的绑定出口 = %q decided=%v，期望 decided=true 且无绑定出口", proxyURL, decided)
	}
	assertConvergenceDial(t, "legacy attempt", s.poolEgress, poolBefore, "")
	assertNoConvergenceDial(t, "legacy attempt", s.boundEgress, boundBefore)
	assertNoConvergenceDial(t, "legacy attempt", s.accountEgress, accountBefore)
	if got := snapshotUpstreamTrace(legacyCtx).InjectedTurnState; got != s.echo {
		t.Fatalf("legacy attempt 注入 = %q，期望客户端回带值 %q", got, s.echo)
	}

	// 阶段 2：探针按粘性模板铸造绑定票据（模板里的 sid-aaaa 被换成新 sid）。
	mintedState, mintedProxyURL, mintedSID := s.mintBoundTicket(t)

	// 阶段 3：绑定票据就位，客户端仍回带自己的票据 → 出站必须回到票据绑定出口，且注入
	// 的是绑定票据：回带值本身不带出口信息，跟着它发必然落到别的出口被上游拒收。
	boundCtx := s.attempt()
	poolBefore, boundBefore, accountBefore = s.poolEgress.count(), s.boundEgress.count(), s.accountEgress.count()
	s.request(t, "bound attempt", boundCtx, s.poolEgress.url())
	if proxyURL, decided := CodexTurnStateBoundEgress(boundCtx); !decided || proxyURL != mintedProxyURL {
		t.Fatalf("bound attempt 的绑定出口 = %q decided=%v，期望 %q decided=true", proxyURL, decided, mintedProxyURL)
	}
	assertConvergenceDial(t, "bound attempt", s.boundEgress, boundBefore, "sid-"+mintedSID)
	assertNoConvergenceDial(t, "bound attempt（分组/代理池 override）", s.poolEgress, poolBefore)
	assertNoConvergenceDial(t, "bound attempt（账号出口）", s.accountEgress, accountBefore)
	if got := snapshotUpstreamTrace(boundCtx).InjectedTurnState; got != mintedState {
		t.Fatalf("bound attempt 注入 = %q，期望绑定票据 %q", got, mintedState)
	}

	// 阶段 3 的反证：把绑定出口从票据上摘掉——正是修复前被无绑定回购顶掉之后的存储
	// 状态——同一个请求立刻落回分组代理池出口并注入回带值。这证明阶段 3 的断言确实由
	// 票据绑定驱动，而不是出站的某种固定偏好；摘掉的绑定随后原样放回。
	mintedTicket := storedTicket(t, s.account)
	clobbered := mintedTicket
	clobbered.ProxyURL, clobbered.ProxySID = "", ""
	s.account.CodexTurnStateTickets[convergenceModel] = clobbered
	controlCtx := s.attempt()
	poolBefore, boundBefore = s.poolEgress.count(), s.boundEgress.count()
	s.request(t, "反证: 票据没有绑定出口", controlCtx, s.poolEgress.url())
	if proxyURL, decided := CodexTurnStateBoundEgress(controlCtx); !decided || proxyURL != "" {
		t.Fatalf("反证: 无绑定票据的尝试出口 = %q decided=%v，期望 decided=true 且无绑定出口", proxyURL, decided)
	}
	assertConvergenceDial(t, "反证: 票据没有绑定出口", s.poolEgress, poolBefore, "")
	assertNoConvergenceDial(t, "反证: 票据没有绑定出口", s.boundEgress, boundBefore)
	if got := snapshotUpstreamTrace(controlCtx).InjectedTurnState; got != s.echo {
		t.Fatalf("反证: 无绑定票据时注入 = %q，期望回带值 %q", got, s.echo)
	}
	s.account.CodexTurnStateTickets[convergenceModel] = mintedTicket

	// 阶段 4：同一次绑定尝试的响应回购：上游新铸的票据继承这次尝试的绑定出口并续期。
	// 修复前这一步会被无绑定票据顶掉，此后所有请求都失去出口绑定。
	before := storedTicket(t, s.account)
	harvested := convergenceTurnStateValue(4, 12)
	harvestOn(boundCtx, harvested)
	after := storedTicket(t, s.account)
	if after.State != harvested {
		t.Fatalf("回购后票据 = %q，期望上游新铸的 %q", after.State, harvested)
	}
	if after.ProxyURL != mintedProxyURL || after.ProxySID != mintedSID {
		t.Fatalf("回购丢了绑定出口: proxy=%q sid=%q，期望 proxy=%q sid=%q", after.ProxyURL, after.ProxySID, mintedProxyURL, mintedSID)
	}
	if after.Source != "response" {
		t.Fatalf("回购票据来源 = %q，期望 response", after.Source)
	}
	if !after.ExpiresAt.After(before.ExpiresAt) {
		t.Fatalf("回购没有续期: before=%s after=%s", before.ExpiresAt, after.ExpiresAt)
	}

	// 阶段 5：闭环。回购后的票据再出一次站，仍走同一条绑定出口；这一轮不给 override，
	// 走的是"账号 proxy_url / 直连"路径，确认绑定优先于账号出口。
	nextCtx := s.attempt()
	accountBefore, boundBefore = s.accountEgress.count(), s.boundEgress.count()
	s.request(t, "post-harvest attempt", nextCtx, "")
	assertConvergenceDial(t, "post-harvest attempt", s.boundEgress, boundBefore, "sid-"+mintedSID)
	assertNoConvergenceDial(t, "post-harvest attempt（账号出口）", s.accountEgress, accountBefore)
	if got := snapshotUpstreamTrace(nextCtx).InjectedTurnState; got != harvested {
		t.Fatalf("post-harvest attempt 注入 = %q，期望回购后的票据 %q", got, harvested)
	}

	// 阶段 6：把这次的实际出口随响应头下发给下游（relay 记录的溯源出口）。客户端下一轮
	// 回带自己的票据时，出站仍必须回到同一条出口——这就是"票据走到哪条出口，后续携带
	// 它的请求就走哪条出口"。
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	relayHeaders := http.Header{}
	relayHeaders.Set(codexTurnStateHeader, harvested)
	relayCodexTurnStateResponseHeader(c, s.affinityKey, s.account, relayHeaders, mintedProxyURL)
	if got := recorder.Header().Get(codexTurnStateHeader); got != harvested {
		t.Fatalf("下发给下游的票据 = %q，期望 %q", got, harvested)
	}
	if got := codexTurnStateProxyForAffinity(s.affinityKey); got != mintedProxyURL {
		t.Fatalf("溯源出口 = %q，期望 %q", got, mintedProxyURL)
	}
	echoCtx := s.attempt()
	boundBefore = s.boundEgress.count()
	s.request(t, "echo attempt", echoCtx, s.poolEgress.url())
	if proxyURL, decided := CodexTurnStateBoundEgress(echoCtx); !decided || proxyURL != mintedProxyURL {
		t.Fatalf("echo attempt 的绑定出口 = %q decided=%v，期望 %q decided=true", proxyURL, decided, mintedProxyURL)
	}
	assertConvergenceDial(t, "echo attempt", s.boundEgress, boundBefore, "sid-"+mintedSID)
	if got := snapshotUpstreamTrace(echoCtx).InjectedTurnState; got != s.echo {
		t.Fatalf("echo attempt 注入 = %q，期望保留的回带值 %q", got, s.echo)
	}
}

// TestCodexTurnStateConvergenceUnboundHarvestKeepsBoundTicket 覆盖需求 4：没有走绑定
// 出口的尝试回购到的无绑定票据，绝不能顶掉同模型上仍然有效的绑定票据。两种触发形态
// 都覆盖，最后一个子用例是反证，确保前面的"没被顶掉"来自绑定保护而非路径失效。
func TestCodexTurnStateConvergenceUnboundHarvestKeepsBoundTicket(t *testing.T) {
	s := newConvergenceScenario(t, 9002, "turn-state-convergence-unbound::api-key:1")
	mintedState, _, mintedSID := s.mintBoundTicket(t)
	bound := storedTicket(t, s.account)

	// 形态 A：调用方没挂逐 attempt 出口决策记录器。今天 BindCodexTurnStateRequest 只在
	// handler.go:4916 调用、紧接着 4919 就挂了记录器，所以这个形态暂不可达；但回购侧唯一
	// 的输入就是"这次尝试的出口决策"，这里按该契约直接构造，防止将来出现"绑定了 pending
	// capture 却没挂记录器"的调用方把绑定回购掉。
	undecided := BindCodexTurnStateRequest(WithCodexClientModel(convergenceTraceContext(NewUpstreamSessionUUID()), convergenceModel), s.account, convergenceModel)
	if _, decided := CodexTurnStateBoundEgress(undecided); decided {
		t.Fatal("没有挂记录器的尝试却报告了已定稿的出口")
	}
	poolBefore, boundBefore := s.poolEgress.count(), s.boundEgress.count()
	s.request(t, "undecided attempt", undecided, s.poolEgress.url())
	if got := snapshotUpstreamTrace(undecided).InjectedTurnState; got != mintedState {
		t.Fatalf("undecided attempt 注入 = %q，期望绑定票据 %q", got, mintedState)
	}
	assertConvergenceDial(t, "undecided attempt", s.boundEgress, boundBefore, "sid-"+mintedSID)
	assertNoConvergenceDial(t, "undecided attempt", s.poolEgress, poolBefore)
	harvestOn(undecided, convergenceTurnStateValue(5, 12))
	if got := storedTicket(t, s.account); got != bound {
		t.Fatalf("无记录器尝试的回购顶掉了绑定票据: %+v", got)
	}

	// 形态 B：出口决策定稿为"没有绑定出口"（账号/分组/直连）。回购侧的唯一输入就是这份
	// 决策，这里按注入侧在无绑定出口时写下的同一个取值落定。
	unbound := s.attempt()
	noteCodexTurnStateEgress(unbound, "")
	if proxyURL, decided := CodexTurnStateBoundEgress(unbound); !decided || proxyURL != "" {
		t.Fatalf("unbound attempt 的出口 = %q decided=%v，期望 decided=true 且无绑定出口", proxyURL, decided)
	}
	harvestOn(unbound, convergenceTurnStateValue(6, 12))
	if got := storedTicket(t, s.account); got != bound {
		t.Fatalf("无绑定出口尝试的回购顶掉了绑定票据: %+v", got)
	}

	// 反证：把绑定票据删掉后，同样的无绑定回购必须照常落库（落成无绑定票据），
	// 否则本用例的"没被顶掉"可能只是回购整体失效。
	delete(s.account.CodexTurnStateTickets, convergenceModel)
	control := s.attempt()
	noteCodexTurnStateEgress(control, "")
	controlState := convergenceTurnStateValue(7, 10)
	harvestOn(control, controlState)
	got := storedTicket(t, s.account)
	if got.State != controlState || got.ProxyURL != "" || got.ProxySID != "" || got.Source != "response" {
		t.Fatalf("反证用例: 无绑定回购没有照常落库: %+v", got)
	}
}
