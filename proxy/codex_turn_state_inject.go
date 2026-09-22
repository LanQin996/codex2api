package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 凭据级 X-Codex-Turn-State 强制注入（配置见 auth/codex_turn_state.go）。
//
// 注入发生在 ExecuteRequest 内部、传输方式与模型定稿之后：HTTP 路径写在账号自定义头
// 之后（自定义头不该顶掉运维显式配的注入值），WebSocket 路径同时写握手头与
// response.create 帧体的 client_metadata——握手头逐连接冻结，复用连接只认帧体。
// 注入不回灌下游请求体，因此不影响入口处基于 turn-state 判定"活跃回合"的调度钉号。
//
// 决策只算一次并挂到 ctx：出站头、帧体与用量日志（upstream_trace）都从同一份取值，
// 两边不会对"注入了没有"给出不同答案。

// codexTurnStateMetadataKey 是 WS response.create 帧体里承载 turn state 的键，
// 与请求头同名（官方客户端把该头原样放进 client_metadata）。
const codexTurnStateMetadataKey = "x-codex-turn-state"

type codexTurnStateInjectionKey struct{}
type codexTurnStateProxyKey struct{}
type codexClientModelKey struct{}
type codexTurnStateBindingKey struct{}
type codexAffinityKeyKey struct{}

// WithCodexClientModel 记录下游请求的原始模型名，供模型名单与上游模型名一并匹配：
// 映射改写之后两者常常不是同一个名字，而操作者填的通常是自己请求时用的那个。
func WithCodexClientModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return ctx
	}
	return context.WithValue(ctx, codexClientModelKey{}, model)
}

func codexClientModelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(codexClientModelKey{}).(string)
	return model
}

// CodexTurnStateBinding 是逐 attempt 可变的出口决策记录器。出站决策（注入哪个
// turn state、走不走票据绑定出口）在 ExecuteRequest 的局部 ctx 里定稿，不可变的
// ctx 值传不回 handler；handler 在每次尝试的 upstreamCtx 上挂一个空实例，注入代码
// 把实际决定写进来，响应采集据此知道这次请求到底有没有走票据绑定出口——
// 没走绑定出口的尝试绝不能拿新票据顶掉旧绑定票据，否则绑定就此丢失。
type CodexTurnStateBinding struct {
	mu       sync.Mutex
	proxyURL string
	decided  bool
	injected string
}

// WithCodexTurnStateBinding 在逐 attempt 的 ctx 上挂一个新的出口决策记录器。
func WithCodexTurnStateBinding(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexTurnStateBindingKey{}, &CodexTurnStateBinding{})
}

func codexTurnStateBindingFromContext(ctx context.Context) *CodexTurnStateBinding {
	if ctx == nil {
		return nil
	}
	binding, _ := ctx.Value(codexTurnStateBindingKey{}).(*CodexTurnStateBinding)
	return binding
}

// noteCodexTurnStateEgress 记录本次尝试最终的出口决策：proxyURL 为空表示这次出站
// 没有绑定出口（账号/分组/代理池/直连）。decided 与取值一起落定，调用方不必猜
// "没记到"到底是没挂记录器还是真的没绑定。ctx 上没有记录器时是空操作。
func noteCodexTurnStateEgress(ctx context.Context, proxyURL string) {
	binding := codexTurnStateBindingFromContext(ctx)
	if binding == nil {
		return
	}
	binding.mu.Lock()
	binding.proxyURL = strings.TrimSpace(proxyURL)
	binding.decided = true
	binding.mu.Unlock()
}

// CodexTurnStateBoundEgress 返回本次尝试已定稿的绑定出口。decided=false 表示
// 没有挂记录器（该路径不产生票据采集），不可当作"这次没有绑定出口"。
func CodexTurnStateBoundEgress(ctx context.Context) (proxyURL string, decided bool) {
	binding := codexTurnStateBindingFromContext(ctx)
	if binding == nil {
		return "", false
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	return binding.proxyURL, binding.decided
}

// codexTurnStateBoundProxy 是 CodexTurnStateBoundEgress 的单值形态。
func codexTurnStateBoundProxy(ctx context.Context) string {
	proxyURL, _ := CodexTurnStateBoundEgress(ctx)
	return proxyURL
}

// WithCodexAffinityKey 把下游会话亲和键挂到逐 attempt 的 ctx 上：注入侧要据它查
// 溯源表——客户端回带的 blob 上一次是从哪条出口下发给这个会话的。
func WithCodexAffinityKey(ctx context.Context, key string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, codexAffinityKeyKey{}, key)
}

func codexAffinityKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(codexAffinityKeyKey{}).(string)
	return key
}

func withCodexTurnStateInjection(ctx context.Context, value string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if binding := codexTurnStateBindingFromContext(ctx); binding != nil {
		binding.mu.Lock()
		binding.injected = value
		binding.mu.Unlock()
	}
	return context.WithValue(ctx, codexTurnStateInjectionKey{}, value)
}

// CodexTurnStateInjectionFromContext 返回本次出站已决定注入的值，空串表示不注入。
func CodexTurnStateInjectionFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(codexTurnStateInjectionKey{}).(string)
	if value == "" {
		if binding := codexTurnStateBindingFromContext(ctx); binding != nil {
			binding.mu.Lock()
			value = binding.injected
			binding.mu.Unlock()
		}
	}
	return value
}

// CodexTurnStateProxyFromContext returns the sticky egress bound to the
// injected ticket. Empty means the normal account/group/pool proxy applies.
func CodexTurnStateProxyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(codexTurnStateProxyKey{}).(string)
	return strings.TrimSpace(value)
}

func withCodexTurnStateProxy(ctx context.Context, value string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexTurnStateProxyKey{}, strings.TrimSpace(value))
}

// prepareCodexTurnStateInjection 决定并落定注入：返回携带决策的 ctx、（可能克隆的）
// 下游头与（WS 时改写了帧体的）请求体。未配置或名单未命中时全部原样返回。
func prepareCodexTurnStateInjection(ctx context.Context, account *auth.Account, requestBody []byte, headers http.Header, websocket bool) (context.Context, []byte, http.Header) {
	injectedProxy := ""
	if account == nil {
		noteCodexTurnStateEgress(ctx, injectedProxy)
		return ctx, requestBody, headers
	}
	upstreamModel := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	clientModel := codexClientModelFromContext(ctx)
	injected := ""
	cfg := CurrentCodexTurnStateTicketConfig()
	if cfg.PreserveExisting && headers != nil {
		existing := observedCodexTurnState(headers.Get(codexTurnStateHeader))
		if auth.ValidCodexTurnStateTicketValue(existing, cfg.TargetLength) {
			injected = existing
			// 回带值本身不带出口信息，能确定的只有"上一次向该会话下发它的那条出口"。
			if cfg.TicketProxySticky {
				injectedProxy = codexTurnStateProxyForAffinity(codexAffinityKeyFromContext(ctx))
			}
			if cfg.TicketProxySticky && injectedProxy == "" && cfg.Enabled && cfg.ModelManaged(clientModel, upstreamModel) {
				// 溯源里没有出口时优先换成账号上已绑定出口的托管票据：无绑定的回带值
				// 会把票据发到别的出口，上游按铸造出口校验必然拒收——绑定票据比无
				// 绑定回带值更可复用，绑定出口的保证不能在这一步丢掉。
				if ticket, ok := boundCodexTurnStateTicket(account, cfg, clientModel, upstreamModel); ok {
					injected = ticket.State
					if cfg.TicketProxySticky {
						injectedProxy = strings.TrimSpace(ticket.ProxyURL)
					}
				}
			}
		}
	}
	if injected == "" && cfg.Enabled && cfg.ModelManaged(clientModel, upstreamModel) {
		if ticket, ok := account.CodexTurnStateTicket(upstreamModel, cfg.TargetLength, time.Now()); ok {
			injected = ticket.State
			if cfg.TicketProxySticky {
				injectedProxy = ticket.ProxyURL
			}
		} else if ticket, ok := account.CodexTurnStateTicket(clientModel, cfg.TargetLength, time.Now()); ok {
			injected = ticket.State
			if cfg.TicketProxySticky {
				injectedProxy = ticket.ProxyURL
			}
		}
	}
	if injected == "" {
		injected = account.CodexTurnStateInjection(clientModel, upstreamModel)
	}
	if injected == "" {
		noteCodexTurnStateEgress(ctx, injectedProxy)
		return ctx, requestBody, headers
	}
	noteCodexTurnStateEgress(ctx, injectedProxy)
	NoteCodexTurnStateInjected()
	ctx = withCodexTurnStateInjection(ctx, injected)
	ctx = withCodexTurnStateProxy(ctx, injectedProxy)
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}
	headers.Set(codexTurnStateHeader, injected)
	if websocket {
		// 帧体承载：WS 的握手头逐连接冻结，复用连接根本发不出新值，官方客户端因此把
		// turn state 放进 response.create 的 client_metadata——WS 路径必须写。
		if updated, err := sjson.SetBytes(requestBody, "client_metadata."+codexTurnStateMetadataKey, injected); err == nil {
			requestBody = updated
		}
	}
	return ctx, requestBody, headers
}

