package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Exercise the actual Handler and native HTTP executor, including request
// compression. Inspect the final loopback wire body, not an execute hook.
func TestResponsesHTTPToolDescriptionDefaultsStayOnUserTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })
	previousResin := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(previousResin) })
	t.Cleanup(resetResponseCacheForTest)

	for _, stream := range []bool{false, true} {
		name := "JSON"
		if stream {
			name = "SSE"
		}
		t.Run(name, func(t *testing.T) {
			requests := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- readUpstreamRequestBody(r)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wsContextTestSSE("resp_tool_description", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}`))
			}))
			defer upstream.Close()
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "tool-description-test"})
			store := newContinuationCodexStore()
			defer store.Stop()
			handler := NewHandler(store, nil, nil, nil)
			var payload map[string]any
			if err := json.Unmarshal([]byte(`{
				"model":"gpt-5.5","store":false,
				"tools":[
					{"type":"tool_search"},
					{"type":"tool_search","description":"user supplied builtin description"},
					{"type":"tool_search","description":null},
					{"type":"function","name":"tool_search","parameters":{"type":null}},
					{"type":"custom","name":"tool_search","format":{"type":"text"}},
					{"type":"function","name":"tool_search","description":"","parameters":{"type":"object"}},
					{"type":"custom","name":"tool_search","description":false},
					{"type":"namespace","name":"nested","tools":[
						{"type":"tool_search"},
						{"type":"function","name":"tool_search","description":"nested user function","parameters":{"type":"object"}}
					]}
				],
				"input":[
					{"role":"user","content":"Reply OK."},
					{"type":"additional_tools","tools":[
						{"type":"tool_search"},
						{"type":"tool_search","description":false},
						{"type":"function","name":"tool_search","description":"additional user function","parameters":{"type":null}},
						{"type":"namespace","name":"deferred","tools":[{"type":"tool_search"}]}
					]}
				]
			}`), &payload); err != nil {
				t.Fatal(err)
			}
			payload["stream"] = stream
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			response := invokeResponsesHandler(t, handler.Responses, raw)
			if response.Code != http.StatusOK {
				t.Fatalf("native request failed before loopback projection: status=%d body=%s", response.Code, response.Body.String())
			}
			body := <-requests
			if !gjson.ValidBytes(body) {
				t.Fatal("loopback did not receive the final decompressed JSON body")
			}
			for _, path := range []string{"tools.0.description", "tools.7.description", "tools.7.tools.0.description",
				`input.#(type=="additional_tools").tools.0.description`, `input.#(type=="additional_tools").tools.3.description`,
				`input.#(type=="additional_tools").tools.3.tools.0.description`} {
				if gjson.GetBytes(body, path).Exists() {
					t.Errorf("gateway injected description into a builtin/carrier at %s: %s", path, body)
				}
			}
			for path, want := range map[string]string{
				"tools.1.description":                                   `"user supplied builtin description"`,
				"tools.2.description":                                   "null",
				"tools.5.description":                                   `""`,
				"tools.6.description":                                   "false",
				"tools.7.tools.1.description":                           `"nested user function"`,
				`input.#(type=="additional_tools").tools.1.description`: "false",
				`input.#(type=="additional_tools").tools.2.description`: `"additional user function"`,
			} {
				if got := gjson.GetBytes(body, path); !got.Exists() || got.Raw != want {
					t.Errorf("explicit description changed at %s: got=%s want=%s", path, got.Raw, want)
				}
			}
			for _, path := range []string{"tools.3", "tools.4"} {
				tool := gjson.GetBytes(body, path)
				if tool.Get("name").String() != "tool_search" || tool.Get("description").String() == "" {
					t.Errorf("user tool named tool_search lost its default description: %s", tool.Raw)
				}
			}
			for _, path := range []string{"tools.3.parameters.type", `input.#(type=="additional_tools").tools.2.parameters.type`} {
				if got := gjson.GetBytes(body, path).String(); got != "object" {
					t.Errorf("user function schema normalization regressed at %s: %q", path, got)
				}
			}
		})
	}
}
