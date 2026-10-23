package store

import (
	"context"
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
