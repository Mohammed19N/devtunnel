package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/protocol"
	"github.com/mohammed19n/passway/internal/server"
)

func TestDirectoryAuthenticate(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	d := &Directory{S: s}
	u := mkUser(t, s, 1, "alice")
	if err := s.SetPlan(ctx, u.ID, "pro"); err != nil {
		t.Fatal(err)
	}
	plain, tok, err := s.CreateToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}

	acc, err := d.Authenticate(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "1" || acc.Login != "alice" || acc.Plan != server.PlanPro || acc.TokenID != tok.ID {
		t.Fatalf("got %+v", acc)
	}
	_, err = d.Authenticate(ctx, "pw_unknown")
	var pe *protocol.Error
	if !errors.Is(err, server.ErrInvalidToken) || errors.As(err, &pe) {
		t.Fatalf("unknown token: %v", err)
	}

	// Admin tokens are for self-hosted relays only.
	adminPlain, _, err := s.CreateToken(ctx, 0, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Authenticate(ctx, adminPlain); !errors.As(err, &pe) || pe.Code != protocol.CodeUnauthorized ||
		pe.Message != "admin tokens only work on self-hosted servers" {
		t.Fatalf("admin token in cloud mode: %v", err)
	}

	// Waitlisted.
	if err := s.SetWaitlisted(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Authenticate(ctx, plain); !errors.As(err, &pe) || pe.Code != protocol.CodeUnauthorized || pe.Message != "your account is on the waitlist" {
		t.Fatalf("waitlist: %v", err)
	}

	// Banned beats waitlisted.
	if err := s.AddBan(ctx, Ban{Kind: "user", Value: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Authenticate(ctx, plain); !errors.As(err, &pe) || pe.Code != protocol.CodeBanned || pe.Message != "account suspended" {
		t.Fatalf("banned: %v", err)
	}
	if err := s.RemoveBan(ctx, "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWaitlisted(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}

	// Revoked.
	if err := s.RevokeToken(ctx, u.ID, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Authenticate(ctx, plain); err == nil {
		t.Fatal("revoked token must fail")
	}
}

func TestDirectoryNamesDefaultFirst(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	d := &Directory{S: s}
	u := mkUser(t, s, 1, "alice")
	for _, c := range []struct {
		name string
		def  bool
	}{{"extra", false}, {"main", true}} {
		if err := s.ClaimSubdomain(ctx, u.ID, c.name, c.def, 0); err != nil {
			t.Fatal(err)
		}
	}
	acc := server.Account{ID: "1", Login: "alice"}
	names, err := d.Names(ctx, acc)
	if err != nil || !reflect.DeepEqual(names, []string{"main", "extra"}) {
		t.Fatalf("names = %v, %v", names, err)
	}
	if ok, err := d.Owns(ctx, acc, "extra"); err != nil || !ok {
		t.Fatalf("Owns extra = %v, %v", ok, err)
	}
	if ok, err := d.Owns(ctx, server.Account{ID: "2"}, "extra"); err != nil || ok {
		t.Fatalf("bob Owns extra = %v, %v", ok, err)
	}
	if ok, err := d.Owns(ctx, acc, "nobody"); err != nil || ok {
		t.Fatalf("Owns unclaimed = %v, %v", ok, err)
	}
}

func TestDirectoryBannedHost(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	d := &Directory{S: s}
	if err := s.AddBan(ctx, Ban{Kind: "host", Value: "Evil"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBan(ctx, Ban{Kind: "user", Value: "mallory"}); err != nil {
		t.Fatal(err)
	}
	acc := server.Account{ID: "1", Login: "alice"}
	if b, err := d.Banned(ctx, acc, "EVIL"); err != nil || !b {
		t.Fatalf("evil banned = %v, %v", b, err)
	}
	if b, err := d.Banned(ctx, acc, "fine"); err != nil || b {
		t.Fatalf("fine banned = %v, %v", b, err)
	}
	if b, err := d.Banned(ctx, server.Account{ID: "9", Login: "Mallory"}, "fine"); err != nil || !b {
		t.Fatalf("mallory banned = %v, %v", b, err)
	}
	users, hosts, err := d.BannedLists(ctx)
	if err != nil || !reflect.DeepEqual(users, []string{"mallory"}) || !reflect.DeepEqual(hosts, []string{"evil"}) {
		t.Fatalf("lists = %v %v %v", users, hosts, err)
	}
}

func TestDirectoryUsageMonth(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	d := &Directory{S: s, Now: func() time.Time { return now }}
	mkUser(t, s, 1, "alice")
	acc := server.Account{ID: "1", Login: "alice", Plan: server.PlanFree}
	if err := d.AddUsage(ctx, acc, 100); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour) // November
	if n, _ := d.Usage(ctx, acc); n != 0 {
		t.Fatalf("november usage = %d", n)
	}
	now = now.Add(-2 * time.Hour)
	if n, _ := d.Usage(ctx, acc); n != 100 {
		t.Fatalf("october usage = %d", n)
	}
	if n, err := d.PlanUsage(ctx, "free"); err != nil || n != 100 {
		t.Fatalf("plan usage = %d, %v", n, err)
	}
}

func TestDirectoryMonthIsUTC(t *testing.T) {
	d := &Directory{Now: func() time.Time {
		return time.Date(2026, 10, 31, 23, 30, 0, 0, time.FixedZone("x", -5*3600))
	}}
	if got := d.month(); got != "2026-11" {
		t.Fatalf("month = %q", got)
	}
}

func TestDirectoryDeletedUserIsInvalidToken(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u := mkUser(t, s, 1, "alice")
	plain, _, err := s.CreateToken(ctx, u.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Directory{S: s}).Authenticate(ctx, plain); !errors.Is(err, server.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestBanUserSurvivesRename(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	d := &Directory{S: s}
	u := mkUser(t, s, 1, "alice")
	plain, _, err := s.CreateToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BanUser(ctx, u, "abuse"); err != nil {
		t.Fatal(err)
	}
	renamed, _, err := s.UpsertGitHubUser(ctx, User{GitHubID: 1, Login: "alice2"})
	if err != nil || renamed.ID != u.ID || renamed.Login != "alice2" {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	var pe *protocol.Error
	if _, err := d.Authenticate(ctx, plain); !errors.As(err, &pe) || pe.Code != protocol.CodeBanned {
		t.Fatalf("renamed banned user authenticated: %v", err)
	}
	if b, err := d.Banned(ctx, server.Account{ID: "1", Login: "alice2"}, ""); err != nil || !b {
		t.Fatalf("Banned after rename = %v, %v", b, err)
	}
	if b, err := s.IsUserBanned(ctx, renamed); err != nil || !b {
		t.Fatalf("IsUserBanned = %v, %v", b, err)
	}
	users, _, _ := d.BannedLists(ctx)
	if !reflect.DeepEqual(users, []string{"id:1"}) {
		t.Errorf("BannedLists users = %v", users)
	}
	if err := s.UnbanUser(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Authenticate(ctx, plain); err != nil {
		t.Fatalf("after unban: %v", err)
	}
}

// A user ban follows the user, not the login: after alice bans, renames and
// is unbanned, a new user who takes the login "alice" starts clean.
func TestUserBanNotPassedToNewLoginHolder(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	d := &Directory{S: s}
	u := mkUser(t, s, 1, "alice")
	if err := s.BanUser(ctx, u, "abuse"); err != nil {
		t.Fatal(err)
	}
	renamed := mkUser(t, s, 1, "alice2")
	if err := s.UnbanUser(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	stranger := mkUser(t, s, 2, "alice")
	plain, _, err := s.CreateToken(ctx, stranger.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := s.IsUserBanned(ctx, stranger); err != nil || b {
		t.Fatalf("IsUserBanned(new alice) = %v, %v", b, err)
	}
	if _, err := d.Authenticate(ctx, plain); err != nil {
		t.Fatalf("Authenticate(new alice): %v", err)
	}
	if users, _, err := d.BannedLists(ctx); err != nil || len(users) != 0 {
		t.Fatalf("BannedLists users = %v, %v; want none", users, err)
	}
}

// UnbanUser also clears a legacy login-keyed row for the user's login, and
// such rows are still honoured until then.
func TestUnbanUserClearsLegacyLoginRow(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u := mkUser(t, s, 1, "alice")
	if err := s.AddBan(ctx, Ban{Kind: "user", Value: "alice"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.IsUserBanned(ctx, u); !b {
		t.Fatal("legacy login ban not honoured")
	}
	if err := s.UnbanUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if bans, _ := s.Bans(ctx); len(bans) != 0 {
		t.Fatalf("bans left: %+v", bans)
	}
}

func TestDirectoryRevokedTokens(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	d := &Directory{S: s}
	u := mkUser(t, s, 1, "alice")
	var ids []int64
	for _, name := range []string{"a", "b", "c"} {
		_, tok, err := s.CreateToken(ctx, u.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tok.ID)
	}
	if err := s.RevokeToken(ctx, u.ID, ids[1]); err != nil {
		t.Fatal(err)
	}
	got, err := d.RevokedTokens(ctx, append(ids, 999))
	if err != nil || !reflect.DeepEqual(got, []int64{ids[1]}) {
		t.Fatalf("RevokedTokens = %v, %v; want [%d]", got, err, ids[1])
	}
	if got, err := d.RevokedTokens(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("RevokedTokens(nil) = %v, %v", got, err)
	}
}

func TestDowngradeBoundsNames(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	d := &Directory{S: s}
	u := mkUser(t, s, 1, "alice")
	if err := s.SetPlan(ctx, u.ID, "pro"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		def  bool
	}{{"acme", false}, {"main", true}, {"beta", false}} {
		if err := s.ClaimSubdomain(ctx, u.ID, c.name, c.def, 0); err != nil {
			t.Fatal(err)
		}
	}
	acc := func(plan string) server.Account {
		return server.Account{ID: "1", Login: "alice", Plan: server.PlanByName(plan)}
	}
	check := func(plan string, want []string, owns map[string]bool) {
		t.Helper()
		names, err := d.Names(ctx, acc(plan))
		if err != nil || !reflect.DeepEqual(names, want) {
			t.Fatalf("%s: names = %v, %v; want %v", plan, names, err, want)
		}
		for n, w := range owns {
			if ok, err := d.Owns(ctx, acc(plan), n); err != nil || ok != w {
				t.Fatalf("%s: Owns(%s) = %v, %v; want %v", plan, n, ok, err, w)
			}
		}
	}
	check("pro", []string{"main", "acme", "beta"}, map[string]bool{"acme": true, "beta": true})
	check("free", []string{"main"}, map[string]bool{"main": true, "acme": false, "beta": false})
	if o, _ := s.SubdomainOwner(ctx, "acme"); o != u.ID {
		t.Fatalf("acme released on downgrade: owner %d", o)
	}
	check("pro", []string{"main", "acme", "beta"}, map[string]bool{"acme": true, "beta": true})
}
