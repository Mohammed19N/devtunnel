package dashboard

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/server/store"
	_ "modernc.org/sqlite"
)

// post sends a form POST with the session cookie and (unless csrf is "") the CSRF field.
func (e *testEnv) post(path string, form url.Values, sc *http.Cookie, csrf string) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	if csrf != "" {
		form.Set("csrf", csrf)
	}
	return e.do("POST", path, form, sc)
}

// signedInAs signs login in through the GitHub flow and returns cookie and CSRF token.
func (e *testEnv) signedInAs(t *testing.T, id int64, login string) (*http.Cookie, string) {
	t.Helper()
	e.gh.addUser(id, login, 400*24*time.Hour)
	w := e.signIn(t, login)
	sc := cookieNamed(w, e.prefix+"passway_session")
	if sc == nil {
		t.Fatalf("%s: no session cookie: %d %s", login, w.Code, w.Body)
	}
	dw := e.do("GET", "/dashboard", nil, sc)
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(dw.Body.String())
	if m == nil {
		// waitlisted users land on /waitlist
		dw = e.do("GET", "/waitlist", nil, sc)
		m = regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(dw.Body.String())
	}
	if m == nil {
		t.Fatalf("%s: no csrf field", login)
	}
	return sc, m[1]
}

func mustUser(t *testing.T, e *testEnv, login string) store.User {
	t.Helper()
	u, err := e.st.UserByLogin(context.Background(), login)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestDashboardShowsTunnelsAndUsage(t *testing.T) {
	var gotID string
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	e := newTestEnv(t, func(c *Config) {
		c.Now = func() time.Time { return now }
		c.Tunnels = func(id string) []TunnelInfo {
			gotID = id
			return []TunnelInfo{{URL: "https://swift-fox-1.passway.test", ClientVersion: "0.3.1", ConnectedAt: now.Add(-5 * time.Minute), Requests: 1234}}
		}
	})
	sc, _ := e.signedIn(t)
	u := mustUser(t, e, "alice")
	if err := e.st.AddUsage(context.Background(), u.ID, "2026-10", 300<<20); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/dashboard", nil, sc)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	if gotID != strconv.FormatInt(u.ID, 10) {
		t.Errorf("Tunnels called with %q", gotID)
	}
	for _, want := range []string{
		"curl -fsSL " + testBaseURL + "/install.sh | sh", "passway login",
		"https://swift-fox-1.passway.test", "0.3.1", "5 minutes ago", "1234",
		"Free", "300 MB of 1 GB", "<progress", "Upgrade to Pro", "disabled", "Coming soon",
		"Reserved subdomains", `href="/dashboard/tokens"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "style=") {
		t.Error("inline style attribute")
	}
	subs, _ := e.st.Subdomains(context.Background(), u.ID)
	if len(subs) != 1 || !strings.Contains(body, subs[0].Name) {
		t.Errorf("default subdomain %v not listed", subs)
	}
}

func TestClaimSubdomainFlow(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	ctx := context.Background()

	w := e.post("/dashboard/subdomains", url.Values{"name": {"acme"}}, sc, csrf)
	if !strings.Contains(w.Body.String(), "your Free plan allows 1 reserved subdomain(s)") {
		t.Fatalf("free claim: %d %s", w.Code, w.Body)
	}
	u := mustUser(t, e, "alice")
	if err := e.st.SetPlan(ctx, u.ID, "pro"); err != nil {
		t.Fatal(err)
	}
	w = e.post("/dashboard/subdomains", url.Values{"name": {"  ACME "}}, sc, csrf)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/dashboard/subdomains" {
		t.Fatalf("pro claim = %d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if !strings.Contains(e.do("GET", "/dashboard/subdomains", nil, sc).Body.String(), "acme") {
		t.Error("acme not listed")
	}

	sc2, csrf2 := e.signedInAs(t, 43, "bob")
	if err := e.st.SetPlan(ctx, mustUser(t, e, "bob").ID, "pro"); err != nil {
		t.Fatal(err)
	}
	w = e.post("/dashboard/subdomains", url.Values{"name": {"acme"}}, sc2, csrf2)
	if !strings.Contains(w.Body.String(), "name is taken") {
		t.Errorf("second user: %d %s", w.Code, w.Body)
	}
}

func TestClaimInvalidName(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	e.st.SetPlan(context.Background(), mustUser(t, e, "alice").ID, "pro")
	for name, want := range map[string]string{
		"-bad":     "name must be 3-32 chars",
		"paypal-x": "blocked word",
	} {
		w := e.post("/dashboard/subdomains", url.Values{"name": {name}}, sc, csrf)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%q: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestReleaseSubdomain(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	ctx := context.Background()
	u := mustUser(t, e, "alice")
	e.st.SetPlan(ctx, u.ID, "pro")
	e.post("/dashboard/subdomains", url.Values{"name": {"acme"}}, sc, csrf)

	w := e.post("/dashboard/subdomains/release", url.Values{"name": {"acme"}}, sc, csrf)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("release = %d %s", w.Code, w.Body)
	}
	if _, err := e.st.SubdomainOwner(ctx, "acme"); err != store.ErrNotFound {
		t.Errorf("acme still owned: %v", err)
	}
	subs, _ := e.st.Subdomains(ctx, u.ID)
	if w := e.post("/dashboard/subdomains/release", url.Values{"name": {subs[0].Name}}, sc, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("release default = %d, want 400", w.Code)
	}
	if w := e.post("/dashboard/subdomains/release", url.Values{"name": {"nobody-owns"}}, sc, csrf); w.Code != http.StatusNotFound {
		t.Errorf("release unowned = %d, want 404", w.Code)
	}
}

func TestCreateTokenShownOnce(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	w := e.post("/dashboard/tokens", url.Values{"name": {"laptop"}}, sc, csrf)
	if w.Code != 200 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	m := regexp.MustCompile(`<code>(pw_[A-Za-z0-9_-]+)</code>`).FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("no token in %s", w.Body)
	}
	tok := m[1]
	if !strings.Contains(w.Body.String(), "Copy it now") || !strings.Contains(w.Body.String(), "passway login "+tok) {
		t.Errorf("missing copy or login line: %s", w.Body)
	}
	lw := e.do("GET", "/dashboard/tokens", nil, sc)
	if strings.Contains(lw.Body.String(), tok) {
		t.Error("plaintext token shown again")
	}
	if !strings.Contains(lw.Body.String(), tok[:10]+"…") || !strings.Contains(lw.Body.String(), "laptop") {
		t.Errorf("list missing prefix/name: %s", lw.Body)
	}
	for _, bad := range []string{"", strings.Repeat("x", 41)} {
		if w := e.post("/dashboard/tokens", url.Values{"name": {bad}}, sc, csrf); w.Code != http.StatusBadRequest {
			t.Errorf("name %q = %d, want 400", bad, w.Code)
		}
	}
}

func TestRevokeToken(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	e.post("/dashboard/tokens", url.Values{"name": {"laptop"}}, sc, csrf)
	toks, _ := e.st.ListTokens(context.Background(), mustUser(t, e, "alice").ID)
	w := e.post("/dashboard/tokens/revoke", url.Values{"id": {strconv.FormatInt(toks[0].ID, 10)}}, sc, csrf)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/dashboard/tokens" {
		t.Fatalf("revoke = %d %q", w.Code, w.Header().Get("Location"))
	}
	toks, _ = e.st.ListTokens(context.Background(), mustUser(t, e, "alice").ID)
	if toks[0].RevokedAt == nil {
		t.Error("not revoked")
	}
	if w := e.post("/dashboard/tokens/revoke", url.Values{"id": {"abc"}}, sc, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d", w.Code)
	}
}

func TestRevokeOtherUsersTokenFails(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	e.post("/dashboard/tokens", url.Values{"name": {"laptop"}}, sc, csrf)
	toks, _ := e.st.ListTokens(context.Background(), mustUser(t, e, "alice").ID)
	sc2, csrf2 := e.signedInAs(t, 43, "bob")
	w := e.post("/dashboard/tokens/revoke", url.Values{"id": {strconv.FormatInt(toks[0].ID, 10)}}, sc2, csrf2)
	if w.Code != http.StatusNotFound {
		t.Errorf("revoke other's = %d, want 404", w.Code)
	}
	toks, _ = e.st.ListTokens(context.Background(), mustUser(t, e, "alice").ID)
	if toks[0].RevokedAt != nil {
		t.Error("alice's token revoked by bob")
	}
}

func TestWaitlistedCannotCreateToken(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.FreeSignupCap = 1 })
	e.signedIn(t) // alice takes the only slot
	sc, csrf := e.signedInAs(t, 43, "bob")
	if !mustUser(t, e, "bob").Waitlisted {
		t.Fatal("bob not waitlisted")
	}
	if w := e.post("/dashboard/tokens", url.Values{"name": {"x"}}, sc, csrf); w.Code != http.StatusForbidden {
		t.Errorf("create token = %d, want 403", w.Code)
	}
	if w := e.post("/dashboard/subdomains", url.Values{"name": {"bobsite"}}, sc, csrf); w.Code != http.StatusForbidden {
		t.Errorf("claim = %d, want 403", w.Code)
	}
	if w := e.do("GET", "/dashboard/tokens", nil, sc); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/waitlist" {
		t.Errorf("GET tokens = %d %q", w.Code, w.Header().Get("Location"))
	}
	if toks, _ := e.st.ListTokens(context.Background(), mustUser(t, e, "bob").ID); len(toks) != 0 {
		t.Errorf("tokens created: %v", toks)
	}
}

func TestAllPostsRequireCSRF(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	for _, p := range []string{"/logout", "/dashboard/subdomains", "/dashboard/subdomains/release", "/dashboard/tokens", "/dashboard/tokens/revoke"} {
		if w := e.post(p, url.Values{"name": {"acme"}, "id": {"1"}}, sc, ""); w.Code != http.StatusForbidden {
			t.Errorf("POST %s without csrf = %d, want 403", p, w.Code)
		}
		if w := e.post(p, url.Values{"name": {"acme"}, "id": {"1"}}, sc, csrf+"x"); w.Code != http.StatusForbidden {
			t.Errorf("POST %s with wrong csrf = %d, want 403", p, w.Code)
		}
	}
}

func TestUserTextEscaped(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	e.post("/dashboard/tokens", url.Values{"name": {"<script>x</script>"}}, sc, csrf)
	w := e.post("/dashboard/tokens", url.Values{"name": {"<script>y</script>"}}, sc, csrf)
	lw := e.do("GET", "/dashboard/tokens", nil, sc)
	for _, b := range []string{w.Body.String(), lw.Body.String()} {
		if strings.Contains(b, "<script>") {
			t.Errorf("unescaped script: %s", b)
		}
	}
	if !strings.Contains(lw.Body.String(), "&lt;script&gt;x&lt;/script&gt;") {
		t.Error("escaped name not shown")
	}
}

func TestPagesRequireSession(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, p := range []string{"/dashboard", "/dashboard/subdomains", "/dashboard/tokens"} {
		if w := e.do("GET", p, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
			t.Errorf("GET %s = %d %q", p, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestMutationsAudited(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, csrf := e.signedIn(t)
	e.st.SetPlan(context.Background(), mustUser(t, e, "alice").ID, "pro")
	e.post("/dashboard/subdomains", url.Values{"name": {"acme"}}, sc, csrf)
	e.post("/dashboard/subdomains/release", url.Values{"name": {"acme"}}, sc, csrf)
	e.post("/dashboard/tokens", url.Values{"name": {"laptop"}}, sc, csrf)
	toks, _ := e.st.ListTokens(context.Background(), mustUser(t, e, "alice").ID)
	e.post("/dashboard/tokens/revoke", url.Values{"id": {strconv.FormatInt(toks[0].ID, 10)}}, sc, csrf)
	var actions []string
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT action FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		rows.Scan(&a)
		actions = append(actions, a)
	}
	got := strings.Join(actions, ",")
	for _, want := range []string{"subdomain.claim", "subdomain.release", "token.create", "token.revoke"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit missing %q in %s", want, got)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 MB", 300 << 20: "300 MB", 1 << 30: "1 GB", 3 << 29: "1.5 GB", 50 << 30: "50 GB", (1 << 30) - 1: "1024 MB",
	} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		5 * time.Second: "just now", time.Minute: "1 minute ago", 5 * time.Minute: "5 minutes ago",
		2 * time.Hour: "2 hours ago", 25 * time.Hour: "1 day ago", 72 * time.Hour: "3 days ago",
	} {
		if got := relativeTime(now, now.Add(-d)); got != want {
			t.Errorf("relativeTime(-%v) = %q, want %q", d, got, want)
		}
	}
}

// --- Task 6 review hardening ---

func TestBannedUserCannotSignIn(t *testing.T) {
	e := newTestEnv(t, nil)
	e.gh.addUser(42, "alice", 400*24*time.Hour)
	if err := e.st.AddBan(context.Background(), store.Ban{Kind: "user", Value: "Alice", Reason: "abuse"}); err != nil {
		t.Fatal(err)
	}
	w := e.signIn(t, "alice")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "account suspended") {
		t.Fatalf("banned sign-in = %d %s", w.Code, w.Body)
	}
	assertNoSession(t, w)
}

func TestBannedUserSessionSignedOut(t *testing.T) {
	e := newTestEnv(t, nil)
	sc, _ := e.signedIn(t)
	if err := e.st.AddBan(context.Background(), store.Ban{Kind: "user", Value: "alice", Reason: "abuse"}); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/dashboard", nil, sc)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Errorf("banned session /dashboard = %d %q", w.Code, w.Header().Get("Location"))
	}
	if c := cookieNamed(w, sc.Name); c == nil || c.MaxAge >= 0 {
		t.Errorf("cookie not cleared: %+v", c)
	}
}

func TestNewRejectsPlainHTTPOffLocalhost(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/p.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := Config{Store: st, GitHubClientID: "a", GitHubClientSecret: "b"}
	for _, base := range []string{"http://passway.run", "http://localhost.evil.com", "http://10.0.0.1:8080"} {
		cfg.BaseURL = base
		if _, err := New(cfg); err == nil {
			t.Errorf("New accepted %q", base)
		}
	}
	for _, base := range []string{"https://passway.run", "http://localhost:8080", "http://x.localhost"} {
		cfg.BaseURL = base
		if _, err := New(cfg); err != nil {
			t.Errorf("New rejected %q: %v", base, err)
		}
	}
}

func TestCallbackAccessDenied(t *testing.T) {
	e := newTestEnv(t, nil)
	stateCookie, state := e.login(t)
	w := e.do("GET", "/auth/github/callback?error=access_denied&state="+url.QueryEscape(state), nil, stateCookie)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cancelled") {
		t.Fatalf("access_denied = %d %s", w.Code, w.Body)
	}
	assertNoSession(t, w)
	if c := cookieNamed(w, stateCookie.Name); c == nil || c.MaxAge >= 0 {
		t.Errorf("state cookie not cleared: %+v", c)
	}
}

func TestStateCookieClearedOnFailure(t *testing.T) {
	e := newTestEnv(t, nil)
	stateCookie, _ := e.login(t)
	w := e.do("GET", "/auth/github/callback?code=c-alice&state=forged", nil, stateCookie)
	if c := cookieNamed(w, stateCookie.Name); c == nil || c.MaxAge >= 0 {
		t.Errorf("mismatch: state cookie not cleared: %+v", c)
	}
	e.gh.addUser(42, "alice", 400*24*time.Hour)
	e.gh.tokenError = "bad_verification_code"
	stateCookie, state := e.login(t)
	w = e.do("GET", "/auth/github/callback?code=c-alice&state="+url.QueryEscape(state), nil, stateCookie)
	if c := cookieNamed(w, stateCookie.Name); c == nil || c.MaxAge >= 0 {
		t.Errorf("github error: state cookie not cleared: %+v", c)
	}
}
