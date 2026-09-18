package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func ticketHarvesterFixture(t *testing.T) (*CodexTurnStateHarvester, *auth.Account) {
	t.Helper()
	oldConfig := CurrentCodexTurnStateTicketConfig()
	oldHarvester := activeCodexTurnStateHarvester.Load()
	t.Cleanup(func() { SetCodexTurnStateTicketConfig(oldConfig); activeCodexTurnStateHarvester.Store(oldHarvester) })
	SetCodexTurnStateTicketConfig(&CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-*"}, TargetLength: len("gAAAAAtest"), TTLSeconds: 3600, RefreshBeforeSeconds: 600})
	account := &auth.Account{DBID: 42, AccessToken: "test", CodexTurnStateTickets: map[string]auth.CodexTurnStateTicket{}}
	store := &auth.Store{}
	store.SetAccountsForTest([]*auth.Account{account})
	h := NewCodexTurnStateHarvester(store, &database.DB{})
	activeCodexTurnStateHarvester.Store(h)
	return h, account
}

func TestCodexTurnStateEchoDoesNotExtendExpiry(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	original := auth.CodexTurnStateTicket{State: "gAAAAAtest", Length: len("gAAAAAtest"), CapturedAt: time.Now().Add(-55 * time.Minute), ExpiresAt: time.Now().Add(5 * time.Minute)}
	account.CodexTurnStateTickets["gpt-test"] = original
	h.recordTicket(account, "gpt-test", original.State, "response", true)
	if got := account.CodexTurnStateTickets["gpt-test"]; !got.ExpiresAt.Equal(original.ExpiresAt) || !got.CapturedAt.Equal(original.CapturedAt) {
		t.Fatal("echo extended ticket lifetime")
	}
	if !h.ticketNeedsRefresh(account, "gpt-test", time.Now(), CurrentCodexTurnStateTicketConfig()) {
		t.Fatal("echo prevented scheduled refresh")
	}
}

func TestCodexTurnStateExpiredLearnedModelRetries(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	account.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{State: "gAAAAAtest", Length: len("gAAAAAtest"), ExpiresAt: time.Now().Add(-time.Minute)}
	h.refresh(context.Background())
	select {
	case key := <-h.tasks:
		if key.model != "gpt-test" {
			t.Fatalf("unexpected key: %+v", key)
		}
		delete(h.queued, key)
		h.states[key].NextAttempt = time.Now().Add(-time.Second)
	default:
		t.Fatal("expired learned model was not queued")
	}
	h.refresh(context.Background())
	select {
	case <-h.tasks:
	default:
		t.Fatal("model was forgotten after expired ticket pruning")
	}
}

func TestCodexTurnStateMissingRequestQueuesWithBackoff(t *testing.T) {
	h, account := ticketHarvesterFixture(t)
	ctx := BindCodexTurnStateRequest(context.Background(), account, "gpt-test")
	key := <-h.tasks
	delete(h.queued, key)
	h.states[key].NextAttempt = time.Now().Add(time.Minute)
	BindCodexTurnStateRequest(context.Background(), account, "gpt-test")
	select {
	case <-h.tasks:
		t.Fatal("request bypassed retry backoff")
	default:
	}
	StageCodexTurnStateValue(ctx, "gAAAAAtest")
	CommitCodexTurnStateRequest(ctx)
	if account.CodexTurnStateTicketInjection("gpt-test", len("gAAAAAtest"), time.Now()) == "" {
		t.Fatal("response ticket not published")
	}
	select {
	case id := <-h.persist:
		if id != account.DBID {
			t.Fatal("wrong account persisted")
		}
	default:
		t.Fatal("response ticket not scheduled for persistence")
	}
}
