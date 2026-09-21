package database

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AccountModelMismatchRow 记录「某账号的某个模型，上游响应体回显的 model 与实际发给
// 上游的 model 不一致」。纯观测数据：只在管理端账号列表展示，不参与调度、冷却或计费。
type AccountModelMismatchRow struct {
	AccountID     int64
	Model         string // 实际发给上游的模型（小写）
	UpstreamModel string // 上游最近一次回显的模型
	HitCount      int64
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
}

// RecordModelMismatch 累加一条模型不一致标记；hits 为本次合并写入的命中次数。
func (db *DB) RecordModelMismatch(ctx context.Context, accountID int64, model, upstreamModel string, hits int64, seenAt time.Time) error {
	model = clampUsageLogText(strings.ToLower(strings.TrimSpace(model)), usageLogTextMaxLen)
	upstreamModel = clampUsageLogText(strings.TrimSpace(upstreamModel), usageLogTextMaxLen)
	if db == nil || accountID <= 0 || model == "" || upstreamModel == "" {
		return nil
	}
	if hits <= 0 {
		hits = 1
	}
	if seenAt.IsZero() {
		seenAt = time.Now()
	}
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO account_model_mismatches (account_id, model, upstream_model, hit_count, first_seen_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT(account_id, model) DO UPDATE SET
			upstream_model = excluded.upstream_model,
			hit_count = account_model_mismatches.hit_count + excluded.hit_count,
			last_seen_at = excluded.last_seen_at
	`, accountID, model, upstreamModel, hits, db.timeArg(seenAt), db.timeArg(seenAt))
	return err
}

// ListModelMismatches 返回全部模型不一致标记（旧全量账号列表用；该表通常很小）。
func (db *DB) ListModelMismatches(ctx context.Context) ([]*AccountModelMismatchRow, error) {
	return db.queryModelMismatches(ctx, "", nil)
}

// ListModelMismatchesForAccounts 只加载当前页账号的标记，避免分页列表扫全表。
func (db *DB) ListModelMismatchesForAccounts(ctx context.Context, accountIDs []int64) ([]*AccountModelMismatchRow, error) {
	accountIDs = positiveUniqueIDs(accountIDs)
	if len(accountIDs) == 0 {
		return nil, nil
	}
	args := make([]interface{}, 0, len(accountIDs))
	placeholders := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		args = append(args, accountID)
		if db.isSQLite() {
			placeholders = append(placeholders, "?")
		} else {
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
	}
	return db.queryModelMismatches(ctx, "WHERE account_id IN ("+strings.Join(placeholders, ",")+")", args)
}

func (db *DB) queryModelMismatches(ctx context.Context, where string, args []interface{}) ([]*AccountModelMismatchRow, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT account_id, model, COALESCE(upstream_model, ''), COALESCE(hit_count, 0), first_seen_at, last_seen_at
		FROM account_model_mismatches
		`+where+`
		ORDER BY account_id, model
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("查询模型不一致标记失败: %w", err)
	}
	defer rows.Close()

	var result []*AccountModelMismatchRow
	for rows.Next() {
		row := &AccountModelMismatchRow{}
		var firstRaw, lastRaw interface{}
		if err := rows.Scan(&row.AccountID, &row.Model, &row.UpstreamModel, &row.HitCount, &firstRaw, &lastRaw); err != nil {
			return nil, err
		}
		// 时间只用于展示，解析失败不值得让整个账号列表报错。
		row.FirstSeenAt, _ = parseDBTimeValue(firstRaw)
		row.LastSeenAt, _ = parseDBTimeValue(lastRaw)
		result = append(result, row)
	}
	return result, rows.Err()
}

// ClearModelMismatches 清除某账号的全部模型不一致标记，返回清除条数。
func (db *DB) ClearModelMismatches(ctx context.Context, accountID int64) (int64, error) {
	if db == nil || accountID <= 0 {
		return 0, nil
	}
	res, err := db.conn.ExecContext(ctx, `DELETE FROM account_model_mismatches WHERE account_id = $1`, accountID)
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()
	return affected, nil
}