// boundCodexTurnStateTicket 找账号上"已绑定出口"的托管票据：上游模型优先，其次是
// 下游客户端模型（映射改写后两者常常不是同一个名字）。没绑定出口的票据不返回。
func boundCodexTurnStateTicket(account *auth.Account, cfg *CodexTurnStateTicketConfig, clientModel, upstreamModel string) (auth.CodexTurnStateTicket, bool) {
	if account == nil || cfg == nil {
		return auth.CodexTurnStateTicket{}, false
	}
	now := time.Now()
	for _, model := range []string{upstreamModel, clientModel} {
		ticket, ok := account.CodexTurnStateTicket(model, cfg.TargetLength, now)
		if ok && strings.TrimSpace(ticket.ProxyURL) != "" {
			return ticket, true
		}
	}
	return auth.CodexTurnStateTicket{}, false
}

// applyCodexTurnStateInjectionHeader 在账号自定义头装配之后落定注入值：自定义头不该
// 把别的状态带回上游顶掉运维显式配的注入。未注入时是空操作。
func applyCodexTurnStateInjectionHeader(ctx context.Context, headers http.Header) {
	if headers == nil {
		return
	}
	if value := CodexTurnStateInjectionFromContext(ctx); value != "" {
		headers.Set(codexTurnStateHeader, value)
	}
}

// ApplyCodexTurnStateInjectionHeader 是 applyCodexTurnStateInjectionHeader 的导出形态，
// 供 wsrelay 在握手头装配末尾调用。
func ApplyCodexTurnStateInjectionHeader(ctx context.Context, headers http.Header) {
	applyCodexTurnStateInjectionHeader(ctx, headers)
}

// 观测到的 state 实测在 300 字符上下，留一个数量级的余量即可；超限的一律丢弃，
// 截断后的 state 既不能复用也会误导排查。
const maxObservedCodexTurnStateBytes = 4096

// observedCodexTurnState 规整一个观测值：只接受单行可见字符串，超限丢弃。
func observedCodexTurnState(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxObservedCodexTurnStateBytes || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

var codexTurnStateFrameNeedles = [][]byte{[]byte("turn-state"), []byte("Turn-State")}

// codexTurnStateFromFrame 从 WS 事件帧里找上游回带的 turn state。官方契约里 WS 路径
// 的值来自握手响应头或 response.metadata 事件；这里按键名等值（大小写不敏感）在几个
// 已知承载位置上找，找不到返回空。先做一次零分配的子串预检，避免每帧都解析 JSON。
func codexTurnStateFromFrame(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	mentioned := false
	for _, needle := range codexTurnStateFrameNeedles {
		if bytes.Contains(payload, needle) {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return ""
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return ""
	}
	for _, path := range []string{"headers", "response.headers", "response.client_metadata", "client_metadata", "response.metadata", "metadata", "response", ""} {
		object := root
		if path != "" {
			object = root.Get(path)
		}
		if !object.IsObject() {
			continue
		}
		state := ""
		object.ForEach(func(key, value gjson.Result) bool {
			if strings.EqualFold(key.String(), codexTurnStateHeader) && value.Type == gjson.String {
				state = value.String()
				return false
			}
			return true
		})
		if state = observedCodexTurnState(state); state != "" {
			return state
		}
	}
	return ""
}

// ObserveCodexTurnStateFrame 供 WS 中继在逐帧转发时调用：发现上游回带的 turn state
// 就记到本次尝试的追踪里（用量日志据此显示"回带 Turn State"）。
func ObserveCodexTurnStateFrame(ctx context.Context, payload []byte) {
	if explicitlyRejectedTicket(payload) {
		rejectManagedTicketFromContext(ctx)
	}
	if state := codexTurnStateFromFrame(payload); state != "" {
		StageCodexTurnStateValue(ctx, state)
		noteUpstreamTurnState(ctx, state)
	}
}
