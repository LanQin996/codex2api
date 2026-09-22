package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

var errTicketRejected = errors.New("upstream explicitly rejected turn-state ticket")

func explicitlyRejectedTicket(payload []byte) bool {
	for _, path := range []string{"error.code", "response.error.code"} {
		switch gjson.GetBytes(payload, path).String() {
		case "invalid_turn_state", "expired_turn_state", "turn_state_invalid", "turn_state_expired":
			return true
		}
	}
	return false
}

func rejectManagedTicketFromContext(ctx context.Context) {
	h := activeCodexTurnStateHarvester.Load()
	if h == nil || ctx == nil {
		return
	}
	id, _ := ctx.Value(codexTurnStatePendingContextKey{}).(uint64)
	h.pendingMu.Lock()
	pending := h.pending[id]
	var key codexTurnStateProbeKey
	if pending != nil {
		key = pending.key
		pending.candidate = ""
	}
	h.pendingMu.Unlock()
	if key.accountID != 0 {
		h.invalidateTicket(h.store.FindByID(key.accountID), key.model, CodexTurnStateInjectionFromContext(ctx))
	}
}

// The stream must finish successfully. HTTP 200 and [DONE] alone are insufficient.
func readTicketReplayStream(r io.Reader) (model, state string, err error) {
	scanner := bufio.NewScanner(io.LimitReader(r, 2*1024*1024))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var data []string
	terminal := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		if raw == "[DONE]" {
			return false, errors.New("stream ended without response.completed")
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Status string `json:"status"`
				Model  string `json:"model"`
			} `json:"response"`
			Headers map[string]string `json:"headers"`
		}
		if json.Unmarshal([]byte(raw), &event) != nil {
			return false, errors.New("malformed SSE data")
		}
		if event.Response.Model != "" {
			model = event.Response.Model
		}
		if explicitlyRejectedTicket([]byte(raw)) {
			return false, errTicketRejected
		}
		if value := codexTurnStateFromFrame([]byte(raw)); value != "" {
			state = value
		}
		for key, value := range event.Headers {
			if strings.EqualFold(key, codexTurnStateHeader) {
				state = value
			}
		}
		switch event.Type {
		case "error", "response.failed", "response.incomplete":
			return false, fmt.Errorf("terminal event %s", event.Type)
		case "response.completed":
			if event.Response.Status != "" && event.Response.Status != "completed" {
				return false, errors.New("response did not complete successfully")
			}
			return true, nil
		}
		return false, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, e := terminal()
			if e != nil || done {
				return model, state, e
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if scanner.Err() != nil {
		return model, state, scanner.Err()
	}
	if done, e := terminal(); done || e != nil {
		return model, state, e
	}
	return model, state, errors.New("stream closed before response.completed")
}

// Compare before deleting: a late failed attempt must never evict a newer ticket.
func (h *CodexTurnStateHarvester) invalidateTicket(account *auth.Account, model, expected string) bool {
	if account == nil || expected == "" {
		return false
	}
	account.Mu().Lock()
	old, ok := account.CodexTurnStateTickets[model]
	if ok && old.State == expected {
		delete(account.CodexTurnStateTickets, model)
	} else {
		ok = false
	}
	account.Mu().Unlock()
	if ok {
		h.enqueuePersist(account.ID())
		h.enqueueProbe(codexTurnStateProbeKey{accountID: account.ID(), model: model}, time.Now())
	}
	return ok
}

// Reloaded credentials are held out of injection until a full replay succeeds.
// A transport error/429 leaves them unverified; it is not proof of invalidity.
func (h *CodexTurnStateHarvester) verifyLoadedTicket(ctx context.Context, account *auth.Account, token, model string, cfg *CodexTurnStateTicketConfig) (bool, error) {
	account.Mu().RLock()
	ticket, ok := account.CodexTurnStateTickets[model]
	account.Mu().RUnlock()
	if !ok || !ticket.NeedsVerification {
		return false, nil
	}
	if !ticket.StoredValid(time.Now(), cfg.TargetLength) || auth.CodexTicketCookieHeader(ticket.RouteCookies, CodexBaseURL+"/responses", time.Now()) == "" {
		h.invalidateTicket(account, model, ticket.State)
		return false, nil
	}
	if err := h.acquireProbeSlot(ctx, account.ID()); err != nil {
		return true, err
	}
	verifyCtx, capture := WithCodexRouteCookieCapture(ctx)
	capture.probe = true
	capture.scope = CodexBaseURL + "/responses"
	capture.seed = append([]auth.CodexRouteCookie(nil), ticket.RouteCookies...)
	state, status, responseModel, err := h.fireTicketProbe(verifyCtx, account, token, model, cfg, h.store.ResolveProxyForAccount(account), uuid.NewString(), ticket.State)
	h.releaseProbeSlot(account.ID())
	if err != nil {
		if errors.Is(err, errTicketRejected) {
			h.invalidateTicket(account, model, ticket.State)
			return false, nil
		}
		if codexTurnStateStopStatus(status) {
			return true, &codexTurnStateProbeStopError{status: status, err: err}
		}
		return true, err
	}
	accepted, valid := acceptedCodexTicket(ticket.State, state, status, model, responseModel, cfg.TargetLength)
	valid = valid && auth.CodexTicketCookieHeader(capturedTicketCookies(capture), CodexBaseURL+"/responses", time.Now()) != ""
	if !valid {
		h.invalidateTicket(account, model, ticket.State)
		return false, nil
	}
	account.Mu().Lock()
	current, exists := account.CodexTurnStateTickets[model]
	updated := exists && current.State == ticket.State
	if updated {
		// Revalidation must not grant another hour to the same opaque ticket.
		current.State = accepted
		current.Length = len(accepted)
		current.NeedsVerification = false
		current.FernetBlocks, _ = auth.CodexTurnStateFernetBlocks(accepted)
		current.VerifiedModel = responseModel
		current.RouteCookies = capturedTicketCookies(capture)
		current.ProxyURL = ""
		for _, c := range current.RouteCookies {
			if c.Expires > 0 && time.Unix(c.Expires, 0).Before(current.ExpiresAt) {
				current.ExpiresAt = time.Unix(c.Expires, 0)
			}
		}
		account.CodexTurnStateTickets[model] = current
	}
	account.Mu().Unlock()
	if updated {
		h.enqueuePersist(account.ID())
	}
	return true, nil
}
