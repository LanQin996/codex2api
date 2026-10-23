package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const codexSchemaCompatibilityMaxIssues = 32
const codexSchemaCompatibilityMaxTools = 1024
const codexSchemaCompatibilityMaxNumberBytes = 1024

// Strict mode checks the schema transformations actually used by the Codex
// adapter. A valid request that would lose constraints must fail before any
// generation; passing this check is not an assertion of hosted-tool support.
func codexResponsesSchemaCompatibilityIssues(raw []byte) []map[string]string {
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil {
		return nil
	}
	var issues []map[string]string
	stopped := false
	analyzedTools := 0
	add := func(field, reason string) {
		if stopped {
			return
		}
		if len(issues) == codexSchemaCompatibilityMaxIssues-1 {
			issues = append(issues, map[string]string{"field": field, "reason": "Codex schema compatibility analysis issue budget exhausted (32); further issues were not analyzed"})
			stopped = true
			return
		}
		issues = append(issues, map[string]string{"field": field, "reason": reason})
	}
	checkTools := func(tools []any, path string) {
		if stopped {
			return
		}
		if len(tools) > maxTools {
			add(path, "Codex tool limit is 128; the request cannot be sent with every tool available")
			return
		}
		if len(tools) > codexSchemaCompatibilityMaxTools-analyzedTools {
			add(path, "Codex schema compatibility analysis tool budget exhausted (1024 across tools and additional_tools)")
			stopped = true
			return
		}
		analyzedTools += len(tools)
		normalized, _ := normalizeFunctionToolsInArray(tools)
		if len(normalized) != len(tools) {
			add(path, "Codex normalization would remove an unrecognized tool declaration")
		}
		for i, rawTool := range normalized {
			if stopped {
				return
			}
			tool, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			if parameters, ok := tool["parameters"].(map[string]any); ok {
				if codexSchemaNumberPrecisionLost(parameters) {
					add(fmt.Sprintf("%s[%d].parameters", path, i), "Codex JSON normalization would lose a JSON Schema number's precision or exceed the numeric analysis limit")
					continue
				}
				if isReservedCodexTool(tool) {
					continue
				}
				before, _ := json.Marshal(parameters)
				normalizeResponsesToolSchema(tool, nil)
				after, _ := json.Marshal(tool["parameters"])
				if !bytes.Equal(before, after) {
					add(fmt.Sprintf("%s[%d].parameters", path, i), "Codex normalization would change the tool's JSON Schema constraints")
				}
			}
			before, _ := json.Marshal(tool)
			wrapper := map[string]any{"tools": []any{tool}}
			if normalizeResponsesWebSearchTools(wrapper) {
				after, _ := json.Marshal(wrapper["tools"].([]any)[0])
				if !bytes.Equal(before, after) {
					add(fmt.Sprintf("%s[%d]", path, i), "Codex normalization would change the requested web search tool configuration")
				}
			}
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		checkTools(tools, "tools")
	}
	if input, ok := body["input"].([]any); ok {
		for i, rawItem := range input {
			if stopped {
				return issues
			}
			item, ok := rawItem.(map[string]any)
			if !ok || item["type"] != "additional_tools" {
				continue
			}
			if tools, ok := item["tools"].([]any); ok {
				checkTools(tools, fmt.Sprintf("input[%d].tools", i))
			}
		}
	}
	if stopped {
		return issues
	}
	checkFormat := func(format map[string]any, path string) {
		if stopped {
			return
		}
		if schema, ok := format["schema"].(map[string]any); ok {
			if codexSchemaNumberPrecisionLost(schema) {
				add(path+".schema", "Codex JSON normalization would lose a JSON Schema number's precision or exceed the numeric analysis limit")
				return
			}
			before, _ := json.Marshal(schema)
			sanitizeStructuredOutputSchemaForUpstream(schema)
			after, _ := json.Marshal(schema)
			if !bytes.Equal(before, after) {
				add(path+".schema", "Codex normalization would change the structured output JSON Schema constraints")
			}
		}
	}
	if text, ok := body["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			checkFormat(format, "text.format")
		}
	}
	if legacy, ok := body["response_format"].(map[string]any); ok {
		if format := responsesTextFormatFromResponseFormat(legacy); format != nil {
			checkFormat(format, "response_format")
		}
	}
	return issues
}

// Compare the numbers as sent on the wire, rather than comparing a decimal to
// its binary floating-point representation: 0.1 and exponent-only formatting
// changes are harmless, but an enum integer rounded past 2^53 is not.
func codexSchemaNumberPrecisionLost(value any) bool {
	switch item := value.(type) {
	case json.Number:
		raw := item.String()
		if len(raw) > codexSchemaCompatibilityMaxNumberBytes {
			return true
		}
		decoded, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return true
		}
		if decoded == 0 {
			// Avoid building an enormous rational denominator for a malicious
			// underflow exponent; a nonzero mantissa cannot be preserved as zero.
			mantissa, _, _ := strings.Cut(strings.ToLower(raw), "e")
			return strings.ContainsAny(mantissa, "123456789")
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			return true
		}
		original, originalOK := new(big.Rat).SetString(raw)
		forwarded, forwardedOK := new(big.Rat).SetString(string(encoded))
		return !originalOK || !forwardedOK || original.Cmp(forwarded) != 0
	case map[string]any:
		for _, child := range item {
			if codexSchemaNumberPrecisionLost(child) {
				return true
			}
		}
	case []any:
		for _, child := range item {
			if codexSchemaNumberPrecisionLost(child) {
				return true
			}
		}
	}
	return false
}
