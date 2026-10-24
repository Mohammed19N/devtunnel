// Package billing talks to Stripe (a small hand-rolled client, no SDK) and
// turns verified webhook events into plan changes in the store.
package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.stripe.com"

// Client calls the Stripe API with a secret key. The key is sent only in the
// Authorization header and never appears in errors or logs.
type Client struct {
	Key     string
	BaseURL string       // default https://api.stripe.com
	HTTP    *http.Client // default: a client with a 30 s timeout
}

var defaultHTTP = &http.Client{Timeout: 30 * time.Second}

// Price is the part of a Stripe price we show.
type Price struct {
	ID, Currency, Interval string
	UnitAmount             int64 // in the currency's smallest unit
}

// zeroDecimal lists Stripe's zero-decimal currencies, whose amounts are not in
// hundredths.
var zeroDecimal = map[string]bool{
	"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true, "kmf": true,
	"krw": true, "mga": true, "pyg": true, "rwf": true, "ugx": true, "vnd": true,
	"vuv": true, "xaf": true, "xof": true, "xpf": true,
}

// Label renders the price: "$7 / month" (or "$7.50 / month") for usd,
// otherwise "7.00 EUR / month".
func (p Price) Label() string {
	cur := strings.ToLower(p.Currency)
	var amount string
	switch {
	case zeroDecimal[cur]:
		amount = fmt.Sprintf("%d %s", p.UnitAmount, strings.ToUpper(cur))
	case cur == "usd" && p.UnitAmount%100 == 0:
		amount = fmt.Sprintf("$%d", p.UnitAmount/100)
	case cur == "usd":
		amount = fmt.Sprintf("$%d.%02d", p.UnitAmount/100, p.UnitAmount%100)
	default:
		amount = fmt.Sprintf("%d.%02d %s", p.UnitAmount/100, p.UnitAmount%100, strings.ToUpper(cur))
	}
	return amount + " / " + p.Interval
}

// Price fetches GET /v1/prices/{id}.
func (c *Client) Price(ctx context.Context, id string) (Price, error) {
	var out struct {
		ID         string `json:"id"`
		Currency   string `json:"currency"`
		UnitAmount *int64 `json:"unit_amount"`
		Recurring  *struct {
			Interval string `json:"interval"`
		} `json:"recurring"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/prices/"+url.PathEscape(id), nil, &out); err != nil {
		return Price{}, err
	}
	if out.UnitAmount == nil || out.Recurring == nil {
		return Price{}, fmt.Errorf("stripe: price %s is not a fixed recurring price", id)
	}
	return Price{ID: out.ID, Currency: out.Currency, Interval: out.Recurring.Interval, UnitAmount: *out.UnitAmount}, nil
}

// CheckoutParams describes a subscription Checkout Session. CustomerID is
// optional; ClientReferenceID carries our user ID back in the webhook.
type CheckoutParams struct {
	PriceID, CustomerID, ClientReferenceID, SuccessURL, CancelURL string
}

// CheckoutURL creates a subscription Checkout Session and returns its URL.
func (c *Client) CheckoutURL(ctx context.Context, p CheckoutParams) (string, error) {
	form := url.Values{
		"mode":                    {"subscription"},
		"line_items[0][price]":    {p.PriceID},
		"line_items[0][quantity]": {"1"},
		"client_reference_id":     {p.ClientReferenceID},
		"success_url":             {p.SuccessURL},
		"cancel_url":              {p.CancelURL},
	}
	if p.CustomerID != "" {
		form.Set("customer", p.CustomerID)
	}
	return c.sessionURL(ctx, "/v1/checkout/sessions", form)
}

// PortalURL creates a Billing Portal session and returns its URL.
func (c *Client) PortalURL(ctx context.Context, customerID, returnURL string) (string, error) {
	return c.sessionURL(ctx, "/v1/billing_portal/sessions", url.Values{
		"customer":   {customerID},
		"return_url": {returnURL},
	})
}

func (c *Client) sessionURL(ctx context.Context, path string, form url.Values) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.do(ctx, http.MethodPost, path, form, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("stripe: %s returned no url", path)
	}
	return out.URL, nil
}

// do sends a form-encoded request (form may be nil) and decodes a 2xx JSON
// body into out. Stripe's error.message becomes the error text.
func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return fmt.Errorf("stripe: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		// *url.Error names the URL, never the headers, so the key stays out.
		return fmt.Errorf("stripe: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe: %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("stripe: %s", e.Error.Message)
		}
		return fmt.Errorf("stripe: %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("stripe: %s %s: decode: %w", method, path, err)
	}
	return nil
}
