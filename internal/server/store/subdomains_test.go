package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestClaimSubdomainRace(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const n = 20
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = mkUser(t, s, int64(i+1), fmt.Sprintf("u%d", i)).ID
	}
	var ok, taken atomic.Int32
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := s.ClaimSubdomain(ctx, id, "acme", false, 0); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrTaken):
				taken.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || taken.Load() != n-1 {
		t.Fatalf("ok=%d taken=%d", ok.Load(), taken.Load())
	}
	if _, err := s.SubdomainOwner(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubdomainOwner(ctx, "zzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestClaimSubdomainLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := mkUser(t, s, 1, "a")
	if err := s.ClaimSubdomain(ctx, u.ID, "one", false, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimSubdomain(ctx, u.ID, "two", false, 1); !errors.Is(err, ErrLimit) {
		t.Fatalf("err = %v", err)
	}
	for _, name := range []string{"two", "three", "four"} {
		if err := s.ClaimSubdomain(ctx, u.ID, name, false, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubdomainsOrderAndRelease(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u, o := mkUser(t, s, 1, "a"), mkUser(t, s, 2, "b")
	s.ClaimSubdomain(ctx, u.ID, "extra", false, 0)
	s.ClaimSubdomain(ctx, u.ID, "main", true, 0)
	list, err := s.Subdomains(ctx, u.ID)
	if err != nil || len(list) != 2 || list[0].Name != "main" || !list[0].IsDefault {
		t.Fatalf("list = %+v %v", list, err)
	}
	if err := s.ReleaseSubdomain(ctx, u.ID, "main"); !errors.Is(err, ErrDefault) {
		t.Fatalf("err = %v", err)
	}
	if err := s.ReleaseSubdomain(ctx, o.ID, "extra"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := s.ReleaseSubdomain(ctx, u.ID, "extra"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubdomainOwner(ctx, "extra"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
