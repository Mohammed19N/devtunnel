package store

import "context"

// AddUsage adds bytes to the user's total for month ("2006-01").
func (s *Store) AddUsage(ctx context.Context, userID int64, month string, bytes int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_monthly (user_id, month, bytes) VALUES (?, ?, ?)
		 ON CONFLICT (user_id, month) DO UPDATE SET bytes = bytes + excluded.bytes`,
		userID, month, bytes)
	return err
}

func (s *Store) Usage(ctx context.Context, userID int64, month string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes), 0) FROM usage_monthly WHERE user_id = ? AND month = ?`, userID, month).Scan(&n)
	return n, err
}

// PlanUsage sums month's usage over all users on plan.
func (s *Store) PlanUsage(ctx context.Context, plan, month string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(m.bytes), 0) FROM usage_monthly m JOIN users u ON u.id = m.user_id
		 WHERE u.plan = ? AND m.month = ?`, plan, month).Scan(&n)
	return n, err
}
