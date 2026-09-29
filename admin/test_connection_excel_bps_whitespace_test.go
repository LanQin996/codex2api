package admin

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestExcelBPSQualityContentPreservesSVGTokenBoundaries(t *testing.T) {
	const html = "<svg viewBox=\"0 0 1000 600\"><path d=\"M0 24 L12 7\"/></svg>\n\t<script>const x = 1;</script>"
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	recorder := newCodexTestRecorder(nil, "test", nil, time.Now())
	hasContent := false
	emit := excelBPSTestContentEmitter(ctx, recorder, &hasContent, true)
	emit("")
	if hasContent {
		t.Fatal("empty delta marked content received")
	}
	// Single-character deltas deliberately put every separator in its own chunk.
	for _, char := range html {
		emit(string(char))
	}
	var output strings.Builder
	for _, event := range decodeCodexTestEvents(t, response.Body.String()) {
		if event.Type == "content" {
			output.WriteString(event.Text)
		}
	}
	if output.String() != html {
		t.Fatalf("corrupted markup: %q", output.String())
	}
	if !hasContent || recorder.details.FirstContentMS == nil {
		t.Fatal("missing content diagnostics")
	}
}

func TestExcelBPSConnectionProbeStillIgnoresBlankContent(t *testing.T) {
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	recorder := newCodexTestRecorder(nil, "test", nil, time.Now())
	hasContent := false
	emit := excelBPSTestContentEmitter(ctx, recorder, &hasContent, false)
	emit(" \n\t")
	if hasContent || response.Body.Len() != 0 {
		t.Fatal("blank probe response counted as content")
	}
	emit(" hello ")
	events := decodeCodexTestEvents(t, response.Body.String())
	if len(events) != 1 || events[0].Text != " hello " {
		t.Fatal("probe text altered")
	}
}
