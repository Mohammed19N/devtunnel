package store

import (
	"context"
	"database/sql"
	"time"
)

// AbuseReport is a report sent through the public abuse form.
type AbuseReport struct {
	ID                                int64
	URL, Category, Details, Email, IP string
	CreatedAt                         time.Time
	ResolvedAt                        *time.Time // nil while open
}

// AddAbuseReport stores r (its ID, CreatedAt and ResolvedAt are ignored) and
// returns the new report's id.
func (s *Store) AddAbuseReport(ctx context.Context, r AbuseReport) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO abuse_reports (url, category, details, email, ip, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.URL, r.Category, r.Details, r.Email, r.IP, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AbuseReports returns up to limit reports, newest first; unresolvedOnly
// leaves out resolved ones.
func (s *Store) AbuseReports(ctx context.Context, unresolvedOnly bool, limit int) ([]AbuseReport, error) {
	q := `SELECT id, url, category, details, email, ip, created_at, resolved_at FROM abuse_reports`
	if unresolvedOnly {
		q += ` WHERE resolved_at IS NULL`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AbuseReport
	for rows.Next() {
		var r AbuseReport
		var created int64
		var resolved sql.NullInt64
		if err := rows.Scan(&r.ID, &r.URL, &r.Category, &r.Details, &r.Email, &r.IP, &created, &resolved); err != nil {
			return nil, err
		}
		r.CreatedAt, r.ResolvedAt = fromTS(created), fromNullTS(resolved)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveAbuseReport marks report id resolved. An unknown id is ErrNotFound;
// resolving twice keeps the first time.
func (s *Store) ResolveAbuseReport(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE abuse_reports SET resolved_at = coalesce(resolved_at, ?) WHERE id = ?`, ts(time.Now()), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
