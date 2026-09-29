package admin

import (
	"context"
	"encoding/json"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCredentialOperationsSettingsPersistenceAndValidation(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	router := gin.New()
	router.GET("/settings", h.GetCredentialOperationsSettings)
	router.PUT("/settings", h.UpdateCredentialOperationsSettings)
	read := func(want int) {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
		var s database.CredentialOperationsSettings
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil || s.Concurrency != want {
			t.Fatalf("settings: %d %s", w.Code, w.Body)
		}
	}
	read(2)
	for _, body := range []string{`{}`, `{"concurrency":0}`, `{"concurrency":9}`, `{"concurrency":-1}`, `{"concurrency":1.5}`, `{"concurrency":"3"}`} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("accepted invalid payload: %s", body)
		}
	}
	read(2)
	for _, n := range []int{1, 8, 3} {
		b, _ := json.Marshal(database.CredentialOperationsSettings{Concurrency: n})
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/settings", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		// A fresh handler reads the persisted database value, not process state.
		h = &Handler{db: db}
		s, err := h.db.GetCredentialOperationsSettings(context.Background())
		if err != nil || s.Concurrency != n {
			t.Fatalf("not persisted: %+v %v", s, err)
		}
		read(n)
	}
	if err := db.SetCredentialOperationsSettings(context.Background(), database.CredentialOperationsSettings{Concurrency: 99}); err == nil {
		t.Fatal("database accepted invalid concurrency")
	}
	read(3)
}
