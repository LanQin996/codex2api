package proxy

import (
	"context"
	"github.com/codex2api/auth"
	"time"
)

// Commit only a successful response that used this exact account's current pair.
// Neither a late response nor another account can rotate the live ticket cookie.
func (h *CodexTurnStateHarvester) commitPairedTicketResponse(ctx context.Context, account *auth.Account, pending *codexTurnStatePendingCapture, current auth.CodexTurnStateTicket) {
	if CodexTurnStateInjectionFromContext(ctx) != current.State {
		return
	}
	if pending.responseModel != "" && !responseModelMatches(pending.key.model, pending.responseModel) {
		h.invalidateTicket(account, pending.key.model, current.State)
		return
	}
	capture := &codexRouteCookieCapture{scope: CodexBaseURL + "/responses", seed: current.RouteCookies}
	if pending.routeCapture != nil {
		capture.lines = append([]string(nil), pending.routeCapture.lines...)
		capture.capturedAt = pending.routeCapture.capturedAt
	}
	cookies := capturedTicketCookies(capture)
	if auth.CodexTicketCookieHeader(cookies, CodexBaseURL+"/responses", time.Now()) == "" {
		h.invalidateTicket(account, pending.key.model, current.State)
		return
	}
	replacement := pending.candidate
	if replacement == "" {
		replacement = current.State
	}
	if !auth.ValidCodexTurnStateTicketValue(replacement, CurrentCodexTurnStateTicketConfig().TargetLength) {
		return
	}
	account.Mu().Lock()
	latest, exists := account.CodexTurnStateTickets[pending.key.model]
	updated := exists && latest.State == current.State
	if updated {
		latest.State = replacement
		latest.Length = len(replacement)
		latest.RouteCookies = cookies
		latest.FernetBlocks, _ = auth.CodexTurnStateFernetBlocks(replacement)
		for _, c := range cookies {
			if c.Expires > 0 && time.Unix(c.Expires, 0).Before(latest.ExpiresAt) {
				latest.ExpiresAt = time.Unix(c.Expires, 0)
			}
		}
		account.CodexTurnStateTickets[pending.key.model] = latest
	}
	account.Mu().Unlock()
	if updated {
		h.enqueuePersist(account.ID())
	}
}
