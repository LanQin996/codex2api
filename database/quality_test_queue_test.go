package database

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestQualityTestPersistentQueueClaimsAndIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	jobs := []QualityTestJob{}
	for i := int64(1); i <= 30; i++ {
		jobs = append(jobs, QualityTestJob{AccountID: i, AccountName: "account", Channel: "codex", Model: "model", Prompt: "<svg/>"})
	}
	batch, err := db.EnqueueQualityTestBatch(ctx, "request-key", "fingerprint", jobs, nil)
	if err != nil || len(batch.JobIDs) != 30 {
		t.Fatalf("enqueue: %+v %v", batch, err)
	}
	replay, err := db.EnqueueQualityTestBatch(ctx, "request-key", "fingerprint", jobs, nil)
	if err != nil || len(replay.JobIDs) != 30 || replay.JobIDs[0] != batch.JobIDs[0] {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if _, err = db.EnqueueQualityTestBatch(ctx, "request-key", "different", jobs, nil); !errors.Is(err, ErrQualityTestIdempotency) {
		t.Fatalf("conflicting replay: %v", err)
	}
	duplicate, err := db.EnqueueQualityTestBatch(ctx, "other-request", "other", jobs[:1], nil)
	if err != nil || len(duplicate.JobIDs) != 0 || len(duplicate.Rejected) != 1 {
		t.Fatalf("busy: %+v %v", duplicate, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Waiting must not consume the execution timeout or reported duration.
	old := time.Now().Add(-time.Hour)
	if _, err = db.conn.ExecContext(ctx, "UPDATE quality_test_jobs SET created_at=$1", db.timeArg(old)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var claimed []QualityTestJob
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := db.ClaimQualityTest(ctx)
			if err != nil {
				t.Error(err)
			}
			if job != nil {
				mu.Lock()
				claimed = append(claimed, *job)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != 3 {
		t.Fatalf("claimed %d, want 3", len(claimed))
	}
	for _, job := range claimed {
		if job.ID > batch.JobIDs[2] || job.StartedAt == nil || job.DurationMS > 3000 || time.Until(job.DeadlineAt) < 9*time.Minute {
			t.Fatalf("bad claim or timing: %+v", job)
		}
	}
	if err = db.CancelQualityTest(ctx, batch.JobIDs[3]); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := db.GetQualityTestJob(ctx, batch.JobIDs[3])
	if cancelled.Status != "stopped" || cancelled.StartedAt != nil || cancelled.CompletedAt == nil {
		t.Fatalf("queued cancellation: %+v", cancelled)
	}
	if job, err := db.ClaimQualityTest(ctx); err != nil || job != nil {
		t.Fatalf("over capacity: %+v %v", job, err)
	}
	finished := claimed[0]
	finished.Status = "completed"
	if err = db.FinishQualityTest(ctx, finished); err != nil {
		t.Fatal(err)
	}
	next, err := db.ClaimQualityTest(ctx)
	if err != nil || next == nil || next.ID != batch.JobIDs[4] {
		t.Fatalf("FIFO after cancelled: %+v %v", next, err)
	}
	if err = db.CancelQualityTestBatch(ctx, batch.ID); err != nil {
		t.Fatal(err)
	}
	summary, err := db.GetQualityTestBatch(ctx, batch.ID)
	if err != nil || summary.Counts["queued"] != 0 || summary.Counts["cancelling"] != 3 || summary.Counts["completed"] != 1 || summary.Counts["stopped"] != 26 {
		t.Fatalf("batch cancel: %+v %v", summary, err)
	}
	if err = db.ExpireQualityTests(ctx, time.Now().Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if job, err := db.ClaimQualityTest(ctx); err != nil || job != nil {
		t.Fatalf("interrupted tasks must not replay: %+v %v", job, err)
	}
}
func TestQualityTestLatestFiltersBeforeDedupAndPagination(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "latest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	accountIDs := []int64{}
	for i := 0; i < 25; i++ {
		id, err := db.InsertAccount(ctx, "owned account", "fixture-token", "")
		if err != nil {
			t.Fatal(err)
		}
		accountIDs = append(accountIDs, id)
	}
	for round := 0; round < 2; round++ {
		for _, id := range accountIDs {
			model := "old"
			if round == 1 {
				model = "new"
			}
			job, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: id, Channel: "codex", Model: model})
			if err != nil {
				t.Fatal(err)
			}
			job.Status = "completed"
			if err = db.FinishQualityTest(ctx, *job); err != nil {
				t.Fatal(err)
			}
		}
	}
	page, err := db.ListQualityTests(ctx, 2, 20, QualityTestFilter{Latest: true, Channel: "codex", Model: "old"})
	if err != nil || page.Total != 25 || len(page.Jobs) != 5 {
		t.Fatalf("latest page: %+v %v", page, err)
	}
	for _, job := range page.Jobs {
		if job.Model != "old" {
			t.Fatalf("filter after dedup: %+v", job)
		}
	}
	page, err = db.ListQualityTests(ctx, 1, 20, QualityTestFilter{Latest: true, Channel: "codex"})
	if err != nil || page.Total != 25 || page.Jobs[0].Model != "new" {
		t.Fatalf("latest: %+v %v", page, err)
	}
	page, err = db.ListQualityTests(ctx, 1, 20, QualityTestFilter{})
	if err != nil || page.Total != 50 {
		t.Fatalf("history: %+v %v", page, err)
	}
	if err := db.SoftDeleteAccount(ctx, accountIDs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, "DELETE FROM accounts WHERE id=$1", accountIDs[1]); err != nil {
		t.Fatal(err)
	}
	page, err = db.ListQualityTests(ctx, 2, 20, QualityTestFilter{Latest: true})
	if err != nil || page.Total != 23 || len(page.Jobs) != 3 || len(page.Facets.Accounts) != 23 {
		t.Fatalf("deleted accounts must not affect recent cards, count or choices: %+v %v", page, err)
	}
	for _, job := range page.Jobs {
		if job.AccountID == accountIDs[0] || job.AccountID == accountIDs[1] {
			t.Fatalf("deleted account visible: %+v", job)
		}
	}
	page, err = db.ListQualityTests(ctx, 1, 20, QualityTestFilter{})
	if err != nil || page.Total != 50 || len(page.Facets.Accounts) != 25 {
		t.Fatalf("full history must remain available: %+v %v", page, err)
	}
}

func TestQualityTestQueueMigrationPreservesLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Recreate the pre-queue schema, including its original state constraint.
	for _, q := range []string{
		"DROP TABLE quality_test_jobs",
		"CREATE TABLE quality_test_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT,slot INTEGER UNIQUE CHECK(slot BETWEEN 1 AND 3),account_id BIGINT NOT NULL,account_name TEXT NOT NULL,plan_type TEXT NOT NULL,channel TEXT NOT NULL,model TEXT NOT NULL,reasoning_effort TEXT NOT NULL,prompt TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'running',output TEXT NOT NULL DEFAULT '',preset_kind TEXT NOT NULL DEFAULT '',preset_ref TEXT NOT NULL DEFAULT '',preset_name TEXT NOT NULL DEFAULT '',metrics_json TEXT NOT NULL DEFAULT '{}',error TEXT NOT NULL DEFAULT '',created_at TIMESTAMP NOT NULL,updated_at TIMESTAMP NOT NULL,deadline_at TIMESTAMP NOT NULL,completed_at TIMESTAMP,CHECK ((slot IS NOT NULL AND status IN ('running','cancelling')) OR (slot IS NULL AND status IN ('completed','error','stopped','interrupted'))))",
	} {
		if _, err = db.conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	now := db.timeArg(time.Now())
	_, err = db.conn.ExecContext(ctx, "INSERT INTO quality_test_jobs(id,account_id,account_name,plan_type,channel,model,reasoning_effort,prompt,status,output,created_at,updated_at,deadline_at,completed_at) VALUES(77,9,'legacy name','pro','codex','model','high','旧提示词','completed','<svg>旧结果</svg>',$1,$1,$1,$1)", now)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	job, err := db.GetQualityTestJob(ctx, 77)
	if err != nil || job.Output != "<svg>旧结果</svg>" || job.Prompt != "旧提示词" || job.AccountName != "legacy name" || job.StartedAt != nil {
		t.Fatalf("legacy migration: %+v %v", job, err)
	}
	batch, err := db.EnqueueQualityTestBatch(ctx, "after-migration", "migration", []QualityTestJob{{AccountID: 9}}, nil)
	if err != nil || len(batch.JobIDs) != 1 || batch.JobIDs[0] <= 77 {
		t.Fatalf("queue after migration: %+v %v", batch, err)
	}
}

func TestQualityTestQueueClaimsAcrossDatabaseInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replicas.db")
	a, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	jobs := []QualityTestJob{}
	for i := int64(1); i <= 12; i++ {
		jobs = append(jobs, QualityTestJob{AccountID: i})
	}
	if _, err = a.EnqueueQualityTestBatch(ctx, "replica-batch", "same", jobs, nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[int64]bool{}
	var count int
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := a
			if i%2 == 1 {
				db = b
			}
			job, err := db.ClaimQualityTest(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if job != nil {
				mu.Lock()
				defer mu.Unlock()
				count++
				claimed[job.ID] = true
			}
		}(i)
	}
	wg.Wait()
	if count != 3 || len(claimed) != 3 {
		t.Fatalf("replicas claimed %d tasks with %d distinct IDs", count, len(claimed))
	}
}
