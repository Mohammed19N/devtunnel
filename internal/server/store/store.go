// Package store is Passway's SQLite persistence layer. Callers use only the
// Store methods, so another backend can replace it later.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("store: not found")
	ErrTaken    = errors.New("store: name already taken")
	ErrLimit    = errors.New("store: limit reached")
	ErrDefault  = errors.New("store: cannot release the default subdomain")
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is a handle to the database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies any
// pending migrations.
func Open(path string) (*Store, error) {
	// SQLite decodes %XX in file: URIs, so escape the characters that would
	// otherwise end the path.
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	dsn := "file:" + esc + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		// The applied check shares the transaction (and its write lock) with
		// the migration, so concurrent starters cannot both apply it.
		if err := s.inTx(context.Background(), func(tx *sql.Tx) error {
			// schema_migrations is created by 0001; before that, nothing is applied.
			var tbl int
			if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&tbl); err != nil {
				return err
			}
			if tbl > 0 {
				var n int
				if err := tx.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					return nil
				}
			}
			if _, err := tx.Exec(string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version)
			return err
		}); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// inTx runs fn in a transaction, committing if it returns nil.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func hashOf(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func ts(t time.Time) int64 { return t.Unix() }

func fromTS(v int64) time.Time { return time.Unix(v, 0).UTC() }

func fromNullTS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromTS(v.Int64)
	return &t
}

func isUnique(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	// SQLITE_CONSTRAINT_UNIQUE, SQLITE_CONSTRAINT_PRIMARYKEY
	return se.Code() == 2067 || se.Code() == 1555
}
