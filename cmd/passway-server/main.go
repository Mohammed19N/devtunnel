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

	"github.com/mohammed19n/passway/internal/server"
	"github.com/mohammed19n/passway/internal/server/dashboard"
	"github.com/mohammed19n/passway/internal/server/store"
)

const idleTimeout = 120 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var err error
	if len(os.Args) > 1 && os.Args[1] == "admin" {
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
	devLogin                    *bool
}

func cloudFlags(fs *flag.FlagSet) cloudOpts {
	return cloudOpts{
		db:               fs.String("db", "", "SQLite database path (required in cloud mode; in self-hosted mode it enables admin tokens)"),
		ghID:             fs.String("github-client-id", "", "GitHub OAuth app client ID (cloud mode)"),
		ghSecret:         fs.String("github-client-secret", "", "GitHub OAuth app client secret (cloud mode; default $PASSWAY_GITHUB_CLIENT_SECRET, the flag wins when set)"),
		baseURL:          fs.String("base-url", "", "public dashboard URL (default <scheme>://<domain>[:port])"),
		minGitHubAge:     fs.Duration("min-github-age", 48*time.Hour, "minimum GitHub account age to sign up (0 disables)"),
		freeSignupCap:    fs.Int("free-signup-cap", 1000, "max Free accounts; further signups are waitlisted"),
		freeBandwidthCap: fs.Int64("free-bandwidth-cap", 500<<30, "monthly bandwidth in bytes across all Free accounts"),
		devLogin:         fs.Bool("dev-login", false, "enable the passwordless /auth/dev login (localhost domains only)"),
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
	tokens := fs.String("tokens", "", "comma-separated accepted tokens (empty = open relay; needs --open unless domain is localhost)")
	open := fs.Bool("open", false, "allow running without --tokens on a non-localhost domain (self-hosted only; ignored in cloud mode)")
	publicPortFlag := fs.Int("public-port", 0, "port shown in public URLs (0 = the listener's port)")
	maxConns := fs.Int("max-conns-per-tunnel", 64, "max concurrent proxied connections per tunnel")
	browserWarning := fs.Bool("browser-warning", false, "show a one-time warning page to browser visitors of tunnels")
	rateLimit := fs.Int("rate-limit", 0, "max requests per minute per tunnel (0 = unlimited)")
	mode := fs.String("mode", envOr("PASSWAY_MODE", "self-hosted"), "cloud or self-hosted (default $PASSWAY_MODE, else self-hosted)")
	blockWords := fs.Bool("block-words", false, "refuse subdomains containing phishing-prone words")
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
	var cert tls.Certificate
	if useTLS {
		if *tlsCert == "" {
			return errors.New("--tls-cert is required with --tls-addr")
		}
		if *tlsKey == "" {
			return errors.New("--tls-key is required with --tls-addr")
		}
		var err error
		if cert, err = tls.LoadX509KeyPair(*tlsCert, *tlsKey); err != nil {
			return fmt.Errorf("load TLS certificate: %w", err)
		}
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
		tlsLn = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
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
		dcfg.Tunnels = func(id string) []dashboard.TunnelInfo {
			var out []dashboard.TunnelInfo
			for _, t := range srv.TunnelsFor(id) {
				out = append(out, dashboard.TunnelInfo{URL: t.URL, ClientVersion: t.ClientVersion, ConnectedAt: t.ConnectedAt, Requests: t.Requests})
			}
			return out
		}
		dcfg.Disconnect = func(id string, tokenID int64) { srv.CloseTokenTunnels(id, tokenID) }
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
			serve(redirectHandler(port), plainLn, "http-redirect")
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
		DevLogin:           *cf.devLogin,
	}
}
