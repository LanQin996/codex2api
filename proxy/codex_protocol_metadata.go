package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func codexEnvEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// CodexMetadataImpersonationEnabled is independent of TLS and the account's
// existing identity/fingerprint settings. Both opt-in switches default off.
func CodexMetadataImpersonationEnabled() bool {
	return codexEnvEnabled("CODEX_METADATA_IMPERSONATE")
}

// prepareCodexProtocolMetadata runs after identity convergence. Existing client
// metadata remains authoritative; absent fields are scoped to the isolated
// upstream session, never collapsed into one account-wide conversation.
func prepareCodexProtocolMetadata(body []byte, account *auth.Account, sessionID string, headers http.Header) ([]byte, http.Header) {
	if !CodexMetadataImpersonationEnabled() || account == nil || account.IsRelayStyle() || !gjson.ValidBytes(body) {
		return body, headers
	}
	meta := gjson.GetBytes(body, "client_metadata")
	// Do not silently replace malformed client input.
	if meta.Exists() && !meta.IsObject() {
		return body, headers
	}
	forward := headers.Clone()
	if forward == nil {
		forward = make(http.Header)
	}
	turnJSON := meta.Get("x-codex-turn-metadata").String()
	if turnJSON == "" {
		turnJSON = headers.Get(codexTurnMetadataHeader)
	}
	var turn map[string]json.RawMessage
	if json.Unmarshal([]byte(turnJSON), &turn) != nil || turn == nil {
		turn = make(map[string]json.RawMessage)
	}
	turnString := func(key string) string {
		var value string
		_ = json.Unmarshal(turn[key], &value)
		return value
	}
	choose := func(values ...string) string {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return value
			}
		}
		return ""
	}
	if sessionID == "" || IsStatelessWebsocketSessionID(sessionID) {
		sessionID = gjson.GetBytes(body, "prompt_cache_key").String()
	}
	sid := choose(meta.Get("session_id").String(), turnString("session_id"), sessionID, headers.Get("session-id"))
	if sid == "" {
		sid = NewUpstreamSessionUUID()
	}
	tid := choose(meta.Get("thread_id").String(), turnString("thread_id"), headers.Get("thread-id"), sid)
	wid := choose(meta.Get("x-codex-window-id").String(), turnString("window_id"), headers.Get(codexWindowIDHeader), tid)
	iid := choose(meta.Get("x-codex-installation-id").String(), turnString("installation_id"),
		deriveStableCodexUUID(fmt.Sprintf("codex-protocol-installation:%d", account.ID())))
	for key, value := range map[string]string{
		"x-codex-installation-id": iid, "session_id": sid, "thread_id": tid, "x-codex-window-id": wid,
	} {
		body, _ = sjson.SetBytes(body, "client_metadata."+key, value)
	}
	forward.Set("thread-id", tid)
	forward.Set(codexClientRequestIDHeader, tid)
	forward.Set(codexWindowIDHeader, wid)
	// The official HTTP compatibility view is a projection of body metadata.
	// Keep the full tool inventory in the body, not in the HTTP header.
	for key, value := range map[string]string{"installation_id": iid, "session_id": sid, "thread_id": tid, "window_id": wid} {
		turn[key], _ = json.Marshal(value)
	}
	for _, field := range [][3]string{
		{"x-codex-parent-thread-id", "parent_thread_id", codexParentThreadIDHeader},
		{"x-openai-subagent", "subagent_header", "X-Openai-Subagent"},
	} {
		value := choose(meta.Get(field[0]).String(), turnString(field[1]), headers.Get(field[2]))
		if value != "" {
			body, _ = sjson.SetBytes(body, "client_metadata."+field[0], value)
			forward.Set(field[2], value)
		}
	}
	if turnString("request_kind") != "" {
		full, err := json.Marshal(turn)
		if err == nil {
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", string(full))
			delete(turn, "tool_namespaces_info")
			bounded, _ := json.Marshal(turn)
			forward.Set(codexTurnMetadataHeader, string(bounded))
		}
	}
	return body, forward
}

// ApplyCodexProtocolHeaders is shared by HTTP, WS and ticket acquisition.
// It intentionally does not change UA, originator, auth, or turn-state.
func ApplyCodexProtocolHeaders(outbound, prepared http.Header) {
	if !CodexMetadataImpersonationEnabled() || outbound == nil {
		return
	}
	for _, name := range []string{codexWindowIDHeader, codexParentThreadIDHeader,
		"X-Openai-Subagent", "X-Openai-Memgen-Request", codexTurnMetadataHeader,
		codexThreadIDHeader, codexClientRequestIDHeader} {
		if value := prepared.Get(name); value != "" {
			outbound.Set(name, value)
		}
	}
}
