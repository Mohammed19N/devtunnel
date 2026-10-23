package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) Audit(ctx context.Context, userID int64, action, detail string) error {
	var uid any
	if userID != 0 {
		uid = userID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (user_id, action, detail, created_at) VALUES (?, ?, ?, ?)`,
		uid, action, detail, ts(time.Now()))
	return err
}

// Setting returns the value for key, creating it with gen on first use. The
// lookup and insert share one transaction, so gen's value wins exactly once.
func (s *Store) Setting(ctx context.Context, key string, gen func() []byte) ([]byte, error) {
	var val []byte
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		val = gen()
		_, err = tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)`, key, val)
		return err
	})
	return val, err
}
