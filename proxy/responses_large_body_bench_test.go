package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// largeCodexResponsesBody builds a Codex-shaped /v1/responses history of about
// targetBytes: reasoning items with opaque encrypted_content, shell tool calls,
// escaped multi-line tool outputs and assistant messages.
func largeCodexResponsesBody(tb testing.TB, targetBytes int) []byte {
	tb.Helper()
	opaque := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("encrypted-reasoning-state-"), 160))
	output := strings.Repeat("src/main.go:42:\tfmt.Println(\"héllo \\\"world\\\"\") // 注释 <tag> & more\n", 120)
	tools := make([]any, 0, 16)
	for i := 0; i < 16; i++ {
		tools = append(tools, map[string]any{
			"type": "function", "name": fmt.Sprintf("tool_%d", i),
			"description": strings.Repeat("Runs a command in the workspace. ", 8),
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"workdir": map[string]any{"type": "string"},
					"timeout": map[string]any{"type": "number"},
				},
				"required":             []string{"command"},
				"additionalProperties": false,
			},
		})
	}
	input := make([]any, 0, 1024)
	size := 0
	for turn := 0; size < targetBytes; turn++ {
		callID := fmt.Sprintf("call_%06d", turn)
		items := []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": fmt.Sprintf("Step %d: inspect the repository and fix the failing test.", turn)}}},
			map[string]any{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": opaque},
			map[string]any{"type": "function_call", "name": "tool_0", "call_id": callID, "arguments": `{"command":["bash","-lc","rg -n \"TODO\" src"],"workdir":"/repo"}`},
			map[string]any{"type": "function_call_output", "call_id": callID, "output": output},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "I found the issue and will patch it next."}}},
		}
		for _, item := range items {
			encoded, err := json.Marshal(item)
			if err != nil {
				tb.Fatal(err)
			}
			size += len(encoded)
		}
		input = append(input, items...)
	}
	body, err := json.Marshal(map[string]any{
		"model":               "gpt-5.5",
		"instructions":        strings.Repeat("You are Codex, a coding agent. Follow the user's instructions carefully. ", 300),
		"input":               input,
		"tools":               tools,
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": "medium", "summary": "auto"},
		"store":               false,
		"stream":              true,
		"include":             []string{"reasoning.encrypted_content"},
		"prompt_cache_key":    "019a0000-0000-7000-8000-000000000000",
	})
	if err != nil {
		tb.Fatal(err)
	}
	return body
}

func zstdCompress(tb testing.TB, data []byte) []byte {
	tb.Helper()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(data, nil)
}

func newLargeBodyResponsesEngine(tb testing.TB) *gin.Engine {
	tb.Helper()
	gin.SetMode(gin.TestMode)
	previousResin := resinCfg.Load()
	tb.Cleanup(func() { resinCfg.Store(previousResin) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSuccessfulAttempt(w)
	}))
	tb.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1, TestModel: "gpt-5.5", MaxRetries: 1})
	tb.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	engine := gin.New()
	engine.Use(api.RequestContextMiddleware())
	engine.Use(security.RequestSizeLimiter(int64(security.MaxRequestBodySize)))
	engine.Use(security.RequestBodyDecompressor(int64(security.MaxRequestBodySize)))
	engine.Use(api.BodyCacheMiddleware())
	engine.POST("/v1/responses", handler.Responses)
	return engine
}

// BenchmarkResponsesLargeCodexBody measures the full HTTP pre-upstream path for
// long Codex histories, including zstd decompression, on one processor:
//
//	GOMAXPROCS=1 go test ./proxy -run '^$' -bench ResponsesLargeCodexBody -benchmem
func BenchmarkResponsesLargeCodexBody(b *testing.B) {
	previousLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previousLogOutput) })
	for _, target := range []int{1 << 20, 10 << 20} {
		b.Run(fmt.Sprintf("%dMiB", target>>20), func(b *testing.B) {
			engine := newLargeBodyResponsesEngine(b)
			plain := largeCodexResponsesBody(b, target)
			compressed := zstdCompress(b, plain)
			b.SetBytes(int64(len(plain)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				recorder := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressed))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Content-Encoding", "zstd")
				engine.ServeHTTP(recorder, req)
				if recorder.Code != http.StatusOK {
					b.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
				}
			}
		})
	}
}
