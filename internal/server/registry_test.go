package server

import (
	"errors"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/protocol"
)

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func newTestRegistry() (*Registry, *time.Time) {
	r := NewRegistry(time.Minute)
	now := time.Unix(1000, 0)
	r.now = func() time.Time { return now }
	return r, &now
}

func TestClaimExplicit(t *testing.T) {
	r, _ := newTestRegistry()
	a := &Tunnel{}
	if err := r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a); err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Lookup("acme"); !ok || got != a {
		t.Fatalf("Lookup = %v, %v", got, ok)
	}
	wantCode(t, r.Claim(ClaimRequest{Name: "acme", SessionID: "B"}, &Tunnel{}), protocol.CodeNameTaken)
}

func TestClaimRandomWhenEmpty(t *testing.T) {
	r, _ := newTestRegistry()
	a := &Tunnel{}
	if err := r.Claim(ClaimRequest{SessionID: "A"}, a); err != nil {
		t.Fatal(err)
	}
	if a.Name == "" || ValidateName(a.Name) != nil || a.SessionID != "A" {
		t.Fatalf("bad tunnel %+v", a)
	}
}

func TestHoldBlocksOthers(t *testing.T) {
	r, _ := newTestRegistry()
	a := &Tunnel{}
	_ = r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a)
	r.Release(a)
	wantCode(t, r.Claim(ClaimRequest{Name: "acme", SessionID: "B"}, &Tunnel{}), protocol.CodeNameTaken)
	b := &Tunnel{}
	if err := r.Claim(ClaimRequest{PreferredName: "acme", SessionID: "B"}, b); err != nil {
		t.Fatal(err)
	}
	if b.Name == "acme" || b.Name == "" {
		t.Fatalf("got %q", b.Name)
	}
}

func TestHoldSameSession(t *testing.T) {
	r, _ := newTestRegistry()
	a := &Tunnel{}
	_ = r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a)
	r.Release(a)
	a2 := &Tunnel{}
	if err := r.Claim(ClaimRequest{PreferredName: "acme", SessionID: "A"}, a2); err != nil {
		t.Fatal(err)
	}
	if a2.Name != "acme" {
		t.Fatalf("got %q", a2.Name)
	}
}

func TestHoldExpires(t *testing.T) {
	r, now := newTestRegistry()
	a := &Tunnel{}
	_ = r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a)
	r.Release(a)
	*now = now.Add(time.Minute + time.Second)
	if err := r.Claim(ClaimRequest{Name: "acme", SessionID: "B"}, &Tunnel{}); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseStaleTunnel(t *testing.T) {
	r, now := newTestRegistry()
	a, b := &Tunnel{}, &Tunnel{}
	_ = r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a)
	r.Release(a)
	*now = now.Add(time.Hour)
	if err := r.Claim(ClaimRequest{Name: "acme", SessionID: "B"}, b); err != nil {
		t.Fatal(err)
	}
	r.Release(a)
	if got, ok := r.Lookup("acme"); !ok || got != b {
		t.Fatalf("Lookup = %v, %v; want B", got, ok)
	}
}
