package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/credentialops"
	"github.com/gin-gonic/gin"
)

func syntheticOpsCredentials(email, workspace string) map[string]any {
	claims, _ := json.Marshal(map[string]any{"email": email, "exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": workspace, "chatgpt_plan_type": "plus"}})
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
	return map[string]any{"access_token": token, "id_token": token, "refresh_token": "synthetic-refresh"}
}
func TestCredentialOperationIdentityValidation(t *testing.T) {
	valid := syntheticOpsCredentials("test@example.com", "workspace-test")
	if _, err := credentialOperationSeed(valid, "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := credentialOperationSeed(valid, "other@example.com"); err == nil {
		t.Fatal("wrong email accepted")
	}
	if _, err := credentialOperationSeed(map[string]any{"email": "test@example.com", "account_id": "workspace-test", "access_token": "opaque", "refresh_token": "rt"}, "test@example.com"); err == nil {
		t.Fatal("unproven identity accepted")
	}
}
func TestCredentialOperationInitialAndCheckpointRetry(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	h := &Handler{db: db, store: store}
	ctx := context.Background()
	a, _ := credentialops.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	login := credentialops.Login{Email: "test@example.com", Password: "synthetic-password", TOTP: "JBSWY3DPEHPK3PXP"}
	encrypted, _ := credentialops.Seal(a, "job", "login", login)
	_, err := db.InsertCredentialOperation(ctx, database.CredentialOperation{ID: "job", Email: login.Email, LoginCipher: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimCredentialOperation(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	credentials := syntheticOpsCredentials(login.Email, "workspace-test")
	// Simulate successful login + account creation, then failed enrollment.
	seed, err := credentialOperationSeed(credentials, login.Email)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertAccountWithCredentials(ctx, login.Email, h.newCodexAccountCredentials(seed), "")
	if err != nil {
		t.Fatal(err)
	}
	job.AccountID = id
	job.ResultCipher, _ = credentialops.Seal(a, job.ID, "result", credentials)
	if err = db.CheckpointCredentialOperation(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.State = "enrollment_pending"
	if err = db.SaveCredentialOperation(ctx, job); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.ClaimCredentialOperation(ctx, "replacement-worker")
	if err != nil {
		t.Fatal(err)
	}
	service := &credentialOperationsService{cipher: a, login: func(context.Context, credentialops.Login) (map[string]any, error) {
		t.Fatal("retry unnecessarily logged in again")
		return nil, nil
	}}
	h.runCredentialOperation(ctx, service, recovered)
	done, err := db.GetCredentialOperation(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != "enrolled" || done.AccountID != id || done.ResultCipher != "" {
		t.Fatalf("retry incomplete: state=%s account=%d message=%s", done.State, done.AccountID, done.Message)
	}
}
func TestCredentialOperationImportRequiresConfiguration(t *testing.T) {
	t.Setenv("CREDENTIAL_OPS_KEY", "")
	h := &Handler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/credential-operations/import", strings.NewReader(`{"content":"never processed"}`))
	h.ImportCredentialOperations(c)
	if w.Code != 503 {
		t.Fatalf("status %d", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("secret endpoint cacheable")
	}
}

func TestCredentialOperationFreshImport(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	h := &Handler{db: db, store: store}
	ctx := context.Background()
	a, _ := credentialops.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	login := credentialops.Login{Email: "fresh@example.com", Password: "synthetic-password", TOTP: "JBSWY3DPEHPK3PXP"}
	encrypted, _ := credentialops.Seal(a, "fresh-job", "login", login)
	_, err := db.InsertCredentialOperation(ctx, database.CredentialOperation{ID: "fresh-job", Email: login.Email, LoginCipher: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimCredentialOperation(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	service := &credentialOperationsService{cipher: a, login: func(context.Context, credentialops.Login) (map[string]any, error) {
		calls++
		return syntheticOpsCredentials(login.Email, "workspace-fresh"), nil
	}}
	h.runCredentialOperation(ctx, service, job)
	done, err := db.GetCredentialOperation(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != "enrolled" || done.AccountID == 0 || calls != 1 {
		t.Fatalf("import not complete: %s %s", done.State, done.Message)
	}
	row, err := db.GetAccountByID(ctx, done.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(row.Credentials)
	if strings.Contains(string(raw), login.Password) || strings.Contains(string(raw), login.TOTP) {
		t.Fatal("login secrets in exportable credentials")
	}
	duplicate, err := db.InsertCredentialOperation(ctx, database.CredentialOperation{ID: "another", Email: login.Email, LoginCipher: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != done.ID || duplicate.AccountID != done.AccountID {
		t.Fatal("repeat submission created a second identity")
	}
}
