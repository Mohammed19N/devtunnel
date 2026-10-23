package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func TestNeedsWarning(t *testing.T) {
	mk := func(method, path string, h map[string]string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	html := map[string]string{"Accept": "text/html,application/xhtml+xml"}
	valid := "passway_warned=" + warnToken(testSecret, "example.com")
	tests := []struct {
		name string
		r    *http.Request
		want bool
	}{
		{"browser GET", mk("GET", "/", html), true},
		{"browser HEAD", mk("HEAD", "/", html), true},
		{"POST", mk("POST", "/", html), false},
		{"json", mk("GET", "/", map[string]string{"Accept": "application/json"}), false},
		{"skip header", mk("GET", "/", map[string]string{"Accept": "text/html", "Passway-Skip-Warning": "1"}), false},
		{"valid cookie", mk("GET", "/", map[string]string{"Accept": "text/html", "Cookie": valid}), false},
		{"forged cookie", mk("GET", "/", map[string]string{"Accept": "text/html", "Cookie": "passway_warned=1"}), true},
		{"cookie for other host", mk("GET", "/", map[string]string{"Accept": "text/html",
			"Cookie": "passway_warned=" + warnToken(testSecret, "other.com")}), true},
		{"continue path", mk("GET", "/_passway/continue", html), false},
		{"form POST navigation", mk("POST", "/", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}), true},
		{"POST navigate only", mk("POST", "/", map[string]string{"Sec-Fetch-Mode": "navigate"}), true},
		{"PUT document dest", mk("PUT", "/", map[string]string{"Sec-Fetch-Dest": "document"}), true},
		{"POST navigation with cookie", mk("POST", "/", map[string]string{"Sec-Fetch-Mode": "navigate", "Cookie": valid}), false},
		{"POST webhook", mk("POST", "/hook", map[string]string{"Content-Type": "application/json", "Accept": "*/*"}), false},
		{"GET image dest", mk("GET", "/a.png", map[string]string{"Accept": "text/html", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"}), false},
		{"GET fetch", mk("GET", "/api", map[string]string{"Accept": "text/html", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}), false},
	}
	for _, tc := range tests {
		if got := needsWarning(tc.r, testSecret); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestWarnToken(t *testing.T) {
	a := warnToken(testSecret, "a.passway.dev")
	if len(a) < 32 {
		t.Fatalf("token %q shorter than 16 bytes", a)
	}
	if a == warnToken(testSecret, "b.passway.dev") || a == warnToken([]byte("other secret"), "a.passway.dev") {
		t.Fatal("token does not depend on host and secret")
	}
	if a != warnToken(testSecret, "A.Passway.Dev") {
		t.Fatal("token must be case-insensitive in host")
	}
	if a == warnToken(testSecret, "a.passway.dev:8080") {
		t.Fatal("token must include the port")
	}
}

func TestSafeReturnPath(t *testing.T) {
	for in, want := range map[string]string{
		"/docs?a=1": "/docs?a=1", "/dash?x=1": "/dash?x=1", "": "/", "//evil.com": "/", `/\evil.com`: "/",
		"https://evil.com": "/", "javascript:alert(1)": "/", "evil.com": "/",
		`/./\evil.com`: "/", `/../\evil.com`: "/", "/%5Cevil.com": "/", "/%5cevil.com": "/",
		"/\tevil.com": "/", "/\nevil.com": "/", "/a\x7f": "/", "/%0d%0aSet-Cookie:x=1": "/",
	} {
		if got := safeReturnPath(in); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleContinue(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_passway/continue?to=%2Fdocs", nil)
	r.Host = "Demo.passway.dev"
	handleContinue(w, r, testSecret, false)
	if w.Code != 303 || w.Header().Get("Location") != "/docs" {
		t.Fatalf("code=%d loc=%q", w.Code, w.Header().Get("Location"))
	}
	c := w.Header().Get("Set-Cookie")
	for _, want := range []string{"passway_warned=" + warnToken(testSecret, "demo.passway.dev"), "HttpOnly", "SameSite=Lax", "Max-Age=604800", "Path=/"} {
		if !strings.Contains(c, want) {
			t.Errorf("cookie %q missing %q", c, want)
		}
	}
	for _, bad := range []string{"Domain", "Secure"} {
		if strings.Contains(c, bad) {
			t.Errorf("cookie has %s: %q", bad, c)
		}
	}
	w = httptest.NewRecorder()
	handleContinue(w, httptest.NewRequest("GET", "/_passway/continue?to=%2F", nil), testSecret, true)
	if c := w.Header().Get("Set-Cookie"); !strings.Contains(c, "; Secure") {
		t.Errorf("https cookie not Secure: %q", c)
	}
}

func TestHandleContinueLocation(t *testing.T) {
	for _, tc := range []struct{ to, want string }{
		{`/./\x`, "/"}, {`/../\x`, "/"}, {"/%5Cx", "/"}, {"/\tx", "/"}, {"/\nx", "/"},
		{"//x", "/"}, {`/\x`, "/"}, {"/dash?x=1", "/dash?x=1"},
	} {
		w := httptest.NewRecorder()
		handleContinue(w, httptest.NewRequest("GET", "/_passway/continue?to="+url.QueryEscape(tc.to), nil), testSecret, false)
		if w.Code != 303 || w.Header().Get("Location") != tc.want {
			t.Errorf("to=%q: code=%d Location=%q, want %q", tc.to, w.Code, w.Header().Get("Location"), tc.want)
		}
	}
}

func TestStripWarnCookie(t *testing.T) {
	h := http.Header{"Cookie": {"a=1; passway_warned=abc; b=2", "passway_warned=x"}}
	stripWarnCookie(h)
	if got := h["Cookie"]; len(got) != 1 || got[0] != "a=1; b=2" {
		t.Fatalf("Cookie = %q", got)
	}
	h = http.Header{"Cookie": {"passway_warned=x"}}
	stripWarnCookie(h)
	if _, ok := h["Cookie"]; ok {
		t.Fatalf("Cookie = %q, want removed", h["Cookie"])
	}
}

func TestDropWarnSetCookie(t *testing.T) {
	h := http.Header{"Set-Cookie": {"sid=1; Path=/", "passway_warned=forged; Domain=localhost; Path=/", " passway_warned =x", "Passway_Warned=keep"}}
	dropWarnSetCookie(h)
	if got := h["Set-Cookie"]; len(got) != 1 || got[0] != "sid=1; Path=/" {
		t.Fatalf("Set-Cookie = %q", got)
	}
	h = http.Header{"Set-Cookie": {"passway_warned=1"}}
	dropWarnSetCookie(h)
	if _, ok := h["Set-Cookie"]; ok {
		t.Fatal("Set-Cookie should be removed")
	}
}

func TestDropWarnSetCookieNameless(t *testing.T) {
	cases := []struct {
		line string
		kept bool
	}{
		{"=passway_warned=abc; Path=/", false},
		{" =passway_warned=abc", false},
		{"Passway_Warned=abc", false},
		{"passway_warned=abc; Domain=localhost", false},
		{"session=1; Path=/", true},
		{"=other", true},
	}
	for _, c := range cases {
		h := http.Header{"Set-Cookie": {c.line}}
		dropWarnSetCookie(h)
		_, got := h["Set-Cookie"]
		if got != c.kept {
			t.Errorf("Set-Cookie %q: kept = %v, want %v", c.line, got, c.kept)
		}
	}
	h := http.Header{"Set-Cookie": {"=passway_warned=bad; Path=/", "=ok=1; Path=/"}}
	dropWarnSetCookie(h)
	if got := h["Set-Cookie"]; len(got) != 1 || got[0] != "=ok=1; Path=/" {
		t.Fatalf("Set-Cookie = %q, want only the good value", got)
	}
}

func TestEdgeWarningFlow(t *testing.T) {
	_, ts := newTestServerCfg(t, nil, func(c *Config) { c.BrowserWarning = true; c.CookieSecret = testSecret })
	resp, sess := dialRaw(t, ts, hello("warn", "A"))
	if resp.Ready == nil {
		t.Fatalf("ready: %+v", resp.Error)
	}
	var hits atomic.Int32
	var appCookie atomic.Value
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		appCookie.Store(strings.Join(r.Header.Values("Cookie"), "|"))
		w.Header().Add("Set-Cookie", "passway_warned=forged; Domain=localhost; Path=/")
		w.Header().Add("Set-Cookie", "sid=1; Path=/")
		w.WriteHeader(200)
		io.WriteString(w, "app")
	})}
	go hs.Serve(streamListener{sess})
	t.Cleanup(func() { hs.Close() })

	u, _ := url.Parse(ts.URL)
	host := "warn.localhost:" + u.Port()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path string, h map[string]string) (*http.Response, string) {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		req.Host = host
		for k, v := range h {
			req.Header.Set(k, v)
		}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}
	html := map[string]string{"Accept": "text/html"}
	r, body := do("GET", "/x?y=1", html)
	if r.StatusCode != 200 || body == "app" || !strings.Contains(body, "continue?to=") || hits.Load() != 0 {
		t.Fatalf("warning: code=%d hits=%d body=%q", r.StatusCode, hits.Load(), body)
	}
	r, body = do("GET", "/x?y=1", map[string]string{"Accept": "text/html", "Cookie": "passway_warned=1"})
	if body == "app" || hits.Load() != 0 {
		t.Fatalf("forged cookie: code=%d hits=%d body=%q", r.StatusCode, hits.Load(), body)
	}
	r, body = do("POST", "/x", map[string]string{"Accept": "text/html", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"})
	if !strings.Contains(body, "continue?to=") || hits.Load() != 0 {
		t.Fatalf("form POST navigation: code=%d hits=%d body=%q", r.StatusCode, hits.Load(), body)
	}

	r, _ = do("GET", "/_passway/continue?to=%2Fx", html)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/x" {
		t.Fatalf("continue: code=%d Location=%q", r.StatusCode, r.Header.Get("Location"))
	}
	var tok string
	for _, c := range r.Cookies() {
		if c.Name == "passway_warned" {
			tok = c.Value
		}
	}
	if tok != warnToken(testSecret, host) {
		t.Fatalf("cookie value %q, want HMAC of %q", tok, host)
	}
	r, _ = do("GET", "/_passway/continue?to="+url.QueryEscape(`/./\evil.com`), html)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/" {
		t.Fatalf("open redirect: code=%d Location=%q", r.StatusCode, r.Header.Get("Location"))
	}

	r, body = do("GET", "/x?y=1", map[string]string{"Accept": "text/html", "Cookie": "a=1; passway_warned=" + tok + "; b=2"})
	if r.StatusCode != 200 || body != "app" || hits.Load() != 1 {
		t.Fatalf("cookie: code=%d hits=%d body=%q", r.StatusCode, hits.Load(), body)
	}
	if got := appCookie.Load(); got != "a=1; b=2" {
		t.Fatalf("app saw Cookie %q, want the warned cookie stripped", got)
	}
	if sc := r.Header.Values("Set-Cookie"); len(sc) != 1 || sc[0] != "sid=1; Path=/" {
		t.Fatalf("Set-Cookie = %q, want only the app's own cookie", sc)
	}
	r, _ = do("POST", "/hook", map[string]string{"Content-Type": "application/json"})
	if r.StatusCode != 200 || hits.Load() != 2 {
		t.Fatalf("webhook: code=%d hits=%d", r.StatusCode, hits.Load())
	}
}

func TestContinueCookieSecureOnHTTPS(t *testing.T) {
	srv, ts := newTestServerCfg(t, nil, func(c *Config) { c.BrowserWarning = true; c.PublicScheme = "https" })
	if len(srv.cfg.CookieSecret) != 32 {
		t.Fatalf("generated secret len = %d, want 32", len(srv.cfg.CookieSecret))
	}
	u, _ := url.Parse(ts.URL)
	req, _ := http.NewRequest("GET", ts.URL+"/_passway/continue?to=%2F", nil)
	req.Host = "any.localhost:" + u.Port()
	r, err := (&http.Transport{}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if c := r.Header.Get("Set-Cookie"); !strings.Contains(c, "; Secure") {
		t.Fatalf("Set-Cookie = %q, want Secure", c)
	}
}

func TestContinueDisabled(t *testing.T) {
	_, ts := newTestServerCfg(t, nil, nil)
	u, _ := url.Parse(ts.URL)
	req, _ := http.NewRequest("GET", ts.URL+"/_passway/continue", nil)
	req.Host = "any.localhost:" + u.Port()
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatalf("status %d", r.StatusCode)
	}
}
