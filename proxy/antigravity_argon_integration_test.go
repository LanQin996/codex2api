package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestAntigravityArgonManifestHonorsAllowedEfforts(t *testing.T) {
	for _, tc := range []struct {
		levels []string
		def    string
	}{
		{[]string{"low", "medium", "high"}, "high"},
		{[]string{"low"}, "low"},
		{[]string{"medium", "high"}, "high"},
		{[]string{"high"}, "high"},
	} {
		t.Run(strings.Join(tc.levels, "-"), func(t *testing.T) {
			var models []api.Model
			for _, level := range tc.levels {
				models = append(models, api.Model{ID: "gemini-4-argon-" + level, OwnedBy: "google"})
			}
			body, err := buildScopedCodexManifest(models)
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Models []scopedCodexManifestItem `json:"models"`
			}
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest.Models) != 1 {
				t.Fatalf("Argon tiers not folded: %s", body)
			}
			model := manifest.Models[0]
			var levels []string
			for _, level := range model.SupportedReasoningLevels {
				levels = append(levels, level.Effort)
			}
			if model.Slug != "gemini-4-argon" || !reflect.DeepEqual(levels, tc.levels) || model.DefaultReasoningLevel != tc.def {
				t.Fatalf("manifest advertises wrong model/efforts: %s", body)
			}
			if model.ContextWindow != 1048576 || !model.SupportsReasoningSummaries || !reflect.DeepEqual(model.InputModalities, []string{"text"}) {
				t.Fatalf("manifest has incorrect gateway capabilities: %s", body)
			}
			choices := collapseAntigravityModelChoices(models)
			if len(choices) != 1 || choices[0].DefaultReasoningLevel != tc.def || !reflect.DeepEqual(choices[0].SupportedReasoningLevels, model.SupportedReasoningLevels) {
				t.Fatalf("model list and manifest disagree: %+v", choices)
			}
			stale := []byte(`{"models":[{"slug":"gemini-4-argon","default_reasoning_level":"xhigh"},{"slug":"gemini-4-argon-low"},{"slug":"gemini-4-argon-high"},{"slug":"unrelated-model"}]}`)
			merged, err := mergeCodexManifestModels(stale, models)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(merged, &manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest.Models) != 2 {
				t.Fatalf("stale Argon aliases retained: %s", merged)
			}
			for _, item := range manifest.Models {
				if item.Slug != "unrelated-model" && !reflect.DeepEqual(item, model) {
					t.Fatalf("stale Argon metadata retained: %s", merged)
				}
			}
		})
	}
}

