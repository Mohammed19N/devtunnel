package server

import (
	"context"
	"testing"
)

func TestPlanValues(t *testing.T) {
	if want := (Plan{Name: "free", MaxTunnels: 1, MaxSubdomains: 1, RateLimit: 60, Bandwidth: 1 << 30, Warning: true}); PlanFree != want {
		t.Fatalf("PlanFree = %+v, want %+v", PlanFree, want)
	}
	if want := (Plan{Name: "pro", MaxTunnels: 5, MaxSubdomains: 5, RateLimit: 600, Bandwidth: 50 << 30}); PlanPro != want {
		t.Fatalf("PlanPro = %+v, want %+v", PlanPro, want)
	}
	if want := (Plan{Name: "unlimited"}); PlanUnlimited != want {
		t.Fatalf("PlanUnlimited = %+v, want %+v", PlanUnlimited, want)
	}
}

func TestPlanByName(t *testing.T) {
	if PlanByName("pro") != PlanPro || PlanByName("free") != PlanFree || PlanByName("unlimited") != PlanUnlimited {
		t.Fatal("known plan names must round-trip")
	}
}

func TestPlanByNameUnknown(t *testing.T) {
	if got := PlanByName("platinum"); got != PlanFree {
		t.Fatalf("got %+v, want PlanFree", got)
	}
}

func TestStaticTokensAccount(t *testing.T) {
	acc, err := StaticTokens{"good"}.Authenticate(context.Background(), "good")
	if err != nil || acc.ID != "static" || acc.Plan != PlanUnlimited {
		t.Fatalf("got %+v, %v", acc, err)
	}
	acc, err = StaticTokens(nil).Authenticate(context.Background(), "anything")
	if err != nil || acc.ID != "anonymous" || acc.Plan != PlanUnlimited {
		t.Fatalf("got %+v, %v", acc, err)
	}
	if _, err := (StaticTokens{"good"}).Authenticate(context.Background(), "bad"); err == nil {
		t.Fatal("bad token must fail")
	}
}
