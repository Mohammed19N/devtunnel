package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func pageReq(accept string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	return r
}

func TestRenderPageHTML(t *testing.T) {
	tests := []struct {
		name   string
		kind   pageKind
		status int
	}{
		{"offline", pageOffline, 502},
		{"ratelimited", pageRateLimited, 429},
		{"warning", pageWarning, 200},
		{"bandwidth", pageBandwidth, 402},
		{"banned", pageBanned, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			renderPage(w, pageReq("text/html,application/xhtml+xml"), tc.kind,
				pageData{Host: "<script>.x.dev", Continue: "/_passway/continue?to=%2F", RetryIn: 7})
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("content-type = %q", ct)
			}
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("cache-control = %q", cc)
			}
			body := w.Body.String()
			if !strings.Contains(body, "&lt;script&gt;") || strings.Contains(body, "<script>") {
				t.Fatalf("host not escaped: %s", body)
			}
			if !strings.Contains(body, "Passway") {
				t.Fatal("missing Passway branding")
			}
			if tc.kind == pageRateLimited && w.Header().Get("Retry-After") != "7" {
				t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
			}
		})
	}
}

func TestRenderPagePlain(t *testing.T) {
	tests := []struct {
		kind   pageKind
		status int
		body   string
	}{
		{pageOffline, 502, "tunnel a.x.dev is offline\n"},
		{pageRateLimited, 429, "rate limit exceeded; retry in 7s\n"},
		{pageWarning, 200, "this tunnel is served from someone's computer; send the Passway-Skip-Warning header to skip this notice\n"},
	}
	for _, tc := range tests {
		w := httptest.NewRecorder()
		renderPage(w, pageReq("application/json"), tc.kind, pageData{Host: "a.x.dev", RetryIn: 7})
		if w.Code != tc.status {
			t.Fatalf("kind %d: status = %d", tc.kind, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("content-type = %q", ct)
		}
		if w.Body.String() != tc.body {
			t.Fatalf("kind %d: body = %q, want %q", tc.kind, w.Body.String(), tc.body)
		}
	}
}

func TestRenderPageWarningLink(t *testing.T) {
	w := httptest.NewRecorder()
	cont := "/_passway/continue?to=" + url.QueryEscape("/docs?a=1")
	renderPage(w, pageReq("text/html"), pageWarning, pageData{Host: "a.x.dev", Continue: cont})
	if want := `href="/_passway/continue?to=%2Fdocs%3Fa%3D1"`; !strings.Contains(w.Body.String(), want) {
		t.Fatalf("missing %s in %s", want, w.Body.String())
	}
}

func TestRenderPageHardeningHeaders(t *testing.T) {
	for _, kind := range []pageKind{pageOffline, pageRateLimited, pageWarning} {
		for _, accept := range []string{"text/html", "application/json"} {
			w := httptest.NewRecorder()
			renderPage(w, pageReq(accept), kind, pageData{Host: "a.x.dev", Continue: "/_passway/continue?to=%2F", RetryIn: 1})
			h := w.Header()
			if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" ||
				!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
				t.Errorf("kind %d accept %q: headers = %v", kind, accept, h)
			}
		}
	}
}

func TestRenderPageOwnerAndPlain(t *testing.T) {
	w := httptest.NewRecorder()
	renderPage(w, pageReq("text/html"), pageWarning, pageData{Host: "a.x.dev", Continue: "/c", Owner: "u1login"})
	if !strings.Contains(w.Body.String(), "<p>This tunnel is run by @u1login.</p>") ||
		strings.Count(w.Body.String(), "You are about to visit") != 1 {
		t.Fatalf("owner line wrong: %s", w.Body)
	}
	w = httptest.NewRecorder()
	renderPage(w, pageReq("text/html"), pageWarning, pageData{Host: "a.x.dev", Continue: "/c"})
	if strings.Contains(w.Body.String(), "run by @") {
		t.Fatal("owner line shown without owner")
	}
	for kind, want := range map[pageKind]string{pageBandwidth: "monthly bandwidth limit reached", pageBanned: "this tunnel has been disabled"} {
		w = httptest.NewRecorder()
		renderPage(w, pageReq("text/plain"), kind, pageData{Host: "a.x.dev"})
		if strings.TrimSpace(w.Body.String()) != want {
			t.Fatalf("plain = %q, want %q", w.Body, want)
		}
	}
}
