// Package dashboard serves the Passway cloud dashboard on the base domain:
// GitHub sign-in, sessions and the account pages.
//
// Tunnels run on subdomains of the same base domain, so every tunnel is a
// hostile neighbour that can set cookies with Domain=<base>. Hence the
// __Host- cookie prefix on https, per-session CSRF tokens on every POST and
// an Origin/Referer check.
package dashboard

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/server/store"
)

// Config configures a Dashboard.
type Config struct {
	Store                              *store.Store
	BaseURL                            string // e.g. "https://passway.run" (no trailing slash)
	GitHubClientID, GitHubClientSecret string

	// Defaults: https://github.com/login/oauth/authorize,
	// https://github.com/login/oauth/access_token, https://api.github.com
	GitHubAuthURL, GitHubTokenURL, GitHubAPIURL string

	MinGitHubAge  time.Duration // 0 disables; the CLI default is 7*24h
	FreeSignupCap int           // 0 = 1000
	DevLogin      bool          // allow /auth/dev?login=X (only when BaseURL host is localhost or *.localhost)

	Tunnels func(accountID string) []TunnelInfo // live tunnels for the dashboard
	Now     func() time.Time                    // nil = time.Now
	Logger  *slog.Logger                        // nil = slog.Default()
}

// TunnelInfo describes one live tunnel.
type TunnelInfo struct {
	URL, ClientVersion string
	ConnectedAt        time.Time
	Requests           int64
}

// Dashboard is the base-domain web UI. It implements http.Handler.
type Dashboard struct {
	cfg           Config
	log           *slog.Logger
	origin        string // BaseURL's scheme://host[:port], as browsers send it in Origin
	secure        bool   // BaseURL is https
	sessionCookie string
	stateCookie   string
	client        *http.Client
	mux           *http.ServeMux
}

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/app.css
var appCSS []byte

var tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

const csp = "default-src 'none'; style-src 'self'; img-src 'self' https://avatars.githubusercontent.com; form-action 'self' https://github.com; frame-ancestors 'none'; base-uri 'none'"

// New validates cfg, applies defaults and returns a Dashboard.
func New(cfg Config) (*Dashboard, error) {
	if cfg.Store == nil {
		return nil, errors.New("dashboard: Store is required")
	}
	u, err := url.Parse(strings.TrimSuffix(cfg.BaseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("dashboard: BaseURL %q must look like https://example.com", cfg.BaseURL)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	origin := u.Scheme + "://" + host
	if port != "" {
		origin = u.Scheme + "://" + net.JoinHostPort(host, port)
	}
	cfg.BaseURL = origin
	if u.Scheme == "http" && host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("dashboard: BaseURL %q must be https (plain http is only allowed on localhost)", cfg.BaseURL)
	}
	if cfg.DevLogin && host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("dashboard: dev login is only allowed on localhost, not %q", host)
	}
	if !cfg.DevLogin && (cfg.GitHubClientID == "" || cfg.GitHubClientSecret == "") {
		return nil, errors.New("dashboard: GitHub client ID and secret are required")
	}
	if cfg.GitHubAuthURL == "" {
		cfg.GitHubAuthURL = "https://github.com/login/oauth/authorize"
	}
	if cfg.GitHubTokenURL == "" {
		cfg.GitHubTokenURL = "https://github.com/login/oauth/access_token"
	}
	if cfg.GitHubAPIURL == "" {
		cfg.GitHubAPIURL = "https://api.github.com"
	}
	cfg.GitHubAPIURL = strings.TrimSuffix(cfg.GitHubAPIURL, "/")
	if cfg.FreeSignupCap == 0 {
		cfg.FreeSignupCap = 1000
	}
	if cfg.Tunnels == nil {
		cfg.Tunnels = func(string) []TunnelInfo { return nil }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	d := &Dashboard{
		cfg:           cfg,
		log:           cfg.Logger,
		origin:        origin,
		secure:        u.Scheme == "https",
		sessionCookie: "passway_session",
		stateCookie:   "passway_oauth_state",
		client:        &http.Client{Timeout: 10 * time.Second},
		mux:           http.NewServeMux(),
	}
	if d.secure {
		d.sessionCookie = "__Host-" + d.sessionCookie
		d.stateCookie = "__Host-" + d.stateCookie
	}
	d.routes()
	return d, nil
}

func (d *Dashboard) routes() {
	m := d.mux
	m.HandleFunc("GET /{$}", d.handleHome)
	m.HandleFunc("GET /login", d.handleLogin)
	m.HandleFunc("GET /auth/github/callback", d.handleCallback)
	if d.cfg.DevLogin {
		m.HandleFunc("GET /auth/dev", d.handleDevLogin)
	}
	m.HandleFunc("POST /logout", d.post(d.handleLogout))
	m.HandleFunc("GET /dashboard", d.page(d.handleDashboard))
	m.HandleFunc("GET /dashboard/subdomains", d.page(d.active(d.handleSubdomains)))
	m.HandleFunc("POST /dashboard/subdomains", d.post(d.active(d.handleClaim)))
	m.HandleFunc("POST /dashboard/subdomains/release", d.post(d.active(d.handleRelease)))
	m.HandleFunc("GET /dashboard/tokens", d.page(d.active(d.handleTokens)))
	m.HandleFunc("POST /dashboard/tokens", d.post(d.active(d.handleCreateToken)))
	m.HandleFunc("POST /dashboard/tokens/revoke", d.post(d.active(d.handleRevokeToken)))
	m.HandleFunc("GET /waitlist", d.page(d.handleWaitlist))
	m.HandleFunc("GET /static/app.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(appCSS)
	})
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.fail(w, http.StatusNotFound, "There's nothing here.")
	})
}

// ServeHTTP sets the security headers and dispatches to the routes.
func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", csp)
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	}
	d.mux.ServeHTTP(w, r)
}

// view is the data every template receives.
type view struct {
	Title   string
	User    *store.User // nil when signed out
	CSRF    string      // the session's CSRF token for POST forms
	Message string      // error page text
	Wide    bool        // account pages use the wide layout

	// Account pages.
	Error      string // inline form error
	BaseURL    string
	Host       string // BaseURL's host[:port]
	Tunnels    []tunnelRow
	Plan       planView
	Subdomains []subdomainRow
	Tokens     []tokenRow
	NewToken   string // plaintext of a token created by this very request
	NewName    string
}

// render executes the named template into a buffer and writes it with status.
func (d *Dashboard) render(w http.ResponseWriter, status int, name string, v view) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, v); err != nil {
		d.log.Error("dashboard: render", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// fail renders the error page with a short human message.
func (d *Dashboard) fail(w http.ResponseWriter, status int, msg string) {
	d.render(w, status, "error", view{Title: http.StatusText(status), Message: msg})
}

func (d *Dashboard) internalError(w http.ResponseWriter, what string, err error) {
	d.log.Error("dashboard: "+what, "err", err)
	d.fail(w, http.StatusInternalServerError, "Something went wrong on our side. Please try again.")
}

func (d *Dashboard) handleHome(w http.ResponseWriter, r *http.Request) {
	s, err := d.currentSession(w, r)
	if err != nil {
		d.internalError(w, "session", err)
		return
	}
	if s != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	d.render(w, http.StatusOK, "home", view{Title: "Sign in"})
}

func (d *Dashboard) handleWaitlist(w http.ResponseWriter, r *http.Request, s *session) {
	if !s.User.Waitlisted {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	d.render(w, http.StatusOK, "waitlist", view{Title: "Waitlist", User: &s.User, CSRF: s.CSRF})
}
