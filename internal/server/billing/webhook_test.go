package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/server/store"
)

const testSecret = "whsec_testsecret"

func sign(payload []byte, secret string, t int64) string {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.%s", t, payload)
	return hex.EncodeToString(m.Sum(nil))
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerifyWebhookValid(t *testing.T) {
	payload := readFixture(t, "checkout.session.completed.json")
	now := time.Unix(1_700_000_000, 0)
	header := fmt.Sprintf("t=%d,v1=%s", now.Unix(), sign(payload, testSecret, now.Unix()))
	ev, err := VerifyWebhook(payload, header, testSecret, now)
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "evt_1Q0checkout" || ev.Type != "checkout.session.completed" || ev.Created != 100 {
		t.Fatalf("event = %+v", ev)
	}
	var obj struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(ev.Data, &obj); err != nil || obj.Mode != "subscription" {
		t.Fatalf("data = %s (%v)", ev.Data, err)
	}
	// Within tolerance on both sides.
	for _, d := range []time.Duration{-300 * time.Second, 300 * time.Second} {
		if _, err := VerifyWebhook(payload, header, testSecret, now.Add(d)); err != nil {
			t.Fatalf("now%+v: %v", d, err)
		}
	}
}

func TestVerifyWebhookBadSig(t *testing.T) {
	payload := readFixture(t, "checkout.session.completed.json")
	now := time.Unix(1_700_000_000, 0)
	ts := now.Unix()
	good := sign(payload, testSecret, ts)
	for name, header := range map[string]string{
		"wrong secret":  fmt.Sprintf("t=%d,v1=%s", ts, sign(payload, "whsec_other", ts)),
		"tampered body": fmt.Sprintf("t=%d,v1=%s", ts, sign(append([]byte(" "), payload...), testSecret, ts)),
		"wrong t":       fmt.Sprintf("t=%d,v1=%s", ts+1, good),
		"v0 only":       fmt.Sprintf("t=%d,v0=%s", ts, good),
		"no t":          "v1=" + good,
		"no v1":         fmt.Sprintf("t=%d", ts),
		"empty":         "",
		"garbage":       "nonsense",
		"bad hex":       fmt.Sprintf("t=%d,v1=zz%s", ts, good[2:]),
		"non-numeric t": "t=abc,v1=" + good,
		"truncated sig": fmt.Sprintf("t=%d,v1=%s", ts, good[:32]),
		"empty secret":  fmt.Sprintf("t=%d,v1=%s", ts, sign(payload, "", ts)),
	} {
		secret := testSecret
		if name == "empty secret" {
			secret = ""
		}
		if _, err := VerifyWebhook(payload, header, secret, now); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestVerifyWebhookStale(t *testing.T) {
	payload := readFixture(t, "checkout.session.completed.json")
	now := time.Unix(1_700_000_000, 0)
	for _, d := range []int64{-301, 301} {
		ts := now.Unix() + d
		header := fmt.Sprintf("t=%d,v1=%s", ts, sign(payload, testSecret, ts))
		if _, err := VerifyWebhook(payload, header, testSecret, now); !errors.Is(err, ErrBadSignature) {
			t.Errorf("t offset %d: err = %v", d, err)
		}
	}
}

func TestVerifyWebhookMultipleV1(t *testing.T) {
	payload := readFixture(t, "checkout.session.completed.json")
	now := time.Unix(1_700_000_000, 0)
	ts := now.Unix()
	header := fmt.Sprintf("t=%d,v1=%s,v1=%s,v0=%s", ts,
		sign(payload, "whsec_old", ts), sign(payload, testSecret, ts), sign(payload, "x", ts))
	if _, err := VerifyWebhook(payload, header, testSecret, now); err != nil {
		t.Fatal(err)
	}
}

// --- Apply ---

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fixtureEvent loads an event fixture, letting edit change the envelope and
// its data.object before it is decoded like VerifyWebhook would.
func fixtureEvent(t *testing.T, name string, edit func(env, obj map[string]any)) Event {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(readFixture(t, name), &env); err != nil {
		t.Fatal(err)
	}
	obj := env["data"].(map[string]any)["object"].(map[string]any)
	if edit != nil {
		edit(env, obj)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := parseEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func mustApply(t *testing.T, st *store.Store, ev Event) []int64 {
	t.Helper()
	changed, err := Apply(context.Background(), st, ev)
	if err != nil {
		t.Fatalf("Apply %s: %v", ev.Type, err)
	}
	return changed
}

// paidUser creates a waitlisted user and applies a checkout for them.
func paidUser(t *testing.T, st *store.Store) store.User {
	t.Helper()
	ctx := context.Background()
	u, _, err := st.UpsertGitHubUser(ctx, store.User{GitHubID: 7, Login: "payer", Waitlisted: true})
	if err != nil {
		t.Fatal(err)
	}
	changed := mustApply(t, st, fixtureEvent(t, "checkout.session.completed.json", func(_, obj map[string]any) {
		obj["client_reference_id"] = fmt.Sprint(u.ID)
	}))
	if !slices.Equal(changed, []int64{u.ID}) {
		t.Fatalf("planChanged = %v", changed)
	}
	return u
}

func plan(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	u, err := st.UserByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return u.Plan
}

func TestApplyCheckoutMakesPro(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	u := paidUser(t, st)
	got, _ := st.UserByID(ctx, u.ID)
	if got.Plan != "pro" || got.Waitlisted {
		t.Fatalf("user = %+v", got)
	}
	subs, _ := st.Subdomains(ctx, u.ID)
	if len(subs) != 1 || !subs[0].IsDefault {
		t.Fatalf("subdomains = %+v", subs)
	}
	sub, err := st.SubscriptionByUser(ctx, u.ID)
	want := store.Subscription{UserID: u.ID, CustomerID: "cus_TEST1", SubscriptionID: "sub_TEST1", Status: "active", EventCreated: 100}
	if err != nil || sub != want {
		t.Fatalf("subscription = %+v, %v", sub, err)
	}
}

func TestApplyCheckoutIgnoredUnlessPaidSubscription(t *testing.T) {
	st := openStore(t)
	u, _, _ := st.UpsertGitHubUser(context.Background(), store.User{GitHubID: 7, Login: "payer"})
	for i, edit := range []func(obj map[string]any){
		func(obj map[string]any) { obj["payment_status"] = "unpaid" },
		func(obj map[string]any) { obj["mode"] = "payment" },
		func(obj map[string]any) { obj["client_reference_id"] = "not-a-number" },
		func(obj map[string]any) { obj["client_reference_id"] = "999999" }, // no such user
	} {
		ev := fixtureEvent(t, "checkout.session.completed.json", func(env, obj map[string]any) {
			env["id"] = fmt.Sprintf("evt_%d", i)
			obj["client_reference_id"] = fmt.Sprint(u.ID)
			edit(obj)
		})
		if changed := mustApply(t, st, ev); len(changed) != 0 {
			t.Errorf("case %d: planChanged = %v", i, changed)
		}
	}
	if p := plan(t, st, u.ID); p != "free" {
		t.Fatalf("plan = %q", p)
	}
}

func TestApplyDuplicateEventNoop(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	u := paidUser(t, st)
	del := fixtureEvent(t, "customer.subscription.deleted.json", nil)
	if changed := mustApply(t, st, del); !slices.Equal(changed, []int64{u.ID}) {
		t.Fatalf("planChanged = %v", changed)
	}
	// An admin puts them back on Pro; a redelivery of the same deletion must not undo it.
	if err := st.SetPlan(ctx, u.ID, "pro"); err != nil {
		t.Fatal(err)
	}
	if changed := mustApply(t, st, del); len(changed) != 0 {
		t.Fatalf("duplicate planChanged = %v", changed)
	}
	if p := plan(t, st, u.ID); p != "pro" {
		t.Fatalf("duplicate event re-applied: plan = %q", p)
	}
	ok, err := st.ApplyStripeEvent(ctx, del.ID, del.Type, func(*store.BillingTx) error { return nil })
	if err != nil || ok {
		t.Fatalf("ApplyStripeEvent dup: applied=%v err=%v", ok, err)
	}
}

func TestApplyOutOfOrder(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	u := paidUser(t, st)
	mustApply(t, st, fixtureEvent(t, "customer.subscription.deleted.json", func(env, _ map[string]any) {
		env["created"] = 200
	}))
	changed := mustApply(t, st, fixtureEvent(t, "customer.subscription.updated.json", func(env, obj map[string]any) {
		env["created"] = 100
		obj["status"] = "active"
	}))
	if len(changed) != 0 {
		t.Fatalf("stale event changed plans: %v", changed)
	}
	if p := plan(t, st, u.ID); p != "free" {
		t.Fatalf("plan = %q, want free", p)
	}
	sub, _ := st.SubscriptionByUser(ctx, u.ID)
	if sub.Status != "canceled" || sub.EventCreated != 200 {
		t.Fatalf("subscription = %+v", sub)
	}
}

func TestApplyOldSubscriptionCannotDowngrade(t *testing.T) {
	st := openStore(t)
	u := paidUser(t, st) // sub_TEST1
	// They resubscribe as sub_TEST2 (checkout at 300) ...
	mustApply(t, st, fixtureEvent(t, "checkout.session.completed.json", func(env, obj map[string]any) {
		env["id"], env["created"] = "evt_checkout2", 300
		obj["client_reference_id"], obj["subscription"] = fmt.Sprint(u.ID), "sub_TEST2"
	}))
	// ... and a later event about the old subscription arrives.
	changed := mustApply(t, st, fixtureEvent(t, "customer.subscription.updated.json", func(env, obj map[string]any) {
		env["created"] = 400
		obj["id"], obj["status"] = "sub_TEST1", "incomplete_expired"
	}))
	if len(changed) != 0 || plan(t, st, u.ID) != "pro" {
		t.Fatalf("old subscription downgraded: changed=%v plan=%s", changed, plan(t, st, u.ID))
	}
}

func TestApplyPastDueKeepsPro(t *testing.T) {
	st := openStore(t)
	u := paidUser(t, st)
	for i, status := range []string{"past_due", "trialing", "active"} {
		changed := mustApply(t, st, fixtureEvent(t, "customer.subscription.updated.json", func(env, obj map[string]any) {
			env["id"], env["created"] = fmt.Sprintf("evt_%d", i), 150+i
			obj["status"] = status
		}))
		if len(changed) != 0 || plan(t, st, u.ID) != "pro" {
			t.Fatalf("%s: changed=%v plan=%s", status, changed, plan(t, st, u.ID))
		}
	}
	for i, status := range []string{"unpaid", "incomplete_expired", "canceled", "paused", "incomplete"} {
		changed := mustApply(t, st, fixtureEvent(t, "customer.subscription.updated.json", func(env, obj map[string]any) {
			env["id"], env["created"] = fmt.Sprintf("evt_down_%d", i), 200+2*i
			obj["status"] = status
		}))
		if !slices.Equal(changed, []int64{u.ID}) || plan(t, st, u.ID) != "free" {
			t.Fatalf("%s: changed=%v plan=%s", status, changed, plan(t, st, u.ID))
		}
		mustApply(t, st, fixtureEvent(t, "customer.subscription.updated.json", func(env, obj map[string]any) {
			env["id"], env["created"] = fmt.Sprintf("evt_up_%d", i), 201+2*i
		}))
		if plan(t, st, u.ID) != "pro" {
			t.Fatalf("%s: back to active did not restore pro", status)
		}
	}
}

func TestApplyUnknownCustomerNoError(t *testing.T) {
	st := openStore(t)
	for _, name := range []string{"customer.subscription.updated.json", "customer.subscription.deleted.json", "invoice.payment_failed.json"} {
		ev := fixtureEvent(t, name, func(_, obj map[string]any) { obj["customer"] = "cus_UNKNOWN" })
		if changed := mustApply(t, st, ev); len(changed) != 0 {
			t.Fatalf("%s: planChanged = %v", name, changed)
		}
	}
}

func TestApplyPaymentFailedAuditsOnly(t *testing.T) {
	st := openStore(t)
	u := paidUser(t, st)
	if changed := mustApply(t, st, fixtureEvent(t, "invoice.payment_failed.json", nil)); len(changed) != 0 {
		t.Fatalf("planChanged = %v", changed)
	}
	if plan(t, st, u.ID) != "pro" {
		t.Fatal("payment_failed changed the plan")
	}
	// The audit row itself is covered by store.TestBillingTxAudit.
}

func TestApplyOtherTypeRecorded(t *testing.T) {
	st := openStore(t)
	ev := Event{ID: "evt_other", Type: "customer.created", Created: 1, Data: json.RawMessage(`{}`)}
	if changed := mustApply(t, st, ev); len(changed) != 0 {
		t.Fatalf("planChanged = %v", changed)
	}
	ok, err := st.ApplyStripeEvent(context.Background(), ev.ID, ev.Type, func(*store.BillingTx) error { return nil })
	if err != nil || ok {
		t.Fatalf("not recorded: applied=%v err=%v", ok, err)
	}
}

// Stripe's Checkout emits customer.subscription.created (status incomplete),
// .updated (active) and checkout.session.completed in the same second, in no
// guaranteed delivery order. A same-second event must not downgrade, except a
// deletion.
func TestApplySameSecondNoDowngrade(t *testing.T) {
	st := openStore(t)
	u := paidUser(t, st) // checkout at created 100
	changed := mustApply(t, st, fixtureEvent(t, "customer.subscription.updated.json", func(env, obj map[string]any) {
		env["id"], env["type"], env["created"] = "evt_created", "customer.subscription.created", 100
		obj["status"] = "incomplete"
	}))
	if len(changed) != 0 || plan(t, st, u.ID) != "pro" {
		t.Fatalf("same-second incomplete downgraded: changed=%v plan=%s", changed, plan(t, st, u.ID))
	}
	changed = mustApply(t, st, fixtureEvent(t, "customer.subscription.deleted.json", func(env, _ map[string]any) {
		env["created"] = 100
	}))
	if !slices.Equal(changed, []int64{u.ID}) || plan(t, st, u.ID) != "free" {
		t.Fatalf("same-second deletion ignored: changed=%v plan=%s", changed, plan(t, st, u.ID))
	}
}