func TestAntigravityArgonGatewayEffortAndKeyScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requests := make(chan []byte, 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests <- body
		response := `{"response":{"candidates":[{"content":{"parts":[{"text":"argon-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}}`
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+response+"\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, response)
		}
	}))
	defer upstream.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{upstream.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, MaxRetries: 0})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID: 4001, UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token",
		AntigravityProjectID: "test-project", Models: []string{"gemini-4-argon-high"},
		HealthTier: auth.HealthTierHealthy, Status: auth.StatusReady,
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	for _, channel := range []string{database.UpstreamChannelAntigravity, database.UpstreamChannelAuto} {
		for _, tc := range []struct {
			name, path, body, action, level, terminal string
			allow, deny                               []string
			status                                    int
		}{
			{name: "responses-default", path: "/v1/responses", body: `{"model":"gemini-4-argon","input":"hi"}`, level: "HIGH", status: 200},
			{name: "responses-stream", path: "/v1/responses", body: `{"model":"gemini-4-argon","input":"hi","stream":true,"reasoning":{"effort":"medium"}}`, level: "MEDIUM", terminal: "response.completed", status: 200},
			{name: "responses-fixed", path: "/v1/responses", body: `{"model":"gemini-4-argon-high","input":"hi","reasoning":{"effort":"low"}}`, level: "HIGH", status: 200},
			{name: "responses-invalid", path: "/v1/responses", body: `{"model":"gemini-4-argon","input":"hi","reasoning":{"effort":"xhigh"}}`, status: 400},
			{name: "responses-denied", path: "/v1/responses", body: `{"model":"gemini-4-argon","input":"hi","reasoning":{"effort":"high"}}`, allow: []string{"gemini-4-argon-low"}, status: 403},
			{name: "responses-allowed", path: "/v1/responses", body: `{"model":"gemini-4-argon","input":"hi","reasoning":{"effort":"low"}}`, level: "LOW", allow: []string{"gemini-4-argon-low"}, status: 200},
			{name: "chat-default", path: "/v1/chat/completions", body: `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`, level: "HIGH", status: 200},
			{name: "chat-effort", path: "/v1/chat/completions", body: `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`, level: "MEDIUM", status: 200},
			{name: "chat-stream", path: "/v1/chat/completions", body: `{"model":"gemini-4-argon-low","messages":[{"role":"user","content":"hi"}],"stream":true}`, level: "LOW", terminal: "[DONE]", status: 200},
			{name: "chat-denied", path: "/v1/chat/completions", body: `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, allow: []string{"gemini-4-argon-low"}, status: 403},
			{name: "messages-default", path: "/v1/messages", body: `{"model":"gemini-4-argon","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`, level: "HIGH", status: 200},
			{name: "messages-effort", path: "/v1/messages", body: `{"model":"gemini-4-argon","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"medium"}}`, level: "MEDIUM", status: 200},
			{name: "messages-stream", path: "/v1/messages", body: `{"model":"gemini-4-argon","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"stream":true,"output_config":{"effort":"low"}}`, level: "LOW", terminal: "message_stop", status: 200},
			{name: "messages-allowed", path: "/v1/messages", body: `{"model":"gemini-4-argon","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"low"}}`, level: "LOW", allow: []string{"gemini-4-argon-low"}, status: 200},
			{name: "messages-bare-denied", path: "/v1/messages", body: `{"model":"gemini-4-argon","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`, deny: []string{"gemini-4-argon-high"}, status: 403},
			{name: "messages-invalid", path: "/v1/messages", body: `{"model":"gemini-4-argon","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"xhigh"}}`, status: 400},
			{name: "messages-unknown-effort", path: "/v1/messages", body: `{"model":"gemini-4-argon","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"unknown"}}`, status: 400},
			{name: "messages-denied", path: "/v1/messages", body: `{"model":"gemini-4-argon-high","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`, allow: []string{"gemini-4-argon-low"}, status: 403},
			{name: "gemini-default", path: "/v1beta/models/gemini-4-argon-high:generateContent", action: "/gemini-4-argon-high:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, level: "HIGH", status: 200},
			{name: "gemini-explicit", path: "/v1beta/models/gemini-4-argon-high:generateContent", action: "/gemini-4-argon-high:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`, level: "LOW", status: 200},
			{name: "gemini-denied", path: "/v1beta/models/gemini-4-argon-high:generateContent", action: "/gemini-4-argon-high:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, allow: []string{"gemini-4-argon-low"}, status: 403},
		} {
			t.Run(channel+"/"+tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 4001, Limits: database.APIKeyLimits{UpstreamChannel: channel, ModelAllow: tc.allow, ModelDeny: tc.deny}})
				switch tc.path {
				case "/v1/responses":
					handler.Responses(c)
				case "/v1/chat/completions":
					handler.ChatCompletions(c)
				case "/v1/messages":
					handler.Messages(c)
				default:
					c.Params = gin.Params{{Key: "action", Value: tc.action}}
					handler.GeminiModelsAction(c)
				}
				if rec.Code != tc.status {
					t.Fatalf("status=%d, want %d: %s", rec.Code, tc.status, rec.Body.String())
				}
				if tc.status != http.StatusOK {
					if len(requests) != 0 {
						t.Fatal("rejected request reached upstream")
					}
					return
				}
				if len(requests) != 1 {
					t.Fatalf("upstream requests=%d, want one", len(requests))
				}
				payload := <-requests
				if gjson.GetBytes(payload, "model").String() != "gemini-4-argon-high" || gjson.GetBytes(payload, "request.generationConfig.thinkingConfig.thinkingLevel").String() != tc.level {
					t.Fatalf("wrong upstream model/effort: %s", payload)
				}
				if !strings.Contains(rec.Body.String(), "argon-ok") || (tc.terminal != "" && !strings.Contains(rec.Body.String(), tc.terminal)) {
					t.Fatalf("incomplete downstream reply: %s", rec.Body.String())
				}
			})
		}
	}
}

func TestAntigravityMessagesKeepsLegacyThinkingDefaults(t *testing.T) {
	for _, tc := range []struct {
		model, wire, level string
		budget             float64
	}{
		{model: "gemini-3.6-flash-low", wire: "gemini-3.6-flash-low", budget: 4096},
		{model: "gemini-3.7-flash", wire: "gemini-3.7-flash-tiered", budget: 8192},
		{model: "gemini-3.8-flash", wire: "gemini-3.8-flash-tiered", level: "LOW"},
		{model: "gemini-3.1-pro", wire: "gemini-pro-agent", budget: 10001},
	} {
		t.Run(tc.model, func(t *testing.T) {
			handler, upstream := newAntigravityTransportTestHandler(t, `{"candidates":[{"content":{"parts":[{"text":"legacy-ok"}]},"finishReason":"STOP"}]}`)
			body := `{"model":"` + tc.model + `","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`
			rec := invokeAntigravityTransport(t, handler, "/v1/messages", body, func(h *Handler, c *gin.Context) {
				c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 4002, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelAntigravity}})
				h.Messages(c)
			})
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "legacy-ok") {
				t.Fatalf("legacy Messages failed: %d %s", rec.Code, rec.Body.String())
			}
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			if upstream.envelope["model"] != tc.wire {
				t.Fatalf("legacy wire changed: %v", upstream.envelope["model"])
			}
			thinking := antigravityTestThinkingConfig(t, upstream.envelope)
			if tc.level != "" && thinking["thinkingLevel"] != tc.level {
				t.Fatalf("legacy level changed: %v", thinking)
			}
			if tc.budget > 0 && thinking["thinkingBudget"] != tc.budget {
				t.Fatalf("legacy budget changed: %v", thinking)
			}
		})
	}
}

