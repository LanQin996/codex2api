package admin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

func canRefreshCodexTest(account *auth.Account) bool {
	if account == nil || account.IsRelayStyle() || account.IsCodexAgentIdentity() {
		return false
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return strings.TrimSpace(account.RefreshToken) != ""
}

func codexTestTokenNeedsRefresh(account *auth.Account, now time.Time) bool {
	account.Mu().RLock()
	token, expiry := account.AccessToken, account.ExpiresAt
	account.Mu().RUnlock()
	if strings.TrimSpace(token) == "" {
		return true
	}
	// JWT expiry wins if cached metadata incorrectly claims a later expiry.
	if info := auth.ParseAccessToken(token); info != nil && !info.ExpiresAt.IsZero() && (expiry.IsZero() || info.ExpiresAt.Before(expiry)) {
		expiry = info.ExpiresAt
	}
	return !expiry.IsZero() && !expiry.After(now.Add(time.Minute))
}

func (h *Handler) executeCodexConnectionTest(ctx context.Context, account *auth.Account, payload []byte, allowRefresh bool) (*http.Response, error) {
	execute := func() (*http.Response, error) {
		return proxy.ExecuteRequest(ctx, account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	}
	refresh := func() error { return h.refreshAccountByIDWithProbe(ctx, account.ID(), false) }
	return executeCodexTestWithRefresh(ctx, account, allowRefresh, execute, refresh)
}

// One refresh per test invocation. A genuine 401 is preserved for normal handling;
// only an explicitly expired bearer is eligible for a refresh/retry.
func executeCodexTestWithRefresh(ctx context.Context, account *auth.Account, allowRefresh bool, execute func() (*http.Response, error), refresh func() error) (*http.Response, error) {
	eligible := allowRefresh && canRefreshCodexTest(account)
	refreshed := false
	if eligible && codexTestTokenNeedsRefresh(account, time.Now()) {
		if err := refresh(); err != nil {
			return nil, fmt.Errorf("Codex token refresh failed: %s", sanitizeCodexTestText(err.Error(), codexTestSecrets(account)))
		}
		refreshed = true
	}
	sentToken := account.GetAccessToken()
	resp, err := execute()
	if err != nil || resp == nil || resp.StatusCode != http.StatusUnauthorized || !eligible || refreshed {
		return resp, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	// Restore every consumed byte for the caller, including oversized or unknown errors.
	original := resp.Body
	resp.Body = &codexTestReplayBody{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}
	if readErr != nil || len(body) > 64*1024 || !(gjson.GetBytes(body, "error.code").String() == "token_expired" || gjson.GetBytes(body, "detail.code").String() == "token_expired") {
		return resp, nil
	}
	if err := ctx.Err(); err != nil {
		resp.Body.Close()
		return nil, err
	}
	// A concurrent refresh may already have published a new bearer.
	if account.GetAccessToken() == sentToken {
		if err := refresh(); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("Codex token refresh failed: %s", sanitizeCodexTestText(err.Error(), codexTestSecrets(account)))
		}
	}
	resp.Body.Close()
	return execute()
}

type codexTestReplayBody struct {
	io.Reader
	io.Closer
}
