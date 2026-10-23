package admin

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPaginationParamsCapsPage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?page=999999999&page_size=100", nil)
	page, pageSize := paginationParams(c, 20)
	if page != maxAdminPage || pageSize != 100 {
		t.Fatalf("paginationParams = (%d, %d), want (%d, 100)", page, pageSize, maxAdminPage)
	}
}

func TestPublicUsagePaginationRejectsOversizedPage(t *testing.T) {
	if _, _, err := parsePublicAPIKeyUsageLogPagination("999999999", "100"); err == nil {
		t.Fatal("oversized page should be rejected")
	}
	page, size, err := parsePublicAPIKeyUsageLogPagination("2", "100")
	if err != nil || page != 2 || size != 100 {
		t.Fatalf("normal page = (%d, %d, %v)", page, size, err)
	}
}