func TestAntigravityMessagesLogicalModelKeepsRelayFallback(t *testing.T) {
	for _, tc := range []struct{ model, backing string }{
		{"gemini-4-argon", "gemini-4-argon-high"},
		{"gemini-3.8-flash", "gemini-3.8-flash-tiered"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			requests := make(chan []byte, 1)
			relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_relay\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"relay-ok\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
			}))
			defer relay.Close()
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, MaxRetries: 0})
			t.Cleanup(store.Stop)
			store.AddAccount(&auth.Account{DBID: 4010, UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", AntigravityProjectID: "test-project", Models: []string{tc.backing}, HealthTier: auth.HealthTierHealthy, Status: auth.StatusCooldown, CooldownUtil: time.Now().Add(time.Hour)})
			store.AddAccount(&auth.Account{DBID: 4011, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: relay.URL + "/v1", APIKey: "test-key", Models: []string{tc.model}, HealthTier: auth.HealthTierHealthy, Status: auth.StatusReady})
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			body := `{"model":"` + tc.model + `","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"medium"}}`
			for _, scope := range []struct {
				name   string
				limits database.APIKeyLimits
				status int
			}{
				{"unrestricted", database.APIKeyLimits{}, http.StatusOK},
				{"logical allowed", database.APIKeyLimits{ModelAllow: []string{tc.model}}, http.StatusOK},
				{"logical denied", database.APIKeyLimits{ModelDeny: []string{tc.model}}, http.StatusForbidden},
				{"fixed tier only", database.APIKeyLimits{ModelAllow: []string{tc.model + "-medium"}}, http.StatusServiceUnavailable},
			} {
				t.Run(scope.name, func(t *testing.T) {
					rec := invokeAntigravityTransport(t, handler, "/v1/messages", body, func(h *Handler, c *gin.Context) {
						c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 4010, Limits: scope.limits})
						h.Messages(c)
					})
					if rec.Code != scope.status {
						t.Fatalf("relay scope: status=%d, want %d: %s", rec.Code, scope.status, rec.Body.String())
					}
					if scope.status != http.StatusOK {
						if len(requests) != 0 {
							t.Fatal("unauthorized relay model reached upstream")
						}
						return
					}
					if len(requests) != 1 {
						t.Fatal("logical relay was excluded")
					}
					request := <-requests
					if gjson.GetBytes(request, "model").String() != tc.model || gjson.GetBytes(request, "reasoning.effort").String() != "medium" {
						t.Fatalf("relay received Antigravity-specific rewrite: %s", request)
					}
				})
			}
		})
	}
}
