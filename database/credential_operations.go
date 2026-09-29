package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrCredentialOperationStale = errors.New("credential operation changed or lease expired")

type CredentialOperation struct {
	ExpectedWorkspace string `json:"-"`
	ID                string `json:"id"`
	Email             string `json:"email"`
	AccountID         int64  `json:"account_id"`
	State             string `json:"state"`
	Enabled           bool   `json:"enabled"`
	AutoRelogin       bool   `json:"auto_relogin"`
	Failures          int    `json:"failures"`
	NextRun           int64  `json:"next_run"`
	Message           string `json:"message"`
	LeaseUntil        int64  `json:"lease_until"`
	LoginCipher       string `json:"-"`
	ResultCipher      string `json:"-"`
	Snapshot          string `json:"-"`
	Lease             string `json:"-"`
}

func (db *DB) EnsureCredentialOperations(ctx context.Context) error {
	db.credentialOpsMu.Lock()
	defer db.credentialOpsMu.Unlock()
	if db.credentialOpsReady {
		return nil
	}
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS credential_operations (
 id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, account_id BIGINT NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'queued', enabled BOOLEAN NOT NULL DEFAULT TRUE, auto_relogin BOOLEAN NOT NULL DEFAULT TRUE,
 failures INTEGER NOT NULL DEFAULT 0, next_run BIGINT NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT '',
 login_cipher TEXT NOT NULL, result_cipher TEXT NOT NULL DEFAULT '', snapshot TEXT NOT NULL DEFAULT '',
 lease TEXT NOT NULL DEFAULT '', lease_until BIGINT NOT NULL DEFAULT 0)`)
	if err == nil {
		_, err = db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS credential_operations_settings (
 id INTEGER PRIMARY KEY CHECK (id=1), concurrency INTEGER NOT NULL CHECK (concurrency BETWEEN 1 AND 8))`)
	}
	if err == nil {
		db.credentialOpsReady = true
	}
	return err
}

const credentialOperationColumns = `id,email,account_id,state,enabled,auto_relogin,failures,next_run,message,login_cipher,result_cipher,snapshot,lease,lease_until`

type credentialOperationScanner interface{ Scan(...any) error }

