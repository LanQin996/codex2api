package proxy

import (
	"context"
	"errors"
	"net/http"

	"github.com/codex2api/auth"
)

// WithAccountBoundEgress freezes an account's bound proxy for this request.
// An empty proxy URL explicitly means direct egress, including when Resin or
// environment proxies are configured. Callers must pass account.GetProxyURL(),
// not the inherited group/global/pool route.
func WithAccountBoundEgress(ctx context.Context, proxyURL string) context.Context {
	return auth.WithAccountBoundEgress(ctx, proxyURL)
}

// AccountBoundEgress reports the explicit route and whether it was requested.
func AccountBoundEgress(ctx context.Context) (string, bool) {
	return auth.AccountBoundEgress(ctx)
}

// requestProxyURL preserves an explicit empty route instead of re-reading an
// account or inheriting configuration after this request started.
func requestProxyURL(ctx context.Context, fallback string) string {
	if route, ok := AccountBoundEgress(ctx); ok {
		return route
	}
	return fallback
}

func getRequestClient(ctx context.Context, account *auth.Account, fallback string) *http.Client {
	if route, ok := AccountBoundEgress(ctx); ok {
		return getPooledClientForRoute(account, route, true)
	}
	return getPooledClient(account, fallback)
}

func recycleRequestClient(ctx context.Context, account *auth.Account, fallback string) {
	if route, ok := AccountBoundEgress(ctx); ok {
		recyclePooledClientForRoute(account, route, true)
		return
	}
	recyclePooledClient(account, fallback)
}

type accountBoundProxyErrorTransport struct{}

func (accountBoundProxyErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("invalid account-bound proxy configuration")
}

func newAccountBoundTransport(proxyURL string) http.RoundTripper {
	return newAccountBoundTransportForMode(proxyURL, codexTransportModeFromEnv())
}

func newAccountBoundTransportForMode(proxyURL, transportMode string) http.RoundTripper {
	// Validate before legacy transport constructors, which deliberately fall back
	// to direct on invalid configuration for ordinary requests.
	if err := auth.ConfigureTransportProxy(&http.Transport{}, proxyURL, nil); err != nil {
		return accountBoundProxyErrorTransport{}
	}
	transport := newLegacyCodexTransport(proxyURL)
	if transportMode == "codex_tls" {
		transport = newCodexProfileTransport(proxyURL)
	}
	if standard, ok := transport.(*http.Transport); ok && proxyURL == "" {
		standard.Proxy = nil
	}
	return transport
}
