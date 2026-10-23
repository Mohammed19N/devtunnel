package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/server/store"
)

const testBaseURL = "http://localhost:8080"

// ghUser is what the fake GitHub returns from /user.
type ghUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	CreatedAt string `json:"created_at"`
}

// fakeGitHub serves /login/oauth/access_token and /user. The code "c-<login>"
// exchanges for the token "t-<login>", which /user resolves to users[login].
type fakeGitHub struct {
	srv *httptest.Server

	mu         sync.Mutex
	users      map[string]ghUser
	tokenError string // non-empty: the token endpoint returns {"error": tokenError}
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{users: map[string]ghUser{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "want Accept: application/json", http.StatusNotAcceptable)
			return
		}
		if r.FormValue("client_id") != "cid" || r.FormValue("client_secret") != "csecret" ||
			!strings.HasSuffix(r.FormValue("redirect_uri"), "/auth/github/callback") {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		if f.tokenError != "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": f.tokenError, "error_description": "<b>nope</b>"})
			return
		}
		login, ok := strings.CutPrefix(r.FormValue("code"), "c-")
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "t-" + login, "token_type": "bearer"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		login, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer t-")
		u, found := f.users[login]
		if !ok || !found {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(u)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// addUser registers a GitHub account aged age.
func (f *fakeGitHub) addUser(id int64, login string, age time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[login] = ghUser{ID: id, Login: login, Name: strings.ToUpper(login), AvatarURL: "https://avatars.githubusercontent.com/u/1",
		CreatedAt: time.Now().Add(-age).UTC().Format(time.RFC3339)}
}

type testEnv struct {
	d      *Dashboard
	st     *store.Store
	gh     *fakeGitHub
	prefix string // "__Host-" on https
}

func newTestEnv(t *testing.T, mod func(*Config)) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gh := newFakeGitHub(t)
	cfg := Config{
		Store:              st,
		BaseURL:            testBaseURL,
		GitHubClientID:     "cid",
		GitHubClientSecret: "csecret",
		GitHubAuthURL:      gh.srv.URL + "/login/oauth/authorize",
		GitHubTokenURL:     gh.srv.URL + "/login/oauth/access_token",
		GitHubAPIURL:       gh.srv.URL,
		MinGitHubAge:       7 * 24 * time.Hour,
		Logger:             slog.New(slog.DiscardHandler),
	}
	if mod != nil {
		mod(&cfg)
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{d: d, st: st, gh: gh}
	if strings.HasPrefix(cfg.BaseURL, "https:") {
		e.prefix = "__Host-"
	}
	return e
}

// do serves one request against the dashboard with the given cookies.
func (e *testEnv) do(method, target string, body url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Host = "localhost:8080"
	for _, c := range cookies {
		if c != nil {
			r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
	}
	w := httptest.NewRecorder()
	e.d.ServeHTTP(w, r)
	return w
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login runs GET /login and returns the state cookie and the state from the redirect.
func (e *testEnv) login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	w := e.do("GET", "/login", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("GET /login = %d, want 302", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	c := cookieNamed(w, e.prefix+"passway_oauth_state")
	if c == nil {
		t.Fatalf("no state cookie in %v", w.Result().Cookies())
	}
	return c, loc.Query().Get("state")
}

// signIn completes the GitHub flow for login and returns the callback response.
func (e *testEnv) signIn(t *testing.T, login string) *httptest.ResponseRecorder {
	t.Helper()
	stateCookie, state := e.login(t)
	return e.do("GET", "/auth/github/callback?code=c-"+login+"&state="+url.QueryEscape(state), nil, stateCookie)
}

func TestLoginRedirect(t *testing.T) {
	e := newTestEnv(t, nil)
	w := e.do("GET", "/login", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != e.gh.srv.URL+"/login/oauth/authorize" {
		t.Errorf("redirect to %q", got)
	}
	q := loc.Query()
	if q.Get("client_id") != "cid" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != testBaseURL+"/auth/github/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("allow_signup") != "true" {
		t.Errorf("allow_signup = %q", q.Get("allow_signup"))
	}
	if q.Has("scope") {
		t.Errorf("scope requested: %q", q.Get("scope"))
	}
	c := cookieNamed(w, "passway_oauth_state")
	if c == nil {
		t.Fatal("no passway_oauth_state cookie")
	}
	if q.Get("state") == "" || q.Get("state") != c.Value {
		t.Errorf("state = %q, cookie = %q", q.Get("state"), c.Value)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != 600 || c.Path != "/" || c.Domain != "" {
		t.Errorf("state cookie attributes: %+v", c)
	}
}

var randomNameRE = regexp.MustCompile(`^[a-z]+-[a-z]+-[1-9][0-9]$`)

func TestCallbackCreatesUserAndSession(t *testing.T) {
	e := newTestEnv(t, nil)
	e.gh.addUser(42, "alice", 400*24*time.Hour)
	w := e.signIn(t, "alice")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/dashboard" {
		t.Fatalf("callback = %d %q, want 303 /dashboard; body %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	ctx := context.Background()
	u, err := e.st.UserByLogin(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.GitHubID != 42 || u.Name != "ALICE" || u.Waitlisted {
		t.Errorf("user = %+v", u)
	}
	subs, err := e.st.Subdomains(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || !subs[0].IsDefault || !randomNameRE.MatchString(subs[0].Name) {
		t.Errorf("subdomains = %+v", subs)
	}
	sc := cookieNamed(w, "passway_session")
	if sc == nil {
		t.Fatalf("no session cookie in %v", w.Result().Cookies())
	}
	if raw := w.Header().Values("Set-Cookie"); strings.Contains(strings.ToLower(strings.Join(raw, "\n")), "domain=") {
		t.Errorf("Set-Cookie has a Domain attribute: %q", raw)
	}
	if sc.Path != "/" || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.MaxAge != 2592000 || sc.Secure {
		t.Errorf("session cookie attributes: %+v", sc)
	}
	if c := cookieNamed(w, "passway_oauth_state"); c == nil || c.MaxAge >= 0 {
		t.Errorf("state cookie not cleared: %+v", c)
	}
	dw := e.do("GET", "/dashboard", nil, sc)
	if dw.Code != http.StatusOK {
		t.Fatalf("GET /dashboard = %d %q", dw.Code, dw.Header().Get("Location"))
	}
	if !strings.Contains(dw.Body.String(), "alice") {
		t.Errorf("dashboard doesn't mention the user: %s", dw.Body)
	}

	// Signing in again reuses the account and doesn't claim another subdomain.
	if w := e.signIn(t, "alice"); w.Code != http.StatusSeeOther {
		t.Fatalf("second sign-in = %d", w.Code)
	}
	if subs, _ := e.st.Subdomains(ctx, u.ID); len(subs) != 1 {
		t.Errorf("subdomains after re-login = %+v", subs)
	}
}

func assertNoSession(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if c := cookieNamed(w, "passway_session"); c != nil && c.MaxAge >= 0 {
		t.Errorf("session cookie set: %+v", c)
	}
}

func TestCallbackStateMismatch(t *testing.T) {
	e := newTestEnv(t, nil)
	e.gh.addUser(42, "alice", 400*24*time.Hour)
	stateCookie, _ := e.login(t)
	w := e.do("GET", "/auth/github/callback?code=c-alice&state=forged", nil, stateCookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	assertNoSession(t, w)
	if _, err := e.st.UserByLogin(context.Background(), "alice"); err != store.ErrNotFound {
		t.Errorf("user created: err = %v", err)
	}
}

func TestCallbackMissingStateCookie(t *testing.T) {
	e := newTestEnv(t, nil)
	e.gh.addUser(42, "alice", 400*24*time.Hour)
	_, state := e.login(t)
	w := e.do("GET", "/auth/github/callback?code=c-alice&state="+url.QueryEscape(state), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	assertNoSession(t, w)
	// An empty state can't match an empty (missing) cookie either.
	w = e.do("GET", "/auth/github/callback?code=c-alice&state=", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty state: code = %d, want 400", w.Code)
	}
}

func TestCallbackGitHubError(t *testing.T) {
	e := newTestEnv(t, nil)
	e.gh.addUser(42, "alice", 400*24*time.Hour)
	e.gh.tokenError = "bad_verification_code"
	w := e.signIn(t, "alice")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	assertNoSession(t, w)
	if strings.Contains(w.Body.String(), "<b>nope</b>") {
		t.Errorf("upstream text echoed unescaped: %s", w.Body)
	}
	if _, err := e.st.UserByLogin(context.Background(), "alice"); err != store.ErrNotFound {
		t.Errorf("user created: err = %v", err)
	}
}

func TestCallbackYoungAccount(t *testing.T) {
	e := newTestEnv(t, nil)
	e.gh.addUser(42, "alice", 2*24*time.Hour)
	w := e.signIn(t, "alice")
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "GitHub accounts must be at least 7 days old to sign up") {
		t.Errorf("body lacks the copy: %s", w.Body)
	}
	assertNoSession(t, w)
	if _, err := e.st.UserByLogin(context.Background(), "alice"); err != store.ErrNotFound {
		t.Errorf("user created: err = %v", err)
	}
}

func TestSignupCapWaitlists(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.FreeSignupCap = 1 })
	e.gh.addUser(1, "alice", 400*24*time.Hour)
	e.gh.addUser(2, "bob", 400*24*time.Hour)
	if w := e.signIn(t, "alice"); w.Header().Get("Location") != "/dashboard" {
		t.Fatalf("alice -> %d %q", w.Code, w.Header().Get("Location"))
	}
	w := e.signIn(t, "bob")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/waitlist" {
		t.Fatalf("bob -> %d %q, want 303 /waitlist", w.Code, w.Header().Get("Location"))
	}
	ctx := context.Background()
	bob, err := e.st.UserByLogin(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !bob.Waitlisted {
		t.Error("bob not waitlisted")
	}
	if subs, _ := e.st.Subdomains(ctx, bob.ID); len(subs) != 0 {
		t.Errorf("waitlisted user got subdomains: %+v", subs)
	}
	sc := cookieNamed(w, "passway_session")
	if sc == nil {
		t.Fatal("waitlisted user has no session")
	}
	if dw := e.do("GET", "/dashboard", nil, sc); dw.Code != http.StatusSeeOther || dw.Header().Get("Location") != "/waitlist" {
		t.Errorf("/dashboard for waitlisted = %d %q", dw.Code, dw.Header().Get("Location"))
	}
	if ww := e.do("GET", "/waitlist", nil, sc); ww.Code != http.StatusOK {
		t.Errorf("/waitlist = %d", ww.Code)
	}
}

func TestDevLoginRefusedOffLocalhost(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, base := range []string{"https://passway.run", "http://localhost.evil.com", "http://127.0.0.1:8080"} {
		if _, err := New(Config{Store: st, BaseURL: base, DevLogin: true}); err == nil {
			t.Errorf("New with DevLogin and BaseURL %q succeeded", base)
		}
	}
	for _, base := range []string{"http://localhost:8080", "http://passway.localhost"} {
		if _, err := New(Config{Store: st, BaseURL: base, DevLogin: true}); err != nil {
			t.Errorf("New with DevLogin and BaseURL %q: %v", base, err)
		}
	}
}

func TestDevLogin(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.DevLogin = true })
	w := e.do("GET", "/auth/dev?login=dev-user", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/dashboard" {
		t.Fatalf("dev login = %d %q", w.Code, w.Header().Get("Location"))
	}
	u, err := e.st.UserByLogin(context.Background(), "dev-user")
	if err != nil {
		t.Fatal(err)
	}
	if u.GitHubID >= 0 {
		t.Errorf("dev GitHubID = %d, want negative", u.GitHubID)
	}
	if dw := e.do("GET", "/dashboard", nil, cookieNamed(w, "passway_session")); dw.Code != http.StatusOK {
		t.Errorf("/dashboard = %d", dw.Code)
	}
	if w := e.do("GET", "/auth/dev?login=", nil); w.Code != http.StatusBadRequest {
		t.Errorf("empty login = %d, want 400", w.Code)
	}

	off := newTestEnv(t, nil)
	if w := off.do("GET", "/auth/dev?login=x", nil); w.Code != http.StatusNotFound {
		t.Errorf("dev login without DevLogin = %d, want 404", w.Code)
	}
}
