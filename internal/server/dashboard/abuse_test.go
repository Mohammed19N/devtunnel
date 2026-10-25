package dashboard

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func abuseForm() url.Values {
	return url.Values{
		"url":      {"https://swift-fox-1.passway.test/login"},
		"category": {"phishing"},
		"details":  {"Copies a bank's sign-in page."},
		"email":    {"reporter@example.com"},
		"website":  {""},
	}
}

// postAbuse POSTs the abuse form from ip with the given Origin ("" = none).
func (e *testEnv) postAbuse(form url.Values, origin, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/abuse", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	e.d.ServeHTTP(w, r)
	return w
}

func (e *testEnv) abuseCount(t *testing.T) int {
	t.Helper()
	list, err := e.st.AbuseReports(context.Background(), false, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAbuseReportStored(t *testing.T) {
	var logs syncBuf
	e := newTestEnv(t, func(c *Config) { c.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	form := abuseForm()
	w := e.postAbuse(form, testBaseURL, "203.0.113.7")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/abuse?sent=1" {
		t.Fatalf("POST /abuse = %d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	list, err := e.st.AbuseReports(context.Background(), true, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("reports = %+v %v", list, err)
	}
	r := list[0]
	if r.URL != form.Get("url") || r.Category != "phishing" || r.Details != form.Get("details") ||
		r.Email != "reporter@example.com" || r.IP != "203.0.113.7" {
		t.Errorf("stored = %+v", r)
	}
	if l := logs.String(); !strings.Contains(l, "level=WARN") || !strings.Contains(l, `msg="abuse report"`) {
		t.Errorf("no WARN abuse report log:\n%s", l)
	}
	if body := e.do("GET", "/abuse?sent=1", nil).Body.String(); !strings.Contains(body, "Thank you") {
		t.Error("no confirmation after sending")
	}
	// Email is optional; a Referer from the site is as good as an Origin.
	form.Del("email")
	rq := httptest.NewRequest("POST", "/abuse", strings.NewReader(form.Encode()))
	rq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rq.Header.Set("Referer", testBaseURL+"/abuse")
	rw := httptest.NewRecorder()
	e.d.ServeHTTP(rw, rq)
	if rw.Code != http.StatusSeeOther || e.abuseCount(t) != 2 {
		t.Errorf("Referer + no email: %d, %d stored", rw.Code, e.abuseCount(t))
	}
}

func TestAbuseCrossOrigin403(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, origin := range []string{"http://evil.localhost:8080", "null", ""} {
		if w := e.postAbuse(abuseForm(), origin, "203.0.113.7"); w.Code != http.StatusForbidden {
			t.Errorf("Origin %q: code %d, want 403", origin, w.Code)
		}
	}
	if n := e.abuseCount(t); n != 0 {
		t.Errorf("%d reports stored", n)
	}
}

func TestAbuseHoneypot(t *testing.T) {
	e := newTestEnv(t, nil)
	form := abuseForm()
	form.Set("website", "http://spam.example")
	w := e.postAbuse(form, testBaseURL, "203.0.113.7")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/abuse?sent=1" {
		t.Errorf("honeypot = %d %q, want a silent 303", w.Code, w.Header().Get("Location"))
	}
	if n := e.abuseCount(t); n != 0 {
		t.Errorf("%d reports stored", n)
	}
}

func TestAbuseRateLimit(t *testing.T) {
	e := newTestEnv(t, nil)
	for i := 0; i < 5; i++ {
		if w := e.postAbuse(abuseForm(), testBaseURL, "203.0.113.7"); w.Code != http.StatusSeeOther {
			t.Fatalf("report %d: %d", i+1, w.Code)
		}
	}
	w := e.postAbuse(abuseForm(), testBaseURL, "203.0.113.7")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th report = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	if n := e.abuseCount(t); n != 5 {
		t.Errorf("%d stored, want 5", n)
	}
	if w := e.postAbuse(abuseForm(), testBaseURL, "203.0.113.8"); w.Code != http.StatusSeeOther {
		t.Errorf("another IP = %d, want 303", w.Code)
	}
}

func TestAbuseInvalidURL(t *testing.T) {
	e := newTestEnv(t, nil)
	bad := []url.Values{}
	for _, u := range []string{"", "javascript:alert(1)", "ftp://x.passway.test/", "https://", "not a url", "https://" + strings.Repeat("a", 2100) + ".test/"} {
		f := abuseForm()
		f.Set("url", u)
		bad = append(bad, f)
	}
	f := abuseForm()
	f.Set("category", "rude")
	bad = append(bad, f)
	f = abuseForm()
	f.Set("details", "  ")
	bad = append(bad, f)
	f = abuseForm()
	f.Set("details", strings.Repeat("x", 5001))
	bad = append(bad, f)
	f = abuseForm()
	f.Set("email", "not-an-email")
	bad = append(bad, f)
	for _, f := range bad {
		w := e.postAbuse(f, testBaseURL, "203.0.113.7")
		if w.Code != http.StatusBadRequest {
			t.Errorf("url=%.40q category=%q details=%d email=%q: code %d, want 400",
				f.Get("url"), f.Get("category"), len(f.Get("details")), f.Get("email"), w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), `class="error"`) {
			t.Errorf("400 without an inline error")
		}
	}
	if n := e.abuseCount(t); n != 0 {
		t.Errorf("%d reports stored", n)
	}
	// Exactly 5000 characters is fine.
	f = abuseForm()
	f.Set("details", strings.Repeat("é", 5000))
	if w := e.postAbuse(f, testBaseURL, "203.0.113.9"); w.Code != http.StatusSeeOther {
		t.Errorf("5000-char details = %d, want 303", w.Code)
	}
}
