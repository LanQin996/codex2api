package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestAntigravityArgonRequiresDiscoveredBacking(t *testing.T) {
	want := []string{"gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high"}
	account := &auth.Account{UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", Models: []string{"gemini-4-argon-high"}}
	if got := antigravityPublicModelsForAccount(account); !reflect.DeepEqual(got, want) {
		t.Fatalf("Argon catalog = %v, want %v", got, want)
	}
	if got := AntigravityPublishedModelIDs([]string{"gemini-4-argon", "gemini-4-argon-high"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed Argon catalog = %v, want only verified tiers %v", got, want)
	}
	for _, model := range append(want, "gemini-4-argon") {
		if wire, ok := antigravityResolvePublicModelForAccount(account, model); !ok || wire != "gemini-4-argon-high" {
			t.Fatalf("Argon %s resolved to %q, %v", model, wire, ok)
		}
	}
	for _, raw := range [][]string{nil, auth.AntigravityDefaultModelIDs(), {"gemini-4-argon"}, {"gemini-4-argon-disabled"}, {"gemini-4-argon-low", "gemini-4-argon-medium"}} {
		unavailable := &auth.Account{UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", Models: raw}
		for _, model := range antigravityPublicModelsForAccount(unavailable) {
			if strings.HasPrefix(model, "gemini-4-argon") {
				t.Fatalf("catalog %v advertised unverified Argon model %q", raw, model)
			}
		}
		for _, model := range append(want, "gemini-4-argon", "gemini-4-argon-disabled") {
			if antigravityAccountSupportsPublicModel(unavailable, model) {
				t.Fatalf("catalog %v accepted unverified Argon model %q", raw, model)
			}
		}
	}
}

func TestAntigravityArgonReasoningUsesNamedLevels(t *testing.T) {
	for _, tc := range []struct {
		name, model, reasoning, level string
	}{
		{"default", "gemini-4-argon", `{}`, "HIGH"},
		{"summary only", "gemini-4-argon", `{"summary":"auto"}`, "HIGH"},
		{"low", "gemini-4-argon", `{"effort":"low"}`, "LOW"},
		{"medium", "gemini-4-argon", `{"effort":"medium"}`, "MEDIUM"},
		{"high", "gemini-4-argon", `{"effort":"high"}`, "HIGH"},
		{"fixed low", "gemini-4-argon-low", `{"effort":"high"}`, "LOW"},
		{"fixed medium", "gemini-4-argon-medium", `{"effort":"low"}`, "MEDIUM"},
		{"fixed high", "gemini-4-argon-high", `{"effort":"low"}`, "HIGH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := responsesToGeminiInternalWithThoughts([]byte(`{"input":"hello","max_output_tokens":1000000,"reasoning":`+tc.reasoning+`}`), "project", tc.model, nil, true)
			if err != nil {
				t.Fatal(err)
			}
			if payload["model"] != "gemini-4-argon-high" {
				t.Fatalf("wire model = %v", payload["model"])
			}
			thinking := antigravityTestThinkingConfig(t, payload)
			if thinking["thinkingLevel"] != tc.level || thinking["includeThoughts"] != true {
				t.Fatalf("thinking = %v, want %s with thoughts", thinking, tc.level)
			}
			if _, exists := thinking["thinkingBudget"]; exists {
				t.Fatalf("Argon inherited numeric budget: %v", thinking)
			}
			cfg := payload["request"].(map[string]any)["generationConfig"].(map[string]any)
			if _, exists := cfg["maxOutputTokens"]; exists {
				t.Fatalf("Argon changed the Gemini output-limit rule: %v", cfg)
			}
		})
	}
}

func TestAntigravityArgonLogicalFoldValidatesEffort(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high"} {
		body := []byte(`{"model":"gemini-4-argon","input":"hello","reasoning":{"effort":"` + effort + `","summary":"auto"}}`)
		folded, model, apiErr := antigravityFoldLogicalModel(body, "gemini-4-argon")
		wantLevel := effort
		if wantLevel == "" {
			wantLevel = "high"
		}
		if apiErr != nil || model != "gemini-4-argon-"+wantLevel || gjson.GetBytes(folded, "model").String() != model {
			t.Fatalf("effort %q folded to %q, error %v", effort, model, apiErr)
		}
		if gjson.GetBytes(folded, "reasoning.summary").String() != "auto" {
			t.Fatal("logical fold lost reasoning summary")
		}
	}
	for _, effort := range []string{"minimal", "xhigh", "unknown"} {
		if _, _, apiErr := antigravityFoldLogicalModel([]byte(`{"reasoning":{"effort":"`+effort+`"}}`), "gemini-4-argon"); apiErr == nil {
			t.Fatalf("unsupported bare Argon effort %q was accepted", effort)
		}
	}
}

func TestAntigravityArgonThoughtVisibility(t *testing.T) {
	withAntigravityExposeThoughts(t, true)
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"input":"hello"}`, true},
		{`{"input":"hello","reasoning":{"summary":"none"}}`, false},
	} {
		payload, err := responsesToGeminiInternal([]byte(tc.body), "project", "gemini-4-argon-high")
		if err != nil {
			t.Fatal(err)
		}
		if got := antigravityTestThinkingConfig(t, payload)["includeThoughts"] == true; got != tc.want {
			t.Fatalf("thought visibility = %v, want %v", got, tc.want)
		}
	}
	withAntigravityExposeThoughts(t, false)
	payload, err := responsesToGeminiInternal([]byte(`{"input":"hello"}`), "project", "gemini-4-argon-high")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := antigravityTestThinkingConfig(t, payload)["includeThoughts"]; exists {
		t.Fatal("disabled thought visibility was overridden")
	}
}

func TestAntigravityArgonToolReplayUsesGeminiSignatureFallback(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"lookup"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"found"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	replayedCall := func(model string) (map[string]any, map[string]any) {
		t.Helper()
		payload, err := responsesToGeminiInternal(body, "project", model)
		if err != nil {
			t.Fatal(err)
		}
		request := payload["request"].(map[string]any)
		contents := request["contents"].([]any)
		return payload, contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	}
	for _, model := range []string{"gemini-4-argon", "gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high"} {
		payload, call := replayedCall(model)
		if call["thoughtSignature"] != antigravityGeminiSkipThoughtSignature || call["thought_signature"] != antigravityGeminiSkipThoughtSignature {
			t.Fatalf("Argon tool replay lacks signature fallback: %v", call)
		}
		if _, exists := payload["request"].(map[string]any)["tools"]; !exists {
			t.Fatal("Argon function declarations were dropped")
		}
	}
	// Raw pass-through models keep their own wire ID; the replay stub must not
	// depend on the gateway recognizing the name (issue #792).
	for _, model := range []string{"gemini-4-future", "gemini-4-argon-next"} {
		payload, call := replayedCall(model)
		if payload["model"] != model {
			t.Fatalf("%q was not passed through: wire model %v", model, payload["model"])
		}
		if call["thoughtSignature"] != antigravityGeminiSkipThoughtSignature || call["thought_signature"] != antigravityGeminiSkipThoughtSignature {
			t.Fatalf("Gemini pass-through %q tool replay lacks signature fallback: %v", model, call)
		}
	}
	for _, model := range []string{"claude-sonnet-4-6", "claude-opus-5-5-high", "gpt-oss-120b-medium"} {
		if antigravityGeminiNeedsToolSignature(model) {
			t.Fatalf("Gemini signature stub leaked to non-Gemini model %q", model)
		}
	}
}

func TestAntigravityArgonNativeGeminiThinkingAndSignature(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"lookup"}]},{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{},"id":"call_1"}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","id":"call_1","response":{"result":"found"}},"thoughtSignature":"wrong"}]}]}`)
	payload, wire, _, err := geminiNativeToAntigravityEnvelope(raw, "project", "gemini-4-argon-low")
	if err != nil {
		t.Fatal(err)
	}
	if wire != "gemini-4-argon-high" || gjson.GetBytes(payload, "request.generationConfig.thinkingConfig.thinkingLevel").String() != "LOW" {
		t.Fatalf("native wire/thinking: %s", payload)
	}
	if gjson.GetBytes(payload, "request.contents.1.parts.0.thoughtSignature").String() != antigravityGeminiSkipThoughtSignature {
		t.Fatalf("native function call missing signature fallback: %s", payload)
	}
	if gjson.GetBytes(payload, "request.contents.2.parts.0.thoughtSignature").Exists() {
		t.Fatalf("native function result retained thought signature: %s", payload)
	}
	var request map[string]any
	if err := json.Unmarshal([]byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"MEDIUM","includeThoughts":false},"maxOutputTokens":1000000}}`), &request); err != nil {
		t.Fatal(err)
	}
	antigravityApplyNativeGeminiThinkingConfig(request, "gemini-4-argon-high", "gemini-4-argon-high")
	cfg := request["generationConfig"].(map[string]any)
	if got := cfg["thinkingConfig"].(map[string]any); got["thinkingLevel"] != "MEDIUM" || got["includeThoughts"] != false {
		t.Fatalf("explicit native thinking was overwritten: %v", got)
	}
	if _, exists := cfg["maxOutputTokens"]; exists {
		t.Fatal("native Argon changed the Gemini output-limit rule")
	}
}
