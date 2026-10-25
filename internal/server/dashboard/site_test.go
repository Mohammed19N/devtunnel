package dashboard

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mohammed19n/passway"
	"github.com/mohammed19n/passway/internal/server"
)

func TestHomeSignedInNoRedirect(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, _ := e.signedIn(t)
	w := e.do("GET", "/", nil, sc)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d %q, want 200", w.Code, w.Header().Get("Location"))
	}
	body := w.Body.String()
	if !strings.Contains(body, "Open dashboard") || strings.Contains(body, "Sign in with GitHub") {
		t.Errorf("signed-in home should offer Open dashboard, not Sign in")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control = %q, want private, no-store", cc)
	}
	if v := w.Header().Values("Vary"); !containsToken(v, "Cookie") {
		t.Errorf("Vary = %q, want Cookie", v)
	}
	// Every public page is private for a signed-in visitor.
	for _, p := range publicPages {
		if cc := e.do("GET", p, nil, sc).Header().Get("Cache-Control"); cc != "private, no-store" {
			t.Errorf("%s signed in: Cache-Control = %q", p, cc)
		}
	}
}

func TestHomeAnonymousCacheable(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, p := range publicPages {
		w := e.do("GET", p, nil)
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
			t.Errorf("%s: Cache-Control = %q, want public, max-age=300", p, cc)
		}
		if v := w.Header().Values("Vary"); !containsToken(v, "Cookie") {
			t.Errorf("%s: Vary = %q, want Cookie", p, v)
		}
		// Lighthouse fetches robots.txt from inside the page; no script can run (script-src falls back to 'none').
		if csp := w.Header().Get("Content-Security-Policy"); csp != siteCSP || !strings.Contains(csp, "connect-src 'self'") || strings.Contains(csp, "script-src") {
			t.Errorf("%s: CSP = %q", p, csp)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("%s: a cacheable response sets cookies", p)
		}
	}
	body := e.do("GET", "/", nil).Body.String()
	for _, want := range []string{
		"Public URLs for localhost, with a webhook inspector built in",
		"curl -fsSL " + testBaseURL + "/install.sh | sh",
		"brew install mohammed19n/tap/passway",
		`src="/static/inspector.png?v=`,
		"Sign in with GitHub",
		"AGPL-3.0", "MIT",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home missing %q", want)
		}
	}
}

func containsToken(vals []string, tok string) bool {
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), tok) {
				return true
			}
		}
	}
	return false
}

func TestPricingShowsPlanLimits(t *testing.T) {
	cell := func(s string) string { return "<td>" + s + "</td>" }
	limits := func(p server.Plan) []string {
		return []string{cell(strconv.Itoa(p.MaxTunnels)), cell(strconv.Itoa(p.MaxSubdomains)),
			cell(formatBytes(p.Bandwidth)), cell(strconv.Itoa(p.RateLimit))}
	}
	e := newTestEnv(t, nil)
	body := e.do("GET", "/pricing", nil).Body.String()
	for _, want := range append(limits(server.PlanFree), limits(server.PlanPro)...) {
		if !strings.Contains(body, want) {
			t.Errorf("pricing missing %s", want)
		}
	}
	if !strings.Contains(body, cell(strconv.Itoa(server.PlanPro.MaxCustomDomains))) {
		t.Errorf("pricing missing Pro custom domains %d", server.PlanPro.MaxCustomDomains)
	}
	if !strings.Contains(body, "Coming soon") {
		t.Error("billing off: Pro should be Coming soon")
	}

	be, _ := billingEnv(t, nil)
	body = be.do("GET", "/pricing", nil).Body.String()
	if !strings.Contains(body, "$7 / month") || strings.Contains(body, "Coming soon") {
		t.Error("billing on: Pro should show the Stripe price")
	}
	if home := be.do("GET", "/", nil).Body.String(); !strings.Contains(home, "$7 / month") {
		t.Error("home Free vs Pro summary should show the Stripe price")
	}
}

