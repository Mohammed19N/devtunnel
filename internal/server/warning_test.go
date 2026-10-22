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

func TestNeedsWarning(t *testing.T) {
	mk := func(method, path string, h map[string]string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	html := map[string]string{"Accept": "text/html,application/xhtml+xml"}
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
		{"cookie", mk("GET", "/", map[string]string{"Accept": "text/html", "Cookie": "passway_warned=1"}), false},
		{"continue path", mk("GET", "/_passway/continue", html), false},
	}
	for _, tc := range tests {
		if got := needsWarning(tc.r); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestSafeReturnPath(t *testing.T) {
	for in, want := range map[string]string{
		"/docs?a=1": "/docs?a=1", "": "/", "//evil.com": "/", `/\evil.com`: "/",
		"https://evil.com": "/", "javascript:alert(1)": "/", "evil.com": "/",
	} {
		if got := safeReturnPath(in); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleContinue(t *testing.T) {
	w := httptest.NewRecorder()
	handleContinue(w, httptest.NewRequest("GET", "/_passway/continue?to=%2Fdocs", nil))
	if w.Code != 303 || w.Header().Get("Location") != "/docs" {
		t.Fatalf("code=%d loc=%q", w.Code, w.Header().Get("Location"))
	}
	c := w.Header().Get("Set-Cookie")
	for _, want := range []string{"passway_warned=1", "HttpOnly", "SameSite=Lax", "Max-Age=604800", "Path=/"} {
		if !strings.Contains(c, want) {
			t.Errorf("cookie %q missing %q", c, want)
		}
	}
	if strings.Contains(c, "Domain") {
		t.Errorf("cookie has Domain: %q", c)
	}
}

func TestEdgeWarningFlow(t *testing.T) {
	_, ts := newTestServerCfg(t, nil, func(c *Config) { c.BrowserWarning = true })
	resp, sess := dialRaw(t, ts, hello("warn", "A"))
	if resp.Ready == nil {
		t.Fatalf("ready: %+v", resp.Error)
	}
	var hits atomic.Int32
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		io.WriteString(w, "app")
	})}
	go hs.Serve(streamListener{sess})
	t.Cleanup(func() { hs.Close() })

	u, _ := url.Parse(ts.URL)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path string, h map[string]string) (int, string) {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		req.Host = "warn.localhost:" + u.Port()
		for k, v := range h {
			req.Header.Set(k, v)
		}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	html := map[string]string{"Accept": "text/html"}
	code, body := do("GET", "/x?y=1", html)
	if code != 200 || body == "app" || !strings.Contains(body, "continue?to=") || hits.Load() != 0 {
		t.Fatalf("warning: code=%d hits=%d body=%q", code, hits.Load(), body)
	}
	code, body = do("GET", "/x?y=1", map[string]string{"Accept": "text/html", "Cookie": "passway_warned=1"})
	if code != 200 || body != "app" || hits.Load() != 1 {
		t.Fatalf("cookie: code=%d hits=%d body=%q", code, hits.Load(), body)
	}
	code, _ = do("POST", "/hook", map[string]string{"Content-Type": "application/json"})
	if code != 200 || hits.Load() != 2 {
		t.Fatalf("webhook: code=%d hits=%d", code, hits.Load())
	}
	code, _ = do("GET", "/_passway/continue?to=%2Fx", html)
	if code != 303 {
		t.Fatalf("continue: %d", code)
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
