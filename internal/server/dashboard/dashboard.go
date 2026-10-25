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

	"github.com/mohammed19n/passway/internal/server"
	"github.com/mohammed19n/passway/internal/server/billing"
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

	MinGitHubAge  time.Duration // 0 disables; the CLI default is 48h
	FreeSignupCap int           // 0 = 1000
	DevLogin      bool          // allow the /auth/dev login form (only when BaseURL host is localhost or *.localhost)

	// Billing is the Stripe client; nil = billing off. With it set, ProPrice
	// (read from Stripe at startup), PriceID and WebhookSecret are required.
	Billing       *billing.Client
	ProPrice      billing.Price
	PriceID       string
	WebhookSecret string
	// PlanChanged is called for each user whose plan a webhook changed. nil = no-op.
	PlanChanged func(userID int64)

	// Resolver answers the custom-domain DNS checks; nil = net.DefaultResolver.
	Resolver Resolver
	// EdgeHost is the CNAME target for custom domains; "" = "edge.<BaseURL host>".
	EdgeHost string
	// DomainRemoved closes userID's own live tunnel on custom domain host
	// and drops its hold, after userID removed its verified row. Other
	// accounts' tunnels on host must stay up. nil = no-op.
	DomainRemoved func(host string, userID int64)
	// DomainVerified closes every other account's live tunnel on host and
	// drops their holds, after userID verified it. nil = no-op.
	DomainVerified func(host string, userID int64)

	// AdminGitHubIDs are the numeric GitHub user IDs that may use /admin.
	// IDs, not logins: a login can be renamed and then taken by someone else.
	AdminGitHubIDs []int64
	// Stats reports live tunnel activity for /admin; nil = zeros.
	Stats func() AdminStats
	// PollBans applies new bans to live tunnels right away; nil = no-op.
	PollBans func()
	// FreeBandwidthCap is the monthly Free-plan cap in bytes shown on /admin.
	FreeBandwidthCap int64

	Tunnels func(accountID string) []TunnelInfo // live tunnels for the dashboard
	// Disconnect closes the live tunnels of accountID that use tokenID; it is
	// called after the token is revoked. nil = no-op.
	Disconnect func(accountID string, tokenID int64)
	Now        func() time.Time // nil = time.Now
	Logger     *slog.Logger     // nil = slog.Default()

	// LegalOperator and LegalJurisdiction fill the legal pages; empty values
	// render "the operator of this service".
	LegalOperator, LegalJurisdiction string
	AbuseEmail                       string // "" = "abuse@<BaseURL host>"
	RepoURL                          string // "" = "https://github.com/mohammed19n/passway"
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
	baseHost      string // BaseURL's host, without port
	secure        bool   // BaseURL is https
	sessionCookie string
	stateCookie   string
	client        *http.Client
	mux           *http.ServeMux
	postRoutes    []string // paths of every POST route, for tests
	abuseLimiter  *server.RateLimiter
}

//go:embed templates/*.html templates/site/*.html
var templateFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{"asset": assetURL}).
	ParseFS(templateFS, "templates/*.html", "templates/site/*.html"))

const csp = "default-src 'none'; style-src 'self'; img-src 'self' https://avatars.githubusercontent.com; form-action 'self' https://github.com https://checkout.stripe.com https://billing.stripe.com; frame-ancestors 'none'; base-uri 'none'"

