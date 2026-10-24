package store

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"

	"github.com/mohammed19n/passway/internal/server"
)

func TestAddDomainPendingDoesNotBlockOwner(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, b := mkUser(t, s, 1, "a"), mkUser(t, s, 2, "b")
	da, err := s.AddDomain(ctx, a.ID, "hooks.example.com", 2)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := hex.DecodeString(da.Token); err != nil || len(raw) != 16 {
		t.Fatalf("token %q is not 16 hex-encoded bytes", da.Token)
	}
	if da.VerifiedAt != nil || da.Host != "hooks.example.com" || da.UserID != a.ID {
		t.Fatalf("got %+v", da)
	}
	// A's pending row must not stop the real owner.
	db, err := s.AddDomain(ctx, b.ID, "hooks.example.com", 2)
	if err != nil {
		t.Fatalf("B add: %v", err)
	}
	if db.Token == da.Token {
		t.Fatal("tokens must differ per user")
	}
	if err := s.VerifyDomain(ctx, b.ID, "hooks.example.com"); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Domains(ctx, a.ID); err != nil || len(list) != 0 {
		t.Fatalf("A's pending row survived: %+v %v", list, err)
	}
	list, err := s.Domains(ctx, b.ID)
	if err != nil || len(list) != 1 || list[0].VerifiedAt == nil {
		t.Fatalf("B's domains = %+v %v", list, err)
	}
	// A cannot re-add or verify it now.
	if _, err := s.AddDomain(ctx, a.ID, "hooks.example.com", 2); !errors.Is(err, ErrTaken) {
		t.Fatalf("A re-add: %v", err)
	}
	if err := s.VerifyDomain(ctx, a.ID, "hooks.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A verify: %v", err)
	}
}

func TestAddDomainVerifiedTaken(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	b, c := mkUser(t, s, 2, "b"), mkUser(t, s, 3, "c")
	if _, err := s.AddDomain(ctx, b.ID, "hooks.example.com", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyDomain(ctx, b.ID, "hooks.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDomain(ctx, c.ID, "HOOKS.example.com.", 2); !errors.Is(err, ErrTaken) {
		t.Fatalf("C add = %v, want ErrTaken", err)
	}
	// B removing it frees it.
	if err := s.RemoveDomain(ctx, b.ID, "hooks.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveDomain(ctx, b.ID, "hooks.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove = %v", err)
	}
	if _, err := s.AddDomain(ctx, c.ID, "hooks.example.com", 2); err != nil {
		t.Fatalf("C add after release: %v", err)
	}
}

func TestAddDomainLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := mkUser(t, s, 1, "a")
	if _, err := s.AddDomain(ctx, u.ID, "a.example.com", 0); !errors.Is(err, ErrLimit) {
		t.Fatalf("max 0 = %v, want ErrLimit (0 means none)", err)
	}
	for _, h := range []string{"a.example.com", "b.example.com"} {
		if _, err := s.AddDomain(ctx, u.ID, h, 2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddDomain(ctx, u.ID, "c.example.com", 2); !errors.Is(err, ErrLimit) {
		t.Fatalf("third = %v, want ErrLimit", err)
	}
	// Re-adding one you already have is not a new domain.
	d, err := s.AddDomain(ctx, u.ID, "a.example.com", 2)
	if err != nil || d.Host != "a.example.com" {
		t.Fatalf("re-add = %+v %v", d, err)
	}
}

func TestDowngradeDisablesDomain(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	dir := &Directory{S: s}
	u := mkUser(t, s, 1, "a")
	for _, h := range []string{"one.example.com", "two.example.com"} {
		if _, err := s.AddDomain(ctx, u.ID, h, 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.VerifyDomain(ctx, u.ID, "one.example.com"); err != nil {
		t.Fatal(err)
	}
	pro := server.Account{ID: strconv.FormatInt(u.ID, 10), Plan: server.PlanPro}
	if ok, err := dir.OwnsDomain(ctx, pro, "ONE.example.com"); err != nil || !ok {
		t.Fatalf("pro verified = %v %v", ok, err)
	}
	if ok, _ := dir.OwnsDomain(ctx, pro, "two.example.com"); ok {
		t.Fatal("pending domain must not be owned")
	}
	free := pro
	free.Plan = server.PlanFree
	if ok, err := dir.OwnsDomain(ctx, free, "one.example.com"); err != nil || ok {
		t.Fatalf("after downgrade = %v %v, want false", ok, err)
	}
	// Within the plan limit, oldest verified first.
	one := pro
	one.Plan.MaxCustomDomains = 1
	if err := s.VerifyDomain(ctx, u.ID, "two.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE custom_domains SET verified_at = verified_at + 10 WHERE host = 'two.example.com'`); err != nil {
		t.Fatal(err)
	}
	if ok, _ := dir.OwnsDomain(ctx, one, "one.example.com"); !ok {
		t.Fatal("oldest verified domain should be usable")
	}
	if ok, _ := dir.OwnsDomain(ctx, one, "two.example.com"); ok {
		t.Fatal("domain beyond the plan limit should not be usable")
	}
	if ok, _ := dir.OwnsDomain(ctx, server.Account{ID: "admin-token", Plan: server.PlanPro}, "one.example.com"); ok {
		t.Fatal("non-database account owns a domain")
	}
}
