// Command passway-server is the Passway tunnel relay server.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/mohammed19n/passway/internal/proxyproto"
	"github.com/mohammed19n/passway/internal/server"
	"github.com/mohammed19n/passway/internal/server/billing"
	"github.com/mohammed19n/passway/internal/server/dashboard"
	"github.com/mohammed19n/passway/internal/server/store"
)

const idleTimeout = 120 * time.Second

var version = "dev" // set with -ldflags at release

func runVersion(w io.Writer) { fmt.Fprintf(w, "passway-server %s\n", version) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var err error
	if len(os.Args) > 1 && os.Args[1] == "version" {
		runVersion(os.Stdout)
	} else if len(os.Args) > 1 && os.Args[1] == "admin" {
		err = runAdmin(ctx, os.Args[2:], os.Stdout)
	} else {
		err = run(ctx, os.Args[1:], os.Stderr, nil)
	}
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	os.Exit(exitCode(err))
}

// usageError is a bad flag value; it exits with status 2.
type usageError struct{ error }

// exitCode maps run's error to the process exit status.
func exitCode(err error) int {
	var ue usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ue):
		return 2
	default:
		return 1
	}
}

// cloudOpts holds the flags that only matter in cloud mode.
type cloudOpts struct {
	db, ghID, ghSecret, baseURL *string
	minGitHubAge                *time.Duration
	freeSignupCap               *int
	freeBandwidthCap            *int64
	adminGitHubIDs              *string
	devLogin                    *bool
	stripePricePro              *string
	legalOperator               *string
	legalJurisdiction           *string
	abuseEmail                  *string
}

