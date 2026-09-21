package proxy

import (
	"time"

	"github.com/codex2api/auth"
)

// Check live ticket state on every selection, including affinity and retries.
// Do not persist an administrative pause: harvesting must keep running while
// an account waits for a ticket, and a new ticket must take effect immediately.
func withCodexTurnStateDispatchFilter(model string, filter auth.AccountFilter) auth.AccountFilter {
	return func(account *auth.Account) bool {
		if account == nil || (filter != nil && !filter(account)) {
			return false
		}
		cfg := CurrentCodexTurnStateTicketConfig()
		if !cfg.Enabled || !cfg.FailClosed || !isNativeCodexOAuth(account) {
			return true
		}
		upstreamModel := model
		if mapped, ok := resolveAccountModelMapping(account, model); ok && mapped != "" {
			upstreamModel = mapped
		}
		if !cfg.ModelManaged(model, upstreamModel) {
			return true
		}
		now := time.Now()
		for _, candidate := range []string{upstreamModel, model} {
			if _, ok := account.CodexTurnStateTicket(candidate, cfg.TargetLength, now); ok {
				return true
			}
		}
		return false
	}
}
