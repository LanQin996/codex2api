package proxy

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

type codexRouteCookieScopeKey struct{}
type codexRouteCookieModelKey struct{}
type codexRouteCookieCaptureKey struct{}

type codexRouteCookieCapture struct {
	scope string
	lines []string
}

func WithCodexRouteCookieScope(ctx context.Context, rawURL, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	scope, ok := codexRouteCookieScope(rawURL)
	if !ok {
		return ctx
	}
	ctx = context.WithValue(ctx, codexRouteCookieScopeKey{}, scope)
	if model = strings.TrimSpace(model); model != "" {
		ctx = context.WithValue(ctx, codexRouteCookieModelKey{}, model)
	}
	return ctx
}

// WithCodexRouteCookieCapture stages Set-Cookie values until the caller has
// accepted the corresponding newly minted ticket.
func WithCodexRouteCookieCapture(ctx context.Context) (context.Context, *codexRouteCookieCapture) {
	if ctx == nil {
		ctx = context.Background()
	}
	capture := &codexRouteCookieCapture{}
	return context.WithValue(ctx, codexRouteCookieCaptureKey{}, capture), capture
}

func codexRouteCookieCaptureFromContext(ctx context.Context) *codexRouteCookieCapture {
	if ctx == nil {
		return nil
	}
	capture, _ := ctx.Value(codexRouteCookieCaptureKey{}).(*codexRouteCookieCapture)
	return capture
}

func ApplyCodexRouteCookies(ctx context.Context, headers http.Header, account *auth.Account, rawURL, model string) {
	if headers == nil || account == nil || strings.TrimSpace(headers.Get(codexTurnStateHeader)) == "" || strings.TrimSpace(headers.Get("Cookie")) != "" {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	scope, ok := codexRouteCookieScope(rawURL)
	if !ok {
		return
	}
	if value := account.CodexRouteCookieHeader(model, scope, time.Now()); value != "" {
		headers.Set("Cookie", value)
	}
}

func ObserveCodexRouteResponseCookies(ctx context.Context, account *auth.Account, fallbackURL string, header http.Header) {
	if account == nil || len(header) == 0 {
		return
	}
	model := codexRouteCookieModelFromContext(ctx)
	if model == "" {
		return
	}
	scope := codexRouteCookieScopeFromContext(ctx)
	if scope == "" {
		var ok bool
		scope, ok = codexRouteCookieScope(fallbackURL)
		if !ok {
			return
		}
	}
	lines := header.Values("Set-Cookie")
	if len(lines) == 0 {
		return
	}
	if capture := codexRouteCookieCaptureFromContext(ctx); capture != nil {
		capture.scope = scope
		capture.lines = append(capture.lines, lines...)
		return
	}
	if account.ObserveCodexRouteSetCookies(model, scope, lines, time.Now()) {
		persistCodexRouteCookies(account)
	}
}

func BindCapturedCodexRouteCookies(account *auth.Account, model string, capture *codexRouteCookieCapture) {
	if account == nil || capture == nil || strings.TrimSpace(model) == "" || capture.scope == "" {
		return
	}
	if account.ReplaceCodexRouteCookies(model, capture.scope, capture.lines, time.Now()) {
		persistCodexRouteCookies(account)
	}
}

// MergeCapturedCodexRouteCookies applies response cookies to the existing
// model jar without clearing cookies that were not mentioned by this response.
// It is used when a normal request reuses the current ticket.
func MergeCapturedCodexRouteCookies(account *auth.Account, model string, capture *codexRouteCookieCapture) {
	if account == nil || capture == nil || strings.TrimSpace(model) == "" || capture.scope == "" || len(capture.lines) == 0 {
		return
	}
	if account.ObserveCodexRouteSetCookies(model, capture.scope, capture.lines, time.Now()) {
		persistCodexRouteCookies(account)
	}
}

func codexRouteCookieScopeFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	scope, _ := ctx.Value(codexRouteCookieScopeKey{}).(string)
	return scope
}

func codexRouteCookieModelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(codexRouteCookieModelKey{}).(string)
	return strings.TrimSpace(model)
}

func codexRouteCookieScope(raw string) (string, bool) {
	if scope, ok := auth.NormalizeCodexRouteCookieScope(raw); ok {
		return scope, true
	}
	embedded, ok := embeddedCodexRouteURL(raw)
	if !ok {
		return "", false
	}
	return auth.NormalizeCodexRouteCookieScope(embedded)
}

func embeddedCodexRouteURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return "", false
	}
	const marker = "/https/"
	idx := strings.LastIndex(u.Path, marker)
	if idx < 0 {
		return "", false
	}
	rest := u.Path[idx+len(marker):]
	host, path, found := strings.Cut(rest, "/")
	if host == "" {
		return "", false
	}
	if !found || path == "" {
		path = "/"
	}
	return "https://" + host + path, true
}

func persistCodexRouteCookies(account *auth.Account) {
	if account == nil || account.ID() <= 0 {
		return
	}
	h := activeCodexTurnStateHarvester.Load()
	if h == nil || h.db == nil {
		return
	}
	accountID := account.ID()
	go func() {
		h.persistMu.Lock()
		defer h.persistMu.Unlock()
		cookies := account.SnapshotCodexRouteCookies()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.db.UpdateCredentials(ctx, accountID, map[string]any{auth.CodexRouteCookiesCredentialKey: cookies}); err != nil {
			log.Printf("保存 Codex 路由 cookie 失败 account=%d: %v", accountID, err)
		}
	}()
}
