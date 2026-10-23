package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
)

func TestAccountBoundEgressHTTPRoutesAndPoolIsolation(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	var direct, bound, resin, inherited atomic.Int32
	server := func(label string, counter *atomic.Int32) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			if label != "resin" && r.Header.Get("X-Resin-Account") != "" {
				t.Error("unexpected Resin header")
			}
			io.WriteString(w, label)
		}))
		t.Cleanup(s.Close)
		return s
	}
	target := server("direct", &direct)
	boundProxy := server("bound", &bound)
	resinProxy := server("resin", &resin)
	envProxy := server("inherited", &inherited)
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	envURL, _ := url.Parse(envProxy.URL)
	transport.Proxy = http.ProxyURL(envURL)
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
	withResin(t, &ResinConfig{BaseURL: resinProxy.URL, PlatformName: "test"})
	account := &auth.Account{DBID: 998101}
	request := func(ctx context.Context, want string) *http.Client {
		t.Helper()
		egress := ResolveCodexRequestEgress(ctx, account, target.URL, envProxy.URL, false)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, egress.URL, nil)
		egress.ApplyHeaders(req.Header)
		resp, err := egress.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != want {
			t.Errorf("route = %q, want %q", body, want)
		}
		return egress.Client()
	}
	plain := request(context.Background(), "resin")
	detector := request(WithCodexDetectorRequest(context.Background()), "resin")
	directClient := request(WithAccountBoundEgress(context.Background(), ""), "direct")
	boundClient := request(WithAccountBoundEgress(context.Background(), boundProxy.URL), "bound")
	if directClient == plain || directClient == detector || directClient == boundClient || directClient == getPooledClient(account, "") {
		t.Error("explicit egress shared a business/Resin/different-route HTTP pool")
	}
	if again := request(WithAccountBoundEgress(context.Background(), ""), "direct"); again != directClient {
		t.Error("same route should reuse HTTP pool")
	}
	if direct.Load() != 2 || bound.Load() != 1 || resin.Load() != 2 || inherited.Load() != 0 {
		t.Errorf("captures direct=%d bound=%d resin=%d inherited=%d", direct.Load(), bound.Load(), resin.Load(), inherited.Load())
	}
}

func TestAccountBoundEgressInvalidProxyFailsClosed(t *testing.T) {
	withResin(t, nil)
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"standard", "utls_chrome"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CODEX_TRANSPORT_MODE", mode)
			for _, raw := range []string{"socks5h://", "ftp://127.0.0.1:9", "http://%"} {
				ctx := WithAccountBoundEgress(context.Background(), raw)
				egress := ResolveCodexRequestEgress(ctx, &auth.Account{DBID: 998102}, target.URL, raw, false)
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, egress.URL, nil)
				resp, err := egress.Client().Do(req)
				if resp != nil {
					resp.Body.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "account-bound proxy") {
					t.Errorf("invalid proxy did not fail closed: %v", err)
				}
			}
		})
	}
	if received.Load() != 0 {
		t.Errorf("direct target received %d requests", received.Load())
	}
}

func TestAccountBoundEgressRelayHTTPUsesSnapshot(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	var direct, bound atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1); io.WriteString(w, "direct") }))
	defer target.Close()
	boundProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bound.Add(1); io.WriteString(w, "bound") }))
	defer boundProxy.Close()
	withResin(t, &ResinConfig{BaseURL: "http://127.0.0.1:1", PlatformName: "unused"})
	for i, claude := range []bool{false, true} {
		account := &auth.Account{DBID: int64(998110 + i), UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: target.URL, APIKey: "local-key", ProxyURL: boundProxy.URL}
		if claude {
			account.UpstreamType = auth.UpstreamClaude
			account.ClaudeAuthKind = auth.ClaudeAuthKindAPIKey
			account.ClaudeBaseURL = target.URL
			account.AccessToken = "local-key"
		}
		for _, route := range []string{"", boundProxy.URL} {
			ctx := WithAccountBoundEgress(context.Background(), route)
			// Both the current account value and inherited override disagree with
			// the frozen direct route; neither may take it over.
			var resp *http.Response
			var err error
			if claude {
				resp, err = ExecuteClaudeMessagesRequest(ctx, account, []byte(claudeAPIKeyTestBody), boundProxy.URL, nil, "")
			} else {
				resp, err = ExecuteOpenAIResponsesRequest(ctx, account, []byte(`{"model":"gpt-5.4","input":"test"}`), boundProxy.URL, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			want := "direct"
			if route != "" {
				want = "bound"
			}
			if string(body) != want {
				t.Errorf("claude=%t route=%q want=%q", claude, body, want)
			}
		}
	}
	if direct.Load() != 2 || bound.Load() != 2 {
		t.Errorf("captures direct=%d bound=%d", direct.Load(), bound.Load())
	}
}

func TestAccountBoundEgressTransportModeAndAccountPools(t *testing.T) {
	account := &auth.Account{DBID: 998120}
	ctx := WithAccountBoundEgress(context.Background(), "")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	standard := getRequestClient(ctx, account, "")
	if transport, ok := standard.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatal("direct standard TLS transport or route lost")
	}
	t.Setenv("CODEX_TRANSPORT_MODE", "utls_chrome")
	chrome := getRequestClient(ctx, account, "")
	if _, ok := chrome.Transport.(*utlsRoundTripper); !ok {
		t.Fatal("uTLS transport mode lost")
	}
	if chrome == standard || chrome == getRequestClient(ctx, &auth.Account{DBID: 998121}, "") {
		t.Fatal("TLS mode/account pool isolation lost")
	}
}

func TestAccountBoundEgressFailureRecyclesOnlyRequestPool(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	withResin(t, nil)
	for i, kind := range []string{"codex", "responses", "claude-api-key", "claude-oauth"} {
		t.Run(kind, func(t *testing.T) {
			account := &auth.Account{DBID: int64(998130 + i), AccessToken: "local-token", BaseURL: "http://127.0.0.1:1", APIKey: "local-key", ClaudeBaseURL: "http://127.0.0.1:1"}
			ctx := WithAccountBoundEgress(WithCodexDetectorRequest(context.Background()), "")
			business := getPooledClient(account, "")
			strict := getRequestClient(ctx, account, "")
			strict.Transport = claudeBoundaryRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("write: broken pipe")
			})
			var resp *http.Response
			var err error
			switch kind {
			case "codex":
				resp, err = ExecuteRequest(ctx, account, []byte(`{"model":"gpt-5.4","input":"test"}`), "", "", "local-key", nil, nil, false)
			case "responses":
				account.UpstreamType = auth.UpstreamOpenAIResponses
				resp, err = ExecuteOpenAIResponsesRequest(ctx, account, []byte(`{"model":"gpt-5.4","input":"test"}`), "", nil)
			default:
				account.UpstreamType = auth.UpstreamClaude
				if kind == "claude-api-key" {
					account.ClaudeAuthKind = auth.ClaudeAuthKindAPIKey
				}
				resp, err = ExecuteClaudeMessagesRequest(ctx, account, []byte(claudeAPIKeyTestBody), "", nil, "")
			}
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "broken pipe") {
				t.Fatalf("expected injected connection failure, got %v", err)
			}
			if getPooledClient(account, "") != business {
				t.Error("detection failure recycled the ordinary business pool")
			}
			if getRequestClient(ctx, account, "") == strict {
				t.Error("detection failure retained the failed request pool")
			}
		})
	}
}