func cloudFlags(fs *flag.FlagSet) cloudOpts {
	return cloudOpts{
		db:                fs.String("db", "", "SQLite database path (required in cloud mode; in self-hosted mode it enables admin tokens)"),
		ghID:              fs.String("github-client-id", "", "GitHub OAuth app client ID (cloud mode)"),
		ghSecret:          fs.String("github-client-secret", "", "GitHub OAuth app client secret (cloud mode; default $PASSWAY_GITHUB_CLIENT_SECRET, the flag wins when set)"),
		baseURL:           fs.String("base-url", "", "public dashboard URL (default <scheme>://<domain>[:port])"),
		minGitHubAge:      fs.Duration("min-github-age", 48*time.Hour, "minimum GitHub account age to sign up (0 disables)"),
		freeSignupCap:     fs.Int("free-signup-cap", 1000, "max Free accounts; further signups are waitlisted"),
		freeBandwidthCap:  fs.Int64("free-bandwidth-cap", 500<<30, "monthly bandwidth in bytes across all Free accounts"),
		adminGitHubIDs:    fs.String("admin-github-ids", "", "comma-separated GitHub numeric user IDs allowed to open /admin (find yours at https://api.github.com/users/<login>)"),
		stripePricePro:    fs.String("stripe-price-pro", "", "Stripe price ID of the Pro plan (cloud mode; billing also needs $PASSWAY_STRIPE_SECRET_KEY and $PASSWAY_STRIPE_WEBHOOK_SECRET)"),
		devLogin:          fs.Bool("dev-login", false, "enable the passwordless /auth/dev login (localhost domains only)"),
		legalOperator:     fs.String("legal-operator", "", `who runs the service, named on the terms, privacy and acceptable use pages (default "the operator of this service")`),
		legalJurisdiction: fs.String("legal-jurisdiction", "", "whose laws govern the terms of service, e.g. \"England and Wales\""),
		abuseEmail:        fs.String("abuse-email", "", "abuse contact address shown on the site (default abuse@<base host>)"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// run starts the server and blocks until ctx is cancelled or a listener fails.
// ready, if non-nil, is called with the bound addresses: the plain HTTP address
// first (if enabled), then the TLS address (if enabled).
func run(ctx context.Context, args []string, stderr io.Writer, ready func(addrs []string)) error {
	fs := flag.NewFlagSet("passway-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	domain := fs.String("domain", "", "base domain, e.g. passway.dev (required)")
	addr := fs.String("addr", ":8080", "plain HTTP listen address (empty disables)")
	tlsAddr := fs.String("tls-addr", "", "TLS listen address")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (required with --tls-addr)")
	tlsKey := fs.String("tls-key", "", "TLS key file (required with --tls-addr)")
	tlsMode := fs.String("tls-mode", "files", "files (--tls-cert/--tls-key) or acme (automatic certificates; needs --acme-email and $PASSWAY_CLOUDFLARE_API_TOKEN)")
	acmeEmail := fs.String("acme-email", "", "ACME account email (--tls-mode acme)")
	acmeDirFlag := fs.String("acme-dir", "", "certificate storage directory (--tls-mode acme; default <dir of --db>/certs, else ./certs)")
	acmeCA := fs.String("acme-ca", certmagic.LetsEncryptProductionCA, "ACME directory URL (--tls-mode acme; for dry runs use "+certmagic.LetsEncryptStagingCA+")")
	tokens := fs.String("tokens", "", "comma-separated accepted tokens (empty = open relay; needs --open unless domain is localhost)")
	open := fs.Bool("open", false, "allow running without --tokens on a non-localhost domain (self-hosted only; ignored in cloud mode)")
	publicPortFlag := fs.Int("public-port", 0, "port shown in public URLs (0 = the listener's port)")
	maxConns := fs.Int("max-conns-per-tunnel", 64, "max concurrent proxied connections per tunnel")
	browserWarning := fs.Bool("browser-warning", false, "show a one-time warning page to browser visitors of tunnels")
	rateLimit := fs.Int("rate-limit", 0, "max requests per minute per tunnel (0 = unlimited)")
	mode := fs.String("mode", envOr("PASSWAY_MODE", "self-hosted"), "cloud or self-hosted (default $PASSWAY_MODE, else self-hosted)")
	blockWords := fs.Bool("block-words", false, "refuse subdomains containing phishing-prone words")
	proxyProto := fs.Bool("proxy-protocol", false, "require a PROXY protocol header (v1 or v2) on TLS connections (behind nginx stream passthrough)")
	cf := cloudFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if *domain == "" {
		return errors.New("--domain is required")
	}
	adminIDs, err := parseGitHubIDs(*cf.adminGitHubIDs)
	if err != nil {
		return usageError{fmt.Errorf("--admin-github-ids: %w", err)}
	}
	if *maxConns < 0 {
		return usageError{fmt.Errorf("--max-conns-per-tunnel must be >= 0, got %d", *maxConns)}
	}
	if *rateLimit < 0 {
		return usageError{fmt.Errorf("--rate-limit must be >= 0, got %d", *rateLimit)}
	}
	secret := *cf.ghSecret
	if secret == "" {
		secret = os.Getenv("PASSWAY_GITHUB_CLIENT_SECRET")
	}
	if *mode != "cloud" && *mode != "self-hosted" {
		return usageError{fmt.Errorf("--mode must be cloud or self-hosted, got %q", *mode)}
	}
	cloud := *mode == "cloud"
	if cloud {
		if *tokens != "" {
			return usageError{errors.New("--tokens only applies with --mode self-hosted")}
		}
		if *cf.db == "" || (!*cf.devLogin && (*cf.ghID == "" || secret == "")) {
			return usageError{errors.New("cloud mode needs --db, --github-client-id and --github-client-secret (or use --mode self-hosted)")}
		}
		if *cf.devLogin && *domain != "localhost" && !strings.HasSuffix(*domain, ".localhost") {
			return usageError{errors.New("--dev-login is only allowed on a localhost domain")}
		}
		if missing := billingMissing(*cf.stripePricePro); len(missing) > 0 && len(missing) < 3 {
			return usageError{fmt.Errorf("billing needs all of --stripe-price-pro, PASSWAY_STRIPE_SECRET_KEY and PASSWAY_STRIPE_WEBHOOK_SECRET; missing %s", strings.Join(missing, ", "))}
		}
		// The dashboard is only routed on Host == --domain.
		if *cf.baseURL != "" {
			if u, err := url.Parse(*cf.baseURL); err != nil || !strings.EqualFold(u.Hostname(), *domain) {
				return usageError{fmt.Errorf("--base-url %q must be on --domain %s", *cf.baseURL, *domain)}
			}
		}
	} else if strings.TrimSpace(*tokens) == "" && *cf.db == "" && !*open && *domain != "localhost" && !strings.HasSuffix(*domain, ".localhost") {
		return fmt.Errorf("refusing to run an open relay on %s: set --tokens or pass --open", *domain)
	}
	useTLS := *tlsAddr != ""
	acme := *tlsMode == "acme"
	var acmeCfg acmeOpts
	var tlsCfg *tls.Config
	switch {
	case *tlsMode != "files" && !acme:
		return usageError{fmt.Errorf("--tls-mode must be files or acme, got %q", *tlsMode)}
	case acme:
		if !useTLS {
			return usageError{errors.New("--tls-mode acme needs --tls-addr")}
		}
		if *tlsCert != "" || *tlsKey != "" {
			return usageError{errors.New("--tls-cert and --tls-key only apply with --tls-mode files")}
		}
		if *acmeEmail == "" {
			return usageError{errors.New("--tls-mode acme needs --acme-email")}
		}
		acmeCfg = acmeOpts{email: *acmeEmail, dir: acmeDir(*acmeDirFlag, *cf.db), ca: *acmeCA, cloudflareToken: os.Getenv("PASSWAY_CLOUDFLARE_API_TOKEN")}
		if acmeCfg.cloudflareToken == "" {
			return usageError{errors.New("--tls-mode acme needs PASSWAY_CLOUDFLARE_API_TOKEN in the environment")}
		}
	case useTLS:
		if *tlsCert == "" {
			return errors.New("--tls-cert is required with --tls-addr")
		}
		if *tlsKey == "" {
			return errors.New("--tls-key is required with --tls-addr")
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	if !useTLS && *addr == "" {
		return errors.New("nothing to listen on: set --addr or --tls-addr")
	}

	var plainLn, tlsLn net.Listener
	var addrs []string
	if *addr != "" {
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", *addr, err)
		}
		plainLn = ln
		addrs = append(addrs, ln.Addr().String())
	}
	if useTLS {
		ln, err := net.Listen("tcp", *tlsAddr)
		if err != nil {
			if plainLn != nil {
				plainLn.Close()
			}
			return fmt.Errorf("listen on %s: %w", *tlsAddr, err)
		}
		// Raw, then PROXY protocol, then TLS. tlsLn stays raw until the
		// TLS config exists (in acme mode that needs the store, below).
		tlsLn = ln
		if *proxyProto {
			tlsLn = proxyproto.NewListener(ln, 10*time.Second)
		}
		addrs = append(addrs, ln.Addr().String())
	}

	scheme, pubLn := "http", plainLn
	if useTLS {
		scheme, pubLn = "https", tlsLn
	}
	port := publicPort(scheme, portOf(pubLn.Addr()), *publicPortFlag)

	log := slog.New(slog.NewTextHandler(stderr, nil))
	var st *store.Store
	var cookieSecret []byte
	if *cf.db != "" {
		var err error
		if st, err = store.Open(*cf.db); err != nil {
			closeListeners(plainLn, tlsLn)
			return fmt.Errorf("open database: %w", err)
		}
		defer st.Close()
		cookieSecret, err = st.Setting(context.Background(), "cookie_secret", func() []byte {
			b := make([]byte, 32)
			_, _ = rand.Read(b) // crypto/rand.Read never fails
			return b
		})
		if err != nil {
			closeListeners(plainLn, tlsLn)
			return fmt.Errorf("cookie secret: %w", err)
		}
	}

	var acmeChallenge http.Handler
	if acme {
		// On-demand certificates need the cloud store; self-hosted manages
		// only <domain> and *.<domain>.
		var certStore *store.Store
		if cloud {
			certStore = st
		}
		var err error
		if tlsCfg, acmeChallenge, err = newCertManager(ctx, *domain, acmeCfg, certStore, log); err != nil {
			closeListeners(plainLn, tlsLn)
			return err
		}
		log.Info("automatic TLS", "domain", *domain, "dir", acmeCfg.dir, "ca", acmeCfg.ca, "on_demand", certStore != nil)
	}
	if useTLS {
		tlsLn = tls.NewListener(tlsLn, tlsCfg)
	}

	var authenticator server.Authenticator
	if !cloud && *tokens != "" {
		var list []string
		for _, t := range strings.Split(*tokens, ",") {
			if t = strings.TrimSpace(t); t != "" {
				list = append(list, t)
			}
		}
		authenticator = server.StaticTokens(list)
	}
	if !cloud && st != nil {
		if authenticator == nil {
			authenticator = store.AdminTokens{S: st}
		} else {
			authenticator = server.Chain{authenticator, store.AdminTokens{S: st}}
		}
	}
	cfg := server.Config{
		Domain:       *domain,
		PublicScheme: scheme,
		PublicPort:   port,
		Auth:         authenticator,
		Logger:       log,

		MaxConnsPerTunnel: *maxConns,
		RateLimit:         *rateLimit,
		BrowserWarning:    *browserWarning,
		CookieSecret:      cookieSecret,
		BlockWords:        cloud || *blockWords,
	}
	var srv *server.Server // set below; the dashboard only reads it per request
	if cloud {
		base := *cf.baseURL
		if base == "" {
			base = scheme + "://" + *domain
			if port != 0 {
				base += ":" + strconv.Itoa(port)
			}
		}
		dcfg := cf.dashboardConfig(st, base, secret)
		dcfg.Logger = log
		if *cf.stripePricePro != "" && len(billingMissing(*cf.stripePricePro)) == 0 {
			bc := &billing.Client{Key: os.Getenv("PASSWAY_STRIPE_SECRET_KEY")}
			pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
			price, err := bc.Price(pctx, *cf.stripePricePro)
			pcancel()
			if err != nil {
				closeListeners(plainLn, tlsLn)
				return fmt.Errorf("stripe: fetch price %s: %w", *cf.stripePricePro, err)
			}
			dcfg.Billing, dcfg.ProPrice, dcfg.PriceID = bc, price, *cf.stripePricePro
			dcfg.WebhookSecret = os.Getenv("PASSWAY_STRIPE_WEBHOOK_SECRET")
			log.Info("billing enabled", "price", price.Label())
		}
		dcfg.PlanChanged = func(id int64) { srv.CloseAccountTunnels(strconv.FormatInt(id, 10)) }
		dcfg.Tunnels = func(id string) []dashboard.TunnelInfo {
			var out []dashboard.TunnelInfo
			for _, t := range srv.TunnelsFor(id) {
				out = append(out, dashboard.TunnelInfo{URL: t.URL, ClientVersion: t.ClientVersion, ConnectedAt: t.ConnectedAt, Requests: t.Requests})
			}
			return out
		}
		dcfg.AdminGitHubIDs = adminIDs
		dcfg.Stats = func() dashboard.AdminStats {
			st := srv.Stats()
			return dashboard.AdminStats{Tunnels: st.Tunnels, ByPlan: st.ByPlan, Requests: st.Requests}
		}
		dcfg.PollBans = func() { srv.PollBansNow(ctx) }
		dcfg.Disconnect = func(id string, tokenID int64) { srv.CloseTokenTunnels(id, tokenID) }
		dcfg.DomainRemoved = func(host string, id int64) { srv.CloseHostTunnelsOf(host, strconv.FormatInt(id, 10)) }
		dcfg.DomainVerified = func(host string, id int64) { srv.CloseHostTunnelsExcept(host, strconv.FormatInt(id, 10)) }
		dash, err := dashboard.New(dcfg)
		if err != nil {
			closeListeners(plainLn, tlsLn)
			return err
		}
		cfg.Directory = &store.Directory{S: st}
		cfg.FreeBandwidthCap = *cf.freeBandwidthCap
		cfg.Dashboard = dash
	}
	srv = server.New(cfg)
	if cloud {
		// Registered after the st.Close defer, so on return: cancel, wait for
		// RunBackground's final meter flush, then close the store.
		bgCtx, bgStop := context.WithCancel(ctx)
		bgDone := make(chan struct{})
		go func() { defer close(bgDone); srv.RunBackground(bgCtx) }()
		defer func() { bgStop(); <-bgDone }()
	}

	var servers []*http.Server
	errc := make(chan error, 2)
	serve := func(h http.Handler, ln net.Listener, kind string) {
		hs := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: idleTimeout}
		servers = append(servers, hs)
		go func() {
			log.Info("listening", "kind", kind, "addr", ln.Addr().String(), "domain", *domain)
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s server: %w", kind, err)
			}
		}()
	}
	if useTLS {
		serve(srv, tlsLn, "https")
		if plainLn != nil {
			var h http.Handler = redirectHandler(port)
			if acmeChallenge != nil {
				h = acmeHTTPHandler(acmeChallenge, h)
			}
			serve(h, plainLn, "http-redirect")
		}
	} else {
		serve(srv, plainLn, "http")
	}
	if ready != nil {
		ready(addrs)
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	srv.CloseTunnels()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, hs := range servers {
		if err := hs.Shutdown(sctx); err != nil {
			log.Warn("shutdown", "err", err)
			hs.Close()
		}
	}
	return runErr
}

// billingMissing lists the billing inputs that are unset. Billing is on only
// when none is missing; all three missing means billing is off.
func billingMissing(pricePro string) []string {
	var m []string
	if pricePro == "" {
		m = append(m, "--stripe-price-pro")
	}
	if os.Getenv("PASSWAY_STRIPE_SECRET_KEY") == "" {
		m = append(m, "PASSWAY_STRIPE_SECRET_KEY")
	}
	if os.Getenv("PASSWAY_STRIPE_WEBHOOK_SECRET") == "" {
		m = append(m, "PASSWAY_STRIPE_WEBHOOK_SECRET")
	}
	return m
}

func closeListeners(lns ...net.Listener) {
	for _, ln := range lns {
		if ln != nil {
			ln.Close()
		}
	}
}

// publicPort returns the port to show in public URLs: override if set, else
// the listener's port, with the scheme's default port reported as 0 (omitted).
func publicPort(scheme string, listenPort, override int) int {
	port := listenPort
	if override != 0 {
		port = override
	}
	if (scheme == "http" && port == 80) || (scheme == "https" && port == 443) {
		return 0
	}
	return port
}

func portOf(a net.Addr) int {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.Port
	}
	return 0
}

// redirectHandler sends every plain-HTTP request to the same URL over HTTPS.
// httpsPort is the public HTTPS port (0 = default 443).
func redirectHandler(httpsPort int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			host = "[" + host + "]" // bare IPv6 literal
		}
		if httpsPort != 0 {
			host += ":" + strconv.Itoa(httpsPort)
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

// dashboardConfig maps the cloud flags to a dashboard.Config; the caller adds
// Tunnels, Disconnect and Logger.
func (cf cloudOpts) dashboardConfig(st *store.Store, base, secret string) dashboard.Config {
	return dashboard.Config{
		Store:              st,
		BaseURL:            base,
		GitHubClientID:     *cf.ghID,
		GitHubClientSecret: secret,
		MinGitHubAge:       *cf.minGitHubAge,
		FreeSignupCap:      *cf.freeSignupCap,
		FreeBandwidthCap:   *cf.freeBandwidthCap,
		DevLogin:           *cf.devLogin,
		LegalOperator:      *cf.legalOperator,
		LegalJurisdiction:  *cf.legalJurisdiction,
		AbuseEmail:         *cf.abuseEmail,
	}
}

// parseGitHubIDs parses a comma-separated list of numeric GitHub user IDs,
// ignoring blanks. Logins are rejected: they can change hands.
func parseGitHubIDs(s string) ([]int64, error) {
	var out []int64
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a numeric GitHub user ID", f)
		}
		out = append(out, id)
	}
	return out, nil
}
