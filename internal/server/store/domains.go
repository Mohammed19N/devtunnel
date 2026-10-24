package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// CustomDomain is a user's custom domain. Until VerifiedAt is set it is a
// pending claim that several users may hold; verifying it removes the others.
type CustomDomain struct {
	Host       string
	UserID     int64
	Token      string // the TXT record value is "passway-verify=" + Token
	VerifiedAt *time.Time
	CreatedAt  time.Time
}

// normHost lowercases host and drops a trailing dot. Callers validate it
// first (see server.NormalizeDomain).
func normHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func domainToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// AddDomain records a pending custom domain for userID with a fresh token.
// Adding one the user already has returns it unchanged. It fails with ErrLimit
// when the user already has max domains (max 0 = none allowed) and with
// ErrTaken when another user has verified host.
func (s *Store) AddDomain(ctx context.Context, userID int64, host string, max int) (CustomDomain, error) {
	host = normHost(host)
	var out CustomDomain
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanDomain(tx.QueryRowContext(ctx,
			`SELECT host, user_id, token, verified_at, created_at FROM custom_domains WHERE host = ? AND user_id = ?`, host, userID))
		if err == nil {
			out = cur
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM custom_domains WHERE user_id = ?`, userID).Scan(&n); err != nil {
			return err
		}
		if max <= 0 || n >= max {
			return ErrLimit
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM custom_domains WHERE host = ? AND verified_at IS NOT NULL`, host).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrTaken
		}
		tok, err := domainToken()
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO custom_domains (host, user_id, token, created_at) VALUES (?, ?, ?, ?)`,
			host, userID, tok, ts(now)); err != nil {
			return err
		}
		out = CustomDomain{Host: host, UserID: userID, Token: tok, CreatedAt: fromTS(ts(now))}
		return nil
	})
	if isUnique(err) {
		return CustomDomain{}, ErrTaken
	}
	return out, err
}

// VerifyDomain marks userID's row for host verified and, in the same
// transaction, deletes every other user's pending row for host. The caller
// checks DNS first. ErrNotFound: userID has no row for host; ErrTaken:
// another user verified it first.
func (s *Store) VerifyDomain(ctx context.Context, userID int64, host string) error {
	host = normHost(host)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var verified sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT verified_at FROM custom_domains WHERE host = ? AND user_id = ?`, host, userID).Scan(&verified)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !verified.Valid {
			if _, err := tx.ExecContext(ctx,
				`UPDATE custom_domains SET verified_at = ? WHERE host = ? AND user_id = ?`,
				ts(time.Now()), host, userID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM custom_domains WHERE host = ? AND user_id != ? AND verified_at IS NULL`, host, userID)
		return err
	})
	if isUnique(err) {
		return ErrTaken
	}
	return err
}

// RemoveDomain deletes userID's row for host; ErrNotFound if there is none.
func (s *Store) RemoveDomain(ctx context.Context, userID int64, host string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM custom_domains WHERE host = ? AND user_id = ?`, normHost(host), userID)
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

// Domains lists userID's custom domains, oldest first.
func (s *Store) Domains(ctx context.Context, userID int64) ([]CustomDomain, error) {
	return s.queryDomains(ctx,
		`SELECT host, user_id, token, verified_at, created_at FROM custom_domains WHERE user_id = ? ORDER BY created_at, host`, userID)
}

// verifiedDomains lists userID's verified domains, oldest verified first.
func (s *Store) verifiedDomains(ctx context.Context, userID int64) ([]CustomDomain, error) {
	return s.queryDomains(ctx,
		`SELECT host, user_id, token, verified_at, created_at FROM custom_domains
		 WHERE user_id = ? AND verified_at IS NOT NULL ORDER BY verified_at, created_at, host`, userID)
}

func (s *Store) queryDomains(ctx context.Context, q string, args ...any) ([]CustomDomain, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustomDomain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDomain(r scanner) (CustomDomain, error) {
	var d CustomDomain
	var verified sql.NullInt64
	var created int64
	err := r.Scan(&d.Host, &d.UserID, &d.Token, &verified, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return CustomDomain{}, ErrNotFound
	}
	if err != nil {
		return CustomDomain{}, err
	}
	d.VerifiedAt = fromNullTS(verified)
	d.CreatedAt = fromTS(created)
	return d, nil
}
