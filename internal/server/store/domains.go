package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/server"
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
// when the user already has max domains (max 0 = none allowed). Another
// user's verification does not block it: domains change hands, and a fresh
// DNS proof (VerifyDomain) evicts the previous owner.
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
// transaction, deletes every other user's row for host, pending or verified:
// the caller has just checked userID's DNS proof, which beats any older one.
// ErrNotFound: userID has no row for host (e.g. a concurrent verify by
// another user deleted it). The partial unique index guarantees at most one
// verified row per host.
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
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM custom_domains WHERE host = ? AND user_id != ?`, host, userID); err != nil {
			return err
		}
		if verified.Valid {
			return nil
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE custom_domains SET verified_at = ? WHERE host = ? AND user_id = ?`,
			ts(time.Now()), host, userID)
		return err
	})
	if isUnique(err) {
		return ErrTaken
	}
	return err
}

// RemoveDomain deletes userID's row for host and reports whether that row
// was verified; ErrNotFound if there is none. Only a verified row ever
// carried a live tunnel, so callers evict only when verified is true.
func (s *Store) RemoveDomain(ctx context.Context, userID int64, host string) (verified bool, err error) {
	var at sql.NullString
	err = s.db.QueryRowContext(ctx,
		`DELETE FROM custom_domains WHERE host = ? AND user_id = ? RETURNING verified_at`,
		normHost(host), userID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return at.Valid, nil
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

// AllowCert reports whether an on-demand certificate may be issued for host:
// a verified custom domain whose owner's plan has MaxCustomDomains > 0 and
// isn't banned. nil allows; the error explains a refusal. The plan rule is
// Directory.OwnsDomain's (oldest verified domains win within the limit), so a
// domain that can't carry a tunnel never gets a certificate either.
func (s *Store) AllowCert(ctx context.Context, host string) error {
	host = normHost(host)
	if host == "" {
		return errors.New("no host name")
	}
	var userID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM custom_domains WHERE host = ? AND verified_at IS NOT NULL`, host).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s is not a verified custom domain", host)
	}
	if err != nil {
		return err
	}
	if banned, err := s.IsBanned(ctx, "host", host); err != nil {
		return err
	} else if banned {
		return fmt.Errorf("%s is banned", host)
	}
	u, err := s.UserByID(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%s has no owner", host)
	}
	if err != nil {
		return err
	}
	if banned, err := s.IsUserBanned(ctx, u); err != nil {
		return err
	} else if banned {
		return fmt.Errorf("the owner of %s is banned", host)
	}
	acc := server.Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Plan: server.PlanByName(u.Plan)}
	ok, err := (&Directory{S: s}).OwnsDomain(ctx, acc, host)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not usable on its owner's plan", host)
	}
	return nil
}

// VerifiedDomainOwner returns the owner of the verified custom domain host,
// or ErrNotFound when no one has verified it.
func (s *Store) VerifiedDomainOwner(ctx context.Context, host string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM custom_domains WHERE host = ? AND verified_at IS NOT NULL`, normHost(host)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}
