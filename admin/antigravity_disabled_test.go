package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestAntigravityAdminCatalogRetainsDisabledWithoutInventedQuota(t *testing.T) {
	disabled := "gemini-4-argon-disabled"
	otherDisabled := "future_disabled_text"
	models := []string{disabled, otherDisabled, "gemini-3.8-flash-tiered", "tab_hidden", "gemini-2.5-pro"}
	want := []string{"gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-3.8-flash-high", otherDisabled, disabled}
	if got := antigravityPublishedModels(models); !reflect.DeepEqual(got, want) {
		t.Fatalf("admin models = %v, want %v", got, want)
	}
	raw := auth.AntigravityQuotaSnapshot{
		CatalogModelIDs:  append(append([]string{}, models...), "internal-model", "internal-disabled-model"),
		InternalModelIDs: []string{"internal-model", "internal-disabled-model"},
		Models: []auth.AntigravityModelQuota{
			{ModelID: "gemini-3.8-flash-tiered", RemainingPercent: 55},
			{ModelID: "internal-model", RemainingPercent: 90},
			{ModelID: "internal-disabled-model", RemainingPercent: 90},
		},
	}
	quota := antigravityPublishedQuota(raw)
	if !reflect.DeepEqual(quota.CatalogModelIDs, want) || len(quota.Models) != 3 || len(quota.InternalModelIDs) != 0 {
		t.Fatalf("admin quota projection = %+v", quota)
	}
	for _, item := range quota.Models {
		if item.RemainingPercent != 55 || strings.Contains(item.ModelID, "disabled") || strings.Contains(item.ModelID, "internal") {
			t.Fatalf("fabricated or internal quota: %+v", item)
		}
	}
	quotaJSON, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	row := &database.AccountRow{ID: 1, Credentials: map[string]any{"upstream_type": auth.UpstreamAntigravity, "access_token": "token", "models": models, "antigravity_quota": string(quotaJSON)}}
	state := antigravityStateFromRow(row)
	if !reflect.DeepEqual(state.Catalog.Models, want) || state.Quota == nil || !reflect.DeepEqual(state.Quota.CatalogModelIDs, want) {
		t.Fatalf("state omitted disabled catalog: %+v", state)
	}
}

func TestAntigravityAdminSyncReplacesDisabledCatalog(t *testing.T) {
	handler, db, id := newAntigravityStateTestHandler(t, map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "token", "project_id": "project", "models": []string{"gemini-4-argon-disabled"},
	})
	handler.antigravitySyncAccount = func(ctx context.Context, accountID int64) antigravityRefreshItem {
		if err := db.UpdateCredentials(ctx, accountID, map[string]any{"models": []string{"gemini-4-argon-high"}}); err != nil {
			t.Fatal(err)
		}
		return antigravityRefreshItem{ID: accountID, OK: true}
	}
	rec := callAntigravityAdminHandler(t, http.MethodPost, "/sync", id, handler.SyncAntigravityAccountState)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "disabled") || !strings.Contains(rec.Body.String(), "gemini-4-argon-high") {
		t.Fatalf("refresh retained stale disabled model: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAntigravityConnectionTestDisabledRequiresExplicitCatalogSelection(t *testing.T) {
	disabled := "gemini-4-argon-disabled"
	account := newAntigravityConnectionTestAccount()
	account.Models = []string{disabled, "gemini-3.8-flash-tiered"}
	if got, err := antigravityConnectionTestModel(account, disabled); err != nil || got != disabled {
		t.Fatalf("explicit disabled model = %q, %v", got, err)
	}
	if got, err := antigravityConnectionTestModel(account, "", disabled); err != nil || got != "gemini-3.8-flash-low" {
		t.Fatalf("automatic model = %q, %v; disabled default must not win", got, err)
	}
	if _, err := antigravityConnectionTestModel(account, "other-disabled-model"); err == nil {
		t.Fatal("disabled name outside the account catalog was accepted")
	}
	account.Models = []string{disabled}
	if _, err := antigravityConnectionTestModel(account, ""); err == nil {
		t.Fatal("automatic test selected a disabled-only catalog")
	}
	account.Models = []string{"future_disabled_text"}
	if got, err := antigravityConnectionTestModel(account, "future_disabled_text"); err != nil || got != "future_disabled_text" {
		t.Fatalf("non-Argon disabled model = %q, %v", got, err)
	}
}

func TestAntigravityDisabledTestExecutorRetainsCatalogAndCredentialGates(t *testing.T) {
	account := newAntigravityConnectionTestAccount()
	handler := &Handler{}
	handler.antigravityCapabilityProbe = func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error) {
		t.Fatal("executor ran for a disabled model outside the current catalog")
		return nil, nil
	}
	if _, err := handler.executeAntigravityConnectionTest(context.Background(), account, "gemini-4-argon-disabled", []byte(`{"input":"hello"}`), "", false); err == nil {
		t.Fatal("disabled model bypassed the current account catalog")
	}
	handler.antigravityCapabilityProbe = nil
	account.Models = []string{"gemini-4-argon-disabled"}
	account.AccessToken = ""
	if _, err := handler.executeAntigravityConnectionTest(context.Background(), account, "gemini-4-argon-disabled", []byte(`{"input":"hello"}`), "", false); err == nil {
		t.Fatal("disabled test admitted an account without credentials")
	}
	account.RefreshToken = "test-refresh-token"
	if _, err := handler.executeAntigravityConnectionTest(context.Background(), account, "gemini-4-argon-disabled", []byte(`{"input":"hello"}`), "", false); err == nil || !strings.Contains(err.Error(), "no project_id or access token") {
		t.Fatalf("disabled test bypassed credential preflight: %v", err)
	}
}

