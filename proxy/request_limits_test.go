package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestResponsesCompactRejectsAmbiguousAndExcessiveInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var calls atomic.Int32
	upstream := newCompactionAffinityCompactRelay(t, &calls)
	defer upstream.Close()
	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "test", Models: []string{"gpt-4.1-direct"}}
	h := NewHandler(newCompactionAffinityStore(account), nil, nil, nil)
	many := strings.TrimSuffix(strings.Repeat(`{},`, maxResponsesInputItems+1), ",")
	for _, input := range []string{
		`"input":[],"input":[]`,
		`"input":[],"\u0069nput":[]`,
		`"input":[],"Input":[]`,
		`"Input":[]`,
		`"input":[` + many + `]`,
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-4.1-direct",`+input+`}`))
		h.ResponsesCompact(c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid compact input accepted: status=%d, body=%s", rec.Code, rec.Body.String())
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid input reached upstream %d times", got)
	}
}

func TestRejectAmbiguousJSONFieldNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	many := strings.TrimSuffix(strings.Repeat(`{},`, maxChatCompletionMessages+1), ",")
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}],"Messages":[` + many + `]}`,
		`{"Messages":[]}`,
		`{"messages":[],"mESSages":[]}`,
		`{"input":[],"Input":[]}`,
		`{"input":[],"\u0069nput":[]}`,
	} {
		field := "messages"
		if strings.Contains(body, `"input"`) {
			field = "input"
		}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		if !rejectDuplicateJSONField(c, []byte(body), field) || recorder.Code != 400 {
			t.Errorf("ambiguous %s accepted (body length %d)", field, len(body))
		}
	}
}

func TestRejectDuplicateJSONField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"messages":[],"messages":[{"a":1}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	if !rejectDuplicateJSONField(c, body, "messages") {
		t.Fatal("expected duplicate top-level field rejection")
	}
	if recorder.Code != 400 {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if rejectDuplicateJSONField(c, []byte(`{"messages":[]}`), "messages") {
		t.Fatal("unexpected rejection for a single field")
	}
}

func TestRejectExcessiveJSONArray(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"input":[{"a":1},{"a":2},{"a":3}]}`)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	if !rejectExcessiveJSONArray(c, body, "input", 2) {
		t.Fatal("expected rejection when item count exceeds limit")
	}
	if recorder.Code != 400 {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	if rejectExcessiveJSONArray(c, body, "input", 3) {
		t.Fatal("unexpected rejection at the limit")
	}
}
