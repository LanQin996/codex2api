package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

type excelMigrationContextKey struct{}

func excelHistoryCanMigrate(body []byte) bool {
	calls := map[string]string{}
	done := map[string]bool{}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		kind := item.Get("type").String()
		id := item.Get("call_id").String()
		switch kind {
		case "compaction", "item_reference":
			return false
		case "function_call", "custom_tool_call":
			field := "arguments"
			if kind == "custom_tool_call" {
				field = "input"
			}
			if id == "" || calls[id] != "" || item.Get("name").String() == "" || item.Get(field).Type != gjson.String {
				return false
			}
			calls[id] = kind
		case "function_call_output", "custom_tool_call_output":
			out := item.Get("output")
			if id == "" || calls[id]+"_output" != kind || done[id] || !(out.Type == gjson.String || out.IsArray()) {
				return false
			}
			done[id] = true
		}
	}
	return len(calls) > 0 && len(calls) == len(done)
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

func excelSessionScope(body []byte, sessionID, clientKey string) string {
	seed := gjson.GetBytes(body, "prompt_cache_key").String()
	if seed == "" {
		seed = sessionID
	}
	if seed == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(clientKey + "\x00" + seed))
	return hex.EncodeToString(digest[:])
}

// Lookup persisted provenance before admission; normal account filters still
// enforce enabled state, quotas, model permissions and concurrency.
func resolveExcelHistoryOwner(ctx context.Context, body []byte, sessionID, clientKey string) (int64, error) {
	ids := []string{}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		switch item.Get("type").String() {
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
			id := item.Get("call_id").String()
			if id == "" {
				return 0, fmt.Errorf("invalid tool history")
			}
			ids = append(ids, id)
		}
	}
	payload, err := json.Marshal(map[string]any{"session": excelSessionScope(body, sessionID, clientKey), "call_ids": ids})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(os.Getenv("EXCEL_BRIDGE_URL"), "/")+"/internal/history-owner", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("EXCEL_BRIDGE_API_KEY"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := excelBridgeClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("history lookup status %d", resp.StatusCode)
	}
	id := gjson.GetBytes(raw, "account_id").Int()
	if id <= 0 {
		return 0, fmt.Errorf("invalid history owner")
	}
	return id, nil
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
	scope := excelSessionScope(body, sessionID, clientKey)
	if scope == "" {
		return nil, ErrInternalError("Excel requires a stable session_id or prompt_cache_key", nil)
	}
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
	if owner, ok := ctx.Value(excelMigrationContextKey{}).(int64); ok {
		mode := "auto"
		if owner != account.ID() {
			mode = "1"
		}
		req.Header.Set("X-Excel-Migrate", mode)
	}
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
