package store

import (
	"context"
	"strings"
	"time"
)

// Ban blocks a user (Kind "user", Value a login) or a host (Kind "host", Value a subdomain).
type Ban struct {
	Kind, Value, Reason string
	CreatedAt           time.Time
}

// Values are stored lowercase, and lookups are case-insensitive.
// AddBan inserts the ban, replacing any existing one for the same kind and value.
func (s *Store) AddBan(ctx context.Context, b Ban) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO bans (kind, value, reason, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (kind, value) DO UPDATE SET reason = excluded.reason`,
		b.Kind, strings.ToLower(b.Value), b.Reason, ts(time.Now()))
	return err
}

func (s *Store) RemoveBan(ctx context.Context, kind, value string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bans WHERE kind = ? AND value = ?`, kind, strings.ToLower(value))
	return err
}

func (s *Store) Bans(ctx context.Context) ([]Ban, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, value, reason, created_at FROM bans ORDER BY created_at, kind, value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ban
	for rows.Next() {
		var b Ban
		var created int64
		if err := rows.Scan(&b.Kind, &b.Value, &b.Reason, &created); err != nil {
			return nil, err
		}
		b.CreatedAt = fromTS(created)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) IsBanned(ctx context.Context, kind, value string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM bans WHERE kind = ? AND value = ?`, kind, strings.ToLower(value)).Scan(&n)
	return n > 0, err
}
