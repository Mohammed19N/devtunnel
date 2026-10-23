package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// signedIn signs alice in and returns her session cookie and CSRF token.
func (e *testEnv) signedIn(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	e.gh.addUser(42, "alice", 400*24*time.Hour)
	w := e.signIn(t, "alice")
	sc := cookieNamed(w, e.prefix+"passway_session")
	if sc == nil {
		t.Fatalf("no session cookie: %d %s", w.Code, w.Body)
	}
	dw := e.do("GET", "/dashboard", nil, sc)
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(dw.Body.String())
	if m == nil {
		t.Fatalf("no csrf field on /dashboard: %s", dw.Body)
	}
	return sc, m[1]
}

func TestSessionCookieHostPrefixOnHTTPS(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.BaseURL = "https://passway.test" })
	if c := cookieNamed(e.do("GET", "/login", nil), "__Host-passway_oauth_state"); c == nil || !c.Secure {
		t.Errorf("state cookie = %+v, want __Host- and Secure", c)
	}
	e.gh.addUser(42, "alice", 400*24*time.Hour)
	w := e.signIn(t, "alice")
	var sc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if strings.HasSuffix(c.Name, "passway_session") {
			sc = c
		}
	}
	if sc == nil {
		t.Fatalf("no session cookie: %d %v", w.Code, w.Result().Cookies())
	}
	if sc.Name != "__Host-passway_session" || !sc.Secure || sc.Domain != "" || sc.Path != "/" {
		t.Errorf("session cookie = %+v", sc)
	}
	// On https the unprefixed name (which a tunnel subdomain could toss onto
	// the base domain) is never read.
	tossed := &http.Cookie{Name: "passway_session", Value: sc.Value}
	if hw := e.do("GET", "/", nil, tossed); hw.Code != http.StatusOK || !strings.Contains(hw.Body.String(), "Sign in with GitHub") {
		t.Errorf("unprefixed cookie accepted on https: %d %q", hw.Code, hw.Header().Get("Location"))
	}
	if hw := e.do("GET", "/", nil, sc); hw.Code != http.StatusSeeOther || hw.Header().Get("Location") != "/dashboard" {
		t.Errorf("signed-in home = %d %q", hw.Code, hw.Header().Get("Location"))
	}
}

func TestTossedCookieRejected(t *testing.T) {
	e := newTestEnv(t, nil)
	w := e.do("GET", "/", nil, &http.Cookie{Name: "passway_session", Value: "dG9zc2VkLWJ5LWEtbmVpZ2hib3VyLXN1YmRvbWFpbg"})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d %q, want signed-out home", w.Code, w.Header().Get("Location"))
	}
	if !strings.Contains(w.Body.String(), "Sign in with GitHub") {
		t.Errorf("not the signed-out home: %s", w.Body)
	}
	if c := cookieNamed(w, "passway_session"); c == nil || c.MaxAge >= 0 {
		t.Errorf("bogus cookie not cleared: %+v", c)
	}
	if dw := e.do("GET", "/dashboard", nil, &http.Cookie{Name: "passway_session", Value: "bogus"}); dw.Code != http.StatusSeeOther || dw.Header().Get("Location") != "/" {
		t.Errorf("/dashboard with bogus cookie = %d %q", dw.Code, dw.Header().Get("Location"))
	}

	// A neighbour's cookie (even a valid session of the attacker's) sent
	// alongside the real one: neither is trusted.
	sc, _ := e.signedIn(t)
	e.gh.addUser(7, "mallory", 400*24*time.Hour)
	mc := cookieNamed(e.signIn(t, "mallory"), "passway_session")
	if dw := e.do("GET", "/dashboard", nil, mc, sc); dw.Code != http.StatusSeeOther || dw.Header().Get("Location") != "/" {
		t.Errorf("/dashboard with duplicate session cookies = %d %q, want signed out", dw.Code, dw.Header().Get("Location"))
	}
}

func TestPostWithoutCSRFRejected(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)

	for name, form := range map[string]url.Values{
		"missing": {},
		"wrong":   {"csrf": {csrf + "x"}},
	} {
		if w := e.do("POST", "/logout", form, sc); w.Code != http.StatusForbidden {
			t.Errorf("%s csrf: POST /logout = %d, want 403", name, w.Code)
		}
	}
	if w := e.do("GET", "/dashboard", nil, sc); w.Code != http.StatusOK {
		t.Fatalf("session broken after rejected logout: %d", w.Code)
	}
	// No session at all.
	if w := e.do("POST", "/logout", url.Values{"csrf": {csrf}}); w.Code != http.StatusForbidden {
		t.Errorf("POST without session = %d, want 403", w.Code)
	}
}

func TestPostForeignOriginRejected(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	for _, h := range []struct{ name, value string }{
		{"Origin", "http://evil.localhost:8080"},
		{"Origin", "null"},
		{"Referer", "http://evil.localhost:8080/page"},
	} {
		r := httptest.NewRequest("POST", "/logout", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set(h.name, h.value)
		r.AddCookie(&http.Cookie{Name: sc.Name, Value: sc.Value})
		w := httptest.NewRecorder()
		e.d.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %q: code = %d, want 403", h.name, h.value, w.Code)
		}
	}
	if w := e.do("GET", "/dashboard", nil, sc); w.Code != http.StatusOK {
		t.Fatalf("session broken: %d", w.Code)
	}
}

func TestLogout(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	r := httptest.NewRequest("POST", "/logout", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", testBaseURL)
	r.AddCookie(&http.Cookie{Name: sc.Name, Value: sc.Value})
	w := httptest.NewRecorder()
	e.d.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("logout = %d %q", w.Code, w.Header().Get("Location"))
	}
	if c := cookieNamed(w, "passway_session"); c == nil || c.MaxAge >= 0 {
		t.Errorf("cookie not cleared: %+v", c)
	}
	if dw := e.do("GET", "/dashboard", nil, sc); dw.Code != http.StatusSeeOther || dw.Header().Get("Location") != "/" {
		t.Errorf("session survives logout: %d %q", dw.Code, dw.Header().Get("Location"))
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newTestEnv(t, nil)
	want := map[string]string{
		"Cache-Control":           "no-store",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; style-src 'self'; img-src 'self' https://avatars.githubusercontent.com; form-action 'self' https://github.com; frame-ancestors 'none'; base-uri 'none'",
	}
	for _, rq := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/login"}, {"GET", "/dashboard"}, {"GET", "/waitlist"},
		{"GET", "/auth/github/callback"}, {"POST", "/logout"}, {"GET", "/static/app.css"}, {"GET", "/nope"},
	} {
		w := e.do(rq.method, rq.path, nil)
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s %s: %s = %q, want %q", rq.method, rq.path, k, got, v)
			}
		}
	}
	css := e.do("GET", "/static/app.css", nil)
	if css.Code != http.StatusOK || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") || !strings.Contains(css.Body.String(), "--accent") {
		t.Errorf("app.css = %d %q", css.Code, css.Header().Get("Content-Type"))
	}
	home := e.do("GET", "/", nil).Body.String()
	if strings.Contains(home, "<style") || strings.Contains(home, "style=") {
		t.Error("home page has inline styles (blocked by CSP)")
	}
}
