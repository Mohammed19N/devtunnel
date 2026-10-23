package server

import (
	"errors"
	"fmt"
	"sync"
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

func claimErr(r *Registry, req ClaimRequest, t *Tunnel) error {
	_, err := r.Claim(req, t)
	return err
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
	if _, err := r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a); err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Lookup("acme"); !ok || got != a {
		t.Fatalf("Lookup = %v, %v", got, ok)
	}
	wantCode(t, claimErr(r, ClaimRequest{Name: "acme", SessionID: "B"}, &Tunnel{}), protocol.CodeNameTaken)
}

func TestClaimRandomWhenEmpty(t *testing.T) {
	r, _ := newTestRegistry()
	a := &Tunnel{}
	if _, err := r.Claim(ClaimRequest{SessionID: "A"}, a); err != nil {
		t.Fatal(err)
	}
	if a.Name == "" || ValidateName(a.Name) != nil || a.SessionID != "A" {
		t.Fatalf("bad tunnel %+v", a)
	}
}

func TestHoldBlocksOthers(t *testing.T) {
	r, _ := newTestRegistry()
	a := &Tunnel{}
	_, _ = r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a)
	r.Release(a)
	wantCode(t, claimErr(r, ClaimRequest{Name: "acme", SessionID: "B"}, &Tunnel{}), protocol.CodeNameTaken)
	b := &Tunnel{}
	if _, err := r.Claim(ClaimRequest{PreferredName: "acme", SessionID: "B"}, b); err != nil {
		t.Fatal(err)
	}
	if b.Name == "acme" || b.Name == "" {
		t.Fatalf("got %q", b.Name)
	}
}

func TestHoldSameSession(t *testing.T) {
	r, _ := newTestRegistry()
	a := &Tunnel{}
	_, _ = r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a)
	r.Release(a)
	a2 := &Tunnel{}
	if _, err := r.Claim(ClaimRequest{PreferredName: "acme", SessionID: "A"}, a2); err != nil {
		t.Fatal(err)
	}
	if a2.Name != "acme" {
		t.Fatalf("got %q", a2.Name)
	}
}

func TestHoldExpires(t *testing.T) {
	r, now := newTestRegistry()
	a := &Tunnel{}
	_, _ = r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a)
	r.Release(a)
	*now = now.Add(time.Minute + time.Second)
	if _, err := r.Claim(ClaimRequest{Name: "acme", SessionID: "B"}, &Tunnel{}); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseStaleTunnel(t *testing.T) {
	r, now := newTestRegistry()
	a, b := &Tunnel{}, &Tunnel{}
	_, _ = r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, a)
	r.Release(a)
	*now = now.Add(time.Hour)
	if _, err := r.Claim(ClaimRequest{Name: "acme", SessionID: "B"}, b); err != nil {
		t.Fatal(err)
	}
	r.Release(a)
	if got, ok := r.Lookup("acme"); !ok || got != b {
		t.Fatalf("Lookup = %v, %v; want B", got, ok)
	}
}

func TestClaimSameSessionTakesOverActive(t *testing.T) {
	r, _ := newTestRegistry()
	old := &Tunnel{}
	_, _ = r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, old)
	nt := &Tunnel{}
	replaced, err := r.Claim(ClaimRequest{Name: "acme", SessionID: "A"}, nt)
	if err != nil || replaced != old {
		t.Fatalf("replaced=%v err=%v", replaced, err)
	}
	if got, _ := r.Lookup("acme"); got != nt {
		t.Fatal("lookup should return new tunnel")
	}
	wantCode(t, claimErr(r, ClaimRequest{Name: "acme", SessionID: "B"}, &Tunnel{}), protocol.CodeNameTaken)
	r.Release(old) // no-op
	if got, _ := r.Lookup("acme"); got != nt {
		t.Fatal("releasing superseded tunnel must be a no-op")
	}
	nt2 := &Tunnel{}
	replaced, err = r.Claim(ClaimRequest{PreferredName: "acme", SessionID: "A"}, nt2)
	if err != nil || replaced != nt || nt2.Name != "acme" {
		t.Fatalf("preferred takeover: replaced=%v name=%q err=%v", replaced, nt2.Name, err)
	}
}

