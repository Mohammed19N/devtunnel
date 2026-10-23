package store

import (
	"context"
	"testing"
)

func TestUsageAdd(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, b := mkUser(t, s, 1, "a"), mkUser(t, s, 2, "b")
	const m = "2026-10"
	for _, c := range []struct {
		id    int64
		month string
		n     int64
	}{{a.ID, m, 100}, {a.ID, m, 50}, {b.ID, m, 7}, {a.ID, "2026-09", 1000}} {
		if err := s.AddUsage(ctx, c.id, c.month, c.n); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Usage(ctx, a.ID, m); err != nil || n != 150 {
		t.Fatalf("usage = %d %v", n, err)
	}
	if n, err := s.Usage(ctx, a.ID, "2020-01"); err != nil || n != 0 {
		t.Fatalf("empty usage = %d", n)
	}
	if n, err := s.PlanUsage(ctx, "free", m); err != nil || n != 157 {
		t.Fatalf("plan usage = %d %v", n, err)
	}
	if n, err := s.PlanUsage(ctx, "pro", m); err != nil || n != 0 {
		t.Fatalf("pro usage = %d", n)
	}
}
