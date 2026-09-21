package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestListAccountsExposesAndClearsModelMismatchMarks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, codexIDs, _ := newPagedAccountsHandler(t)
	ctx := context.Background()
	if err := handler.db.RecordModelMismatch(ctx, codexIDs[1], "gpt-5.4", "gpt-5.4-mini", 4, time.Now()); err != nil {
		t.Fatalf("RecordModelMismatch: %v", err)
	}

	// 分页视图与旧全量视图都要带上标记（列表可见，不属于 detail-only 字段）。
	for _, target := range []string{
		"/api/admin/accounts?view=page&channel=codex&page=1&page_size=10",
		"/api/admin/accounts?channel=codex",
	} {
		recorder := invokeListAccounts(t, handler, target)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", target, recorder.Code, recorder.Body.String())
		}
		var resp accountsResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %s: %v", target, err)
		}
		marked := 0
		for _, account := range resp.Accounts {
			if len(account.ModelMismatches) == 0 {
				continue
			}
			marked++
			got := account.ModelMismatches[0]
			if account.ID != codexIDs[1] || got.Model != "gpt-5.4" || got.UpstreamModel != "gpt-5.4-mini" || got.HitCount != 4 || got.LastSeenAt == "" {
				t.Fatalf("%s 账号 %d 标记 = %+v", target, account.ID, got)
			}
		}
		if marked != 1 {
			t.Fatalf("%s 带标记的账号数 = %d, want 1", target, marked)
		}
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodDelete, "/api/admin/accounts/x/model-mismatches", nil)
	ginContext.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(codexIDs[1], 10)}}
	handler.ClearAccountModelMismatches(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("clear status = %d: %s", recorder.Code, recorder.Body.String())
	}
	rows, err := handler.db.ListModelMismatches(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("清除后 rows = %+v, err = %v", rows, err)
	}
}