func TestReleasePurgesExpiredHolds(t *testing.T) {
	r, now := newTestRegistry()
	a := &Tunnel{}
	if _, err := r.Claim(ClaimRequest{Name: "aaa", SessionID: "s1"}, a); err != nil {
		t.Fatal(err)
	}
	r.Release(a)
	*now = now.Add(time.Minute + time.Second)
	b := &Tunnel{}
	if _, err := r.Claim(ClaimRequest{Name: "bbb", SessionID: "s2"}, b); err != nil {
		t.Fatal(err)
	}
	r.Release(b)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.held["bbb"]; len(r.held) != 1 || !ok {
		t.Fatalf("held = %v, want only bbb", r.held)
	}
}

func TestClaimEnforcesAccountLimit(t *testing.T) {
	r, _ := newTestRegistry()
	acc := Account{ID: "u1"}
	a := &Tunnel{Account: acc}
	req := func(name, sess string) ClaimRequest {
		return ClaimRequest{Name: name, SessionID: sess, AccountID: "u1", PlanName: "free", MaxTunnels: 1}
	}
	if _, err := r.Claim(req("one", "A"), a); err != nil {
		t.Fatal(err)
	}
	wantCode(t, claimErr(r, req("two", "B"), &Tunnel{Account: acc}), protocol.CodeLimitReached)
	// Same session takes over without counting against the limit.
	if _, err := r.Claim(req("one", "A"), &Tunnel{Account: acc}); err != nil {
		t.Fatal(err)
	}
	if n := r.CountAccount("u1", "A"); n != 0 {
		t.Fatalf("CountAccount = %d", n)
	}
	if n := r.CountAccount("u1", ""); n != 1 || len(r.ByAccount("u1")) != 1 {
		t.Fatalf("CountAccount = %d", n)
	}
	// Another account is unaffected.
	if _, err := r.Claim(ClaimRequest{Name: "xyz", SessionID: "C", AccountID: "u2", MaxTunnels: 1}, &Tunnel{Account: Account{ID: "u2"}}); err != nil {
		t.Fatal(err)
	}
}

func TestClaimSameSessionOtherNameCounts(t *testing.T) {
	r, _ := newTestRegistry()
	acc := Account{ID: "u1"}
	req := func(name string) ClaimRequest {
		return ClaimRequest{Name: name, SessionID: "S", AccountID: "u1", PlanName: "free", MaxTunnels: 1}
	}
	if _, err := r.Claim(req("aaa"), &Tunnel{Account: acc}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, claimErr(r, req("bbb"), &Tunnel{Account: acc}), protocol.CodeLimitReached)
	if _, err := r.Claim(req("aaa"), &Tunnel{Account: acc}); err != nil {
		t.Fatalf("takeover of own name: %v", err)
	}
}

func TestClaimConcurrentRespectsLimit(t *testing.T) {
	for _, max := range []int{1, 3} {
		t.Run(fmt.Sprintf("max%d", max), func(t *testing.T) {
			r, _ := newTestRegistry()
			const n = 20
			start := make(chan struct{})
			errs := make(chan error, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := r.Claim(ClaimRequest{
						Name: fmt.Sprintf("name-%d", i), SessionID: fmt.Sprintf("sess-%d", i),
						AccountID: "u1", PlanName: "free", MaxTunnels: max,
					}, &Tunnel{Account: Account{ID: "u1"}})
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			ok, limited := 0, 0
			for err := range errs {
				var pe *protocol.Error
				switch {
				case err == nil:
					ok++
				case errors.As(err, &pe) && pe.Code == protocol.CodeLimitReached:
					limited++
				default:
					t.Errorf("unexpected %v", err)
				}
			}
			if ok != max || limited != n-max {
				t.Fatalf("ok=%d limited=%d, want %d and %d", ok, limited, max, n-max)
			}
		})
	}
}
