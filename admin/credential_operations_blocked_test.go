package admin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/credentialops"
	"testing"
)

func TestCredentialOperationBlockedLoginStopsRecovery(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		db := newTestAdminDB(t)
		h := &Handler{db: db, store: auth.NewStore(db, cache.NewMemory(1), nil)}
		ctx := context.Background()
		a, _ := credentialops.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
		encrypted, err := credentialops.Seal(a, "job", "login", credentialops.Login{Email: "test@example.com", Password: "synthetic", TOTP: "JBSWY3DPEHPK3PXP"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.InsertCredentialOperation(ctx, database.CredentialOperation{ID: "job", Email: "test@example.com", LoginCipher: encrypted}); err != nil {
			t.Fatal(err)
		}
		calls := 0
		service := &credentialOperationsService{cipher: a, login: func(context.Context, credentialops.Login) (map[string]any, error) {
			calls++
			if blocked {
				return nil, credentialops.ErrAccountBlocked
			}
			return nil, errors.New("synthetic network failure")
		}}
		job, err := db.ClaimCredentialOperation(ctx, "worker")
		if err != nil {
			t.Fatal(err)
		}
		h.runCredentialOperation(ctx, service, job)
		saved, err := db.GetCredentialOperation(ctx, "job")
		if err != nil {
			t.Fatal(err)
		}
		expected := "login_failed"
		if blocked {
			expected = "recovery_blocked"
		}
		if saved.State != expected || calls != 1 {
			t.Fatalf("unexpected recovery: %s calls=%d", saved.State, calls)
		}
		if _, err = db.ClaimCredentialOperation(ctx, "next"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("failed login automatically reclaimed", err)
		}
	}
}
