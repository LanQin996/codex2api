package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

type upstreamTraceContextKey struct{}
type upstreamTraceAttempt struct {
	accountID int64
	requestID string
	proxy     auth.ProxyAuditLabel
	// account 是本次尝试的账号，重算代理标签时据它套用 Resin 规则。
	account *auth.Account
	// injectedTurnState 是本次尝试实际注入到出站请求上的凭据级 X-Codex-Turn-State；
	// upstreamTurnState 是上游响应回带的观测值。均为空串表示没有。
	injectedTurnState string
	upstreamTurnState string
}

type upstreamTraceSnapshot struct {
	RequestID         string
	accountID         int64
	UpstreamRequestID string
	Proxy             auth.ProxyAuditLabel
	InjectedTurnState string
	UpstreamTurnState string
}

func snapshotUpstreamTrace(ctx context.Context) upstreamTraceSnapshot {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return upstreamTraceSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	result := upstreamTraceSnapshot{RequestID: a.requestID}
	if a.current != nil {
		result.accountID = a.current.accountID
		result.UpstreamRequestID = a.current.requestID
		result.Proxy = a.current.proxy
		result.InjectedTurnState = a.current.injectedTurnState
		result.UpstreamTurnState = a.current.upstreamTurnState
	}
	return result
}

func (s upstreamTraceSnapshot) apply(input *database.UsageLogInput) {
	input.RequestID = s.RequestID
	if s.accountID == input.AccountID {
		input.UpstreamRequestID = s.UpstreamRequestID
		input.UpstreamProxyID = s.Proxy.ID
		input.UpstreamProxyName = s.Proxy.Name
		input.InjectedTurnState = s.InjectedTurnState
		input.UpstreamTurnState = s.UpstreamTurnState
	}
}

type upstreamTraceAudit struct {
	mu        sync.Mutex
	requestID string
	store     *auth.Store
	current   *upstreamTraceAttempt
}

func upstreamTraceFromContext(ctx context.Context) *upstreamTraceAudit {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(upstreamTraceContextKey{}).(*upstreamTraceAudit)
	return a
}

func attachUpstreamTrace(c *gin.Context, store *auth.Store) {
	if c == nil || c.Request == nil {
		return
	}
	a := &upstreamTraceAudit{requestID: NewUpstreamSessionUUID(), store: store}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), upstreamTraceContextKey{}, a))
	c.Header("X-Codex2API-Request-ID", a.requestID)
}

func resetUpstreamRequestTrace(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	if a := upstreamTraceFromContext(c.Request.Context()); a != nil {
		a.mu.Lock()
		a.requestID = NewUpstreamSessionUUID()
		a.current = nil
		a.mu.Unlock()
	}
}

func resetUpstreamAttemptTrace(ctx context.Context) {
	if a := upstreamTraceFromContext(ctx); a != nil {
		a.mu.Lock()
		a.current = nil
		a.mu.Unlock()
	}
}

func beginUpstreamTrace(ctx context.Context, account *auth.Account, proxyURL string, ws bool) func(*http.Response) {
	a := upstreamTraceFromContext(ctx)
	if a == nil || account == nil {
		return func(*http.Response) {}
	}
	label := upstreamProxyAuditLabel(a.store, account, proxyURL, ws)
	attempt := &upstreamTraceAttempt{accountID: account.ID(), account: account, proxy: label, injectedTurnState: CodexTurnStateInjectionFromContext(ctx)}
	a.mu.Lock()
	a.current = attempt
	a.mu.Unlock()
	header := account.GetUpstreamRequestIDHeader()
	return func(resp *http.Response) {
		if resp == nil || ws {
			return
		} // A WS handshake ID is not a per-turn ID; WS turn state arrives per frame, see ObserveCodexTurnStateFrame.
		turnState := observedCodexTurnState(resp.Header.Get(codexTurnStateHeader))
		id := ""
		if header != "" && auth.ValidateUpstreamRequestIDHeader(header) == nil {
			id = resp.Header.Get(header)
		} else if header == "" {
			for _, name := range []string{"X-Request-Id", "Request-Id", "X-Goog-Request-Id"} {
				if id = resp.Header.Get(name); strings.TrimSpace(id) != "" {
					break
				}
			}
		}
		id = security.SafeTruncate(strings.TrimSpace(id), 128)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current == attempt {
			attempt.requestID = id
			if turnState != "" {
				attempt.upstreamTurnState = turnState
			}
		}
	}
}

// noteUpstreamTurnState 把上游回带的 turn state 记到当前尝试上；WS 路径逐帧调用，
// 后到的值覆盖先到的。
func noteUpstreamTurnState(ctx context.Context, state string) {
	a := upstreamTraceFromContext(ctx)
	if a == nil || state == "" {
		return
	}
	a.mu.Lock()
	if a.current != nil {
		a.current.upstreamTurnState = state
	}
	a.mu.Unlock()
}

// upstreamProxyAuditLabel 把一条出站代理 URL 折算成用量日志里的代理标签，口径与
// beginUpstreamTrace 完全一致：未注册 URL → unmanaged，空 → direct/no_proxy
// （WS 下空是 unknown，因为 WS 拨号不经过 HTTP 代理池），Resin 承担出站 → resin。
func upstreamProxyAuditLabel(store *auth.Store, account *auth.Account, proxyURL string, ws bool) auth.ProxyAuditLabel {
	label := store.ProxyAuditForURL(proxyURL)
	if ws && proxyURL == "" {
		label = auth.ProxyAuditLabel{Name: "unknown"}
	}
	if resinCarriesEgress(account) {
		label = auth.ProxyAuditLabel{Name: "resin"}
	}
	label.Name = security.MaskSensitiveData(label.Name)
	return label
}

// NoteUpstreamTraceProxy 按"最终定稿的拨号出口"重刷当前尝试的代理标签。WS 路径的
// 出口（票据绑定出口 / Resin 反代）在 beginUpstreamTrace 之后才定稿，不重刷的话
// 用量日志只会显示调用方传入的入参代理，看不出这次到底从哪条出口出去。
func NoteUpstreamTraceProxy(ctx context.Context, proxyURL string, ws bool) {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil {
		return
	}
	a.current.proxy = upstreamProxyAuditLabel(a.store, a.current.account, proxyURL, ws)
}

func doTracedUpstreamRequest(client *http.Client, req *http.Request, account *auth.Account, proxyURL string) (*http.Response, error) {
	record := beginUpstreamTrace(req.Context(), account, proxyURL, false)
	resp, err := client.Do(req)
	record(resp)
	if resp != nil {
		fallback := ""
		if req != nil && req.URL != nil {
			fallback = req.URL.String()
		}
		ObserveCodexRouteResponseCookies(req.Context(), account, fallback, resp.Header)
	}
	return resp, err
}

func populateUpstreamTrace(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	if input.RequestID != "" {
		return
	} // Hidden continuation rounds carry their own snapshot.
	a := upstreamTraceFromContext(c.Request.Context())
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	input.RequestID = a.requestID
	if current := a.current; current != nil && current.accountID == input.AccountID {
		input.UpstreamRequestID = current.requestID
		input.UpstreamProxyID = current.proxy.ID
		input.UpstreamProxyName = current.proxy.Name
		input.InjectedTurnState = current.injectedTurnState
		input.UpstreamTurnState = current.upstreamTurnState
	}
}
