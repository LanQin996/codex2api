package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexHTTPWirePayload is immutable across retries. Native responses and the
// opt-in ticket path share cleanup, cache identity, metadata and compression.
type codexHTTPWirePayload struct {
	body     []byte
	encoded  []byte
	encoding string
	cacheKey string
	headers  http.Header
}

func prepareCodexHTTPWirePayload(body []byte, account *auth.Account, sessionID string, headers http.Header) codexHTTPWirePayload {
	// 1. 确保 instructions 字段存在（Codex 后端要求）
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}

	// 2. 清理可能导致上游报错的多余字段
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	// 注意：HTTP /responses 上游不接受 prompt_cache_retention（会 400），必须删除；
	// 该字段的 cache 收益只在 WS 路径注入（见 wsrelay 的 prepareWebsocketBody）。
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "disable_response_storage")
	// 顶层 type 是 Responses WS 事件信封字段（response.create），native WS ingress 的
	// 1009 降级、生图强制 HTTP、Agent Identity 强制 HTTP 都会复用带信封的 body，而
	// HTTP /responses 上游不接受它（400 Unsupported parameter: type）。此处为出站
	// 收口兜底；sjson 只删顶层路径，input[] 等嵌套 type 不受影响（issue #548）。
	body, _ = sjson.DeleteBytes(body, "type")

	// 3. 注入 prompt_cache_key（如果请求体中没有，且 sessionID 不为空）
	existingCacheKey := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	cacheKey := existingCacheKey
	if sessionID != "" {
		cacheKey = sessionID
		body, _ = sjson.SetBytes(body, "prompt_cache_key", cacheKey)
	}

	body, headers = prepareCodexProtocolMetadata(body, account, cacheKey, headers)
	encoded, encoding := CompressCodexRequestBody(body)
	return codexHTTPWirePayload{body: body, encoded: encoded, encoding: encoding, cacheKey: cacheKey, headers: headers}
}

func newCodexHTTPWireRequest(ctx context.Context, endpoint string, account *auth.Account, token, apiKey string, deviceCfg *DeviceProfileConfig, payload codexHTTPWirePayload) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload.encoded))
	if err != nil {
		return nil, err
	}
	applyCodexRequestHeaders(req, account, token, payload.cacheKey, apiKey, deviceCfg, payload.headers)
	applyCodexTurnStateInjectionHeader(ctx, req.Header)
	if payload.encoding != "" {
		req.Header.Set("Content-Encoding", payload.encoding)
	}
	ApplyCodexRoutingHint(req.Header, account, payload.body)
	return req, nil
}
