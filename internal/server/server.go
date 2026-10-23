package server

import (
	"crypto/rand"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/protocol"
)

// Config configures a Server.
type Config struct {
	Domain       string        // e.g. "localhost", "passway.dev"; lowercased by New
	PublicScheme string        // "http" or "https"
	PublicPort   int           // 0 = scheme default (omitted from URLs)
	Auth         Authenticator // nil = StaticTokens(nil); ignored when Directory is set
	NameHold     time.Duration // 0 = 2 * time.Minute
	Logger       *slog.Logger  // nil = slog.Default()

	MaxConnsPerTunnel int    // concurrent proxied connections per tunnel; 0 = 64
	RateLimit         int    // requests per minute per tunnel; 0 = unlimited
	BrowserWarning    bool   // show a one-time interstitial to browser navigations
	CookieSecret      []byte // HMAC key for the warned cookie; empty = 32 random bytes per start

	Directory        Directory // cloud account directory; nil = self-hosted
	FreeBandwidthCap int64     // bytes per month across all Free accounts; 0 = 500<<30 when Directory != nil
}

// Server is the Passway tunnel server.
type Server struct {
	cfg      Config
	registry *Registry
	log      *slog.Logger
	limiter  *RateLimiter // nil = unlimited
}

// New creates a Server from cfg, applying defaults.
func New(cfg Config) *Server {
	cfg.Domain = strings.ToLower(cfg.Domain)
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.NameHold == 0 {
		cfg.NameHold = 2 * time.Minute
	}
	if cfg.MaxConnsPerTunnel == 0 {
		cfg.MaxConnsPerTunnel = 64
	}
	if len(cfg.CookieSecret) == 0 {
		cfg.CookieSecret = make([]byte, 32)
		_, _ = rand.Read(cfg.CookieSecret) // crypto/rand.Read never fails
	}
	if cfg.Directory != nil {
		if cfg.Auth != nil {
			cfg.Logger.Warn("both Auth and Directory are set: Auth is ignored")
		}
		cfg.Auth = cfg.Directory
		if cfg.FreeBandwidthCap == 0 {
			cfg.FreeBandwidthCap = 500 << 30
		}
	}
	if cfg.Auth == nil {
		cfg.Auth = StaticTokens(nil)
	}
	if st, ok := cfg.Auth.(StaticTokens); ok && len(st) == 0 {
		cfg.Logger.Warn("no auth tokens configured: every client token is accepted (development only)")
	}
	s := &Server{cfg: cfg, registry: NewRegistry(cfg.NameHold), log: cfg.Logger}
	if cfg.RateLimit > 0 {
		s.limiter = NewRateLimiter(cfg.RateLimit)
	}
	return s
}

// PublicURL returns the public URL for a tunnel name.
func (s *Server) PublicURL(name string) string {
	host := name + "." + s.cfg.Domain
	if s.cfg.PublicPort != 0 {
		host = net.JoinHostPort(host, strconv.Itoa(s.cfg.PublicPort))
	}
	return s.cfg.PublicScheme + "://" + host
}

// TunnelSnapshot describes one live tunnel for the dashboard.
type TunnelSnapshot struct {
	URL, ClientVersion string
	ConnectedAt        time.Time
	Requests           int64
}

// TunnelsFor returns the live tunnels of accountID, oldest first.
func (s *Server) TunnelsFor(accountID string) []TunnelSnapshot {
	var out []TunnelSnapshot
	for _, t := range s.registry.ByAccount(accountID) {
		out = append(out, TunnelSnapshot{URL: s.PublicURL(t.Name), ClientVersion: t.ClientVersion, ConnectedAt: t.ConnectedAt, Requests: t.Requests()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.Before(out[j].ConnectedAt) })
	return out
}

// CloseTunnels closes every active tunnel session. The server keeps accepting new tunnels.
func (s *Server) CloseTunnels() {
	for _, t := range s.registry.All() {
		if t.session != nil {
			t.session.Close()
		}
	}
}

// ServeHTTP routes requests. Base-domain routes are served here; <label>.<Domain> goes to a tunnel.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if host := hostOnly(r.Host); host != s.cfg.Domain {
		s.serveEdge(w, r, host)
		return
	}
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	case protocol.TunnelPath:
		s.handleControl(w, r)
	default:
		http.NotFound(w, r)
	}
}

// hostOnly lowercases host and strips any port.
func hostOnly(host string) string {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
