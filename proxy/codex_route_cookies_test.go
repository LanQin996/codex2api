package proxy

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func ticketWithAge(age time.Duration) string {
	raw := make([]byte, 1+8+16+10*16+32)
	raw[0] = 0x80
	ts := time.Now().Add(-age).Unix()
	for i := 0; i < 8; i++ {
		raw[8-i] = byte(ts >> (8 * i))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestApplyCodexRouteCookiesRequiresTicketAndPreservesCallerCookie(t *testing.T) {
	a := &auth.Account{}
	scope := "https://chatgpt.com/backend-api/codex/responses"
	a.ObserveCodexRouteSetCookies("gpt-5.6-sol", scope, []string{"__oailb=route; Path=/; Secure"}, time.Now())

	without := http.Header{}
	ApplyCodexRouteCookies(context.Background(), without, a, scope, "gpt-5.6-sol")
	if got := without.Get("Cookie"); got != "" {
		t.Fatalf("mint request cookie=%q", got)
	}

	withTicket := http.Header{}
	withTicket.Set(codexTurnStateHeader, "ticket")
	ApplyCodexRouteCookies(context.Background(), withTicket, a, scope, "gpt-5.6-sol")
	if got := withTicket.Get("Cookie"); got != "__oailb=route" {
		t.Fatalf("replay cookie=%q", got)
	}

	operator := http.Header{}
	operator.Set(codexTurnStateHeader, "ticket")
	operator.Set("Cookie", "operator=value")
	ApplyCodexRouteCookies(context.Background(), operator, a, scope, "gpt-5.6-sol")
	if got := operator.Get("Cookie"); got != "operator=value" {
		t.Fatalf("operator cookie=%q", got)
	}
}

func TestObserveCodexRouteCookiesStagesUntilCommit(t *testing.T) {
	a := &auth.Account{}
	ctx, capture := WithCodexRouteCookieCapture(WithCodexRouteCookieScope(context.Background(), "https://chatgpt.com/backend-api/codex/responses", "gpt-5.6-sol"))
	ObserveCodexRouteResponseCookies(ctx, a, "", http.Header{"Set-Cookie": {"__oailb=staged; Path=/; Secure"}})
	if len(capture.lines) != 1 {
		t.Fatalf("captured lines=%d", len(capture.lines))
	}
	if got := a.CodexRouteCookieHeader("gpt-5.6-sol", "https://chatgpt.com/backend-api/codex/responses", time.Now()); got != "" {
		t.Fatalf("staged cookie was published=%q", got)
	}
	BindCapturedCodexRouteCookies(a, "gpt-5.6-sol", capture)
	if got := a.CodexRouteCookieHeader("gpt-5.6-sol", "https://chatgpt.com/backend-api/codex/responses", time.Now()); got != "__oailb=staged" {
		t.Fatalf("committed cookie=%q", got)
	}
}

func TestHealthyTicketRefreshWindowUsesFernetAge(t *testing.T) {
	h := &CodexTurnStateHarvester{}
	a := &auth.Account{CodexTurnStateTickets: map[string]auth.CodexTurnStateTicket{
		"gpt-5.6-sol": {State: ticketWithAge(210 * time.Second), ExpiresAt: time.Now().Add(time.Hour)},
	}}
	cfg := &CodexTurnStateTicketConfig{TTLSeconds: 3600, RefreshBeforeSeconds: 600}
	if !h.ticketNeedsRefresh(a, "gpt-5.6-sol", time.Now(), cfg) {
		t.Fatal("ticket older than the 200s premint window was not refreshed")
	}
	a.CodexTurnStateTickets["gpt-5.6-sol"] = auth.CodexTurnStateTicket{State: ticketWithAge(90 * time.Second), ExpiresAt: time.Now().Add(time.Hour)}
	if h.ticketNeedsRefresh(a, "gpt-5.6-sol", time.Now(), cfg) {
		t.Fatal("young ticket was refreshed too early")
	}
}
