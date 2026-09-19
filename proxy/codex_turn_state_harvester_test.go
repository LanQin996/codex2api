package proxy

import (
	"context"
	"fmt"
	"strings"
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

func TestCodexTurnStateMixedTicketLengths(t *testing.T) {
	for _, configured := range []int{292, 332} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			h, account := ticketHarvesterFixture(t)
			cfg := *CurrentCodexTurnStateTicketConfig()
			cfg.TargetLength = configured
			SetCodexTurnStateTicketConfig(&cfg)
			for _, length := range []int{292, 332} {
				model := fmt.Sprintf("gpt-test-%d", length)
				state := "gAAAAA" + strings.Repeat("x", length-6)
				ctx := BindCodexTurnStateRequest(context.Background(), account, model)
				StageCodexTurnStateValue(ctx, state)
				CommitCodexTurnStateRequest(ctx)
				h.pruneAccountTickets(account, &cfg, time.Now())
				if got := account.CodexTurnStateTicketInjection(model, configured, time.Now()); got != state {
					t.Fatalf("length %d not retained with config %d", length, configured)
				}
				_, _, headers := prepareCodexTurnStateInjection(context.Background(), account, []byte(fmt.Sprintf("{\"model\":\"%s\"}", model)), nil, false)
				if headers.Get(codexTurnStateHeader) != state {
					t.Fatalf("length %d not injected", length)
				}
			}
			if auth.ValidCodexTurnStateTicketValue("gAAAAA"+strings.Repeat("x", 300-6), configured) {
				t.Fatal("unexpected length accepted")
			}
			if auth.ValidCodexTurnStateTicketValue("gAAAAA"+strings.Repeat("x", 292-7)+"\n", configured) {
				t.Fatal("malformed ticket accepted")
			}
		})
	}
}

func TestCodexTurnStateProbeSkipsUnavailableAccounts(t *testing.T) {
	for _, reason := range []string{"quota", "disabled", "model"} {
		t.Run(reason, func(t *testing.T) {
			h, account := ticketHarvesterFixture(t)
			key := codexTurnStateProbeKey{accountID: account.ID(), model: "gpt-test"}
			switch reason {
			case "quota":
				account.UsagePercent7d = 100
				account.UsagePercent7dValid = true
				account.Reset7dAt = time.Now().Add(time.Hour)
			case "disabled":
				account.Disabled = 1
			case "model":
				account.SetModelCooldownUntil(key.model, "rate_limited", time.Now().Add(time.Hour))
			}
			h.enqueueProbe(key, time.Now())
			if len(h.tasks) != 0 {
				t.Fatal("unavailable account queued")
			}
			if TriggerCodexTurnStateProbe(account.ID(), key.model) {
				t.Fatal("manual probe bypassed availability")
			}
			h.runProbeTask(context.Background(), key)
			if h.probed.Load() != 0 {
				t.Fatal("queued probe ran after account became unavailable")
			}
			account.UsagePercent7d = 0
			account.Disabled = 0
			account.ClearModelCooldown(key.model)
			h.enqueueProbe(key, time.Now())
			if len(h.tasks) != 1 {
				t.Fatal("recovered account did not resume probing")
			}
		})
	}
}

func TestCodexTurnStateProbeErrorPreservesOriginal(t *testing.T) {
	original := "CONNECT rejected: 502 Bad Gateway; proxy http://test-user:test-password@proxy.example:3010"
	err := &codexTurnStateStageError{stage: "proxy/upstream_request", err: fmt.Errorf("%s", original)}
	if got := codexTurnStateProbeError(err); got != "proxy/upstream_request: "+original {
		t.Fatalf("error detail changed: %s", got)
	}
	if got := codexTurnStateProbeError(nil); got != "" {
		t.Fatal("nil error must be empty")
	}
}
