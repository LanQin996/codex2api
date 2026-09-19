package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestQualityTestBatchPartialAdmissionReplayOptionsAndRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	// Deliberately do not start dispatch: this test inspects admission without upstream requests.
	h := &Handler{db: db, store: store, qualityTestContext: context.Background()}
	router := gin.New()
	router.POST("/batches", h.CreateQualityTestBatch)
	router.POST("/options", h.QualityTestBatchOptions)
	router.GET("/batches/:id", h.GetQualityTestBatch)
	router.POST("/batches/:id/cancel", h.CancelQualityTestBatch)
	router.POST("/jobs/:id/retry", h.RetryQualityTest)
	ids := []int64{}
	for i := 0; i < 2; i++ {
		id, err := db.InsertAccountWithCredentials(context.Background(), fmt.Sprintf("batch-%d", i), map[string]interface{}{"upstream_type": "openai_responses", "api_key": "fixture", "models": []string{"gpt-4o-mini"}, "base_url": "http://127.0.0.1:1"}, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		store.AddAccount(&auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "fixture", BaseURL: "http://127.0.0.1:1", Models: []string{"gpt-4o-mini"}, Status: auth.StatusReady})
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	opts := call("POST", "/options", fmt.Sprintf("{\"account_ids\":[%d,%d]}", ids[0], ids[1]))
	if opts.Code != 200 || !strings.Contains(opts.Body.String(), "gpt-4o-mini") {
		t.Fatalf("options %d %s", opts.Code, opts.Body.String())
	}
	body := fmt.Sprintf("{\"request_id\":\"batch-request-0001\",\"account_ids\":[%d,%d,999999],\"channel\":\"codex\",\"model\":\"gpt-4o-mini\",\"reasoning_effort\":\"high\",\"prompt\":\"original SVG prompt\",\"preset_key\":\"pelican\",\"preset_name\":\"Original preset\"}", ids[0], ids[1])
	response := call("POST", "/batches", body)
	if response.Code != http.StatusAccepted {
		t.Fatalf("create %d %s", response.Code, response.Body.String())
	}
	var batch database.QualityTestBatch
	if err := json.Unmarshal(response.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.JobIDs) != 2 || len(batch.Rejected) != 1 {
		t.Fatalf("partial %+v", batch)
	}
	response = call("POST", "/batches", body)
	var replay database.QualityTestBatch
	json.Unmarshal(response.Body.Bytes(), &replay)
	if response.Code != 202 || len(replay.JobIDs) != 2 || replay.JobIDs[0] != batch.JobIDs[0] {
		t.Fatalf("replay %d %s", response.Code, response.Body.String())
	}
	response = call("POST", "/batches", strings.Replace(body, "original SVG prompt", "different", 1))
	if response.Code != 409 {
		t.Fatalf("idempotency conflict %d", response.Code)
	}
	response = call("POST", "/batches/batch-request-0001/cancel", "")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = call("POST", fmt.Sprintf("/jobs/%d/retry", batch.JobIDs[0]), "")
	if response.Code != 202 {
		t.Fatalf("retry %d %s", response.Code, response.Body.String())
	}
	var retry struct {
		Job database.QualityTestJob `json:"job"`
	}
	json.Unmarshal(response.Body.Bytes(), &retry)
	if retry.Job.Prompt != "original SVG prompt" || retry.Job.Model != "gpt-4o-mini" || retry.Job.PresetRef != "pelican" || retry.Job.PresetName != "Original preset" || retry.Job.Status != "queued" {
		t.Fatalf("snapshot retry %+v", retry.Job)
	}
	response = call("POST", fmt.Sprintf("/jobs/%d/retry", batch.JobIDs[0]), "")
	if response.Code != 409 {
		t.Fatalf("duplicate retry %d", response.Code)
	}
}

func TestQualityTestFinalDurationExcludesQueueWait(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db, store: auth.NewStore(db, nil, nil)}
	ctx := context.Background()
	job, err := db.CreateQualityTestJob(ctx, database.QualityTestJob{AccountID: 999999, Model: "gpt-4o-mini", Prompt: "HTML"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	job.CreatedAt = start.Add(-time.Hour)
	job.StartedAt = &start
	h.runQualityTestJob(ctx, *job, qualityTestRequest{Model: job.Model, Prompt: job.Prompt})
	stored, err := db.GetQualityTestJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "error" || stored.DurationMS > 3000 {
		t.Fatalf("queue wait counted or missing account not finalized: %+v", stored)
	}
}
