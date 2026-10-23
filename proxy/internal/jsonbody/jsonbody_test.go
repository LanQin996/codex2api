package jsonbody

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var differentialBodies = []string{
	`{}`,
	`{"model":"gpt-5.5","input":[{"type":"message"}],"stream":true}`,
	`{"type":"response.create","stream":false,"prompt_cache_key":"abc-123","service_tier":"priority"}`,
	`{ "type" : "response.create" , "stream" : true , "x" : 1 }`,
	`{"type":"a","type":"b","stream":true,"stream":false}`,
	`{"ty\u0070e":"response.create","stre\u0061m":true}`,
	`{"prompt_cache_key":"a\"b","service_tier":"\u0070riority"}`,
	`{"prompt_cache_key":null,"stream":"true","type":1}`,
	`{"client_metadata":{"x-codex-turn-state":"s1","ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`,
	`{"client_metadata":"not-an-object","stream_id":"lane-1"}`,
	`{"input":[{"type":"function_call","type":"x"}],"tools":[]}`,
	`[{"type":"a"},{"stream":true}]`,
	`"type"`,
	`null`,
	``,
	`   `,
	`{"type":"response.create","stream":tr`,
	`{"type":`,
	`{"stream":true,}`,
	`{"a":{"b":{"c":"d"}},"type":"x"}`,
}

var differentialPaths = []string{
	"type", "stream", "stream_id", "prompt_cache_key", "previous_response_id",
	"prompt_cache_retention", "safety_identifier", "disable_response_storage",
	"service_tier", "serviceTier", "instructions",
	"client_metadata.x-codex-turn-state",
	"client_metadata.ws_request_header_x_openai_internal_codex_responses_lite",
	"a.b.c", "input",
}

func TestDeletePathMatchesSJSON(t *testing.T) {
	for _, body := range differentialBodies {
		for _, path := range differentialPaths {
			want, _ := sjson.DeleteBytes([]byte(body), path)
			got := DeletePath([]byte(body), path)
			if !bytes.Equal(got, want) {
				t.Fatalf("DeletePath(%q, %q) = %q, sjson = %q", body, path, got, want)
			}
		}
	}
}

func TestSetStringMatchesSJSON(t *testing.T) {
	values := []string{"response.create", "abc-123", "priority", "true", "", "a\"b", "é", "<tag>&", "line\nbreak", "s1"}
	for _, body := range differentialBodies {
		for _, path := range differentialPaths {
			for _, value := range values {
				want, _ := sjson.SetBytes([]byte(body), path, value)
				got := SetString([]byte(body), path, value)
				if !bytes.Equal(got, want) {
					t.Fatalf("SetString(%q, %q, %q) = %q, sjson = %q", body, path, value, got, want)
				}
			}
		}
	}
}

func TestSetBoolMatchesSJSON(t *testing.T) {
	for _, body := range differentialBodies {
		for _, path := range differentialPaths {
			for _, value := range []bool{true, false} {
				want, _ := sjson.SetBytes([]byte(body), path, value)
				got := SetBool([]byte(body), path, value)
				if !bytes.Equal(got, want) {
					t.Fatalf("SetBool(%q, %q, %v) = %q, sjson = %q", body, path, value, got, want)
				}
			}
		}
	}
}

func TestNoOpEditsReturnOriginalSlice(t *testing.T) {
	body := []byte(`{"type":"response.create","stream":true,"prompt_cache_key":"k"}`)
	same := func(got []byte) bool { return len(got) > 0 && &got[0] == &body[0] }
	if !same(DeletePath(body, "stream_id")) {
		t.Fatal("deleting an absent path must not copy the body")
	}
	if !same(SetString(body, "type", "response.create")) || !same(SetString(body, "prompt_cache_key", "k")) {
		t.Fatal("writing an identical string must not copy the body")
	}
	if !same(SetBool(body, "stream", true)) {
		t.Fatal("writing an identical bool must not copy the body")
	}
	if same(SetBool(body, "stream", false)) {
		t.Fatal("a real change must produce a new body")
	}
}

func TestViewMatchesCopyingParse(t *testing.T) {
	for _, body := range differentialBodies {
		if got, want := View([]byte(body)).Raw, gjson.ParseBytes([]byte(body)).Raw; got != want {
			t.Fatalf("View(%q).Raw = %q, want %q", body, got, want)
		}
		for _, path := range differentialPaths {
			got, want := GetView([]byte(body), path), gjson.GetBytes([]byte(body), path)
			if got.Raw != want.Raw || got.Type != want.Type || got.Index != want.Index || got.Exists() != want.Exists() {
				t.Fatalf("GetView(%q, %q) = %+v, want %+v", body, path, got, want)
			}
		}
	}
}

func TestTopLevelMatchesGetBytes(t *testing.T) {
	keys := []string{"type", "stream", "model", "prompt_cache_key", "service_tier", "client_metadata", "missing", "a", "input"}
	bodies := append([]string{
		`{"model":"m1","model":"m2","input":[{"model":"nested"}]}`,
		`{"mod\u0065l":"escaped","model":"plain"}`,
		`{"a":1 "model":"x"}`,
		`{"input":[1,2,3],"model":null,"stream":false}`,
		`{"model":"m"`,
		`{"model":}`,
	}, differentialBodies...)
	for _, body := range bodies {
		values, ok := TopLevel([]byte(body), keys...)
		if !ok {
			if gjson.ParseBytes([]byte(body)).IsObject() {
				t.Fatalf("TopLevel(%q) rejected an object root", body)
			}
			continue
		}
		for i, key := range keys {
			want := gjson.GetBytes([]byte(body), key)
			got := values[i]
			if got.Raw != want.Raw || got.Type != want.Type || got.Exists() != want.Exists() || got.String() != want.String() {
				t.Fatalf("TopLevel(%q)[%q] = %+v, GetBytes = %+v", body, key, got, want)
			}
		}
	}
}

func BenchmarkDeleteAbsentPath(b *testing.B) {
	body := []byte(`{"input":"` + strings.Repeat("x", 10<<20) + `","model":"gpt-5.5"}`)
	b.Run("sjson", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = sjson.DeleteBytes(body, "stream_id")
		}
	})
	b.Run("DeletePath", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = DeletePath(body, "stream_id")
		}
	})
}
