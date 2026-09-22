package auth

import (
	"testing"
	"time"
)

func TestCodexRouteCookiesAreModelScopedAndFiltered(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := &Account{}
	scope := "https://chatgpt.com/backend-api/codex/responses"
	if !a.ObserveCodexRouteSetCookies("gpt-5.6-sol", scope, []string{
		"__oailb=route; Path=/backend-api; Max-Age=3600; Secure; HttpOnly",
		"__cflb=west; Path=/; Secure; HttpOnly",
		"__cf_bm=private; Path=/; Secure; HttpOnly",
		"__oailb=insecure; Path=/backend-api",
	}, now) {
		t.Fatal("expected allowlisted cookies to be stored")
	}
	if got := a.CodexRouteCookieHeader("gpt-5.6-sol", scope, now); got != "__oailb=route; __cflb=west" {
		t.Fatalf("header=%q", got)
	}
	if got := a.CodexRouteCookieHeader("gpt-5.5", scope, now); got != "" {
		t.Fatalf("other model header=%q", got)
	}
	if got := a.CodexRouteCookieHeader("gpt-5.6-sol", "https://chatgpt.com/outside", now); got != "__cflb=west" {
		t.Fatalf("path-filtered header=%q", got)
	}
}

func TestReplaceCodexRouteCookiesClearsOldPair(t *testing.T) {
	a := &Account{}
	scope := "https://chatgpt.com/backend-api/codex/responses"
	a.ObserveCodexRouteSetCookies("gpt-5.6-sol", scope, []string{"__oailb=old; Path=/; Secure"}, time.Now())
	if !a.ReplaceCodexRouteCookies("gpt-5.6-sol", scope, nil, time.Now()) {
		t.Fatal("expected replacement to clear old cookies")
	}
	if got := a.CodexRouteCookieHeader("gpt-5.6-sol", scope, time.Now()); got != "" {
		t.Fatalf("stale header=%q", got)
	}
}
