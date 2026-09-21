package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestCodexTicketReplayRequiresCompletedStream(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"complete", `data: {"type":"response.completed","response":{"status":"completed","model":"gpt-6-astra"}}` + "\n\n", true},
		{"failed in HTTP 200", `data: {"type":"response.failed"}` + "\n\n", false},
		{"incomplete", `data: {"type":"response.incomplete"}` + "\n\n", false},
		{"done only", "data: [DONE]\n\n", false},
		{"truncated", `data: {"type":"response.created"}` + "\n\n", false},
		{"empty", "", false},
		{"malformed", "data: {broken}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := readTicketReplayStream(strings.NewReader(tc.body))
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestCodexTicketReloadQuarantinesWithoutExtendingExpiry(t *testing.T) {
	h, a := ticketHarvesterFixture(t)
	state := testTurnStateValue(10)
	expires := time.Now().Add(20 * time.Minute)
	raw := map[string]auth.CodexTurnStateTicket{"gpt-test": {State: state, Length: len(state), ExpiresAt: expires, ProxyURL: "http://proxy.example"}}
	a.CodexTurnStateTickets = auth.ParseCodexTurnStateTickets(raw)
	ticket := a.CodexTurnStateTickets["gpt-test"]
	if !ticket.NeedsVerification || ticket.Valid(time.Now(), 292) || !ticket.StoredValid(time.Now(), 292) {
		t.Fatal("loaded ticket was trusted or lost")
	}
	if a.CodexTurnStateTicketInjection("gpt-test", 292, time.Now()) != "" {
		t.Fatal("unverified ticket injected")
	}
	h.pruneAccountTickets(a, CurrentCodexTurnStateTicketConfig(), time.Now())
	if len(a.CodexTurnStateTickets) != 1 || !h.ticketNeedsRefresh(a, "gpt-test", time.Now(), CurrentCodexTurnStateTicketConfig()) {
		t.Fatal("ticket not retained for verification")
	}
	encoded, _ := json.Marshal(ticket)
	if strings.Contains(string(encoded), "NeedsVerification") {
		t.Fatal("runtime flag persisted")
	}
	if !ticket.ExpiresAt.Equal(expires) {
		t.Fatal("restart extended expiry")
	}
}

func TestCodexTicketDegradedReplyEvictsAndPersists(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		h, a := ticketHarvesterFixture(t)
		state := convergenceTurnStateValue(1, 10)
		a.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{State: state, Length: len(state), ExpiresAt: time.Now().Add(time.Hour)}
		ctx := WithCodexTurnStateBinding(BindCodexTurnStateRequest(context.Background(), a, "gpt-test"))
		// ExecuteRequest gets a child context; the handler observes the parent.
		_ = withCodexTurnStateInjection(ctx, state)
		if replaced {
			newState := convergenceTurnStateValue(2, 10)
			a.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{State: newState, Length: len(newState), ExpiresAt: time.Now().Add(time.Hour)}
		}
		StageCodexTurnStateValue(ctx, testTurnStateValue(11))
		AbortCodexTurnStateRequest(ctx)
		if (len(a.CodexTurnStateTickets) > 0) != replaced {
			t.Fatalf("replaced=%v unexpected eviction", replaced)
		}
		if !replaced {
			select {
			case <-h.persist:
			default:
				t.Fatal("eviction not persisted")
			}
			select {
			case <-h.tasks:
			default:
				t.Fatal("replacement not scheduled")
			}
		}
	}
}

func TestCodexTicketExplicitRejectionDoesNotTreatQuotaAsInvalid(t *testing.T) {
	for _, code := range []string{"invalid_turn_state", "rate_limit_exceeded", "server_error"} {
		h, a := ticketHarvesterFixture(t)
		state := testTurnStateValue(10)
		a.CodexTurnStateTickets["gpt-test"] = auth.CodexTurnStateTicket{State: state, Length: len(state), ExpiresAt: time.Now().Add(time.Hour)}
		ctx := withCodexTurnStateInjection(BindCodexTurnStateRequest(context.Background(), a, "gpt-test"), state)
		payload, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]string{"code": code}}})
		ObserveCodexTurnStateFrame(ctx, payload)
		if (len(a.CodexTurnStateTickets) == 0) != (code == "invalid_turn_state") {
			t.Fatalf("wrong invalidation for %s", code)
		}
		_ = h
	}
}
