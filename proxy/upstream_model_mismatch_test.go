package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestUpstreamModelMatches(t *testing.T) {
	cases := []struct {
		sent, upstream string
		want           bool
	}{
		{"gpt-5.4", "gpt-5.4", true},
		{"gpt-5.4", "GPT-5.4", true},
		{"gpt-5.4", "", true},
		{"gpt-5", "gpt-5-2025-08-07", true},
		{"gpt-4", "gpt-4-0613", true},
		{"claude-sonnet-4-5", "claude-sonnet-4-5-20250929", true},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5", true},
		{"claude-3-5-sonnet-latest", "claude-3-5-sonnet-20241022", true},
		{"claude-sonnet-4-5[1m]", "claude-sonnet-4-5-20250929", true},
		{"gpt-5.4", "openai/gpt-5.4", true},
		{"gpt-5.4-openai-compact", "gpt-5.4", true},
		{"gpt-5.4", "gpt-5.4-mini", false},
		{"gpt-5.4", "gpt-4o", false},
		{"gpt-5", "gpt-5.4", false},
		{"claude-opus-5", "claude-sonnet-5", false},
	}
	for _, tc := range cases {
		if got := upstreamModelMatches(tc.sent, tc.upstream); got != tc.want {
			t.Errorf("upstreamModelMatches(%q, %q) = %v, want %v", tc.sent, tc.upstream, got, tc.want)
		}
	}
}

func TestUpstreamModelFromPayload(t *testing.T) {
	cases := map[string]string{
		`{"type":"response.completed","response":{"model":"gpt-5.4","usage":{}}}`: "gpt-5.4",
		`{"object":"response","model":"gpt-5.4-mini"}`:                           "gpt-5.4-mini",
		`{"type":"message_start","message":{"model":"claude-opus-5"}}`:           "claude-opus-5",
		`{"type":"response.output_text.delta","delta":"hi"}`:                     "",
		`{"model":123}`: "",
	}
	for payload, want := range cases {
		if got := upstreamModelFromPayload([]byte(payload)); got != want {
			t.Errorf("upstreamModelFromPayload(%s) = %q, want %q", payload, got, want)
		}
	}
}

func TestUpstreamModelMismatchTrackerThrottlesPersists(t *testing.T) {
	var tracker upstreamModelMismatchTracker
	now := time.Now()
	if hits := tracker.note(1, "gpt-5.4", "gpt-4o", now); hits != 1 {
		t.Fatalf("首次命中 hits = %d, want 1", hits)
	}
	if hits := tracker.note(1, "gpt-5.4", "gpt-4o", now.Add(time.Second)); hits != 0 {
		t.Fatalf("间隔内命中 hits = %d, want 0", hits)
	}
	// 上游换成了另一个模型：立即落库，并带上此前攒下的命中。
	if hits := tracker.note(1, "gpt-5.4", "gpt-4o-mini", now.Add(2*time.Second)); hits != 2 {
		t.Fatalf("上游模型变化 hits = %d, want 2", hits)
	}
	if hits := tracker.note(1, "gpt-5.4", "gpt-4o-mini", now.Add(3*time.Second)); hits != 0 {
		t.Fatalf("间隔内命中 hits = %d, want 0", hits)
	}
	if hits := tracker.note(1, "gpt-5.4", "gpt-4o-mini", now.Add(3*time.Second+upstreamModelMismatchPersistInterval)); hits != 2 {
		t.Fatalf("间隔过后 hits = %d, want 2", hits)
	}
}

func TestNoteUpstreamModelMismatchMarksAccountModel(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()
	h := &Handler{db: db}

	// 一致、映射后一致、失败响应、缺上游模型：都不打标记。
	h.noteUpstreamModelMismatch(&database.UsageLogInput{AccountID: 3, Model: "gpt-5.4", UpstreamModel: "gpt-5.4", StatusCode: 200})
	h.noteUpstreamModelMismatch(&database.UsageLogInput{AccountID: 3, Model: "gpt-5-high", EffectiveModel: "gpt-5", UpstreamModel: "gpt-5-2025-08-07", StatusCode: 200})
	h.noteUpstreamModelMismatch(&database.UsageLogInput{AccountID: 3, Model: "gpt-5.4", UpstreamModel: "gpt-4o", StatusCode: 502})
	h.noteUpstreamModelMismatch(&database.UsageLogInput{AccountID: 3, Model: "gpt-5.4", StatusCode: 200})
	rows, err := db.ListModelMismatches(context.Background())
	if err != nil {
		t.Fatalf("ListModelMismatches 返回错误: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("不应打标记，实际 %d 条: %+v", len(rows), rows[0])
	}

	// 以实际发给上游的模型（映射后）为准。
	h.noteUpstreamModelMismatch(&database.UsageLogInput{AccountID: 3, Model: "claude-opus-5", EffectiveModel: "GPT-5.4", UpstreamModel: "gpt-5.4-mini", StatusCode: 200})
	rows, err = db.ListModelMismatches(context.Background())
	if err != nil {
		t.Fatalf("ListModelMismatches 返回错误: %v", err)
	}
	if len(rows) != 1 || rows[0].AccountID != 3 || rows[0].Model != "gpt-5.4" || rows[0].UpstreamModel != "gpt-5.4-mini" || rows[0].HitCount != 1 {
		t.Fatalf("rows = %+v, want 账号 3 的 gpt-5.4 → gpt-5.4-mini ×1", rows)
	}
}

// 端到端：中转上游回显了别的模型，用量日志要记下上游模型，账号的该模型要被打上标记。
func TestResponsesRelayRecordsUpstreamModelAndMismatchMark(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
					_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_swap\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.4-mini\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_swap","object":"response","status":"completed","model":"gpt-5.4-mini","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`))
			}))
			defer upstream.Close()

			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
			if err != nil {
				t.Fatalf("database.New(sqlite) error = %v", err)
			}
			defer db.Close()
			db.SetUsageLogConfig(database.UsageLogModeFull, 1, 1)

			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0, MaxRateLimitRetries: 0})
			store.AddAccount(&auth.Account{
				DBID:         1,
				UpstreamType: auth.UpstreamOpenAIResponses,
				BaseURL:      upstream.URL,
				APIKey:       "sk-upstream",
				Models:       []string{"gpt-5.4"},
				PlanType:     "api",
			})
			handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
			router := gin.New()
			handler.RegisterRoutes(router)

			body := `{"model":"gpt-5.4","input":"hello"}`
			if stream {
				body = `{"model":"gpt-5.4","input":"hello","stream":true}`
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}

			deadline := time.Now().Add(2 * time.Second)
			for {
				logs, listErr := db.ListRecentUsageLogs(context.Background(), 10)
				if listErr != nil {
					t.Fatalf("ListRecentUsageLogs error = %v", listErr)
				}
				if len(logs) > 0 {
					if logs[0].UpstreamModel != "gpt-5.4-mini" {
						t.Fatalf("UpstreamModel = %q, want gpt-5.4-mini", logs[0].UpstreamModel)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("timed out waiting for usage log flush")
				}
				time.Sleep(10 * time.Millisecond)
			}

			rows, err := db.ListModelMismatchesForAccounts(context.Background(), []int64{1})
			if err != nil {
				t.Fatalf("ListModelMismatchesForAccounts error = %v", err)
			}
			if len(rows) != 1 || rows[0].Model != "gpt-5.4" || rows[0].UpstreamModel != "gpt-5.4-mini" {
				t.Fatalf("mismatch rows = %+v, want gpt-5.4 → gpt-5.4-mini", rows)
			}
		})
	}
}
