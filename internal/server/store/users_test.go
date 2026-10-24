package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

func TestSignupCapConcurrent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const cap, n = 5, 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.SignupGitHubUser(ctx, User{GitHubID: int64(1000 + i), Login: fmt.Sprintf("u%d", i)}, cap)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := s.CountUsers(ctx, "free")
	if err != nil {
		t.Fatal(err)
	}
	if got != cap {
		t.Errorf("admitted free users = %d, want %d", got, cap)
	}
}

func TestSignupWaitlistedNotCounted(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, created, err := s.SignupGitHubUser(ctx, User{GitHubID: 1, Login: "a"}, 1)
	if err != nil || !created || a.Waitlisted {
		t.Fatalf("A = %+v created=%v err=%v, want admitted", a, created, err)
	}
	for i, login := range []string{"b", "c"} {
		u, created, err := s.SignupGitHubUser(ctx, User{GitHubID: int64(2 + i), Login: login}, 1)
		if err != nil || !created || !u.Waitlisted {
			t.Fatalf("%s = %+v created=%v err=%v, want waitlisted", login, u, created, err)
		}
	}
	if n, _ := s.CountUsers(ctx, "free"); n != 1 {
		t.Errorf("CountUsers(free) = %d, want 1", n)
	}
	// An existing user keeps their state on sign-in.
	again, created, err := s.SignupGitHubUser(ctx, User{GitHubID: 1, Login: "a"}, 1)
	if err != nil || created || again.Waitlisted {
		t.Errorf("A again = %+v created=%v err=%v", again, created, err)
	}
}

func TestOpenExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "none.db")
	_, err := OpenExisting(path)
	if err == nil || err.Error() != "no database at "+path {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("OpenExisting created the file")
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
}
