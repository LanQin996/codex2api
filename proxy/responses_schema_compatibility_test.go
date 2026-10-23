package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codex2api/api"
)

func TestStrictResponsesCompatibilitySchemaConstraints(t *testing.T) {
	for _, tc := range []struct {
		name, body, field string
	}{
		{"function constraint", `{"tools":[{"type":"function","name":"lookup","strict":true,"parameters":{"type":"object","properties":{"n":{"type":"integer","minimum":1}}}}]}`, "tools[0].parameters"},
		{"structured output", `{"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"x":{"type":"string","pattern":"^a"}},"required":["x"],"additionalProperties":false}}}}`, "text.format.schema"},
		{"legacy output", `{"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{"n":{"type":"integer","minimum":1}},"required":["n"],"additionalProperties":false}}}}`, "response_format.schema"},
		{"lite tool schema", `{"input":[{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"n":{"type":"integer","minimum":1}}}}]}]}`, "input[0].tools[0].parameters"},
		{"web search dropped option", `{"tools":[{"type":"web_search","unsupported_setting":true}]}`, "tools[0]"},
		{"unrecognized declaration", `{"tools":[{}]}`, "tools"},
		{"ordinary function", `{"tools":[{"type":"function","name":"lookup","strict":true,"parameters":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}}]}`, ""},
		{"reserved tool exact schema", `{"tools":[{"type":"function","name":"collaboration.spawn_agent","parameters":{"type":"object","properties":{"n":{"type":"integer","minimum":1}}}}]}`, ""},
		{"reserved tool numeric precision", `{"tools":[{"type":"function","name":"collaboration.spawn_agent","parameters":{"type":"object","properties":{"n":{"type":"integer","enum":[9007199254740993]}}}}]}`, "tools[0].parameters"},
		{"hosted capabilities left to upstream", `{"tools":[{"type":"mcp","server_label":"test","server_url":"https://example.invalid/mcp"},{"type":"file_search","vector_store_ids":["vs_test"]}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := codexResponsesSchemaCompatibilityIssues([]byte(tc.body))
			if tc.field == "" {
				if len(issues) != 0 {
					t.Fatalf("unexpected schema loss: %#v", issues)
				}
				return
			}
			if len(issues) != 1 || issues[0]["field"] != tc.field {
				t.Fatalf("issues = %#v, want field %s", issues, tc.field)
			}
		})
	}
}

func TestStrictResponsesCompatibilityToolLimit(t *testing.T) {
	tools := make([]map[string]any, maxTools+1)
	for i := range tools {
		tools[i] = map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	}
	body, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "input": "hi", "tools": tools})
	err := validateCodexRelayResponsesCompatibility(body)
	if err == nil || err.Code != api.ErrCodeUnsupportedFeature || !strings.Contains(err.Error(), "Responses") {
		t.Fatalf("tool overflow must fail explicitly: %#v", err)
	}
	fields := err.Details.(map[string]any)["fields"].([]map[string]string)
	if len(fields) != 1 || fields[0]["field"] != "tools" {
		t.Fatalf("tool overflow fields = %#v", fields)
	}
}

func TestStrictResponsesCompatibilitySchemaNumberPrecision(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		loss         bool
	}{
		{"large enum integer", `{"type":"integer","enum":[9007199254740993]}`, true},
		{"large const integer", `{"type":"integer","const":9007199254740993}`, true},
		{"large negative integer", `{"type":"integer","enum":[-9007199254740993]}`, true},
		{"exact large integer", `{"type":"integer","enum":[9007199254740992]}`, false},
		{"ordinary decimal", `{"type":"number","enum":[0.1]}`, false},
		{"exponent spelling", `{"type":"number","enum":[1e3]}`, false},
		{"negative zero", `{"type":"number","enum":[-0.0]}`, false},
		{"long decimal", `{"type":"number","enum":[0.10000000000000001]}`, true},
		{"underflow exponent", `{"type":"number","enum":[1e-999999999]}`, true},
		{"zero underflow exponent", `{"type":"number","enum":[0e-999999999]}`, false},
		{"overflow exponent", `{"type":"number","enum":[1e999999999]}`, true},
		{"numeric literal budget", `{"type":"number","enum":[0.` + strings.Repeat("0", codexSchemaCompatibilityMaxNumberBytes) + `1]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, format := range []bool{false, true} {
				schema := `{"type":"object","properties":{"n":` + tc.schema + `},"required":["n"],"additionalProperties":false}`
				body := `{"tools":[{"type":"function","name":"lookup","parameters":` + schema + `}]}`
				field := "tools[0].parameters"
				if format {
					body = `{"text":{"format":{"type":"json_schema","name":"answer","schema":` + schema + `}}}`
					field = "text.format.schema"
				}
				issues := codexResponsesSchemaCompatibilityIssues([]byte(body))
				if !tc.loss && len(issues) != 0 {
					t.Fatalf("number whose wire value is preserved was rejected: %#v", issues)
				}
				if tc.loss && (len(issues) != 1 || issues[0]["field"] != field || !strings.Contains(issues[0]["reason"], "precision")) {
					t.Fatalf("number precision loss was not rejected: %#v", issues)
				}
			}
		})
	}
}

