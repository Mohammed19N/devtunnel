package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mohammed19n/passway/internal/server"
)

// Subscription is a user's Stripe subscription as of the newest applied
// event (EventCreated, Stripe's event.created in Unix seconds).
type Subscription struct {
	UserID         int64
	CustomerID     string
	SubscriptionID string
	Status         string
	EventCreated   int64
}

const subCols = `user_id, customer_id, subscription_id, status, event_created`

func scanSub(r scanner) (Subscription, error) {
	var s Subscription
	err := r.Scan(&s.UserID, &s.CustomerID, &s.SubscriptionID, &s.Status, &s.EventCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	return s, err
}

// SubscriptionByUser returns ErrNotFound for a user who never subscribed.
func (s *Store) SubscriptionByUser(ctx context.Context, userID int64) (Subscription, error) {
	return scanSub(s.db.QueryRowContext(ctx, `SELECT `+subCols+` FROM subscriptions WHERE user_id = ?`, userID))
}

// errSeen aborts ApplyStripeEvent's transaction for an already-recorded event.
var errSeen = errors.New("store: stripe event already applied")

// ApplyStripeEvent runs in one IMMEDIATE transaction: if eventID was already
// recorded it returns (false, nil) without calling apply. Otherwise it records
// the id and calls apply; if apply fails, nothing (not even the id) is kept,
// so Stripe's retry is applied afresh.
func (s *Store) ApplyStripeEvent(ctx context.Context, eventID, typ string, apply func(*BillingTx) error) (bool, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO stripe_events (id, type, received_at) VALUES (?, ?, ?)`,
			eventID, typ, ts(time.Now()))
		if isUnique(err) {
			return errSeen
		}
		if err != nil {
			return err
		}
		return apply(&BillingTx{ctx: ctx, tx: tx})
	})
	if errors.Is(err, errSeen) {
		return false, nil
	}
	return err == nil, err
}

// BillingTx is the view of the store inside ApplyStripeEvent's transaction.
// It is valid only during the apply callback.
type BillingTx struct {
	ctx context.Context
	tx  *sql.Tx
}

// UserByCustomer returns ErrNotFound for an unknown Stripe customer.
func (b *BillingTx) UserByCustomer(customerID string) (int64, error) {
	var id int64
	err := b.tx.QueryRowContext(b.ctx, `SELECT user_id FROM subscriptions WHERE customer_id = ?`, customerID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// Subscription returns ErrNotFound for a user who never subscribed.
func (b *BillingTx) Subscription(userID int64) (Subscription, error) {
	return scanSub(b.tx.QueryRowContext(b.ctx, `SELECT `+subCols+` FROM subscriptions WHERE user_id = ?`, userID))
}

// PutSubscription inserts or replaces the user's subscription row.
func (b *BillingTx) PutSubscription(sub Subscription) error {
	_, err := b.tx.ExecContext(b.ctx,
		`INSERT INTO subscriptions (user_id, customer_id, subscription_id, status, event_created, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (user_id) DO UPDATE SET
		   customer_id = excluded.customer_id, subscription_id = excluded.subscription_id,
		   status = excluded.status, event_created = excluded.event_created, updated_at = excluded.updated_at`,
		sub.UserID, sub.CustomerID, sub.SubscriptionID, sub.Status, sub.EventCreated, ts(time.Now()))
	return err
}

// Plan returns the user's current plan (ErrNotFound for no such user).
func (b *BillingTx) Plan(userID int64) (string, error) {
	var plan string
	err := b.tx.QueryRowContext(b.ctx, `SELECT plan FROM users WHERE id = ?`, userID).Scan(&plan)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return plan, err
}

// SetPlan sets the user's plan (ErrNotFound for no such user). For "pro" it
// also admits the user, like `admin admit`: it clears waitlisted and, if the
// user has no default subdomain, claims a random one, since a payer may never
// sign in again to have the dashboard claim it.
func (b *BillingTx) SetPlan(userID int64, plan string) error {
	res, err := b.tx.ExecContext(b.ctx, `UPDATE users SET plan = ? WHERE id = ?`, plan, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if plan != "pro" {
		return nil
	}
	if _, err := b.tx.ExecContext(b.ctx, `UPDATE users SET waitlisted = 0 WHERE id = ?`, userID); err != nil {
		return err
	}
	var n int
	if err := b.tx.QueryRowContext(b.ctx,
		`SELECT count(*) FROM subdomains WHERE user_id = ? AND is_default = 1`, userID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	for range 20 {
		// A failed INSERT on a constraint undoes only that statement in
		// SQLite, so the transaction stays usable for the next attempt.
		_, err = b.tx.ExecContext(b.ctx,
			`INSERT INTO subdomains (name, user_id, is_default, created_at) VALUES (?, ?, 1, ?)`,
			server.RandomName(), userID, ts(time.Now()))
		if !isUnique(err) {
			return err
		}
	}
	return ErrTaken
}

// Audit writes an audit_log row inside the transaction (Store.Audit would
// wait on the single connection this transaction holds).
func (b *BillingTx) Audit(userID int64, action, detail string) error {
	var uid any
	if userID != 0 {
		uid = userID
	}
	_, err := b.tx.ExecContext(b.ctx,
		`INSERT INTO audit_log (user_id, action, detail, created_at) VALUES (?, ?, ?, ?)`,
		uid, action, detail, ts(time.Now()))
	return err
}
