package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Token struct {
	ID, UserID int64
	Name       string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

const tokenCols = `id, COALESCE(user_id, 0), name, prefix, created_at, last_used_at, revoked_at`

func scanToken(r scanner) (Token, error) {
	var t Token
	var created int64
	var used, revoked sql.NullInt64
	err := r.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &created, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	t.CreatedAt = fromTS(created)
	t.LastUsedAt = fromNullTS(used)
	t.RevokedAt = fromNullTS(revoked)
	return t, err
}

// CreateToken returns the plaintext once; only its sha256 is stored. userID 0
// creates a self-hosted admin token (user_id NULL).
func (s *Store) CreateToken(ctx context.Context, userID int64, name string) (string, Token, error) {
	rnd, err := randomB64(32)
	if err != nil {
		return "", Token{}, err
	}
	plain := "pw_" + rnd
	var uid any
	if userID != 0 {
		uid = userID
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (user_id, name, hash, prefix, created_at) VALUES (?, ?, ?, ?, ?)`,
		uid, name, hashOf(plain), plain[:10], ts(now))
	if err != nil {
		return "", Token{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", Token{}, err
	}
	return plain, Token{ID: id, UserID: userID, Name: name, Prefix: plain[:10], CreatedAt: fromTS(ts(now))}, nil
}

// TokenByPlain finds a live token and records its use.
func (s *Store) TokenByPlain(ctx context.Context, plain string) (Token, error) {
	var t Token
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = scanToken(tx.QueryRowContext(ctx,
			`SELECT `+tokenCols+` FROM tokens WHERE hash = ? AND revoked_at IS NULL`, hashOf(plain)))
		if err != nil {
			return err
		}
		now := fromTS(ts(time.Now()))
		t.LastUsedAt = &now
		_, err = tx.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, now.Unix(), t.ID)
		return err
	})
	if err != nil {
		return Token{}, err
	}
	return t, nil
}

// ListTokens lists a user's tokens, or the admin tokens for userID 0.
func (s *Store) ListTokens(ctx context.Context, userID int64) ([]Token, error) {
	q := `SELECT ` + tokenCols + ` FROM tokens WHERE user_id = ? ORDER BY id`
	args := []any{userID}
	if userID == 0 {
		q = `SELECT ` + tokenCols + ` FROM tokens WHERE user_id IS NULL ORDER BY id`
		args = nil
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken revokes a live token owned by userID (0 = admin tokens).
func (s *Store) RevokeToken(ctx context.Context, userID, tokenID int64) error {
	q := `UPDATE tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`
	args := []any{ts(time.Now()), tokenID, userID}
	if userID == 0 {
		q = `UPDATE tokens SET revoked_at = ? WHERE id = ? AND user_id IS NULL AND revoked_at IS NULL`
		args = args[:2]
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
