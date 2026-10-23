package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

type Session struct {
	UserID    int64
	CSRF      string
	ExpiresAt time.Time
}

func sessionKey(id string) string { return hex.EncodeToString(hashOf(id)) }

// CreateSession returns a new random session id (shown once, never stored; the
// DB keeps its sha256) together with its CSRF token.
func (s *Store) CreateSession(ctx context.Context, userID int64, ttl time.Duration) (string, Session, error) {
	id, err := randomB64(32)
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := randomB64(32)
	if err != nil {
		return "", Session{}, err
	}
	now := time.Now()
	exp := now.Add(ttl)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id_hash, user_id, csrf, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		sessionKey(id), userID, csrf, ts(now), ts(exp)); err != nil {
		return "", Session{}, err
	}
	return id, Session{UserID: userID, CSRF: csrf, ExpiresAt: fromTS(ts(exp))}, nil
}

// SessionByID returns ErrNotFound for unknown or expired sessions.
func (s *Store) SessionByID(ctx context.Context, id string) (Session, error) {
	var sess Session
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, csrf, expires_at FROM sessions WHERE id_hash = ?`, sessionKey(id)).
		Scan(&sess.UserID, &sess.CSRF, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	sess.ExpiresAt = fromTS(exp)
	if !sess.ExpiresAt.After(time.Now()) {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, sessionKey(id))
	return err
}
