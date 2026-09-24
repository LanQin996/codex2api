package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestExcelRouteSwitchPersistenceAndRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("EXCEL_BRIDGE_URL", "http://excel-bridge:8000")
	t.Setenv("EXCEL_BRIDGE_API_KEY", "01234567890123456789012345678901")
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: store}
	for _, mode := range []string{"oauth", "session_file", "off"} {
		result := patchAccountScheduler(t, h, id, `{"excel_route_mode":"`+mode+`"}`)
		if result.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", mode, result.Code, result.Body.String())
		}
		row, err := db.GetAccountByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if row.GetCredential(auth.ExcelRouteCredentialKey) != mode || store.FindByID(id).ExcelRouteMode() != mode {
			t.Fatal("database/runtime mismatch")
		}
		view := h.buildAccountResponse(row, nil, nil, nil, nil, true)
		if view.ExcelRouteMode != mode || !view.ExcelBridgeConfigured {
			t.Fatal("configuration not exposed")
		}
		if err := store.LoadAccountByID(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if store.FindByID(id).ExcelRouteMode() != mode {
			t.Fatal("reload lost configuration")
		}
	}
}

func TestExcelRouteSwitchRejectsInvalidAndUnconfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("EXCEL_BRIDGE_URL", "")
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	h := &Handler{db: db, store: auth.NewStore(db, nil, nil)}
	for _, body := range []string{`{"excel_route_mode":"oauth"}`, `{"excel_route_mode":"word"}`, `{"excel_route_mode":null}`} {
		if result := patchAccountScheduler(t, h, id, body); result.Code != http.StatusBadRequest {
			t.Fatalf("expected rejection: %s", result.Body.String())
		}
	}
	row, _ := db.GetAccountByID(context.Background(), id)
	if row.GetCredential(auth.ExcelRouteCredentialKey) != "" {
		t.Fatal("rejected request modified credentials")
	}
}
