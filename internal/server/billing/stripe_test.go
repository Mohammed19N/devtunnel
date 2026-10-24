package billing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

const testKey = "sk_test_secretkey123"

func TestPriceLabel(t *testing.T) {
	for _, tc := range []struct {
		p    Price
		want string
	}{
		{Price{Currency: "usd", UnitAmount: 700, Interval: "month"}, "$7 / month"},
		{Price{Currency: "usd", UnitAmount: 750, Interval: "month"}, "$7.50 / month"},
		{Price{Currency: "eur", UnitAmount: 1250, Interval: "month"}, "12.50 EUR / month"},
		{Price{Currency: "eur", UnitAmount: 700, Interval: "year"}, "7.00 EUR / year"},
		{Price{Currency: "jpy", UnitAmount: 1000, Interval: "month"}, "1000 JPY / month"},
	} {
		if got := tc.p.Label(); got != tc.want {
			t.Errorf("%+v.Label() = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// fakeStripe serves one endpoint and hands the parsed request to check.
func fakeStripe(t *testing.T, method, path string, status int, body string, check func(r *http.Request, form url.Values)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method || r.URL.Path != path {
			t.Errorf("request %s %s, want %s %s", r.Method, r.URL.Path, method, path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		var form url.Values
		if r.Method == http.MethodPost {
			if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q", ct)
			}
			raw, _ := io.ReadAll(r.Body)
			var err error
			if form, err = url.ParseQuery(string(raw)); err != nil {
				t.Errorf("form: %v", err)
			}
		}
		if check != nil {
			check(r, form)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{Key: testKey, BaseURL: srv.URL, HTTP: srv.Client()}
}

func wantForm(t *testing.T, form url.Values, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got := form.Get(k); got != v {
			t.Errorf("form[%s] = %q, want %q", k, got, v)
		}
	}
	if len(form) != len(want) {
		t.Errorf("form = %v, want exactly %v", form, want)
	}
}

func TestPriceRequest(t *testing.T) {
	body, err := os.ReadFile("testdata/price.json")
	if err != nil {
		t.Fatal(err)
	}
	c := fakeStripe(t, http.MethodGet, "/v1/prices/price_TEST", 200, string(body), nil)
	p, err := c.Price(context.Background(), "price_TEST")
	if err != nil {
		t.Fatal(err)
	}
	want := Price{ID: "price_TEST", Currency: "usd", Interval: "month", UnitAmount: 700}
	if p != want {
		t.Fatalf("price = %+v, want %+v", p, want)
	}
	if p.Label() != "$7 / month" {
		t.Fatalf("label = %q", p.Label())
	}
}

func TestCheckoutURLRequest(t *testing.T) {
	c := fakeStripe(t, http.MethodPost, "/v1/checkout/sessions", 200, `{"id":"cs_1","url":"https://checkout.stripe.com/x"}`,
		func(r *http.Request, form url.Values) {
			wantForm(t, form, map[string]string{
				"mode":                    "subscription",
				"line_items[0][price]":    "price_TEST",
				"line_items[0][quantity]": "1",
				"client_reference_id":     "42",
				"success_url":             "https://passway.dev/billing/success",
				"cancel_url":              "https://passway.dev/dashboard",
			})
		})
	got, err := c.CheckoutURL(context.Background(), CheckoutParams{
		PriceID:           "price_TEST",
		ClientReferenceID: "42",
		SuccessURL:        "https://passway.dev/billing/success",
		CancelURL:         "https://passway.dev/dashboard",
	})
	if err != nil || got != "https://checkout.stripe.com/x" {
		t.Fatalf("CheckoutURL = %q, %v", got, err)
	}
}

func TestCheckoutURLWithCustomer(t *testing.T) {
	c := fakeStripe(t, http.MethodPost, "/v1/checkout/sessions", 200, `{"url":"https://checkout.stripe.com/y"}`,
		func(r *http.Request, form url.Values) {
			if got := form.Get("customer"); got != "cus_TEST1" {
				t.Errorf("customer = %q", got)
			}
		})
	if _, err := c.CheckoutURL(context.Background(), CheckoutParams{
		PriceID: "price_TEST", CustomerID: "cus_TEST1", ClientReferenceID: "1",
		SuccessURL: "https://a/s", CancelURL: "https://a/c",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPortalURLRequest(t *testing.T) {
	c := fakeStripe(t, http.MethodPost, "/v1/billing_portal/sessions", 200, `{"id":"bps_1","url":"https://billing.stripe.com/p/session/x"}`,
		func(r *http.Request, form url.Values) {
			wantForm(t, form, map[string]string{
				"customer":   "cus_TEST1",
				"return_url": "https://passway.dev/dashboard",
			})
		})
	got, err := c.PortalURL(context.Background(), "cus_TEST1", "https://passway.dev/dashboard")
	if err != nil || got != "https://billing.stripe.com/p/session/x" {
		t.Fatalf("PortalURL = %q, %v", got, err)
	}
}

func TestStripeErrorMessage(t *testing.T) {
	c := fakeStripe(t, http.MethodPost, "/v1/billing_portal/sessions", 400,
		`{"error":{"message":"No such customer: 'cus_nope'","type":"invalid_request_error"}}`, nil)
	_, err := c.PortalURL(context.Background(), "cus_nope", "https://a/")
	if err == nil || !strings.Contains(err.Error(), "No such customer: 'cus_nope'") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaks the key: %v", err)
	}

	// A body that is not Stripe's error JSON still yields an error with the status.
	c = fakeStripe(t, http.MethodGet, "/v1/prices/p", 502, `<html>bad gateway</html>`, nil)
	if _, err := c.Price(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}

	// A 200 without a URL is an error, not an empty redirect.
	c = fakeStripe(t, http.MethodPost, "/v1/checkout/sessions", 200, `{"id":"cs_1"}`, nil)
	if _, err := c.CheckoutURL(context.Background(), CheckoutParams{PriceID: "p"}); err == nil {
		t.Fatal("no error for a session without url")
	}
}