func TestStrictResponsesCompatibilitySchemaAnalysisBudgets(t *testing.T) {
	ordinary := map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	constraint := map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer", "minimum": 1}}}}
	t.Run("oversized group stops immediately", func(t *testing.T) {
		tools := make([]any, 4096)
		for i := range tools {
			tools[i] = constraint
		}
		raw, _ := json.Marshal(map[string]any{"tools": tools})
		issues := codexResponsesSchemaCompatibilityIssues(raw)
		if len(issues) != 1 || issues[0]["field"] != "tools" || !strings.Contains(issues[0]["reason"], "128") {
			t.Fatalf("oversized tools were analyzed after rejection: %#v", issues)
		}
	})
	t.Run("issue count is bounded", func(t *testing.T) {
		tools := make([]any, maxTools)
		for i := range tools {
			tools[i] = constraint
		}
		raw, _ := json.Marshal(map[string]any{"tools": tools})
		issues := codexResponsesSchemaCompatibilityIssues(raw)
		if len(issues) != codexSchemaCompatibilityMaxIssues || !strings.Contains(issues[len(issues)-1]["reason"], "issue budget exhausted (32)") {
			t.Fatalf("schema issues exceeded their bounded response: %#v", issues)
		}
	})
	t.Run("all additional tools share a budget", func(t *testing.T) {
		tools := make([]any, maxTools)
		for i := range tools {
			tools[i] = ordinary
		}
		input := make([]any, codexSchemaCompatibilityMaxTools/maxTools+1)
		for i := range input {
			input[i] = map[string]any{"type": "additional_tools", "tools": tools}
		}
		raw, _ := json.Marshal(map[string]any{"input": input})
		issues := codexResponsesSchemaCompatibilityIssues(raw)
		if len(issues) != 1 || issues[0]["field"] != "input[8].tools" || !strings.Contains(issues[0]["reason"], "tool budget exhausted (1024") {
			t.Fatalf("additional_tools escaped the aggregate budget: %#v", issues)
		}
	})
	t.Run("top-level and additional tools share a budget", func(t *testing.T) {
		tools := make([]any, maxTools)
		for i := range tools {
			tools[i] = ordinary
		}
		input := make([]any, codexSchemaCompatibilityMaxTools/maxTools)
		for i := range input {
			input[i] = map[string]any{"type": "additional_tools", "tools": tools}
		}
		raw, _ := json.Marshal(map[string]any{"tools": tools, "input": input})
		issues := codexResponsesSchemaCompatibilityIssues(raw)
		if len(issues) != 1 || issues[0]["field"] != "input[7].tools" || !strings.Contains(issues[0]["reason"], "tool budget exhausted (1024") {
			t.Fatalf("top-level tools escaped the aggregate budget: %#v", issues)
		}
	})
}

func TestStrictResponsesCompatibilitySchemaMalformedShapesDoNotPanic(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `true`, `{}`, `{"tools":null}`,
		`{"tools":[null,[],"tool",true,1,{"type":{}},{"type":[]},{"name":{},"parameters":true}]}`,
		`{"input":[null,[],{"type":[]},{"type":{}},{"type":"additional_tools","tools":[null,{"type":"function","parameters":null}]}]}`,
		`{"text":{"format":{"schema":{"type":{},"properties":{"x":[]},"items":false,"required":{}}}}}`,
		`{"response_format":{"type":"json_schema","json_schema":{"schema":null}}}`,
	} {
		issues := codexResponsesSchemaCompatibilityIssues([]byte(body))
		if len(issues) > codexSchemaCompatibilityMaxIssues {
			t.Fatalf("malformed shape exceeded issue budget: %#v", issues)
		}
	}
}
