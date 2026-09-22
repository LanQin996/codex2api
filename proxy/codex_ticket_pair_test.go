package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func pairCookies(value string) []auth.CodexRouteCookie {
	return auth.CodexTicketCookies([]string{"__oailb=" + value + "; Path=/; Secure; Max-Age=60"}, CodexBaseURL+"/responses", time.Now())
}

func TestCodexTicketCandidateCookieNeverUsesOldJar(t *testing.T) {
	a := &auth.Account{}
	a.ObserveCodexRouteSetCookies("gpt-test", CodexBaseURL+"/responses", []string{"__oailb=old; Path=/; Secure"}, time.Now())
	ctx, capture := WithCodexRouteCookieCapture(WithCodexRouteCookieScope(context.Background(), CodexBaseURL+"/responses", "gpt-test"))
	capture.probe = true
	ObserveCodexRouteResponseCookies(ctx, a, "", http.Header{"Set-Cookie": {"__oailb=new; Path=/; Secure; Max-Age=60", "session=secret; Path=/; Secure", "__cf_bm=ignored; Path=/; Secure"}})
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "candidate")
	headers.Set("Cookie", "operator=value; __oailb=stale; __cflb=stale")
	ApplyCodexRouteCookies(ctx, headers, a, CodexBaseURL+"/responses", "gpt-test")
	if got := headers.Get("Cookie"); got != "operator=value; __oailb=new" {
		t.Fatalf("cookie=%s", got)
	}
	if a.CodexRouteCookieHeader("gpt-test", CodexBaseURL+"/responses", time.Now()) != "__oailb=old" {
		t.Fatal("unverified capture mutated account")
	}
	ObserveCodexRouteResponseCookies(ctx, a, "", http.Header{"Set-Cookie": {"__oailb=; Path=/; Secure; Max-Age=0"}})
	ApplyCodexRouteCookies(ctx, headers, a, CodexBaseURL+"/responses", "gpt-test")
	if strings.Contains(headers.Get("Cookie"), "__oailb") {
		t.Fatal("deleted candidate cookie resurrected")
	}
}

func TestCodexTicketPairSnapshotFollowsRequestNotLaterRotation(t *testing.T) {
	h, a := ticketHarvesterFixture(t)
	cfg := *CurrentCodexTurnStateTicketConfig()
	cfg.PreserveExisting = false
	cfg.TicketProxySticky = true
	SetCodexTurnStateTicketConfig(&cfg)
	state := testTurnStateValue(10)
	h.recordTicketWithBinding(a, "gpt-test", state, "probe", false, "http://mint-proxy:3010", "sid", "gpt-test", "203.0.113.1", pairCookies("paired"))
	ctx, _, headers := prepareCodexTurnStateInjection(context.Background(), a, []byte(`{"model":"gpt-test"}`), http.Header{}, false)
	if CodexTurnStateProxyFromContext(ctx) != "" {
		t.Fatal("paired ticket forced mint proxy")
	}
	a.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{State: convergenceTurnStateValue(2, 10), RouteCookies: pairCookies("next")}
	ApplyCodexRouteCookies(ctx, headers, a, CodexBaseURL+"/responses", "gpt-test")
	if headers.Get("Cookie") != "__oailb=paired" {
		t.Fatal("request used rotated cookie")
	}
	other := &auth.Account{}
	separate := http.Header{}
	separate.Set(codexTurnStateHeader, state)
	ApplyCodexRouteCookies(ctx, separate, other, CodexBaseURL+"/responses", "gpt-test")
	if separate.Get("Cookie") != "" {
		t.Fatal("snapshot leaked across accounts")
	}
}

func TestCodexTicketPairPersistenceAndExpiry(t *testing.T) {
	h, a := ticketHarvesterFixture(t)
	state := testTurnStateValue(10)
	h.recordTicketWithBinding(a, "gpt-test", state, "probe", false, "http://mint-proxy:3010", "sid", "gpt-test", "203.0.113.1", pairCookies("paired"))
	ticket := a.CodexTurnStateTickets["gpt-test"]
	if ticket.ProxyURL != "" || ticket.ProxySID != "sid" {
		t.Fatal("mint provenance and egress binding confused")
	}
	if time.Until(ticket.ExpiresAt) > 61*time.Second {
		t.Fatal("ticket outlives cookie")
	}
	data, _ := json.Marshal(map[string]auth.CodexTurnStateTicket{"gpt-test": ticket})
	var stored any
	json.Unmarshal(data, &stored)
	reloaded := auth.ParseCodexTurnStateTickets(stored)["gpt-test"]
	if !reloaded.NeedsVerification || len(reloaded.RouteCookies) != 1 {
		t.Fatal("pair lost across reload")
	}
	if reloaded.Valid(time.Now(), 292) {
		t.Fatal("reload bypassed revalidation")
	}
	ticket.RouteCookies[0].Expires = time.Now().Add(-time.Second).Unix()
	if ticket.Valid(time.Now(), 292) {
		t.Fatal("expired cookie still usable")
	}
}

func TestCodexTicketPairResponsePreservesAndRotatesCookie(t *testing.T) {
	h, a := ticketHarvesterFixture(t)
	state := testTurnStateValue(10)
	h.recordTicketWithBinding(a, "gpt-test", state, "probe", false, "", "", "gpt-test", "", pairCookies("original"))
	initialExpiry := a.CodexTurnStateTickets["gpt-test"].ExpiresAt
	ctx := BindCodexTurnStateRequest(context.Background(), a, "gpt-test")
	ctx = withCodexTurnStateInjection(ctx, state)
	ObserveCodexRouteResponseCookies(WithCodexRouteCookieScope(ctx, CodexBaseURL+"/responses", "gpt-test"), a, "", http.Header{"Set-Cookie": {"__cf_bm=irrelevant; Path=/; Secure"}})
	CommitCodexTurnStateRequest(ctx)
	if got := auth.CodexTicketCookieHeader(a.CodexTurnStateTickets["gpt-test"].RouteCookies, CodexBaseURL+"/responses", time.Now()); got != "__oailb=original" {
		t.Fatal("unrelated response cookie erased pair")
	}
	ctx = BindCodexTurnStateRequest(context.Background(), a, "gpt-test")
	ctx = withCodexTurnStateInjection(ctx, state)
	ObserveCodexRouteResponseCookies(WithCodexRouteCookieScope(ctx, CodexBaseURL+"/responses", "gpt-test"), a, "", http.Header{"Set-Cookie": {"__oailb=rotated; Path=/; Secure; Max-Age=120"}})
	CommitCodexTurnStateRequest(ctx)
	current := a.CodexTurnStateTickets["gpt-test"]
	if got := auth.CodexTicketCookieHeader(current.RouteCookies, CodexBaseURL+"/responses", time.Now()); got != "__oailb=rotated" {
		t.Fatal("cookie rotation not committed")
	}
	if !current.ExpiresAt.Equal(initialExpiry) {
		t.Fatal("cookie rotation extended original ticket expiry")
	}
}

func TestCodexTicketPairMissingCookieCannotPublishReady(t *testing.T) {
	h, a := ticketHarvesterFixture(t)
	h.recordTicketWithBinding(a, "gpt-test", testTurnStateValue(10), "probe", false, "", "", "", "", nil)
	if len(a.CodexTurnStateTickets) != 0 {
		t.Fatal("published a pair without a cookie")
	}
}
