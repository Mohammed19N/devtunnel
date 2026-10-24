package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type User struct {
	ID         int64
	GitHubID   int64
	Login      string
	Name       string
	AvatarURL  string
	Plan       string // "free" | "pro"
	Waitlisted bool
	BannedAt   *time.Time
	CreatedAt  time.Time
}

const userCols = `id, github_id, login, name, avatar_url, plan, waitlisted, banned_at, created_at`

type scanner interface{ Scan(...any) error }

func scanUser(r scanner) (User, error) {
	var u User
	var banned sql.NullInt64
	var created int64
	err := r.Scan(&u.ID, &u.GitHubID, &u.Login, &u.Name, &u.AvatarURL, &u.Plan, &u.Waitlisted, &banned, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	u.BannedAt = fromNullTS(banned)
	u.CreatedAt = fromTS(created)
	return u, err
}

// UpsertGitHubUser inserts or updates by GitHubID. Login, name and avatar are
// refreshed; plan and waitlist state are kept.
func (s *Store) UpsertGitHubUser(ctx context.Context, u User) (User, bool, error) {
	return s.upsertGitHubUser(ctx, u, -1)
}

// SignupGitHubUser upserts u like UpsertGitHubUser. When it creates the user,
// in the same IMMEDIATE transaction, it waitlists them if admitted Free users
// are already at freeCap, so concurrent signups never exceed freeCap. An
// existing user keeps their state.
func (s *Store) SignupGitHubUser(ctx context.Context, u User, freeCap int) (User, bool, error) {
	return s.upsertGitHubUser(ctx, u, freeCap)
}

// upsertGitHubUser is the shared body; freeCap < 0 trusts u.Waitlisted.
func (s *Store) upsertGitHubUser(ctx context.Context, u User, freeCap int) (User, bool, error) {
	var out User
	var created bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// A login may have been renamed on GitHub without us noticing; free
		// it up by renaming any other holder to a placeholder ('~' cannot
		// appear in a GitHub login).
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET login = login || '~' || id WHERE lower(login) = lower(?) AND github_id <> ?`,
			u.Login, u.GitHubID); err != nil {
			return err
		}
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE github_id = ?`, u.GitHubID).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			created = true
			if freeCap >= 0 {
				var n int
				if err := tx.QueryRowContext(ctx,
					`SELECT count(*) FROM users WHERE plan = 'free' AND waitlisted = 0`).Scan(&n); err != nil {
					return err
				}
				u.Waitlisted = n >= freeCap
			}
			res, err := tx.ExecContext(ctx,
				`INSERT INTO users (github_id, login, name, avatar_url, waitlisted, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				u.GitHubID, u.Login, u.Name, u.AvatarURL, u.Waitlisted, ts(time.Now()))
			if err != nil {
				return err
			}
			if id, err = res.LastInsertId(); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET login = ?, name = ?, avatar_url = ? WHERE id = ?`,
				u.Login, u.Name, u.AvatarURL, id); err != nil {
				return err
			}
		}
		out, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
		return err
	})
	if err != nil {
		return User{}, false, err
	}
	return out, created, nil
}

func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// UserByLogin matches case-insensitively.
func (s *Store) UserByLogin(ctx context.Context, login string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE lower(login) = lower(?)`, login))
}

func (s *Store) updateUser(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetPlan(ctx context.Context, userID int64, plan string) error {
	return s.updateUser(ctx, `UPDATE users SET plan = ? WHERE id = ?`, plan, userID)
}

func (s *Store) SetWaitlisted(ctx context.Context, userID int64, w bool) error {
	return s.updateUser(ctx, `UPDATE users SET waitlisted = ? WHERE id = ?`, w, userID)
}

// CountUsers counts non-waitlisted users on plan.
func (s *Store) CountUsers(ctx context.Context, plan string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE plan = ? AND waitlisted = 0`, plan).Scan(&n)
	return n, err
}
