package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/server/store"
)

// Event is a Stripe event envelope; Data is its data.object.
type Event struct {
	ID, Type string
	Created  int64
	Data     json.RawMessage
}

var ErrBadSignature = errors.New("billing: bad signature")

// tolerance is how far the signed timestamp may be from now, either way.
const tolerance = 300 * time.Second

// VerifyWebhook checks the Stripe-Signature header (t=…, one or more v1=…):
// HMAC-SHA256 of "<t>.<payload>" with secret, constant-time, |now - t| <= 300s.
// Other schemes (v0) are ignored. Every failure wraps ErrBadSignature.
func VerifyWebhook(payload []byte, header, secret string, now time.Time) (Event, error) {
	if secret == "" {
		return Event{}, fmt.Errorf("%w: no webhook secret", ErrBadSignature)
	}
	var tsRaw string
	var sigs [][]byte
	for _, item := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			if tsRaw != "" {
				return Event{}, fmt.Errorf("%w: repeated timestamp", ErrBadSignature)
			}
			tsRaw = v
		case "v1":
			if b, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, b)
			}
		}
	}
	if tsRaw == "" || len(sigs) == 0 {
		return Event{}, fmt.Errorf("%w: missing t or v1", ErrBadSignature)
	}
	t, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return Event{}, fmt.Errorf("%w: bad timestamp", ErrBadSignature)
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(tsRaw))
	m.Write([]byte("."))
	m.Write(payload)
	want := m.Sum(nil)
	match := false
	for _, s := range sigs {
		if hmac.Equal(s, want) {
			match = true
		}
	}
	if !match {
		return Event{}, fmt.Errorf("%w: no matching v1", ErrBadSignature)
	}
	// Checked after the HMAC so a forged timestamp cannot be told apart from
	// a forged signature. Compared in seconds to avoid Duration overflow.
	if d := now.Unix() - t; d > int64(tolerance/time.Second) || d < -int64(tolerance/time.Second) {
		return Event{}, fmt.Errorf("%w: timestamp outside tolerance", ErrBadSignature)
	}
	return parseEvent(payload)
}

func parseEvent(payload []byte) (Event, error) {
	var env struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Created int64  `json:"created"`
		Data    struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return Event{}, fmt.Errorf("billing: decode event: %w", err)
	}
	if env.ID == "" || env.Type == "" {
		return Event{}, errors.New("billing: event without id or type")
	}
	return Event{ID: env.ID, Type: env.Type, Created: env.Created, Data: env.Data.Object}, nil
}

// ProStatus reports whether a subscription in this status keeps Pro (and is
// still billed).
func ProStatus(s string) bool {
	return s == "active" || s == "trialing" || s == "past_due"
}

// terminalStatus reports whether a subscription in this status can never
// become active again.
func terminalStatus(s string) bool {
	return s == "canceled" || s == "incomplete_expired"
}