func TestAntigravityCapabilityProbeDoesNotSelectDisabledModel(t *testing.T) {
	handler, _, id := newAntigravityStateTestHandler(t, map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "token", "project_id": "project", "models": []string{"gemini-4-argon-disabled"},
	})
	handler.antigravityCapabilityProbe = func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error) {
		t.Fatal("automatic capability probe selected a disabled model")
		return nil, nil
	}
	rec := callAntigravityAdminHandler(t, http.MethodPost, "/probe", id, handler.ProbeAntigravityAccountCapabilities)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no probe model") {
		t.Fatalf("disabled capability probe = %d %s", rec.Code, rec.Body.String())
	}
}

func TestConnectionAntigravityDisabledModelPreservesAccountOnModelRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			store := auth.NewStore(nil, nil, nil)
			t.Cleanup(store.Stop)
			account := newAntigravityConnectionTestAccount()
			account.Models = []string{"gemini-4-argon-disabled"}
			store.AddAccount(account)
			handler := &Handler{store: store}
			calls := 0
			handler.antigravityCapabilityProbe = func(_ context.Context, gotAccount *auth.Account, model string, _ []byte, stream bool, _ string) (*http.Response, error) {
				calls++
				if gotAccount != account || model != account.Models[0] || !stream {
					t.Fatalf("wrong manual probe account/model: %v %q %v", gotAccount == account, model, stream)
				}
				body := `{"error":{"status":"NOT_FOUND","message":"model disabled"}}`
				if status == http.StatusOK {
					body = antigravityTestSSEBody("ok")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			router := gin.New()
			router.GET("/api/admin/accounts/:id/test", handler.TestConnection)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/accounts/7/test?model=gemini-4-argon-disabled", nil))
			if calls != 1 || rec.Code != http.StatusOK {
				t.Fatalf("disabled test blocked: calls=%d status=%d %s", calls, rec.Code, rec.Body.String())
			}
			if account.RuntimeStatus() == "error" || account.RuntimeStatus() == "unauthorized" {
				t.Fatalf("single disabled model rejection penalized account: %s", account.RuntimeStatus())
			}
		})
	}
}

func TestAntigravityDisabledOnlyBatchDoesNotPenalizeAccount(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	account := newAntigravityConnectionTestAccount()
	account.Models = []string{"gemini-4-argon-disabled"}
	store.AddAccount(account)
	handler := &Handler{store: store}
	handler.antigravityCapabilityProbe = func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error) {
		t.Fatal("automatic batch tested a disabled model")
		return nil, nil
	}
	if status, _ := handler.runSingleBatchTest(context.Background(), account); status != "failed" {
		t.Fatalf("disabled-only batch status = %q", status)
	}
	if account.RuntimeStatus() == "error" {
		t.Fatal("missing enabled model marked the whole account as error")
	}
}
