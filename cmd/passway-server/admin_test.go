package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohammed19n/passway/internal/server/store"
)

func adm(t *testing.T, db string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runAdmin(context.Background(), append([]string{"--db", db}, args...), &out)
	return out.String(), err
}

func adminDB(t *testing.T) (string, *store.Store, store.User) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := st.UpsertGitHubUser(context.Background(), store.User{GitHubID: 7, Login: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	return db, st, u
}

func TestAdminBanUser(t *testing.T) {
	db, st, u := adminDB(t)
	defer st.Close()
	ctx := context.Background()
	if out, err := adm(t, db, "ban", "user", "alice", "--reason", "spam"); err != nil || strings.Count(out, "\n") != 1 {
		t.Fatalf("ban: %q, %v", out, err)
	}
	if b, _ := st.IsUserBanned(ctx, u); !b {
		t.Fatal("user not banned")
	}
	if _, err := adm(t, db, "unban", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.IsUserBanned(ctx, u); b {
		t.Fatal("user still banned")
	}
	_, err := adm(t, db, "ban", "user", "ghost")
	if err == nil || err.Error() != `no user "ghost"` || exitCode(err) != 1 {
		t.Fatalf("err = %v, want no user \"ghost\" exit 1", err)
	}
	if _, err := adm(t, db, "ban", "host", "Evil", "--reason", "phish"); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.IsBanned(ctx, "host", "evil"); !b {
		t.Fatal("host not banned")
	}
	if _, err := adm(t, db, "unban", "host", "evil"); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.IsBanned(ctx, "host", "evil"); b {
		t.Fatal("host still banned")
	}
}

func TestAdminSetPlan(t *testing.T) {
	db, st, u := adminDB(t)
	defer st.Close()
	if out, err := adm(t, db, "set-plan", "alice", "pro"); err != nil || out != "@Alice is now on the Pro plan\n" {
		t.Fatalf("set-plan = %q, %v", out, err)
	}
	got, _ := st.UserByID(context.Background(), u.ID)
	if got.Plan != "pro" {
		t.Fatalf("plan = %q", got.Plan)
	}
	if _, err := adm(t, db, "set-plan", "alice", "gold"); exitCode(err) != 2 {
		t.Fatalf("bad plan err = %v, want usage error", err)
	}
	if _, err := adm(t, db, "set-plan", "ghost", "pro"); exitCode(err) != 1 {
		t.Fatalf("unknown user err = %v, want exit 1", err)
	}
}

func TestAdminWhois(t *testing.T) {
	db, st, u := adminDB(t)
	defer st.Close()
	if err := st.ClaimSubdomain(context.Background(), u.ID, "acme", true, 5); err != nil {
		t.Fatal(err)
	}
	out, err := adm(t, db, "whois", "acme")
	if want := "acme -> @Alice (free, user 1)\n"; err != nil || out != want {
		t.Fatalf("whois = %q, %v; want %q", out, err, want)
	}
	out, err = adm(t, db, "whois", "nobody")
	if want := "nobody -> unclaimed\n"; err != nil || out != want {
		t.Fatalf("whois = %q, %v; want %q", out, err, want)
	}
}

func TestAdminTokens(t *testing.T) {
	db, st, _ := adminDB(t)
	defer st.Close()
	out, err := adm(t, db, "token", "create", "ci")
	if err != nil || !strings.Contains(out, "pw_") {
		t.Fatalf("create: %q, %v", out, err)
	}
	out, err = adm(t, db, "token", "list")
	if err != nil || !strings.Contains(out, "ci") || strings.Count(out, "pw_") != 1 {
		t.Fatalf("list: %q, %v (want name and prefix only)", out, err)
	}
	if _, err := adm(t, db, "token", "revoke", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := adm(t, db, "token", "revoke", "1"); exitCode(err) != 1 {
		t.Fatalf("second revoke err = %v, want exit 1", err)
	}
}

func TestAdminBanSurvivesRename(t *testing.T) {
	db, st, u := adminDB(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := adm(t, db, "ban", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	u2, _, err := st.UpsertGitHubUser(ctx, store.User{GitHubID: u.GitHubID, Login: "alice2"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := st.IsUserBanned(ctx, u2); !b {
		t.Fatal("ban lost after rename")
	}
	if _, err := adm(t, db, "unban", "user", "alice2"); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.IsUserBanned(ctx, u2); b {
		t.Fatal("still banned after unban")
	}
}

func TestAdminHelp(t *testing.T) {
	var out bytes.Buffer
	err := runAdmin(context.Background(), []string{"-h"}, &out)
	if exitCode(err) != 0 || !strings.Contains(out.String(), "usage: passway-server admin") {
		t.Fatalf("err = %v, out = %q; want usage and exit 0", err, out.String())
	}
}

func TestAdminMissingDB(t *testing.T) {
	db := filepath.Join(t.TempDir(), "nope.db")
	for _, args := range [][]string{{"whois", "x"}, {"token", "list"}, {"ban", "user", "x"}, {"admit", "x"}, {"token", "revoke", "1"}} {
		_, err := adm(t, db, args...)
		if err == nil || err.Error() != "database "+db+" does not exist" || exitCode(err) != 1 {
			t.Errorf("%v: err = %v, want missing-database error, exit 1", args, err)
		}
	}
	if _, err := os.Stat(db); err == nil {
		t.Error("admin created the database")
	}
}

func TestAdminAdmit(t *testing.T) {
	db, st, u := adminDB(t)
	defer st.Close()
	ctx := context.Background()
	if err := st.SetWaitlisted(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if out, err := adm(t, db, "admit", "alice"); err != nil || out != "admitted @Alice\n" {
		t.Fatalf("admit = %q, %v", out, err)
	}
	if got, _ := st.UserByID(ctx, u.ID); got.Waitlisted {
		t.Fatal("still waitlisted")
	}
	_, err := adm(t, db, "admit", "ghost")
	if err == nil || err.Error() != `no user "ghost"` || exitCode(err) != 1 {
		t.Fatalf("err = %v, want no user \"ghost\" exit 1", err)
	}
	if _, err := adm(t, db, "admit"); exitCode(err) != 2 {
		t.Fatalf("admit without login: err = %v, want usage error", err)
	}
}

// token create works on a fresh install: it creates the database.
func TestAdminTokenCreateCreatesDB(t *testing.T) {
	db := filepath.Join(t.TempDir(), "new.db")
	out, err := adm(t, db, "token", "create", "ci")
	if err != nil {
		t.Fatalf("token create on a new database: %v", err)
	}
	plain := strings.TrimSpace(out[strings.LastIndex(out, " "):])
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := (store.AdminTokens{S: st}).Authenticate(context.Background(), plain); err != nil {
		t.Fatalf("created token %q does not authenticate: %v", plain, err)
	}
}
