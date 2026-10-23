package store

import (
	"context"
	"database/sql"
	"errors"
)

// Setting returns the value for key, creating it with gen on first use. gen
// runs outside the transaction, so a race may call it more than once, but only
// one value is ever stored and every caller gets that one.
func (s *Store) Setting(ctx context.Context, key string, gen func() []byte) ([]byte, error) {
	get := func() ([]byte, error) {
		var val []byte
		err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
		return val, err
	}
	val, err := get()
	if err == nil {
		return val, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	val = gen()
	if len(val) == 0 {
		return nil, errors.New("store: setting generator returned no value")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`, key, val); err != nil {
		return nil, err
	}
	return get()
}
