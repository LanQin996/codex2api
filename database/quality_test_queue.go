package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SQLite requires a transactional table rebuild to change its legacy CHECK.
func (db *DB) ensureQualityTestQueueSchema(ctx context.Context) error {
	if db.isSQLite() {
		var ddl string
		if err := db.conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='quality_test_jobs'").Scan(&ddl); err != nil {
			return err
		}
		if !strings.Contains(ddl, "'queued'") {
			if err := db.withSQLiteWriteLock(ctx, func() error {
				tx, err := db.conn.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				ddl = strings.Replace(ddl, "quality_test_jobs", "quality_test_jobs_queue_migration", 1)
				ddl = strings.Replace(ddl, "'completed','error','stopped','interrupted'", "'queued','completed','error','stopped','interrupted'", 1)
				for _, q := range []string{ddl, "INSERT INTO quality_test_jobs_queue_migration SELECT * FROM quality_test_jobs", "DROP TABLE quality_test_jobs", "ALTER TABLE quality_test_jobs_queue_migration RENAME TO quality_test_jobs"} {
					if _, err = tx.ExecContext(ctx, q); err != nil {
						return err
					}
				}
				return tx.Commit()
			}); err != nil {
				return err
			}
		}
		if err := db.ensureSQLiteColumn(ctx, "quality_test_jobs", "batch_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := db.ensureSQLiteColumn(ctx, "quality_test_jobs", "started_at", "TIMESTAMP"); err != nil {
			return err
		}
	} else {
		rows, err := db.conn.QueryContext(ctx, "SELECT conname FROM pg_constraint WHERE conrelid='quality_test_jobs'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%status%' AND pg_get_constraintdef(oid) NOT LIKE '%queued%'")
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			names = append(names, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, name := range names {
			if _, err = tx.ExecContext(ctx, "ALTER TABLE quality_test_jobs DROP CONSTRAINT "+fmt.Sprintf("%q", name)); err != nil {
				return err
			}
		}
		if len(names) > 0 {
			if _, err = tx.ExecContext(ctx, "ALTER TABLE quality_test_jobs ADD CONSTRAINT quality_test_queue_state CHECK ((slot IS NOT NULL AND status IN ('running','cancelling')) OR (slot IS NULL AND status IN ('queued','completed','error','stopped','interrupted')))"); err != nil {
				return err
			}
		}
		for _, q := range []string{"ALTER TABLE quality_test_jobs ADD COLUMN IF NOT EXISTS batch_id TEXT NOT NULL DEFAULT ''", "ALTER TABLE quality_test_jobs ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ"} {
			if _, err = tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	for _, q := range []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_quality_test_pending_account ON quality_test_jobs(account_id) WHERE status IN ('queued','running','cancelling')",
		"CREATE INDEX IF NOT EXISTS idx_quality_test_created ON quality_test_jobs(id DESC)",
		"CREATE INDEX IF NOT EXISTS idx_quality_test_queue ON quality_test_jobs(status,id)",
		"CREATE INDEX IF NOT EXISTS idx_quality_test_account_latest ON quality_test_jobs(account_id,id DESC)",
		"CREATE INDEX IF NOT EXISTS idx_quality_test_batch ON quality_test_jobs(batch_id,id)",
		"CREATE TABLE IF NOT EXISTS quality_test_batches (id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, result_json TEXT NOT NULL)",
	} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

type QualityTestRejection struct {
	AccountID int64  `json:"account_id"`
	Error     string `json:"error"`
}
type QualityTestBatch struct {
	ID       string                 `json:"id"`
	JobIDs   []int64                `json:"job_ids"`
	Rejected []QualityTestRejection `json:"rejected"`
	Counts   map[string]int         `json:"counts"`
}

var ErrQualityTestIdempotency = errors.New("幂等标识已用于其他检测配置")

// The batch admission and its replay result commit together. No upstream work runs here.
func (db *DB) EnqueueQualityTestBatch(ctx context.Context, key, fingerprint string, jobs []QualityTestJob, rejected []QualityTestRejection) (*QualityTestBatch, error) {
	result := &QualityTestBatch{ID: key, JobIDs: []int64{}, Rejected: rejected}
	if result.Rejected == nil {
		result.Rejected = []QualityTestRejection{}
	}
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		inserted, err := tx.ExecContext(ctx, "INSERT INTO quality_test_batches(id,fingerprint,result_json) VALUES($1,$2,'{}') ON CONFLICT DO NOTHING", key, fingerprint)
		if err != nil {
			return err
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			var existing, body string
			if err = tx.QueryRowContext(ctx, "SELECT fingerprint,result_json FROM quality_test_batches WHERE id=$1", key).Scan(&existing, &body); err != nil {
				return err
			}
			if existing != fingerprint {
				return ErrQualityTestIdempotency
			}
			if err = json.Unmarshal([]byte(body), result); err != nil {
				return err
			}
			return tx.Commit()
		}
		now := db.timeArg(time.Now().UTC())
		for _, job := range jobs {
			var id int64
			err = tx.QueryRowContext(ctx, "INSERT INTO quality_test_jobs(account_id,account_name,plan_type,channel,model,reasoning_effort,prompt,status,created_at,updated_at,deadline_at,preset_kind,preset_ref,preset_name,batch_id) VALUES($1,$2,$3,$4,$5,$6,$7,'queued',$8,$8,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING RETURNING id", job.AccountID, job.AccountName, job.PlanType, job.Channel, job.Model, job.ReasoningEffort, job.Prompt, now, job.PresetKind, job.PresetRef, job.PresetName, key).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				result.Rejected = append(result.Rejected, QualityTestRejection{job.AccountID, ErrQualityTestAccountBusy.Error()})
				continue
			}
			if err != nil {
				return err
			}
			result.JobIDs = append(result.JobIDs, id)
			if job.PresetKind == "custom" {
				if presetID, parseErr := strconv.ParseInt(job.PresetRef, 10, 64); parseErr == nil && presetID > 0 {
					if _, err = tx.ExecContext(ctx, "UPDATE quality_test_prompts SET usage_count=usage_count+1,last_used_at=CURRENT_TIMESTAMP WHERE id=$1", presetID); err != nil {
						return err
					}
				}
			}
		}
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE quality_test_batches SET result_json=$1 WHERE id=$2", string(body), key); err != nil {
			return err
		}
		return tx.Commit()
	})
	return result, err
}

