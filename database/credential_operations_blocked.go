package database

import (
	"context"
	"time"
)

// Terminal until explicit administrator retry/replacement. Fence stale workers
// just like checkpoint/completion, and discard unusable pending login output.
func (db *DB) BlockCredentialOperation(ctx context.Context, job *CredentialOperation) error {
	res, err := db.conn.ExecContext(ctx, `UPDATE credential_operations SET state='recovery_blocked',auto_relogin=FALSE,result_cipher='',snapshot='',next_run=0,lease='',lease_until=0,message='上游明确返回账号封禁或停用，已停止自动巡检和重登；确认解封后手动重试' WHERE id=$1 AND lease=$2 AND lease_until>$3 AND enabled=TRUE`, job.ID, job.Lease, time.Now().Unix())
	return credentialOperationAffected(res, err)
}
