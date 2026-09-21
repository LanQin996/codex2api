package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestModelMismatchRecordListAndClear(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	first := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := db.RecordModelMismatch(ctx, 7, " GPT-5.4 ", "gpt-5.4-mini", 2, first); err != nil {
		t.Fatalf("RecordModelMismatch 返回错误: %v", err)
	}
	// 同一账号同一模型再次命中：累加次数，上游模型取最近一次。
	if err := db.RecordModelMismatch(ctx, 7, "gpt-5.4", "gpt-4o", 3, first.Add(30*time.Minute)); err != nil {
		t.Fatalf("RecordModelMismatch 返回错误: %v", err)
	}
	if err := db.RecordModelMismatch(ctx, 9, "gpt-5.4", "gpt-4o", 1, first); err != nil {
		t.Fatalf("RecordModelMismatch 返回错误: %v", err)
	}
	// 无效输入静默忽略。
	if err := db.RecordModelMismatch(ctx, 0, "gpt-5.4", "gpt-4o", 1, first); err != nil {
		t.Fatalf("RecordModelMismatch(无效账号) 返回错误: %v", err)
	}

	rows, err := db.ListModelMismatchesForAccounts(ctx, []int64{7})
	if err != nil {
		t.Fatalf("ListModelMismatchesForAccounts 返回错误: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Model != "gpt-5.4" || got.UpstreamModel != "gpt-4o" || got.HitCount != 5 {
		t.Fatalf("row = %+v, want model=gpt-5.4 upstream=gpt-4o hits=5", got)
	}
	if !got.FirstSeenAt.Equal(first) || !got.LastSeenAt.Equal(first.Add(30*time.Minute)) {
		t.Fatalf("first/last = %v / %v, want %v / %v", got.FirstSeenAt, got.LastSeenAt, first, first.Add(30*time.Minute))
	}

	all, err := db.ListModelMismatches(ctx)
	if err != nil {
		t.Fatalf("ListModelMismatches 返回错误: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}

	cleared, err := db.ClearModelMismatches(ctx, 7)
	if err != nil || cleared != 1 {
		t.Fatalf("ClearModelMismatches = %d, %v, want 1, nil", cleared, err)
	}
	if rows, _ := db.ListModelMismatchesForAccounts(ctx, []int64{7}); len(rows) != 0 {
		t.Fatalf("清除后仍有 %d 条标记", len(rows))
	}
}

func TestUsageLogsPersistUpstreamModel(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:     1,
		Endpoint:      "/v1/responses",
		Model:         "gpt-5.4",
		UpstreamModel: "gpt-5.4-mini",
		StatusCode:    200,
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 || logs[0].UpstreamModel != "gpt-5.4-mini" {
		t.Fatalf("logs = %+v, want 1 条且 UpstreamModel=gpt-5.4-mini", logs)
	}
}
