package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/protocol"
	"github.com/mohammed19n/passway/internal/server"
)

// Directory implements server.Directory on top of a Store.
type Directory struct {
	S   *Store
	Now func() time.Time // nil = time.Now
}

var _ server.Directory = (*Directory)(nil)

func (d *Directory) month() string {
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	return now().UTC().Format("2006-01")
}

// Authenticate resolves a token to its account. An admin token (user 0) maps
// to an unlimited "admin-token" account.
func (d *Directory) Authenticate(ctx context.Context, token string) (server.Account, error) {
	t, err := d.S.TokenByPlain(ctx, token)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return server.Account{}, server.ErrInvalidToken
		}
		return server.Account{}, err
	}
	if t.UserID == 0 {
		return server.Account{ID: "admin-token", Plan: server.PlanUnlimited}, nil
	}
	u, err := d.S.UserByID(ctx, t.UserID)
	if errors.Is(err, ErrNotFound) {
		return server.Account{}, server.ErrInvalidToken
	}
	if err != nil {
		return server.Account{}, err
	}
	banned, err := d.S.IsUserBanned(ctx, u)
	if err != nil {
		return server.Account{}, err
	}
	if banned {
		return server.Account{}, &protocol.Error{Code: protocol.CodeBanned, Message: "account suspended"}
	}
	if u.Waitlisted {
		return server.Account{}, &protocol.Error{Code: protocol.CodeUnauthorized, Message: "your account is on the waitlist"}
	}
	return server.Account{
		ID:    strconv.FormatInt(u.ID, 10),
		Login: u.Login,
		Plan:  server.PlanByName(u.Plan),
	}, nil
}

// userID parses acc.ID; ok is false for non-database accounts.
func userID(acc server.Account) (int64, bool) {
	id, err := strconv.ParseInt(acc.ID, 10, 64)
	return id, err == nil
}

// Names returns acc's subdomains, default first.
func (d *Directory) Names(ctx context.Context, acc server.Account) ([]string, error) {
	id, ok := userID(acc)
	if !ok {
		return nil, nil
	}
	subs, err := d.S.Subdomains(ctx, id)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(subs))
	for i, s := range subs {
		names[i] = s.Name
	}
	return names, nil
}

// Owns reports whether acc owns name.
func (d *Directory) Owns(ctx context.Context, acc server.Account, name string) (bool, error) {
	id, ok := userID(acc)
	if !ok {
		return false, nil
	}
	owner, err := d.S.SubdomainOwner(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil && owner == id, err
}

// Banned reports whether acc's login or the host label is banned.
func (d *Directory) Banned(ctx context.Context, acc server.Account, label string) (bool, error) {
	if id, ok := userID(acc); ok {
		if b, err := d.S.IsUserBannedBy(ctx, acc.Login, id); err != nil || b {
			return b, err
		}
	} else if acc.Login != "" {
		if b, err := d.S.IsBanned(ctx, "user", acc.Login); err != nil || b {
			return b, err
		}
	}
	if label == "" {
		return false, nil
	}
	return d.S.IsBanned(ctx, "host", strings.ToLower(label))
}

// AddUsage records bytes for acc this month; non-database accounts are ignored.
func (d *Directory) AddUsage(ctx context.Context, acc server.Account, bytes int64) error {
	id, ok := userID(acc)
	if !ok {
		return nil
	}
	return d.S.AddUsage(ctx, id, d.month(), bytes)
}

// Usage returns acc's bytes this month.
func (d *Directory) Usage(ctx context.Context, acc server.Account) (int64, error) {
	id, ok := userID(acc)
	if !ok {
		return 0, nil
	}
	return d.S.Usage(ctx, id, d.month())
}

// PlanUsage returns this month's bytes across all accounts on plan.
func (d *Directory) PlanUsage(ctx context.Context, plan string) (int64, error) {
	return d.S.PlanUsage(ctx, plan, d.month())
}

// BannedLists returns banned user logins (and "id:<n>" keys) and host labels.
func (d *Directory) BannedLists(ctx context.Context) (users, hosts []string, err error) {
	bans, err := d.S.Bans(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, b := range bans {
		switch b.Kind {
		case "user":
			users = append(users, b.Value)
		case "host":
			hosts = append(hosts, b.Value)
		}
	}
	return users, hosts, nil
}

// AdminTokens authenticates self-hosted admin tokens (user 0) only; a token
// that belongs to a cloud user is rejected.
type AdminTokens struct{ S *Store }

var _ server.Authenticator = AdminTokens{}

// Authenticate implements server.Authenticator.
func (a AdminTokens) Authenticate(ctx context.Context, token string) (server.Account, error) {
	t, err := a.S.TokenByPlain(ctx, token)
	if errors.Is(err, ErrNotFound) || (err == nil && t.UserID != 0) {
		return server.Account{}, server.ErrInvalidToken
	}
	if err != nil {
		return server.Account{}, err
	}
	return server.Account{ID: "admin-token", Plan: server.PlanUnlimited}, nil
}
