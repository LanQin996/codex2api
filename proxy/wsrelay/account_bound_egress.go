package wsrelay

import (
	"context"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

// Keep explicit routes separate from the business pool, whose same nominal
// proxy may actually dial through Resin or inherited dialer configuration.
// The route snapshot also prevents an empty override re-reading account state.
func requestProxyKey(ctx context.Context, account *auth.Account, override string) string {
	if route, ok := proxy.AccountBoundEgress(ctx); ok {
		return "\x00account-bound|" + strings.TrimSpace(route)
	}
	return effectiveProxyURL(account, override)
}
