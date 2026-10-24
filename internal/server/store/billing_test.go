package store

import (
	"context"
	"errors"
	"testing"
)

func TestApplyStripeEventIdempotent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	calls := 0
	apply := func(*BillingTx) error { calls++; return nil }
	ok, err := s.ApplyStripeEvent(ctx, "evt_1", "x.y", apply)
	if err != nil || !ok {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	ok, err = s.ApplyStripeEvent(ctx, "evt_1", "x.y", apply)
	if err != nil || ok {
		t.Fatalf("second: ok=%v err=%v", ok, err)
	}
	if calls != 1 {
		t.Fatalf("apply ran %d times", calls)
	}
}

func TestApplyStripeEventErrorRollsBack(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := mkUser(t, s, 1, "a")
	boom := errors.New("boom")
	_, err := s.ApplyStripeEvent(ctx, "evt_1", "x.y", func(b *BillingTx) error {
		if err := b.PutSubscription(Subscription{UserID: u.ID, CustomerID: "cus_1", Status: "active"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.SubscriptionByUser(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("subscription kept after rollback: %v", err)
	}
	// The event was not recorded, so Stripe's retry is applied.
	ok, err := s.ApplyStripeEvent(ctx, "evt_1", "x.y", func(*BillingTx) error { return nil })
	if err != nil || !ok {
		t.Fatalf("retry: ok=%v err=%v", ok, err)
	}
}

func TestBillingTxSubscriptionUpsert(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := mkUser(t, s, 1, "a")
	_, err := s.ApplyStripeEvent(ctx, "evt_1", "t", func(b *BillingTx) error {
		if _, err := b.UserByCustomer("cus_1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("UserByCustomer unknown: %v", err)
		}
		if _, err := b.Subscription(u.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("Subscription unknown: %v", err)
		}
		if err := b.PutSubscription(Subscription{UserID: u.ID, CustomerID: "cus_1", SubscriptionID: "sub_1", Status: "active", EventCreated: 100}); err != nil {
			return err
		}
		if err := b.PutSubscription(Subscription{UserID: u.ID, CustomerID: "cus_1", SubscriptionID: "sub_1", Status: "past_due", EventCreated: 200}); err != nil {
			return err
		}
		id, err := b.UserByCustomer("cus_1")
		if err != nil || id != u.ID {
			t.Errorf("UserByCustomer = %d, %v", id, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SubscriptionByUser(ctx, u.ID)
	want := Subscription{UserID: u.ID, CustomerID: "cus_1", SubscriptionID: "sub_1", Status: "past_due", EventCreated: 200}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestBillingTxSetPlanProAdmits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u, _, err := s.UpsertGitHubUser(ctx, User{GitHubID: 1, Login: "a", Waitlisted: true})
	if err != nil {
		t.Fatal(err)
	}
	// A non-default subdomain does not count as the default.
	if err := s.ClaimSubdomain(ctx, u.ID, "extra", false, 0); err != nil {
		t.Fatal(err)
	}
	set := func(plan string) {
		t.Helper()
		if _, err := s.ApplyStripeEvent(ctx, "evt_"+plan+t.Name(), "t", func(b *BillingTx) error {
			return b.SetPlan(u.ID, plan)
		}); err != nil {
			t.Fatal(err)
		}
	}
	set("pro")
	got, _ := s.UserByID(ctx, u.ID)
	if got.Plan != "pro" || got.Waitlisted {
		t.Fatalf("user = %+v", got)
	}
	subs, _ := s.Subdomains(ctx, u.ID)
	if len(subs) != 2 || !subs[0].IsDefault {
		t.Fatalf("subdomains = %+v", subs)
	}
	def := subs[0].Name

	// Going free keeps the default; going pro again claims no second one.
	if _, err := s.ApplyStripeEvent(ctx, "evt_free", "t", func(b *BillingTx) error { return b.SetPlan(u.ID, "free") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyStripeEvent(ctx, "evt_pro2", "t", func(b *BillingTx) error { return b.SetPlan(u.ID, "pro") }); err != nil {
		t.Fatal(err)
	}
	subs, _ = s.Subdomains(ctx, u.ID)
	if len(subs) != 2 || subs[0].Name != def {
		t.Fatalf("subdomains after re-pro = %+v", subs)
	}

	if _, err := s.ApplyStripeEvent(ctx, "evt_missing", "t", func(b *BillingTx) error { return b.SetPlan(9999, "pro") }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
}

func TestBillingTxSetPlanFreeKeepsWaitlist(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u, _, _ := s.UpsertGitHubUser(ctx, User{GitHubID: 1, Login: "a", Waitlisted: true})
	if _, err := s.ApplyStripeEvent(ctx, "evt_1", "t", func(b *BillingTx) error { return b.SetPlan(u.ID, "free") }); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if !got.Waitlisted {
		t.Fatal("free un-waitlisted the user")
	}
	if subs, _ := s.Subdomains(ctx, u.ID); len(subs) != 0 {
		t.Fatalf("free claimed %+v", subs)
	}
}

func TestBillingTxAudit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := mkUser(t, s, 1, "a")
	if _, err := s.ApplyStripeEvent(ctx, "evt_1", "invoice.payment_failed", func(b *BillingTx) error {
		return b.Audit(u.ID, "billing.payment_failed", "invoice=in_1")
	}); err != nil {
		t.Fatal(err)
	}
	var uid int64
	var action, detail string
	if err := s.db.QueryRow(`SELECT user_id, action, detail FROM audit_log`).Scan(&uid, &action, &detail); err != nil {
		t.Fatal(err)
	}
	if uid != u.ID || action != "billing.payment_failed" || detail != "invoice=in_1" {
		t.Fatalf("audit = %d %q %q", uid, action, detail)
	}
}
