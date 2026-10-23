package proxy

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPrepareOpenAIResponsesBodyPreservesNativeReasoningAndSchema(t *testing.T) {
	for _, effort := range []string{"persistent", "max", "future_provider_effort", ""} {
		t.Run("effort_"+effort, func(t *testing.T) {
			raw := []byte(`{"model":"provider-custom-model","input":"return name","reasoning":{"effort":` + strconv.Quote(effort) + `,"summary":"detailed"},"text":{"format":{"type":"json_schema","name":"native","strict":true,"schema":{"type":"object","properties":{"count":{"type":"integer","minimum":9007199254740993,"maximum":9007199254741001,"multipleOf":3},"name":{"type":"string","pattern":"^[a-z]+$","minLength":2,"maxLength":20}},"required":["count","name"],"additionalProperties":false}}},"metadata":{"native":"keep"}}`)
			got := PrepareOpenAIResponsesBody(raw)
			if gjson.GetBytes(got, "reasoning.effort").String() != effort || gjson.GetBytes(got, "input").String() != "return name" {
				t.Fatalf("native reasoning or prompt changed: %s", got)
			}
			assertRelayNativeJSONEqual(t, gjson.GetBytes(raw, "text").Raw, gjson.GetBytes(got, "text").Raw)
			if gjson.GetBytes(got, "text.format.schema.properties.count.minimum").Raw != "9007199254740993" {
				t.Fatalf("native integer precision lost: %s", got)
			}
			if gjson.GetBytes(got, "reasoning.summary").String() != "detailed" || gjson.GetBytes(got, "metadata.native").String() != "keep" {
				t.Fatalf("native fields lost: %s", got)
			}
		})
	}
}

func TestPrepareOpenAIResponsesBodyOnlyTransformsLegacyAliases(t *testing.T) {
	t.Run("legacy reasoning", func(t *testing.T) {
		got := PrepareOpenAIResponsesBody([]byte(`{"model":"gpt-5.4","input":"test","reasoning_effort":"max"}`))
		if gjson.GetBytes(got, "reasoning.effort").String() != "xhigh" || gjson.GetBytes(got, "reasoning_effort").Exists() {
			t.Fatalf("legacy alias was not explicitly consumed: %s", got)
		}
	})
	t.Run("native reasoning wins", func(t *testing.T) {
		got := PrepareOpenAIResponsesBody([]byte(`{"model":"gpt-5.4","input":"test","reasoning_effort":"high","reasoning":{"effort":"persistent"}}`))
		if gjson.GetBytes(got, "reasoning.effort").String() != "persistent" || gjson.GetBytes(got, "reasoning_effort").Exists() {
			t.Fatalf("legacy alias replaced native reasoning: %s", got)
		}
	})
	t.Run("legacy JSON hint", func(t *testing.T) {
		got := PrepareOpenAIResponsesBody([]byte(`{"model":"gpt-5.4","input":"return name","response_format":{"type":"json_object"}}`))
		input := gjson.GetBytes(got, "input").String()
		if !strings.Contains(strings.ToLower(input), "json") || !strings.Contains(input, "return name") || gjson.GetBytes(got, "text.format.type").String() != "json_object" || gjson.GetBytes(got, "response_format").Exists() {
			t.Fatalf("legacy JSON mode conversion changed: %s", got)
		}
	})
	t.Run("native format wins", func(t *testing.T) {
		raw := []byte(`{"model":"gpt-5.4","input":"return name","response_format":{"type":"json_object"},"text":{"format":{"type":"json_schema","name":"native","schema":{"type":"string","pattern":"^native$","minLength":4}}}}`)
		got := PrepareOpenAIResponsesBody(raw)
		assertRelayNativeJSONEqual(t, gjson.GetBytes(raw, "text").Raw, gjson.GetBytes(got, "text").Raw)
		if gjson.GetBytes(got, "input").String() != "return name" || gjson.GetBytes(got, "response_format").Exists() {
			t.Fatalf("native format was rewritten by legacy alias: %s", got)
		}
	})
}

func assertRelayNativeJSONEqual(t *testing.T, want, got string) {
	t.Helper()
	decode := func(raw string) any {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(decode(want), decode(got)) {
		t.Fatalf("native JSON changed:\nwant=%s\ngot=%s", want, got)
	}
}
