package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestResponsesStreamAppliesCodexRateLimitsFrameToAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	now := time.Now()
	events := [][]string{{
		`{"type":"codex.rate_limits","plan_type":"plus","rate_limits":{` +
			`"primary":{"used_percent":42,"window_minutes":300,"reset_at":` + strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10) + `},` +
			`"secondary":{"used_percent":8,"window_minutes":10080,"reset_at":` + strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10) + `}}}`,
		`{"type":"response.created","response":{"id":"resp_frame_1"}}`,
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":"response.completed","response":{"id":"resp_frame_1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	}}
	upstream, _ := newAttemptSequenceSSEServer(t, events)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	account := &auth.Account{
		DBID:        1,
		AccessToken: "at-1",
		AccountID:   "acct-1",
		Models:      []string{"gpt-5.6-sol"},
		PlanType:    "plus",
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"gpt-5.6-sol","input":"hello","stream":true}`,
	))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	pct5h, ok := account.GetUsagePercent5h()
	if !ok || pct5h != 42 {
		t.Fatalf("5h usage = %v valid=%v, want 42 from the rate_limits frame", pct5h, ok)
	}
	pct7d, ok := account.GetUsagePercent7d()
	if !ok || pct7d != 8 {
		t.Fatalf("7d usage = %v valid=%v, want 8 from the rate_limits frame", pct7d, ok)
	}
}

func TestResponsesNonStreamAppliesCodexRateLimitsFrameToAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	now := time.Now()
	events := [][]string{{
		`{"type":"codex.rate_limits","plan_type":"plus","rate_limits":{` +
			`"primary":{"used_percent":42,"window_minutes":300,"reset_at":` + strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10) + `}}}`,
		`{"type":"response.created","response":{"id":"resp_nonstream_1"}}`,
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":"response.completed","response":{"id":"resp_nonstream_1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	}}
	upstream, _ := newAttemptSequenceSSEServer(t, events)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	account := &auth.Account{
		DBID: 1, AccessToken: "at-1", AccountID: "acct-1",
		Models: []string{"gpt-5.6-sol"}, PlanType: "plus",
		Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"gpt-5.6-sol","input":"hello","stream":false}`,
	))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if pct, ok := account.GetUsagePercent5h(); !ok || pct != 42 {
		t.Fatalf("5h usage = %v valid=%v, want 42 from the non-streaming frame", pct, ok)
	}
}

func TestChatCompletionsStreamAppliesCodexRateLimitsFrameToAccount(t *testing.T) {
	handler, _ := newChatStreamTerminalTestHandler(t, []string{
		`{"type":"codex.rate_limits","plan_type":"plus","rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}}`,
		`{"type":"response.created","response":{"id":"resp_chat_frame"}}`,
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":"response.completed","response":{"id":"resp_chat_frame","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	})

	recorder := invokeChatCompletionsStream(t, handler)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	account := handler.store.FindByID(1)
	if account == nil {
		t.Fatal("test account missing")
	}
	if pct, ok := account.GetUsagePercent5h(); !ok || pct != 42 {
		t.Fatalf("5h usage = %v valid=%v, want 42 from the chat frame", pct, ok)
	}
}

func TestMessagesStreamAppliesCodexRateLimitsFrameToAccount(t *testing.T) {
	handler, _ := newAnthropicStreamFailureTestHandler(t, func(_ int32, w http.ResponseWriter) {
		writeCodexSSE(w,
			`{"type":"codex.rate_limits","plan_type":"plus","rate_limits":{"primary":{"used_percent":42,"window_minutes":300}}}`,
			`{"type":"response.created","response":{"id":"resp_messages_frame"}}`,
			`{"type":"response.output_text.delta","delta":"hello"}`,
			`{"type":"response.completed","response":{"id":"resp_messages_frame","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		)
	})

	recorder := invokeAnthropicMessagesStream(t, handler)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	account := handler.store.FindByID(1)
	if account == nil {
		t.Fatal("test account missing")
	}
	if pct, ok := account.GetUsagePercent5h(); !ok || pct != 42 {
		t.Fatalf("5h usage = %v valid=%v, want 42 from the messages frame", pct, ok)
	}
}
