package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestCredentialOperationDeletionCleanup(t *testing.T) {
	for _, mode := range []string{"soft", "batch", "purge", "purge-all", "legacy-list", "legacy-claim"} {
		t.Run(mode, func(t *testing.T) {
			db := credentialOperationDB(t)
			ctx := context.Background()
			id, err := db.InsertAccountWithCredentials(ctx, "test", map[string]any{"email": "test@example.com"}, "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.InsertCredentialOperation(ctx, CredentialOperation{ID: "managed", Email: "test@example.com", LoginCipher: "encrypted"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.conn.ExecContext(ctx, `UPDATE credential_operations SET account_id=$1 WHERE id='managed'`, id); err != nil {
				t.Fatal(err)
			}
			job, err := db.ClaimCredentialOperation(ctx, "worker")
			if err != nil {
				t.Fatal(err)
			}
			// A queued, unrelated import must survive cleanup.
			if _, err = db.InsertCredentialOperation(ctx, CredentialOperation{ID: "unbound", Email: "other@example.com", LoginCipher: "other-encrypted"}); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "soft":
				err = db.SoftDeleteAccount(ctx, id)
			case "batch":
				err = db.BatchSoftDeleteAccounts(ctx, []int64{id})
			default:
				// Simulate an account removed by a pre-upgrade version.
				if _, err = db.conn.ExecContext(ctx, `UPDATE accounts SET status='deleted' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "purge":
					err = db.PurgeAccount(ctx, id)
				case "purge-all":
					_, err = db.PurgeDeletedAccounts(ctx)
				case "legacy-list":
					_, err = db.ListCredentialOperations(ctx)
				case "legacy-claim":
					_, err = db.ClaimCredentialOperation(ctx, "next-worker")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.GetCredentialOperation(ctx, "managed"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("record survived: %v", err)
			}
			if _, err = db.GetCredentialOperation(ctx, "unbound"); err != nil {
				t.Fatal("unrelated import removed", err)
			}
			if err = db.CheckpointCredentialOperation(ctx, job); !errors.Is(err, ErrCredentialOperationStale) {
				t.Fatal("deleted job checkpoint accepted", err)
			}
			if err = db.SaveCredentialOperation(ctx, job); !errors.Is(err, ErrCredentialOperationStale) {
				t.Fatal("deleted job saved", err)
			}
			if err = db.CompleteCredentialOperation(ctx, job, nil); !errors.Is(err, ErrCredentialOperationStale) {
				t.Fatal("deleted job completed", err)
			}
			if mode == "soft" || mode == "batch" {
				if err = db.RestoreAccount(ctx, id); err != nil {
					t.Fatal(err)
				}
				if _, err = db.GetCredentialOperation(ctx, "managed"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("restore resurrected login secrets")
				}
			}
		})
	}
}

func TestCredentialOperationFailedPurgeKeepsEnrollment(t *testing.T) {
	db := credentialOperationDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "test", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.InsertCredentialOperation(ctx, CredentialOperation{ID: "job", Email: "test@example.com", LoginCipher: "encrypted"}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.conn.ExecContext(ctx, `UPDATE credential_operations SET account_id=$1 WHERE id='job'`, id); err != nil {
		t.Fatal(err)
	}
	if err = db.PurgeAccount(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("active purge should fail", err)
	}
	if _, err = db.GetCredentialOperation(ctx, "job"); err != nil {
		t.Fatal("failed transaction removed enrollment", err)
	}
}
