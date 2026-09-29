package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func credentialOperationDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func TestCredentialOperationDurableLeaseAndRedaction(t *testing.T) {
	db := credentialOperationDB(t)
	ctx := context.Background()
	first, err := db.InsertCredentialOperation(ctx, CredentialOperation{ID: "one", Email: "test@example.com", LoginCipher: "encrypted-login"})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := db.InsertCredentialOperation(ctx, CredentialOperation{ID: "two", Email: first.Email, LoginCipher: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != first.ID || duplicate.LoginCipher != first.LoginCipher {
		t.Fatal("duplicate replaced existing job")
	}
	a, err := db.ClaimCredentialOperation(ctx, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimCredentialOperation(ctx, "worker-b"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("duplicate claim: %v", err)
	}
	a.ResultCipher = "encrypted-result"
	if err := db.CheckpointCredentialOperation(ctx, a); err != nil {
		t.Fatal(err)
	}
	persisted, err := db.GetCredentialOperation(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != "enrollment_pending" || persisted.ResultCipher != a.ResultCipher {
		t.Fatal("checkpoint lost")
	}
	b, _ := json.Marshal(persisted)
	for _, secret := range []string{"encrypted-login", "encrypted-result", "worker-a"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("secret leaked into API view")
		}
	}
	if _, err = db.conn.ExecContext(ctx, `UPDATE credential_operations SET lease_until=0 WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := db.ClaimCredentialOperation(ctx, "worker-b")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.State != "enrollment_pending" || reclaimed.ResultCipher != a.ResultCipher {
		t.Fatal("reclaim lost checkpoint")
	}
	if err := db.SaveCredentialOperation(ctx, a); !errors.Is(err, ErrCredentialOperationStale) {
		t.Fatal("stale worker wrote state")
	}
	if err := db.ControlCredentialOperation(ctx, a.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCredentialOperation(ctx, reclaimed); !errors.Is(err, ErrCredentialOperationStale) {
		t.Fatal("paused worker wrote state")
	}
	if _, err := db.ClaimCredentialOperation(ctx, "worker-c"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("paused job claimed")
	}
	if err := db.ControlCredentialOperation(ctx, a.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimCredentialOperation(ctx, "worker-c"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("resume overlapped the previous protocol process")
	}
	if err := db.ReplaceCredentialOperationLogin(ctx, a.ID, "replacement"); !errors.Is(err, ErrCredentialOperationStale) {
		t.Fatal("replaced an active login")
	}
}
func TestCredentialOperationCompletionCAS(t *testing.T) {
	db := credentialOperationDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "synthetic", map[string]any{"email": "test@example.com", "account_id": "workspace-test", "refresh_token": "old-rt", "access_token": "old-at"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.InsertCredentialOperation(ctx, CredentialOperation{ID: "job", Email: "test@example.com", LoginCipher: "encrypted"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimCredentialOperation(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	job.AccountID = id
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	job.Snapshot = CredentialOperationSnapshot(row)
	// A concurrent normal token refresh wins; the old login cannot overwrite it.
	if err = db.UpdateCredentials(ctx, id, map[string]any{"access_token": "newer-at"}); err != nil {
		t.Fatal(err)
	}
	if err = db.CompleteCredentialOperation(ctx, job, map[string]any{"access_token": "stale-at"}); !errors.Is(err, ErrCredentialOperationStale) {
		t.Fatalf("expected CAS failure, got %v", err)
	}
	row, _ = db.GetAccountByID(ctx, id)
	if row.GetCredential("access_token") != "newer-at" {
		t.Fatal("token overwritten")
	}
	pending, _ := db.GetCredentialOperation(ctx, job.ID)
	if pending.State == "enrolled" {
		t.Fatal("failed completion reported success")
	}
	job.Snapshot = CredentialOperationSnapshot(row)
	if err = db.CompleteCredentialOperation(ctx, job, map[string]any{"access_token": "reauth-at"}); err != nil {
		t.Fatal(err)
	}
	done, _ := db.GetCredentialOperation(ctx, job.ID)
	if done.State != "enrolled" || done.AccountID != id || done.ResultCipher != "" || done.Lease != "" || done.NextRun <= time.Now().Unix() {
		t.Fatal("completion not atomic")
	}
	row, _ = db.GetAccountByID(ctx, id)
	if row.GetCredential("access_token") != "reauth-at" {
		t.Fatal("completion did not persist token")
	}
}
