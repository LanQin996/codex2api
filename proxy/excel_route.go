package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This transport is intentionally independent of Codex fingerprint, turn-state,
// compression and WS transport. No inbound credential/header is copied.
var excelBridgeClient = &http.Client{
	Timeout:       300 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 32},
}

func hasExcelToolHistory(body []byte) bool {
	for _, item := range gjson.GetBytes(body, "input").Array() {
		switch item.Get("type").String() {
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
			return true
		}
	}
	return false
}

// Restrict only Excel continuations; ordinary Codex requests retain their
// existing overflow/failover behavior. Keep the filter for the entire retry loop.
func pinExcelContinuationFilter(model string, body []byte, accountID int64, filter auth.AccountFilter) auth.AccountFilter {
	if accountID == 0 || !hasExcelToolHistory(body) {
		return filter
	}
	return func(account *auth.Account) bool {
		target := model
		if mapped, ok := ResolveAccountModelMapping(account, model); ok {
			target = mapped
		}
		if auth.IsExcelModel(target) && account.ID() != accountID {
			return false
		}
		return filter == nil || filter(account)
	}
}

func isExcelModelAccessChanged(status int, body []byte) bool {
	return status == http.StatusForbidden &&
		gjson.GetBytes(body, "error.code").String() == "basispoints_model_access_changed"
}

func executeExcelRequest(ctx context.Context, account *auth.Account, body []byte, sessionID, clientKey string) (*http.Response, error) {
	route, enabled := account.ExcelRoute()
	if !enabled {
		return nil, ErrNoAvailableAccount()
	}
	base := strings.TrimRight(os.Getenv("EXCEL_BRIDGE_URL"), "/")
	key := os.Getenv("EXCEL_BRIDGE_API_KEY")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(key) < 32 {
		return nil, ErrInternalError("Excel bridge is not configured", nil)
	}
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return nil, ErrInternalError("Excel requires full input history", nil)
	}
	// Namespace the cache key by downstream API key without revealing that key.
	seed := gjson.GetBytes(body, "prompt_cache_key").String()
	if seed == "" {
		seed = sessionID
	}
	if seed == "" {
		return nil, ErrInternalError("Excel requires a stable session_id or prompt_cache_key", nil)
	}
	digest := sha256.Sum256([]byte(clientKey + "\x00" + seed))
	scope := hex.EncodeToString(digest[:])
	body, err = sjson.SetBytes(body, "prompt_cache_key", scope)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Excel-Account", fmt.Sprint(account.ID()))
	req.Header.Set("X-Excel-Session", scope)
	req.Header.Set("X-Excel-Credential-Mode", route.CredentialMode)
	req.Header.Set("X-Excel-ChatGPT-Account", account.EffectiveAccountID())
	if route.CredentialMode == "oauth" {
		account.Mu().RLock()
		token := account.AccessToken
		account.Mu().RUnlock()
		accountID := account.EffectiveAccountID()
		if token == "" || accountID == "" {
			return nil, ErrNoAvailableAccount()
		}
		req.Header.Set("X-Excel-Access-Token", token)
		req.Header.Set("X-Excel-ChatGPT-Account", accountID)
	}
	if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(body, "model").String()); err != nil {
		return nil, err
	}
	// Do not use traced upstream transport: this internal hop contains a token
	// in a private header. Account admission/release remains in the handler.
	return excelBridgeClient.Do(req)
}
