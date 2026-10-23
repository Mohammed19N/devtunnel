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
	if err := s.ClaimSubdomain(ctx, u.ID, "extra", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimSubdomain(ctx, u.ID, "main", true, 0); err != nil {
		t.Fatal(err)
	}
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

func TestClaimSubdomainLimitRace(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := mkUser(t, s, 1, "a")
	var ok, limited atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := s.ClaimSubdomain(ctx, u.ID, fmt.Sprintf("n%d", i), false, 1); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrLimit):
				limited.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || limited.Load() != 19 {
		t.Fatalf("ok=%d limited=%d", ok.Load(), limited.Load())
	}
}

func TestSubdomainCaseInsensitive(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u, o := mkUser(t, s, 1, "a"), mkUser(t, s, 2, "b")
	if err := s.ClaimSubdomain(ctx, u.ID, "Acme", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimSubdomain(ctx, o.ID, "ACME", false, 0); !errors.Is(err, ErrTaken) {
		t.Fatalf("err = %v", err)
	}
	if id, err := s.SubdomainOwner(ctx, "aCmE"); err != nil || id != u.ID {
		t.Fatalf("owner = %d %v", id, err)
	}
	if list, _ := s.Subdomains(ctx, u.ID); len(list) != 1 || list[0].Name != "acme" {
		t.Fatalf("list = %+v", list)
	}
	if err := s.ReleaseSubdomain(ctx, u.ID, "ACME"); err != nil {
		t.Fatal(err)
	}
}
