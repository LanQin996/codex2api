package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestCredentialOperationBlockedIsTerminal(t *testing.T) {
	db := credentialOperationDB(t)
	ctx := context.Background()
	if _, err := db.InsertCredentialOperation(ctx, CredentialOperation{ID: "blocked", Email: "test@example.com", LoginCipher: "encrypted"}); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimCredentialOperation(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.BlockCredentialOperation(ctx, job); err != nil {
		t.Fatal(err)
	}
	record, err := db.GetCredentialOperation(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != "recovery_blocked" || record.AutoRelogin || record.Lease != "" {
		t.Fatal("block not persisted")
	}
	for i := 0; i < 3; i++ {
		if _, err = db.ClaimCredentialOperation(ctx, "other"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("blocked task reclaimed", err)
		}
	}
	if err = db.SaveCredentialOperation(ctx, job); !errors.Is(err, ErrCredentialOperationStale) {
		t.Fatal("stale worker cleared block", err)
	}
	if err = db.ControlCredentialOperation(ctx, job.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if err = db.ControlCredentialOperation(ctx, job.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClaimCredentialOperation(ctx, "other"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("resume cleared terminal block", err)
	}
	if err = db.ControlCredentialOperation(ctx, job.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	next, err := db.ClaimCredentialOperation(ctx, "manual")
	if err != nil || !next.AutoRelogin {
		t.Fatal("explicit retry not enabled", err)
	}
}
