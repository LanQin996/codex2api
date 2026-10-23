package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCORSMiddlewareAllowsCodex2APIAffinityHeaderPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handlerCalled := false
	router := gin.New()
	router.Use(CORSMiddleware())
	router.OPTIONS("/v1/responses", func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
	request.Header.Set("Origin", "https://client.example")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "authorization, x-codex2api-affinity-key")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if handlerCalled {
		t.Fatal("OPTIONS preflight reached the downstream handler")
	}

	allowedHeaders := strings.ToLower(recorder.Header().Get("Access-Control-Allow-Headers"))
	if !strings.Contains(allowedHeaders, strings.ToLower("X-Codex2API-Affinity-Key")) {
		t.Fatalf("Access-Control-Allow-Headers = %q, missing X-Codex2API-Affinity-Key", recorder.Header().Get("Access-Control-Allow-Headers"))
	}
}

func TestBodyCacheSkipsLargeImportUpload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(BodyCacheMiddleware())
	rawBodyCached := false
	router.POST("/api/admin/accounts/import", func(c *gin.Context) {
		_, rawBodyCached = c.Get("raw_body")
		c.Status(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", strings.NewReader("not read before auth"))
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if rawBodyCached {
		t.Fatal("large import body must not be cached/read before authentication")
	}
}
