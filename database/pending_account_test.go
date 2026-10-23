package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPendingAccountTransaction(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			if driver == "postgres" && os.Getenv("CODEX2API_TEST_POSTGRES_DSN") == "" {
				t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
			}
			for _, failure := range []string{"none", "note", "tags", "family", "commit"} {
				t.Run(failure, func(t *testing.T) {
					db := pendingAccountDB(t, driver)
					ctx := context.Background()
					if failure != "none" {
						removeFault := injectPendingAccountFailure(t, db, failure)
						id, err := db.InsertPendingAccountWithCredentials(ctx, "pending", map[string]interface{}{"access_token": "fixture-token"}, "", "联系人: O'Reilly", []string{"self-service", "待审核"})
						wantError := "injected"
						if driver == "sqlite" && failure == "commit" {
							wantError = "FOREIGN KEY constraint failed"
						}
						if err == nil || id != 0 || !strings.Contains(err.Error(), wantError) {
							t.Fatalf("injected %s failure: id=%d err=%v", failure, id, err)
						}
						var count int
						if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE name=$1`, "pending").Scan(&count); err != nil || count != 0 {
							t.Fatalf("failed transaction left rows (including deleted): count=%d err=%v", count, err)
						}
						removeFault()
					}
					// Retry uses exactly the same identity and review metadata.
					id, err := db.InsertPendingAccountWithCredentials(ctx, "pending", map[string]interface{}{"access_token": "fixture-token"}, "", "联系人: O'Reilly", []string{"self-service", "待审核"})
					if err != nil {
						t.Fatal(err)
					}
					row, err := db.GetAccountByID(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if row.Enabled || row.Note != "联系人: O'Reilly" || !reflect.DeepEqual(row.Tags, []string{"self-service", "待审核"}) || row.CredentialFamilyID == "" || row.GetCredential("access_token") != "fixture-token" {
						t.Fatalf("incomplete pending account: enabled=%v note=%q tags=%v family=%q", row.Enabled, row.Note, row.Tags, row.CredentialFamilyID)
					}
				})
			}
		})
	}
}

func pendingAccountDB(t *testing.T, driver string) *DB {
	t.Helper()
	if driver == "sqlite" {
		db, err := New(driver, filepath.Join(t.TempDir(), "pending.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("pending_test_%d", time.Now().UnixNano())
	query := parsed.Query()
	query.Set("options", "-c search_path="+schema+",public")
	parsed.RawQuery = query.Encode()
	db, err := New(driver, parsed.String(), schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.ExecContext(context.Background(), "DROP SCHEMA "+quotePostgresIdent(schema)+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	return db
}

// Fail at real database boundaries without introducing production test hooks.
func injectPendingAccountFailure(t *testing.T, db *DB, failure string) func() {
	t.Helper()
	exec := func(query string) {
		t.Helper()
		if _, err := db.conn.ExecContext(context.Background(), query); err != nil {
			t.Fatal(err)
		}
	}
	event := "INSERT"
	condition := "NEW.note <> ''"
	if failure == "tags" {
		condition = "CAST(NEW.tags AS TEXT) LIKE '%self-service%'"
	}
	if failure == "family" {
		event = "UPDATE OF credential_family_id"
		condition = "NEW.credential_family_id <> ''"
	}
	if db.isSQLite() {
		if failure == "commit" {
			// One connection ensures the FK pragma also applies to the transaction.
			db.conn.SetMaxOpenConns(1)
			exec(`PRAGMA foreign_keys=ON`)
			exec(`CREATE TABLE pending_parent (id INTEGER PRIMARY KEY)`)
			exec(`CREATE TABLE pending_probe (id INTEGER REFERENCES pending_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
			exec(`CREATE TRIGGER pending_fault AFTER INSERT ON accounts BEGIN INSERT INTO pending_probe VALUES (1); END`)
		} else {
			exec("CREATE TRIGGER pending_fault AFTER " + event + " ON accounts WHEN " + condition + " BEGIN SELECT RAISE(ABORT, 'injected pending failure'); END")
		}
		return func() { exec(`DROP TRIGGER pending_fault`) }
	}
	exec(`CREATE FUNCTION pending_fault_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected pending failure'; END $$`)
	if failure == "commit" {
		exec(`CREATE CONSTRAINT TRIGGER pending_fault AFTER INSERT ON accounts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION pending_fault_fn()`)
	} else {
		exec("CREATE TRIGGER pending_fault AFTER " + event + " ON accounts FOR EACH ROW WHEN (" + condition + ") EXECUTE FUNCTION pending_fault_fn()")
	}
	return func() {
		exec(`DROP TRIGGER pending_fault ON accounts`)
		exec(`DROP FUNCTION pending_fault_fn()`)
	}
}
