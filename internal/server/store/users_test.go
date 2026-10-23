package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestUpsertGitHubUser(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u, created, err := s.UpsertGitHubUser(ctx, User{GitHubID: 7, Login: "Old", Name: "N"})
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	if u.Plan != "free" || u.ID == 0 || u.CreatedAt.IsZero() {
		t.Fatalf("user = %+v", u)
	}
	if err := s.SetPlan(ctx, u.ID, "pro"); err != nil {
		t.Fatal(err)
	}
	u2, created, err := s.UpsertGitHubUser(ctx, User{GitHubID: 7, Login: "New", Name: "N2", AvatarURL: "a"})
	if err != nil || created {
		t.Fatalf("second: created=%v err=%v", created, err)
	}
	if u2.ID != u.ID || u2.Login != "New" || u2.Name != "N2" || u2.AvatarURL != "a" || u2.Plan != "pro" {
		t.Fatalf("u2 = %+v", u2)
	}
	got, err := s.UserByLogin(ctx, "nEW")
	if err != nil || got.ID != u.ID {
		t.Fatalf("UserByLogin: %+v %v", got, err)
	}
	if _, err := s.UserByLogin(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.UserByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestUpsertRenamedLoginCollision(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := mkUser(t, s, 1, "foo")
	// B signs up with A's old login before A logs in again.
	b, created, err := s.UpsertGitHubUser(ctx, User{GitHubID: 2, Login: "Foo"})
	if err != nil || !created || b.Login != "Foo" {
		t.Fatalf("B: %+v %v %v", b, created, err)
	}
	got, err := s.UserByID(ctx, a.ID)
	if err != nil || got.Login != fmt.Sprintf("foo~%d", a.ID) {
		t.Fatalf("A login = %q %v", got.Login, err)
	}
	if _, _, err := s.UpsertGitHubUser(ctx, User{GitHubID: 1, Login: "bar"}); err != nil {
		t.Fatalf("A rename: %v", err)
	}
	// B renames away, then A takes foo again (update branch collision).
	if _, _, err := s.UpsertGitHubUser(ctx, User{GitHubID: 2, Login: "baz"}); err != nil {
		t.Fatal(err)
	}
	a2, _, err := s.UpsertGitHubUser(ctx, User{GitHubID: 1, Login: "foo"})
	if err != nil || a2.Login != "foo" {
		t.Fatalf("A back: %+v %v", a2, err)
	}
	// C takes foo while A holds it: update-branch collision for an existing user.
	if _, _, err := s.UpsertGitHubUser(ctx, User{GitHubID: 2, Login: "foo"}); err != nil {
		t.Fatalf("B takes foo: %v", err)
	}
}

func TestCountUsersExcludesWaitlisted(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, b := mkUser(t, s, 1, "a"), mkUser(t, s, 2, "b")
	mkUser(t, s, 3, "c")
	if err := s.SetWaitlisted(ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountUsers(ctx, "free"); err != nil || n != 2 {
		t.Fatalf("free = %d %v", n, err)
	}
	if err := s.SetPlan(ctx, a.ID, "pro"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountUsers(ctx, "free"); err != nil || n != 1 {
		t.Fatalf("free = %d %v", n, err)
	}
	if err := s.SetPlan(ctx, 999, "pro"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
