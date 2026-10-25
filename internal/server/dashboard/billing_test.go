package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/server/billing"
)

const whSecret = "whsec_dash"

type fakeStripe struct {
	srv  *httptest.Server
	mu   sync.Mutex
	form map[string]url.Values // by path
}

func newFakeStripe(t *testing.T) *fakeStripe {
	t.Helper()
	f := &fakeStripe{form: map[string]url.Values{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.form[r.URL.Path] = r.PostForm
		f.mu.Unlock()
		switch r.URL.Path {
		case "/v1/checkout/sessions":
			fmt.Fprint(w, `{"url":"https://checkout.stripe.com/c/pay/cs_test_1"}`)
		case "/v1/billing_portal/sessions":
			fmt.Fprint(w, `{"url":"https://billing.stripe.com/p/session/bps_1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStripe) got(path string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.form[path]
}

func billingEnv(t *testing.T, planChanged func(int64)) (*testEnv, *fakeStripe) {
	t.Helper()
	fs := newFakeStripe(t)
	e := newTestEnv(t, func(c *Config) {
		c.Billing = &billing.Client{Key: "sk_test_x", BaseURL: fs.srv.URL}
		c.ProPrice = billing.Price{ID: "price_1", Currency: "usd", Interval: "month", UnitAmount: 700}
		c.PriceID = "price_1"
		c.WebhookSecret = whSecret
		c.PlanChanged = planChanged
	})
	return e, fs
}

func whSig(payload string, secret string, ts int64) string {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.%s", ts, payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(m.Sum(nil)))
}

func (e *testEnv) webhook(payload, header string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/stripe/webhook", strings.NewReader(payload))
	r.Host = "localhost:8080"
	if header != "" {
		r.Header.Set("Stripe-Signature", header)
	}
	w := httptest.NewRecorder()
	e.d.ServeHTTP(w, r)
	return w
}

func checkoutEvent(userID int64) string {
	return fmt.Sprintf(`{"id":"evt_1","type":"checkout.session.completed","created":100,"data":{"object":{"mode":"subscription","payment_status":"paid","client_reference_id":"%d","customer":"cus_1","subscription":"sub_1"}}}`, userID)
}

func TestCheckoutRedirect(t *testing.T) {
	e, fs := billingEnv(t, nil)
	sc, csrf := e.signedIn(t)
	u := mustUser(t, e, "alice")
	w := e.post("/billing/checkout", nil, sc, csrf)
	if w.Code != 303 || w.Header().Get("Location") != "https://checkout.stripe.com/c/pay/cs_test_1" {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Location"))
	}
	f := fs.got("/v1/checkout/sessions")
	if f.Get("client_reference_id") != strconv.FormatInt(u.ID, 10) ||
		f.Get("line_items[0][price]") != "price_1" ||
		f.Get("success_url") != testBaseURL+"/dashboard?upgraded=1" ||
		f.Get("cancel_url") != testBaseURL+"/dashboard" || f.Get("customer") != "" {
		t.Fatalf("form = %v", f)
	}
}

func TestCheckoutReusesCustomer(t *testing.T) {
	e, fs := billingEnv(t, nil)
	sc, csrf := e.signedIn(t)
	u := mustUser(t, e, "alice")
	if w := e.webhook(checkoutEvent(u.ID), whSig(checkoutEvent(u.ID), whSecret, time.Now().Unix())); w.Code != 200 {
		t.Fatalf("webhook %d", w.Code)
	}
	// A returning customer (subscription ended) checks out again as itself.
	ended := `{"id":"evt_2","type":"customer.subscription.deleted","created":101,"data":{"object":{"id":"sub_1","customer":"cus_1","status":"canceled"}}}`
	if w := e.webhook(ended, whSig(ended, whSecret, time.Now().Unix())); w.Code != 200 {
		t.Fatalf("webhook %d", w.Code)
	}
	if w := e.post("/billing/checkout", nil, sc, csrf); w.Code != 303 {
		t.Fatalf("checkout after cancel = %d %s", w.Code, w.Body)
	}
	if got := fs.got("/v1/checkout/sessions").Get("customer"); got != "cus_1" {
		t.Fatalf("customer = %q", got)
	}
}

// A second checkout while a subscription keeps Pro would start a second paid
// subscription (review M2): refuse it with 409 and never call Stripe.
func TestCheckoutRefusedWhileSubscribed(t *testing.T) {
	e, fs := billingEnv(t, nil)
	sc, csrf := e.signedIn(t)
	u := mustUser(t, e, "alice")
	if w := e.webhook(checkoutEvent(u.ID), whSig(checkoutEvent(u.ID), whSecret, time.Now().Unix())); w.Code != 200 {
		t.Fatalf("webhook %d", w.Code)
	}
	w := e.post("/billing/checkout", nil, sc, csrf)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already on Pro") {
		t.Fatalf("second checkout = %d %s", w.Code, w.Body)
	}
	if f := fs.got("/v1/checkout/sessions"); f != nil {
		t.Fatalf("Stripe checkout was called: %v", f)
	}
}

func TestPortalRequiresCustomer(t *testing.T) {
	e, fs := billingEnv(t, nil)
	sc, csrf := e.signedIn(t)
	if w := e.post("/billing/portal", nil, sc, csrf); w.Code != 404 {
		t.Fatalf("no customer: %d", w.Code)
	}
	u := mustUser(t, e, "alice")
	p := checkoutEvent(u.ID)
	e.webhook(p, whSig(p, whSecret, time.Now().Unix()))
	w := e.post("/billing/portal", nil, sc, csrf)
	if w.Code != 303 || w.Header().Get("Location") != "https://billing.stripe.com/p/session/bps_1" {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Location"))
	}
	f := fs.got("/v1/billing_portal/sessions")
	if f.Get("customer") != "cus_1" || f.Get("return_url") != testBaseURL+"/dashboard" {
		t.Fatalf("form = %v", f)
	}
}

func TestCheckoutRequiresCSRF(t *testing.T) {
	e, fs := billingEnv(t, nil)
	sc, csrf := e.signedIn(t)
	for _, path := range []string{"/billing/checkout", "/billing/portal"} {
		if w := e.post(path, nil, sc, ""); w.Code != 403 {
			t.Errorf("%s without csrf: %d", path, w.Code)
		}
		if w := e.post(path, nil, nil, csrf); w.Code != 403 {
			t.Errorf("%s without session: %d", path, w.Code)
		}
	}
	if fs.got("/v1/checkout/sessions") != nil {
		t.Error("Stripe was called")
	}
}

func TestBillingRoutesAbsentWithoutBilling(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	if w := e.post("/billing/checkout", nil, sc, csrf); w.Code != 404 {
		t.Errorf("checkout: %d", w.Code)
	}
	if w := e.webhook("{}", "t=1,v1=00"); w.Code != 404 {
		t.Errorf("webhook: %d", w.Code)
	}
}

func TestWebhookBadSignature400(t *testing.T) {
	var called []int64
	e, _ := billingEnv(t, func(id int64) { called = append(called, id) })
	sc, _ := e.signedIn(t)
	u := mustUser(t, e, "alice")
	p := checkoutEvent(u.ID)
	now := time.Now().Unix()
	for name, h := range map[string]string{
		"missing":      "",
		"wrong secret": whSig(p, "whsec_other", now),
		"stale":        whSig(p, whSecret, now-3600),
		"future":       whSig(p, whSecret, now+3600),
	} {
		if w := e.webhook(p, h); w.Code != 400 {
			t.Errorf("%s: code %d, want 400", name, w.Code)
		}
	}
	if got := mustUser(t, e, "alice").Plan; got != "free" || len(called) != 0 {
		t.Fatalf("plan = %q, PlanChanged calls = %v; want nothing changed", got, called)
	}
	_ = sc
}

func TestWebhookUpgradeCallsPlanChanged(t *testing.T) {
	var called []int64
	e, _ := billingEnv(t, func(id int64) { called = append(called, id) })
	sc, _ := e.signedIn(t)
	u := mustUser(t, e, "alice")
	p := checkoutEvent(u.ID)
	if w := e.webhook(p, whSig(p, whSecret, time.Now().Unix())); w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	if got := mustUser(t, e, "alice").Plan; got != "pro" {
		t.Fatalf("plan = %q", got)
	}
	if len(called) != 1 || called[0] != u.ID {
		t.Fatalf("PlanChanged = %v", called)
	}
	body := e.do("GET", "/dashboard", nil, sc).Body.String()
	if !strings.Contains(body, "Manage billing") || strings.Contains(body, "Upgrade to Pro") {
		t.Fatalf("pro dashboard: %s", body)
	}
	// Redelivery changes nothing.
	called = nil
	if w := e.webhook(p, whSig(p, whSecret, time.Now().Unix())); w.Code != 200 || len(called) != 0 {
		t.Fatalf("redelivery: %d %v", w.Code, called)
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	e, _ := billingEnv(t, nil)
	big := strings.Repeat("a", 1<<20+1)
	if w := e.webhook(big, whSig(big, whSecret, time.Now().Unix())); w.Code == 200 {
		t.Fatalf("oversized body accepted")
	}
}

func TestUpgradeButtonShownWithBilling(t *testing.T) {
	e, _ := billingEnv(t, nil)
	sc, _ := e.signedIn(t)
	body := e.do("GET", "/dashboard", nil, sc).Body.String()
	if !strings.Contains(body, "Upgrade to Pro — $7 / month") || !strings.Contains(body, `action="/billing/checkout"`) || strings.Contains(body, "Coming soon") {
		t.Fatalf("body: %s", body)
	}
	body = e.do("GET", "/dashboard?upgraded=1", nil, sc).Body.String()
	if !strings.Contains(body, "Payment received — your plan updates in a few seconds.") {
		t.Fatalf("no payment copy: %s", body)
	}
}

func TestUpgradeButtonHiddenWithoutBilling(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, _ := e.signedIn(t)
	body := e.do("GET", "/dashboard?upgraded=1", nil, sc).Body.String()
	if strings.Contains(body, "/billing/checkout") || strings.Contains(body, "Payment received") ||
		!strings.Contains(body, "Coming soon") || !strings.Contains(body, "disabled") {
		t.Fatalf("body: %s", body)
	}
}

func TestCSPAllowsStripeFormAction(t *testing.T) {
	e := newTestEnv(t, nil)
	csp := e.do("GET", "/login", nil).Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' https://github.com https://checkout.stripe.com https://billing.stripe.com;") {
		t.Fatalf("csp = %s", csp)
	}
}