// Apply records ev (once, by ID) and applies its effect in one transaction.
// planChanged lists the users whose plan actually changed. A redelivered
// event changes nothing. An error means nothing was recorded, so the webhook
// should answer non-2xx and let Stripe retry.
func Apply(ctx context.Context, st *store.Store, ev Event) (planChanged []int64, err error) {
	log := slog.Default().With("event", ev.ID, "type", ev.Type)
	_, err = st.ApplyStripeEvent(ctx, ev.ID, ev.Type, func(b *store.BillingTx) error {
		planChanged = nil
		a := applier{b: b, ev: ev, log: log, changed: &planChanged}
		switch ev.Type {
		case "checkout.session.completed":
			return a.checkout()
		case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
			return a.subscription()
		case "invoice.payment_failed":
			return a.paymentFailed()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return planChanged, nil
}

type applier struct {
	b       *store.BillingTx
	ev      Event
	log     *slog.Logger
	changed *[]int64
}

func (a applier) decode(v any) error {
	if err := json.Unmarshal(a.ev.Data, v); err != nil {
		return fmt.Errorf("billing: decode %s object: %w", a.ev.Type, err)
	}
	return nil
}

// setPlan sets the plan and notes the user if it changed.
func (a applier) setPlan(userID int64, plan string) error {
	old, err := a.b.Plan(userID)
	if err != nil {
		return err
	}
	if err := a.b.SetPlan(userID, plan); err != nil {
		return err
	}
	if old == plan {
		return nil
	}
	*a.changed = append(*a.changed, userID)
	return a.b.Audit(userID, "billing.plan", fmt.Sprintf("%s->%s event=%s", old, plan, a.ev.ID))
}

func (a applier) checkout() error {
	var s struct {
		Mode              string `json:"mode"`
		PaymentStatus     string `json:"payment_status"`
		ClientReferenceID string `json:"client_reference_id"`
		Customer          string `json:"customer"`
		Subscription      string `json:"subscription"`
	}
	if err := a.decode(&s); err != nil {
		return err
	}
	if s.Mode != "subscription" || s.PaymentStatus != "paid" {
		a.log.Info("billing: checkout ignored", "mode", s.Mode, "payment_status", s.PaymentStatus)
		return nil
	}
	userID, err := strconv.ParseInt(s.ClientReferenceID, 10, 64)
	if err != nil || userID <= 0 || s.Customer == "" {
		a.log.Warn("billing: checkout without a usable user or customer",
			"client_reference_id", s.ClientReferenceID, "customer", s.Customer)
		return nil
	}
	if _, err := a.b.Plan(userID); errors.Is(err, store.ErrNotFound) {
		a.log.Warn("billing: checkout for unknown user", "user", userID, "customer", s.Customer)
		return nil
	} else if err != nil {
		return err
	}
	if old, err := a.b.Subscription(userID); err == nil && a.ev.Created < old.EventCreated {
		a.log.Info("billing: stale checkout ignored", "user", userID)
		return nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := a.b.PutSubscription(store.Subscription{
		UserID: userID, CustomerID: s.Customer, SubscriptionID: s.Subscription,
		Status: "active", EventCreated: a.ev.Created,
	}); err != nil {
		return err
	}
	return a.setPlan(userID, "pro")
}

func (a applier) subscription() error {
	var s struct {
		ID       string `json:"id"`
		Customer string `json:"customer"`
		Status   string `json:"status"`
	}
	if err := a.decode(&s); err != nil {
		return err
	}
	userID, err := a.b.UserByCustomer(s.Customer)
	if errors.Is(err, store.ErrNotFound) {
		a.log.Info("billing: subscription event for unknown customer", "customer", s.Customer)
		return nil
	}
	if err != nil {
		return err
	}
	stored, err := a.b.Subscription(userID)
	if err != nil {
		return err
	}
	deleted := a.ev.Type == "customer.subscription.deleted"
	created := a.ev.Type == "customer.subscription.created"
	pro := ProStatus(s.Status) && !deleted
	storedPro := ProStatus(stored.Status)
	current := stored.SubscriptionID == "" || s.ID == stored.SubscriptionID
	switch {
	case !current && !(created && pro):
		// Only checkout.session.completed or a (Pro) .created may switch the
		// stored subscription. Anything else about another subscription is
		// ignored, so it can neither end nor take over the current one.
		a.log.Info("billing: event for a non-current subscription ignored", "user", userID, "subscription", s.ID)
		return nil
	case current && terminalStatus(stored.Status):
		// canceled and incomplete_expired are final in Stripe; nothing later
		// about the same subscription can revive it, whatever its timestamp.
		a.log.Info("billing: event for an ended subscription ignored", "user", userID, "subscription", s.ID)
		return nil
	case a.ev.Created < stored.EventCreated:
		a.log.Info("billing: stale subscription event ignored", "user", userID)
		return nil
	case a.ev.Created == stored.EventCreated && storedPro && !pro && !deleted:
		// Same second: Checkout's "incomplete" created-event can arrive after
		// activation. Subscriptions never go back to incomplete, so keep Pro.
		a.log.Info("billing: same-second downgrade ignored", "user", userID, "status", s.Status)
		return nil
	}
	if err := a.b.PutSubscription(store.Subscription{
		UserID: userID, CustomerID: s.Customer, SubscriptionID: s.ID,
		Status: s.Status, EventCreated: a.ev.Created,
	}); err != nil {
		return err
	}
	plan := "free"
	if pro {
		plan = "pro"
	}
	return a.setPlan(userID, plan)
}

func (a applier) paymentFailed() error {
	var inv struct {
		ID       string `json:"id"`
		Customer string `json:"customer"`
	}
	if err := a.decode(&inv); err != nil {
		return err
	}
	userID, err := a.b.UserByCustomer(inv.Customer)
	if errors.Is(err, store.ErrNotFound) {
		a.log.Info("billing: payment failure for unknown customer", "customer", inv.Customer)
		return nil
	}
	if err != nil {
		return err
	}
	return a.b.Audit(userID, "billing.payment_failed", "invoice="+inv.ID)
}