// siteCSP is csp plus connect-src 'self' for the public pages, so tools such
// as Lighthouse can fetch /robots.txt from the page. No script can run on
// either: script-src falls back to default-src 'none'.
const siteCSP = csp + "; connect-src 'self'"

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
	if cfg.Disconnect == nil {
		cfg.Disconnect = func(string, int64) {}
	}
	if cfg.Billing != nil && (cfg.PriceID == "" || cfg.WebhookSecret == "") {
		return nil, errors.New("dashboard: billing needs PriceID and WebhookSecret")
	}
	if cfg.PlanChanged == nil {
		cfg.PlanChanged = func(int64) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.DomainRemoved == nil {
		cfg.DomainRemoved = func(string, int64) {}
	}
	if cfg.DomainVerified == nil {
		cfg.DomainVerified = func(string, int64) {}
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	cfg.EdgeHost = strings.TrimSuffix(strings.ToLower(cfg.EdgeHost), ".")
	if cfg.EdgeHost == "" {
		cfg.EdgeHost = "edge." + host
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.AbuseEmail == "" {
		cfg.AbuseEmail = "abuse@" + host
	}
	cfg.RepoURL = strings.TrimSuffix(cfg.RepoURL, "/")
	if cfg.RepoURL == "" {
		cfg.RepoURL = "https://github.com/mohammed19n/passway"
	}

	d := &Dashboard{
		cfg:           cfg,
		log:           cfg.Logger,
		origin:        origin,
		baseHost:      host,
		secure:        u.Scheme == "https",
		sessionCookie: "passway_session",
		stateCookie:   "passway_oauth_state",
		client:        &http.Client{Timeout: 10 * time.Second},
		mux:           http.NewServeMux(),
		abuseLimiter:  server.NewRateLimiterWindow(abusePerHour, time.Hour),
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
	d.siteRoutes()
	m.HandleFunc("GET /login", d.handleLogin)
	m.HandleFunc("GET /auth/github/callback", d.handleCallback)
	if d.cfg.DevLogin {
		m.HandleFunc("GET /auth/dev", d.handleDevLoginForm)
		m.HandleFunc("POST /auth/dev", d.handleDevLogin)
	}
	d.handlePost("/logout", d.handleLogout)
	m.HandleFunc("GET /dashboard", d.page(d.handleDashboard))
	m.HandleFunc("GET /dashboard/subdomains", d.page(d.active(d.handleSubdomains)))
	d.handlePost("/dashboard/subdomains", d.active(d.handleClaim))
	d.handlePost("/dashboard/subdomains/release", d.active(d.handleRelease))
	m.HandleFunc("GET /dashboard/domains", d.page(d.active(d.handleDomains)))
	d.handlePost("/dashboard/domains", d.active(d.handleAddDomain))
	d.handlePost("/dashboard/domains/verify", d.active(d.handleVerifyDomain))
	d.handlePost("/dashboard/domains/remove", d.active(d.handleRemoveDomain))
	m.HandleFunc("GET /dashboard/tokens", d.page(d.active(d.handleTokens)))
	d.handlePost("/dashboard/tokens", d.active(d.handleCreateToken))
	d.handlePost("/dashboard/tokens/revoke", d.active(d.handleRevokeToken))
	if d.cfg.Billing != nil {
		d.handlePost("/billing/checkout", d.active(d.handleCheckout))
		d.handlePost("/billing/portal", d.active(d.handlePortal))
		// Stripe calls this: no session, CSRF or origin checks; the signature is the auth.
		m.HandleFunc("POST /stripe/webhook", d.handleStripeWebhook)
	}
	m.HandleFunc("GET /admin", d.page(d.adminOnly(d.handleAdmin)))
	d.handlePost("/admin/ban", d.adminOnly(d.handleAdminBan))
	d.handlePost("/admin/resolve", d.adminOnly(d.handleAdminResolve))
	m.HandleFunc("GET /waitlist", d.page(d.handleWaitlist))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.fail(w, http.StatusNotFound, "There's nothing here.")
	})
}

// handlePost registers a POST route behind the session, origin and CSRF
// checks and records its path in postRoutes. Every POST goes through here.
func (d *Dashboard) handlePost(path string, h func(http.ResponseWriter, *http.Request, *session)) {
	d.postRoutes = append(d.postRoutes, path)
	d.mux.HandleFunc("POST "+path, d.post(h))
}

// ServeHTTP sets the security headers, gzips text responses and dispatches
// to the routes.
func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	gw := newGzipWriter(w, r)
	defer gw.close()
	w = gw
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", csp)
	if r.Method == http.MethodPost && r.URL.Path != "/stripe/webhook" {
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
	RepoURL string      // the footer's GitHub link

	// Public site pages (SEO set = indexable site layout, nil = noindex).
	SEO          *seo
	InstallCmd   string
	Free, Pro    planCard
	Operator     string
	Jurisdiction string
	AbuseEmail   string
	Abuse        abuseFormState
	Sent         bool // ?sent=1 after an abuse report

	// Account pages.
	Error       string // inline form error
	BaseURL     string
	Host        string // BaseURL's host[:port]
	Tunnels     []tunnelRow
	Plan        planView
	Subdomains  []subdomainRow
	ClaimFull   bool // the plan's reserved subdomains are all used: no claim form
	ClaimMax    int
	Upgrade     bool   // on Free, so ClaimFull suggests upgrading
	Billing     bool   // billing is on: show Upgrade / Manage billing
	ProLabel    string // e.g. "$7 / month"
	HasCustomer bool   // the user has a Stripe customer: show Manage billing
	Upgraded    bool   // ?upgraded=1 after checkout
	Domains     []domainRow
	DomainMax   int    // the plan's MaxCustomDomains; 0 = custom domains are not included
	DomainFull  bool   // all DomainMax are used: no add form
	EdgeHost    string // the CNAME target for custom domains
	Tokens      []tokenRow
	NewToken    string // plaintext of a token created by this very request
	NewName     string
	Admin       *adminView
}

// render executes the named template into a buffer and writes it with status.
func (d *Dashboard) render(w http.ResponseWriter, status int, name string, v view) {
	v.RepoURL = d.cfg.RepoURL
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

func (d *Dashboard) handleWaitlist(w http.ResponseWriter, r *http.Request, s *session) {
	if !s.User.Waitlisted {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	d.render(w, http.StatusOK, "waitlist", view{Title: "Waitlist", User: &s.User, CSRF: s.CSRF})
}
