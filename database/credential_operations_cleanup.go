package database

import "context"

// Repair pre-upgrade orphan records too. Never match solely by email: different
// workspaces can share an email, and unbound imports are not deleted accounts.
func (db *DB) cleanupDeletedCredentialOperations(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM credential_operations
 WHERE account_id>0 AND NOT EXISTS (
 SELECT 1 FROM accounts WHERE accounts.id=credential_operations.account_id
 AND status<>'deleted' AND COALESCE(error_message,'')<>'deleted')`)
	return err
}
