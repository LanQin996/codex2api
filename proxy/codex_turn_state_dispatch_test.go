package proxy

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestCodexTurnStateDispatchTransitions(t *testing.T) {
	for _, engine := range []string{"legacy", "indexed", "lazy"} {
		t.Run(engine, func(t *testing.T) {
			harvester, account := ticketHarvesterFixture(t)
			cfg := *CurrentCodexTurnStateTicketConfig()
			cfg.FailClosed = true
			SetCodexTurnStateTicketConfig(&cfg)
			store := harvester.store
			store.SetSchedulerEngine(engine)
			if engine == "lazy" {
				store.SetLazyMode(true)
			}
			handler := &Handler{store: store}
			filter := handler.withModelCooldownFilter("gpt-test", nil)
			check := func(want bool) {
				t.Helper()
				got := store.NextExcludingWithFilter(0, nil, filter)
				if got != nil {
					store.Release(got)
				}
				if (got != nil) != want {
					t.Fatalf("selected=%v, want %v", got != nil, want)
				}
			}
			check(false)
			if !account.IsAvailableForTicketMaintenance() {
				t.Fatal("missing ticket blocked harvesting")
			}
			state := testTurnStateValue(10)
			ticket := auth.CodexTurnStateTicket{State: state, Length: len(state), ExpiresAt: time.Now().Add(time.Hour)}
			store.ApplyAccountCodexTurnStateTicket(account.DBID, "gpt-test", ticket)
			check(true)
			atomic.StoreInt32(&account.DispatchPaused, 1)
			check(false)
			atomic.StoreInt32(&account.DispatchPaused, 0)
			ticket.ExpiresAt = time.Now().Add(-time.Second)
			store.ApplyAccountCodexTurnStateTicket(account.DBID, "gpt-test", ticket)
			check(false)
			ticket.ExpiresAt = time.Now().Add(time.Hour)
			ticket.NeedsVerification = true
			store.ApplyAccountCodexTurnStateTicket(account.DBID, "gpt-test", ticket)
			check(false)
			ticket.NeedsVerification = false
			store.ApplyAccountCodexTurnStateTicket(account.DBID, "gpt-test", ticket)
			check(true)
			store.DropAccountCodexTurnStateTicket(account.DBID, "gpt-test")
			check(false)
			cfg.FailClosed = false
			SetCodexTurnStateTicketConfig(&cfg)
			check(true)
		})
	}
}

func TestCodexTurnStateDispatchScope(t *testing.T) {
	_, account := ticketHarvesterFixture(t)
	cfg := *CurrentCodexTurnStateTicketConfig()
	cfg.FailClosed = true
	cfg.Models = []string{"gpt-test"}
	SetCodexTurnStateTicketConfig(&cfg)
	if !withCodexTurnStateDispatchFilter("other", nil)(account) {
		t.Fatal("unmanaged model blocked")
	}
	if withCodexTurnStateDispatchFilter("gpt-test", nil)(account) {
		t.Fatal("missing ticket accepted")
	}
	if !withCodexTurnStateDispatchFilter("gpt-test", nil)(&auth.Account{UpstreamType: "claude"}) {
		t.Fatal("non-Codex account blocked")
	}
	if withCodexTurnStateDispatchFilter("other", func(*auth.Account) bool { return false })(account) {
		t.Fatal("existing filter bypassed")
	}
	cfg.Enabled = false
	SetCodexTurnStateTicketConfig(&cfg)
	if !withCodexTurnStateDispatchFilter("gpt-test", nil)(account) {
		t.Fatal("disabled collection blocked dispatch")
	}
}
