package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestExcelAliasesPassIngressModelValidation(t *testing.T) {
	h := &Handler{}
	validate := h.modelValidator([]string{"gpt-6-astra"})
	for _, model := range []string{"gpt-6-astra-excel", "gpt-6-sol-excel", "gpt-5.6-sol-excel", "gpt-5.6-luna-excel", "gpt-5.6-terra-excel"} {
		if err := validate(gjson.Parse(`"`+model+`"`), "model"); err != nil {
			t.Fatalf("Excel alias rejected before routing: %s: %v", model, err)
		}
		if accountFilterForModel(model)(&auth.Account{DBID: 999}) {
			t.Fatalf("unconfigured account admitted for %s", model)
		}
	}
	for _, model := range []string{"gpt-6-astra-execl", "unknown-excel"} {
		if err := validate(gjson.Parse(`"`+model+`"`), "model"); err == nil {
			t.Fatalf("unknown alias accepted: %s", model)
		}
	}
}

func TestExcelTransportUsesCurrentOAuthWithoutCodexHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(path, []byte(`{"42":{"credential_mode":"oauth"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXCEL_ROUTES_FILE", path)
	t.Setenv("EXCEL_BRIDGE_API_KEY", "01234567890123456789012345678901")
	a := &auth.Account{DBID: 42, AccessToken: "first-token", AccountID: "workspace"}
	wantToken := "first-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("X-Excel-Account") != "42" {
			t.Error("wrong route or account")
		}
		if r.Header.Get("X-Excel-Access-Token") != wantToken {
			t.Error("stale OAuth token")
		}
		for _, key := range []string{"Originator", "Session_id", "X-Codex-Turn-State"} {
			if r.Header.Get(key) != "" {
				t.Errorf("leaked Codex header %s", key)
			}
		}
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "metadata.turn_id").String() != "turn" {
			t.Error("metadata lost")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":22384,"output_tokens":3,"input_tokens_details":{"cached_tokens":20000}}}`))
	}))
	defer server.Close()
	t.Setenv("EXCEL_BRIDGE_URL", server.URL)
	for _, token := range []string{"first-token", "rotated-token"} {
		wantToken = token
		a.Mu().Lock()
		a.AccessToken = token
		a.Mu().Unlock()
		resp, err := ExecuteRequest(context.Background(), a, []byte(`{"model":"gpt-5.6-sol-excel","metadata":{"turn_id":"turn"}}`), "session", "", "client", nil, http.Header{"Originator": {"codex"}}, true)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if gjson.GetBytes(data, "usage.input_tokens").Int() != 22384 {
			t.Fatal("upstream usage was replaced")
		}
	}
}

func TestExcelPreparationPreservesProtocol(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol-excel","metadata":{"turn_id":"turn","agent_iteration":"2"},"context_management":[],"reasoning_effort":"high","input":[{"type":"reasoning","encrypted_content":"cipher"},{"type":"function_call","call_id":"call","id":"native","name":"tool","arguments":"{}"}]}`)
	body, _ := PrepareResponsesBody(raw)
	for _, path := range []string{"metadata", "context_management", "reasoning_effort", "input"} {
		if gjson.GetBytes(raw, path).Raw != gjson.GetBytes(body, path).Raw {
			t.Errorf("%s was rewritten by Codex preparation", path)
		}
	}
	if accountFilterForResponsesWebSocket("gpt-5.6-sol-excel")(&auth.Account{DBID: 42}) {
		t.Fatal("Excel must not enter native Codex WS")
	}
}
