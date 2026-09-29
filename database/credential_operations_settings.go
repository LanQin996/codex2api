package database

import (
	"context"
	"database/sql"
	"errors"
)

type CredentialOperationsSettings struct {
	Concurrency int `json:"concurrency"`
}

func (db *DB) GetCredentialOperationsSettings(ctx context.Context) (CredentialOperationsSettings, error) {
	s := CredentialOperationsSettings{Concurrency: 2}
	if err := db.EnsureCredentialOperations(ctx); err != nil {
		return s, err
	}
	err := db.conn.QueryRowContext(ctx, `SELECT concurrency FROM credential_operations_settings WHERE id=1`).Scan(&s.Concurrency)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return s, err
}

func (db *DB) SetCredentialOperationsSettings(ctx context.Context, s CredentialOperationsSettings) error {
	if s.Concurrency < 1 || s.Concurrency > 8 {
		return errors.New("concurrency must be between 1 and 8")
	}
	if err := db.EnsureCredentialOperations(ctx); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO credential_operations_settings(id,concurrency) VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET concurrency=excluded.concurrency`, s.Concurrency)
	return err
}