func TestLegalPagesUseOperator(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, p := range []string{"/terms", "/privacy", "/aup"} {
		body := e.do("GET", p, nil).Body.String()
		if !strings.Contains(body, "the operator of this service") {
			t.Errorf("%s: no default operator text", p)
		}
	}
	e = newTestEnv(t, func(c *Config) {
		c.LegalOperator = "Example Ltd"
		c.LegalJurisdiction = "England and Wales"
		c.AbuseEmail = "reports@example.com"
	})
	for _, p := range []string{"/terms", "/privacy", "/aup"} {
		body := e.do("GET", p, nil).Body.String()
		if !strings.Contains(body, "Example Ltd") || strings.Contains(body, "the operator of this service") {
			t.Errorf("%s: operator not substituted", p)
		}
	}
	if body := e.do("GET", "/terms", nil).Body.String(); !strings.Contains(body, "England and Wales") {
		t.Error("terms: jurisdiction not substituted")
	}
	if body := e.do("GET", "/abuse", nil).Body.String(); !strings.Contains(body, "reports@example.com") {
		t.Error("abuse page: AbuseEmail not shown")
	}
	if body := newTestEnv(t, nil).do("GET", "/abuse", nil).Body.String(); !strings.Contains(body, "abuse@localhost") {
		t.Error("abuse page: default AbuseEmail should be abuse@<base host>")
	}
}

func TestInstallScriptServed(t *testing.T) {
	e := newTestEnv(t, nil)
	w := e.do("GET", "/install.sh", nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), passway.InstallScript) {
		t.Fatalf("install.sh = %d, %d bytes", w.Code, w.Body.Len())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func gzipGet(e *testEnv, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Accept-Encoding", "br, gzip, deflate")
	w := httptest.NewRecorder()
	e.d.ServeHTTP(w, r)
	return w
}

func TestGzip(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, p := range []string{"/", "/pricing", "/sitemap.xml", "/robots.txt", cssPath(t, e)} {
		plain := e.do("GET", p, nil)
		if plain.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s: encoded without Accept-Encoding", p)
		}
		w := gzipGet(e, p)
		if w.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("%s: Content-Encoding = %q, want gzip", p, w.Header().Get("Content-Encoding"))
			continue
		}
		if !containsToken(w.Header().Values("Vary"), "Accept-Encoding") {
			t.Errorf("%s: Vary = %q, want Accept-Encoding", p, w.Header().Values("Vary"))
		}
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		got, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(got, plain.Body.Bytes()) {
			t.Errorf("%s: gunzipped body differs (%v)", p, err)
		}
	}
	if w := gzipGet(e, "/og.png"); w.Header().Get("Content-Encoding") != "" {
		t.Error("og.png should not be gzipped")
	}
}

// cssPath returns the versioned stylesheet URL the pages link.
func cssPath(t *testing.T, e *testEnv) string {
	t.Helper()
	m := regexp.MustCompile(`href="(/static/app\.css\?v=[0-9a-f]+)"`).FindStringSubmatch(e.do("GET", "/", nil).Body.String())
	if m == nil {
		t.Fatal("no versioned stylesheet link")
	}
	return m[1]
}

func TestStaticImmutable(t *testing.T) {
	e := newTestEnv(t, nil)
	sum := sha256.Sum256(appCSS)
	want := "/static/app.css?v=" + hex.EncodeToString(sum[:])[:8]
	if got := cssPath(t, e); got != want {
		t.Fatalf("stylesheet = %q, want %q", got, want)
	}
	sc, _ := e.signedIn(t)
	if !strings.Contains(e.do("GET", "/dashboard", nil, sc).Body.String(), `href="`+want+`"`) {
		t.Error("dashboard pages should link the versioned stylesheet too")
	}
	w := e.do("GET", want, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), appCSS) {
		t.Fatalf("css = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if cc := e.do("GET", "/static/app.css?v=00000000", nil).Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("stale version cached as immutable: %q", cc)
	}

	m := regexp.MustCompile(`src="(/static/inspector\.png\?v=[0-9a-f]{8})"`).FindStringSubmatch(e.do("GET", "/", nil).Body.String())
	if m == nil {
		t.Fatal("no versioned inspector screenshot")
	}
	w = e.do("GET", m[1], nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" ||
		w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("inspector.png = %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
	}
	for path, ct := range map[string]string{"/og.png": "image/png", "/favicon.svg": "image/svg+xml"} {
		w := e.do("GET", path, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != ct || w.Body.Len() == 0 {
			t.Errorf("%s = %d %q", path, w.Code, w.Header().Get("Content-Type"))
		}
		if cc := w.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=") {
			t.Errorf("%s: Cache-Control = %q", path, cc)
		}
	}
}
