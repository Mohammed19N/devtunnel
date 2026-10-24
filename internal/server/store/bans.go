package store

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Ban blocks a user (Kind "user", Value "id:<user id>", or a login in legacy
// rows) or a host (Kind "host", Value a subdomain label or a custom domain's
// full hostname).
type Ban struct {
	Kind, Value, Reason string
	CreatedAt           time.Time
}

// banValue normalises a ban value: lowercase, and for hosts no trailing dot.
func banValue(kind, value string) string {
	value = strings.ToLower(value)
	if kind == "host" {
		value = strings.TrimSuffix(value, ".")
	}
	return value
}

// Values are stored lowercase, and lookups are case-insensitive.
// AddBan inserts the ban, replacing any existing one for the same kind and value.
func (s *Store) AddBan(ctx context.Context, b Ban) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO bans (kind, value, reason, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (kind, value) DO UPDATE SET reason = excluded.reason`,
		b.Kind, banValue(b.Kind, b.Value), b.Reason, ts(time.Now()))
	return err
}

func (s *Store) RemoveBan(ctx context.Context, kind, value string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bans WHERE kind = ? AND value = ?`, kind, banValue(kind, value))
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
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM bans WHERE kind = ? AND value = ?`, kind, banValue(kind, value)).Scan(&n)
	return n > 0, err
}

// userBanKey is the ban value that follows a user across GitHub renames.
func userBanKey(id int64) string { return "id:" + strconv.FormatInt(id, 10) }

// BanUser bans u by id only, so the ban follows the user across GitHub
// renames and never passes to whoever takes the login next.
func (s *Store) BanUser(ctx context.Context, u User, reason string) error {
	return s.AddBan(ctx, Ban{Kind: "user", Value: userBanKey(u.ID), Reason: reason})
}

// UnbanUser removes u's id ban and any legacy login ban on u's current login.
func (s *Store) UnbanUser(ctx context.Context, u User) error {
	if err := s.RemoveBan(ctx, "user", u.Login); err != nil {
		return err
	}
	return s.RemoveBan(ctx, "user", userBanKey(u.ID))
}

// IsUserBanned reports whether u's id, or (a legacy ban) its current login, is banned.
func (s *Store) IsUserBanned(ctx context.Context, u User) (bool, error) {
	return s.IsUserBannedBy(ctx, u.Login, u.ID)
}

// IsUserBannedBy is IsUserBanned for a login and numeric id.
func (s *Store) IsUserBannedBy(ctx context.Context, login string, id int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM bans WHERE kind = 'user' AND value IN (?, ?)`,
		strings.ToLower(login), userBanKey(id)).Scan(&n)
	return n > 0, err
}
