package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Subdomain struct {
	Name      string
	UserID    int64
	IsDefault bool
	CreatedAt time.Time
}

// ClaimSubdomain fails with ErrTaken if anyone holds name and ErrLimit if the
// user already holds max subdomains (max 0 = unlimited).
func (s *Store) ClaimSubdomain(ctx context.Context, userID int64, name string, isDefault bool, max int) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if max > 0 {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM subdomains WHERE user_id = ?`, userID).Scan(&n); err != nil {
				return err
			}
			if n >= max {
				return ErrLimit
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO subdomains (name, user_id, is_default, created_at) VALUES (?, ?, ?, ?)`,
			name, userID, isDefault, ts(time.Now()))
		return err
	})
	if isUnique(err) {
		return ErrTaken
	}
	return err
}

// ReleaseSubdomain frees a non-default subdomain owned by userID.
func (s *Store) ReleaseSubdomain(ctx context.Context, userID int64, name string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var def bool
		err := tx.QueryRowContext(ctx, `SELECT is_default FROM subdomains WHERE name = ? AND user_id = ?`, name, userID).Scan(&def)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if def {
			return ErrDefault
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM subdomains WHERE name = ?`, name)
		return err
	})
}

// Subdomains lists a user's subdomains, default first, then by creation time.
func (s *Store) Subdomains(ctx context.Context, userID int64) ([]Subdomain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, user_id, is_default, created_at FROM subdomains WHERE user_id = ? ORDER BY is_default DESC, created_at, name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subdomain
	for rows.Next() {
		var d Subdomain
		var created int64
		if err := rows.Scan(&d.Name, &d.UserID, &d.IsDefault, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = fromTS(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SubdomainOwner(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM subdomains WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}
