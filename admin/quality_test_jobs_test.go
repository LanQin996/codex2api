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

func TestQualityTestJobsSurviveRequestCancellationAndLimitThreeAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html><svg>\"}\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"</svg></html>\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	h := &Handler{db: db, store: store}
	parent, stop := context.WithCancel(context.Background())
	h.StartQualityTests(parent)
	defer func() { stop(); h.WaitQualityTests() }()
	router := gin.New()
	router.POST("/accounts/:id/quality-test", h.CreateQualityTestJob)
	other := &Handler{db: db, store: store}
	router.POST("/quality-tests/:id/cancel", other.CancelQualityTest)
	var ids []int64
	for i := 0; i < 4; i++ {
		id, err := db.InsertAccountWithCredentials(context.Background(), fmt.Sprintf("account-%d", i), map[string]interface{}{"upstream_type": "openai_responses", "api_key": "test-key", "models": []string{"gpt-4o-mini"}, "base_url": upstream.URL}, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		store.AddAccount(&auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "test-key", BaseURL: upstream.URL, Models: []string{"gpt-4o-mini"}, PlanType: "pro", Status: auth.StatusReady})
	}
	create := func(id int64) (*httptest.ResponseRecorder, database.QualityTestJob) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/quality-test", id), strings.NewReader(`{"model":"gpt-4o-mini","reasoning_effort":"high","prompt":"HTML"}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		cancel()
		var body struct {
			Job database.QualityTestJob `json:"job"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		return response, body.Job
	}
	var jobs []database.QualityTestJob
	for _, id := range ids[:3] {
		response, job := create(id)
		if response.Code != http.StatusAccepted {
			t.Fatalf("create=%d %s", response.Code, response.Body.String())
		}
		jobs = append(jobs, job)
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("detached job did not reach upstream")
		}
	}
	response4, queued := create(ids[3])
	if response4.Code != http.StatusAccepted || queued.Status != "queued" {
		t.Fatalf("fourth task not queued: %d %+v", response4.Code, queued)
	}
	if response, _ := create(ids[0]); response.Code != http.StatusConflict {
		t.Fatalf("duplicate account accepted: %d", response.Code)
	}
	waitStatus := func(id int64, want string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			job, err := db.GetQualityTestJob(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if job.Status == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("job %d did not reach %s", id, want)
	}
	// All submitting HTTP contexts are cancelled; records still run and keep partial output.
	time.Sleep(1100 * time.Millisecond)
	for _, job := range jobs {
		stored, err := db.GetQualityTestJob(context.Background(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != "running" || stored.Output != "<html><svg>" {
			t.Fatalf("request cancellation stopped job/progress: %+v", stored)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/quality-tests/%d/cancel", jobs[0].ID), nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	waitStatus(jobs[0].ID, "stopped")
	waitStatus(queued.ID, "running")
	close(release)
	for _, job := range jobs[1:] {
		waitStatus(job.ID, "completed")
		stored, _ := db.GetQualityTestJob(context.Background(), job.ID)
		if stored.Output != "<html><svg></svg></html>" || stored.ReasoningEffort != "high" || stored.AccountName == "" || stored.CompletedAt == nil {
			t.Fatalf("missing persisted result: %+v", stored)
		}
	}
}
