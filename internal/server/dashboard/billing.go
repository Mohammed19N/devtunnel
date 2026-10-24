package dashboard

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/mohammed19n/passway/internal/server/billing"
	"github.com/mohammed19n/passway/internal/server/store"
)

const maxWebhookBody = 1 << 20

func (d *Dashboard) handleCheckout(w http.ResponseWriter, r *http.Request, s *session) {
	p := billing.CheckoutParams{
		PriceID:           d.cfg.PriceID,
		ClientReferenceID: strconv.FormatInt(s.User.ID, 10),
		SuccessURL:        d.cfg.BaseURL + "/dashboard?upgraded=1",
		CancelURL:         d.cfg.BaseURL + "/dashboard",
	}
	sub, err := d.cfg.Store.SubscriptionByUser(r.Context(), s.User.ID)
	switch {
	case err == nil:
		p.CustomerID = sub.CustomerID
	case !errors.Is(err, store.ErrNotFound):
		d.internalError(w, "subscription", err)
		return
	}
	u, err := d.cfg.Billing.CheckoutURL(r.Context(), p)
	if err != nil {
		d.log.Error("dashboard: checkout", "user", s.User.ID, "err", err)
		d.fail(w, http.StatusBadGateway, "We couldn't reach our payment provider. Please try again in a moment.")
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func (d *Dashboard) handlePortal(w http.ResponseWriter, r *http.Request, s *session) {
	sub, err := d.cfg.Store.SubscriptionByUser(r.Context(), s.User.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sub.CustomerID == "") {
		d.fail(w, http.StatusNotFound, "There's no billing account to manage yet.")
		return
	}
	if err != nil {
		d.internalError(w, "subscription", err)
		return
	}
	u, err := d.cfg.Billing.PortalURL(r.Context(), sub.CustomerID, d.cfg.BaseURL+"/dashboard")
	if err != nil {
		d.log.Error("dashboard: portal", "user", s.User.ID, "err", err)
		d.fail(w, http.StatusBadGateway, "We couldn't reach our payment provider. Please try again in a moment.")
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// handleStripeWebhook verifies the signature, applies the event and reports
// changed plans. A bad or stale signature is 400 and changes nothing.
func (d *Dashboard) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ev, err := billing.VerifyWebhook(body, r.Header.Get("Stripe-Signature"), d.cfg.WebhookSecret, d.cfg.Now())
	if err != nil {
		d.log.Warn("dashboard: stripe webhook rejected", "err", err)
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	changed, err := billing.Apply(r.Context(), d.cfg.Store, ev)
	if err != nil {
		d.log.Error("dashboard: stripe webhook", "event", ev.ID, "type", ev.Type, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, id := range changed {
		d.cfg.PlanChanged(id)
	}
	w.WriteHeader(http.StatusOK)
}
