package proxy

import (
	"context"
	"encoding/json"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"net/http"
	"testing"
)

func TestExcelProtocolRejectionDoesNotRetryOrPenalize(t *testing.T) {
	errBody := map[string]string{"code": "invalid_tool_call", "message": "Tool relay rejected: catalog_or_schema_mismatch; no tool dispatched."}
	body, _ := json.Marshal(map[string]any{"error": errBody})
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 42, AccessToken: "test", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{store: store}
	before := account.RuntimeStatus()
	for _, status := range []int{400, 401, 403, 500, 502} {
		general, rate := 0, 0
		if shouldRetryHTTPStatus(status, body, &general, &rate, 2, 2, policy) || continuousRetryHTTPSelected(policy, status, body) || general != 0 || rate != 0 {
			t.Fatalf("protocol rejection retried: %d", status)
		}
		h.applyCooldownForModel(account, status, body, nil, "gpt-6-sol-excel")
		if account.RuntimeStatus() != before || account.Disabled != 0 {
			t.Fatal("protocol rejection penalized account")
		}
	}
	payload, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{"error": errBody}})
	outcome := classifyResponseFailedOutcome(payload)
	if outcome.penalize || !outcome.requestScoped || continuousRetryStreamSelected(outcome, payload, "response.failed", policy) || continuousRetryStreamFailureSelected(outcome, payload, "response.failed", policy) {
		t.Fatal("stream rejection retried or penalized")
	}
	h.reportStreamOutcomeFailure(account, outcome, 0)
	for _, e := range []map[string]string{{"code": "invalid_tool_call", "message": "other error"}, {"code": "incomplete_tool_stream", "message": "Tool relay rejected: incomplete_tool_stream"}, {"code": "unauthorized", "message": "expired token"}} {
		raw, _ := json.Marshal(map[string]any{"error": e})
		if isExcelToolContractError(raw) {
			t.Fatal("unrelated error misclassified")
		}
	}
}

func TestExcelDedicatedCompactRejectedBeforeTransport(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"model": "gpt-6-sol-excel"})
	response, err := ExecuteCompactRequest(context.Background(), nil, body, "", "", "", nil, nil)
	if response != nil || StatusCodeFromError(err) != http.StatusBadRequest || IsRetryableError(err) {
		t.Fatalf("unexpected compact result: %v %v", response, err)
	}
	filter := accountFilterForCompactResponsesModelWithOriginal("gpt-6-sol-excel", "gpt-6-sol-excel", true)
	if filter(nil) || filter(&auth.Account{DBID: 42}) {
		t.Fatal("Excel admitted to dedicated compact executor")
	}
}