// Slot uniqueness and the conditional UPDATE arbitrate claim races across replicas.
func (db *DB) ClaimQualityTest(ctx context.Context) (*QualityTestJob, error) {
	if err := db.ExpireQualityTests(ctx, time.Now()); err != nil {
		return nil, err
	}
	for slot := 1; slot <= QualityTestConcurrency; slot++ {
		var id int64
		err := db.withSQLiteWriteLock(ctx, func() error {
			now := time.Now().UTC()
			return db.conn.QueryRowContext(ctx, "UPDATE quality_test_jobs SET status='running',slot=$1,started_at=$2,updated_at=$2,deadline_at=$3 WHERE id=(SELECT id FROM quality_test_jobs WHERE status='queued' ORDER BY id LIMIT 1) AND status='queued' AND NOT EXISTS(SELECT 1 FROM quality_test_jobs WHERE slot=$1) RETURNING id", slot, db.timeArg(now), db.timeArg(now.Add(10*time.Minute))).Scan(&id)
		})
		if err == nil {
			return db.GetQualityTestJob(ctx, id)
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		// Another replica may win the same unique slot between snapshot and update.
		text := strings.ToLower(err.Error())
		if strings.Contains(text, "unique") || strings.Contains(text, "duplicate key") {
			continue
		}
		return nil, err
	}
	return nil, nil
}
func (db *DB) GetQualityTestBatch(ctx context.Context, id string) (*QualityTestBatch, error) {
	var body string
	if err := db.conn.QueryRowContext(ctx, "SELECT result_json FROM quality_test_batches WHERE id=$1", id).Scan(&body); err != nil {
		return nil, err
	}
	var result QualityTestBatch
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		return nil, err
	}
	result.Counts = map[string]int{}
	rows, err := db.conn.QueryContext(ctx, "SELECT status,COUNT(*) FROM quality_test_jobs WHERE batch_id=$1 GROUP BY status", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		result.Counts[status] = n
	}
	return &result, rows.Err()
}
func (db *DB) CancelQualityTestBatch(ctx context.Context, id string) error {
	_, err := db.conn.ExecContext(ctx, "UPDATE quality_test_jobs SET status=CASE WHEN status='queued' THEN 'stopped' ELSE 'cancelling' END,completed_at=CASE WHEN status='queued' THEN $1 ELSE completed_at END,updated_at=$1 WHERE batch_id=$2 AND status IN ('queued','running')", db.timeArg(time.Now()), id)
	return err
}
