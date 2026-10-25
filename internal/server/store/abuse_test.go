package store

import (
	"context"
	"errors"
	"testing"
)

func TestAbuseReports(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id1, err := s.AddAbuseReport(ctx, AbuseReport{URL: "https://a.passway.test/login", Category: "phishing",
		Details: "fake bank login", Email: "me@example.com", IP: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.AddAbuseReport(ctx, AbuseReport{URL: "https://b.passway.test/", Category: "spam", Details: "spam", IP: "192.0.2.2"})
	if err != nil {
		t.Fatal(err)
	}
	if id1 <= 0 || id2 <= id1 {
		t.Fatalf("ids = %d, %d", id1, id2)
	}

	all, err := s.AbuseReports(ctx, true, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("unresolved = %+v %v", all, err)
	}
	// Newest first.
	if all[0].ID != id2 || all[1].ID != id1 {
		t.Fatalf("order = %d, %d", all[0].ID, all[1].ID)
	}
	r := all[1]
	if r.URL != "https://a.passway.test/login" || r.Category != "phishing" || r.Details != "fake bank login" ||
		r.Email != "me@example.com" || r.IP != "192.0.2.1" || r.CreatedAt.IsZero() || r.ResolvedAt != nil {
		t.Fatalf("report = %+v", r)
	}

	if err := s.ResolveAbuseReport(ctx, id1); err != nil {
		t.Fatal(err)
	}
	open, err := s.AbuseReports(ctx, true, 10)
	if err != nil || len(open) != 1 || open[0].ID != id2 {
		t.Fatalf("unresolved after resolve = %+v %v", open, err)
	}
	all, err = s.AbuseReports(ctx, false, 10)
	if err != nil || len(all) != 2 || all[1].ResolvedAt == nil {
		t.Fatalf("all after resolve = %+v %v", all, err)
	}
	if lim, err := s.AbuseReports(ctx, false, 1); err != nil || len(lim) != 1 || lim[0].ID != id2 {
		t.Fatalf("limit 1 = %+v %v", lim, err)
	}
	if err := s.ResolveAbuseReport(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve unknown = %v, want ErrNotFound", err)
	}
}
