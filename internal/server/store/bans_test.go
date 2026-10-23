package store

import (
	"context"
	"testing"
)

func TestBans(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.AddBan(ctx, Ban{Kind: "user", Value: "evil", Reason: "abuse"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBan(ctx, Ban{Kind: "user", Value: "evil", Reason: "again"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.IsBanned(ctx, "user", "evil"); err != nil || !ok {
		t.Fatalf("banned = %v %v", ok, err)
	}
	if ok, _ := s.IsBanned(ctx, "host", "evil"); ok {
		t.Fatal("host ban leaked from user ban")
	}
	list, err := s.Bans(ctx)
	if err != nil || len(list) != 1 || list[0].Reason != "again" {
		t.Fatalf("bans = %+v %v", list, err)
	}
	if err := s.RemoveBan(ctx, "user", "evil"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsBanned(ctx, "user", "evil"); ok {
		t.Fatal("still banned")
	}
}