func scanCredentialOperation(s credentialOperationScanner) (*CredentialOperation, error) {
	r := &CredentialOperation{}
	err := s.Scan(&r.ID, &r.Email, &r.AccountID, &r.State, &r.Enabled, &r.AutoRelogin, &r.Failures, &r.NextRun, &r.Message, &r.LoginCipher, &r.ResultCipher, &r.Snapshot, &r.Lease, &r.LeaseUntil)
	return r, err
}
func (db *DB) ListCredentialOperations(ctx context.Context) ([]CredentialOperation, error) {
	if err := db.EnsureCredentialOperations(ctx); err != nil {
		return nil, err
	}
	if err := db.cleanupDeletedCredentialOperations(ctx); err != nil {
		return nil, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT `+credentialOperationColumns+` FROM credential_operations ORDER BY email LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CredentialOperation{}
	for rows.Next() {
		r, err := scanCredentialOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
func (db *DB) GetCredentialOperation(ctx context.Context, id string) (*CredentialOperation, error) {
	return scanCredentialOperation(db.conn.QueryRowContext(ctx, `SELECT `+credentialOperationColumns+` FROM credential_operations WHERE id=$1`, id))
}

func (db *DB) FindCredentialOperationByEmail(ctx context.Context, email string) (*CredentialOperation, error) {
	if err := db.EnsureCredentialOperations(ctx); err != nil {
		return nil, err
	}
	return scanCredentialOperation(db.conn.QueryRowContext(ctx, `SELECT `+credentialOperationColumns+` FROM credential_operations WHERE email=$1`, email))
}
func (db *DB) ReplaceCredentialOperationLogin(ctx context.Context, id, encrypted string) error {
	res, err := db.conn.ExecContext(ctx, `UPDATE credential_operations SET login_cipher=$1,result_cipher='',snapshot='',state='queued',failures=0,message='',next_run=0,enabled=TRUE,auto_relogin=TRUE WHERE id=$2 AND lease_until<=$3`, encrypted, id, time.Now().Unix())
	return credentialOperationAffected(res, err)
}

// Insert is idempotent by normalized login email. Existing jobs and secrets are
// never silently replaced by a duplicate submission (including lost responses).
func (db *DB) InsertCredentialOperation(ctx context.Context, r CredentialOperation) (*CredentialOperation, error) {
	if err := db.EnsureCredentialOperations(ctx); err != nil {
		return nil, err
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO credential_operations(id,email,login_cipher) VALUES($1,$2,$3) ON CONFLICT(email) DO NOTHING`, r.ID, r.Email, r.LoginCipher)
	if err != nil {
		return nil, err
	}
	return scanCredentialOperation(db.conn.QueryRowContext(ctx, `SELECT `+credentialOperationColumns+` FROM credential_operations WHERE email=$1`, r.Email))
}
func (db *DB) ClaimCredentialOperation(ctx context.Context, owner string) (*CredentialOperation, error) {
	if err := db.EnsureCredentialOperations(ctx); err != nil {
		return nil, err
	}
	if err := db.cleanupDeletedCredentialOperations(ctx); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	// A single conditional UPDATE is safe across processes on both databases.
	return scanCredentialOperation(db.conn.QueryRowContext(ctx, `UPDATE credential_operations SET lease=$1,lease_until=$2
 WHERE id=(SELECT id FROM credential_operations WHERE enabled=TRUE AND next_run<=$3 AND lease_until<=$3
 AND state IN ('queued','enrollment_pending','enrolled') ORDER BY next_run,id LIMIT 1) AND lease_until<=$3
 RETURNING `+credentialOperationColumns, owner, now+1800, now))
}
func (db *DB) SaveCredentialOperation(ctx context.Context, r *CredentialOperation) error {
	res, err := db.conn.ExecContext(ctx, `UPDATE credential_operations SET account_id=$1,state=$2,failures=$3,next_run=$4,message=$5,result_cipher=$6,snapshot=$7,lease='',lease_until=0
 WHERE id=$8 AND lease=$9 AND lease_until>$10 AND enabled=TRUE`, r.AccountID, r.State, r.Failures, r.NextRun, r.Message, r.ResultCipher, r.Snapshot, r.ID, r.Lease, time.Now().Unix())
	return credentialOperationAffected(res, err)
}

// Checkpoint preserves the lease while durably retaining the login result.
func (db *DB) CheckpointCredentialOperation(ctx context.Context, r *CredentialOperation) error {
	res, err := db.conn.ExecContext(ctx, `UPDATE credential_operations SET result_cipher=$1,snapshot=$2,state='enrollment_pending' WHERE id=$3 AND lease=$4 AND lease_until>$5 AND enabled=TRUE`, r.ResultCipher, r.Snapshot, r.ID, r.Lease, time.Now().Unix())
	return credentialOperationAffected(res, err)
}
func (db *DB) ControlCredentialOperation(ctx context.Context, id, action string) error {
	if err := db.EnsureCredentialOperations(ctx); err != nil {
		return err
	}
	var q string
	switch action {
	case "pause":
		// Invalidate write authority but retain the occupancy deadline: the
		// external protocol may still be running, so resume must not overlap it.
		q = `UPDATE credential_operations SET enabled=FALSE,lease='' WHERE id=$1`
	case "resume":
		q = `UPDATE credential_operations SET enabled=TRUE,next_run=0 WHERE id=$1`
	case "retry":
		q = `UPDATE credential_operations SET state=CASE WHEN result_cipher<>'' THEN 'enrollment_pending' WHEN account_id>0 THEN 'enrolled' ELSE 'queued' END,auto_relogin=TRUE,next_run=0,message='' WHERE id=$1 AND enabled=TRUE AND lease_until<=$2`
	default:
		return errors.New("invalid operation action")
	}
	var res sql.Result
	var err error
	if action == "retry" {
		res, err = db.conn.ExecContext(ctx, q, id, time.Now().Unix())
	} else {
		res, err = db.conn.ExecContext(ctx, q, id)
	}
	return credentialOperationAffected(res, err)
}
func credentialOperationAffected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCredentialOperationStale
	}
	return nil
}

// Snapshot deliberately excludes usage observations but includes all identity
// and token fields, so background refresh and manual reauthorization win.
func CredentialOperationSnapshot(row *AccountRow) string {
	b, _ := json.Marshal([]any{row.CredentialGeneration, row.Platform, row.Type, row.GetCredential("access_token"), row.GetCredential("refresh_token"), row.GetCredential("id_token"), row.GetCredential("email"), row.GetCredential("account_id"), row.GetCredential("workspace_id"), row.Credentials["custom_headers"]})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Complete atomically writes tokens and completes enrollment. An invalidated
// lease, disabled/deleted account, or rotated token aborts the entire write.
func (db *DB) CompleteCredentialOperation(ctx context.Context, r *CredentialOperation, updates map[string]any) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		q := `SELECT lease FROM credential_operations WHERE id=$1 AND lease=$2 AND lease_until>$3 AND enabled=TRUE`
		if !db.isSQLite() {
			q += ` FOR UPDATE`
		}
		var lease string
		if err = tx.QueryRowContext(ctx, q, r.ID, r.Lease, time.Now().Unix()).Scan(&lease); err != nil {
			return ErrCredentialOperationStale
		}
		q = `SELECT credentials,credential_generation,platform,type FROM accounts WHERE id=$1 AND status<>'deleted' AND COALESCE(error_message,'')<>'deleted' AND COALESCE(enabled,TRUE)=TRUE`
		if !db.isSQLite() {
			q += ` FOR UPDATE`
		}
		row := AccountRow{ID: r.AccountID}
		var raw any
		if err = tx.QueryRowContext(ctx, q, r.AccountID).Scan(&raw, &row.CredentialGeneration, &row.Platform, &row.Type); err != nil {
			return err
		}
		row.Credentials = decodeCredentials(raw)
		workspace := row.GetCredential("workspace_id")
		if workspace == "" {
			workspace = row.GetCredential("account_id")
		}
		if row.Platform != "openai" || row.Type != "oauth" || !strings.EqualFold(row.GetCredential("email"), r.Email) || (r.ExpectedWorkspace != "" && workspace != r.ExpectedWorkspace) {
			return errors.New("credential operation account identity changed")
		}
		if len(updates) > 0 {
			if CredentialOperationSnapshot(&row) != r.Snapshot {
				return ErrCredentialOperationStale
			}
			merged := mergeCredentialMaps(row.Credentials, updates)
			if err = db.writeCodexRefreshCredentials(ctx, tx, row, merged, true); err != nil {
				return err
			}
			// Do not remove an unrelated rate-limit cooldown.
			if _, err = tx.ExecContext(ctx, `UPDATE accounts SET cooldown_reason='',cooldown_until=NULL WHERE id=$1 AND cooldown_reason='unauthorized'`, r.AccountID); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE credential_operations SET account_id=$1,state='enrolled',result_cipher='',snapshot='',failures=0,next_run=$2,message='',lease='',lease_until=0 WHERE id=$3 AND lease=$4`, r.AccountID, time.Now().Add(30*time.Minute).Unix(), r.ID, r.Lease)
		if err = credentialOperationAffected(res, err); err != nil {
			return err
		}
		return tx.Commit()
	})
}
