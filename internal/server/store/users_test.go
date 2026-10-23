package store

import (
	"context"
	"errors"
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

func TestCountUsersExcludesWaitlisted(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, b := mkUser(t, s, 1, "a"), mkUser(t, s, 2, "b")
	mkUser(t, s, 3, "c")
	if err := s.SetWaitlisted(ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountUsers(ctx, "free"); n != 2 {
		t.Fatalf("free = %d", n)
	}
	s.SetPlan(ctx, a.ID, "pro")
	if n, _ := s.CountUsers(ctx, "free"); n != 1 {
		t.Fatalf("free = %d", n)
	}
	if err := s.SetPlan(ctx, 999, "pro"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
