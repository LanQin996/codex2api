package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/tidwall/gjson"
)

// responsesBodyRequestsImageGenerationByPath is the per-path implementation
// the single-pass version must keep matching.
func responsesBodyRequestsImageGenerationByPath(body []byte) bool {
	if isImageOnlyModel(gjson.GetBytes(body, "model").String()) {
		return true
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if choice.Type == gjson.String && strings.EqualFold(strings.TrimSpace(choice.String()), "image_generation") {
		return true
	}
	if choice.Exists() && strings.EqualFold(strings.TrimSpace(choice.Get("type").String()), "image_generation") {
		return true
	}
	for _, key := range responsesImageGenerationOptionFields {
		value := gjson.GetBytes(body, key)
		if key == "background" && value.Type != gjson.String {
			continue
		}
		if value.Exists() && value.Type != gjson.Null {
			return true
		}
	}
	return false
}

func imageGenerationScanCorpus() []string {
	bodies := []string{
		``, `null`, `[]`, `"model"`, `{}`, `{"model":"gpt-image-2"}`,
		`[{"model":"gpt-image-2"}]`,
		`{"model":"gpt-5.5","model":"gpt-image-2"}`,
		`{"model":"gpt-image-2","model":"gpt-5.5"}`,
		`{"mod\u0065l":"gpt-image-2"}`,
		`{"input":[{"model":"gpt-image-2","size":"1024x1024","tool_choice":"image_generation"}]}`,
		`{"tool_choice":" Image_Generation "}`,
		`{"tool_choice":{"type":"image_generation"}}`,
		`{"tool_choice":{"type":"function","name":"image_generation"}}`,
		`{"tool_choice":"auto","tool_choice":"image_generation"}`,
		`{"background":true}`, `{"background":"transparent"}`, `{"background":null}`,
		`{"size":null}`, `{"size":"auto"}`, `{"partial_images":0}`, `{"moderation":"low"}`,
		`{"model":"gpt-5.5" "size":"1024x1024"}`,
		`{"model":"gpt-5.5","size":`,
		`{"model":"gpt-image-2"`,
		`{"model":}`,
	}
	fields := []string{
		`"model":"gpt-5.5"`, `"model":"gpt-image-1"`, `"tool_choice":"auto"`,
		`"tool_choice":{"type":"image_generation"}`, `"background":"opaque"`, `"background":false`,
		`"quality":null`, `"output_format":"png"`, `"input":[{"type":"message","size":"x"}]`,
		`"stream":true`,
	}
	for mask := 0; mask < 1<<len(fields); mask++ {
		var parts []string
		for i, field := range fields {
			if mask&(1<<i) != 0 {
				parts = append(parts, field)
			}
		}
		bodies = append(bodies, "{"+strings.Join(parts, ",")+"}")
	}
	return bodies
}

func TestResponsesBodyRequestsImageGenerationMatchesPerPathScan(t *testing.T) {
	for _, body := range imageGenerationScanCorpus() {
		got := responsesBodyRequestsImageGeneration([]byte(body))
		want := responsesBodyRequestsImageGenerationByPath([]byte(body))
		if got != want {
			t.Fatalf("responsesBodyRequestsImageGeneration(%q) = %v, per-path scan = %v", body, got, want)
		}
	}
}

func TestLargeBodyScansKeepResults(t *testing.T) {
	body := largeCodexResponsesBody(t, 1<<20)
	if responsesBodyRequestsImageGeneration(body) != responsesBodyRequestsImageGenerationByPath(body) {
		t.Fatal("large-body image-generation scan diverged")
	}
	if err := ValidateResponsesFunctionNames(body); err != nil {
		t.Fatalf("ValidateResponsesFunctionNames: %v", err)
	}
	if meta := requestBodyCompactionMeta(body); meta.ProtocolTriggered || meta.HasHistory {
		t.Fatalf("unexpected compaction meta %+v", meta)
	}
	if got := normalizeCompactionTriggerFinal(body, false); &got[0] != &body[0] {
		t.Fatal("a body without a trigger must be returned unchanged")
	}
	if err := responsesHTTPContextEnvelopeError(body); err != nil {
		t.Fatalf("envelope check: %v", err)
	}
	duplicated := fmt.Sprintf(`{"input":[],"model":"gpt-5.5","in\u0070ut":%s}`, gjson.GetBytes(body, "input").Raw)
	err := responsesHTTPContextEnvelopeError([]byte(duplicated))
	if err == nil {
		t.Fatal("duplicate input must still be rejected")
	}
	if detail, _ := err.Details.(api.ErrorDetail); detail.Field != "input" {
		t.Fatalf("duplicate field = %+v, want input", err.Details)
	}
}
