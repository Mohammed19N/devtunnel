package server

import (
	"log/slog"
	"net"
	"net/http"
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
	Auth         Authenticator // nil = StaticTokens(nil)
	NameHold     time.Duration // 0 = 2 * time.Minute
	Logger       *slog.Logger  // nil = slog.Default()

	MaxConnsPerTunnel int // concurrent proxied connections per tunnel; 0 = 64
}

// Server is the Passway tunnel server.
type Server struct {
	cfg      Config
	registry *Registry
	log      *slog.Logger
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
	if cfg.Auth == nil {
		cfg.Auth = StaticTokens(nil)
	}
	if st, ok := cfg.Auth.(StaticTokens); ok && len(st) == 0 {
		cfg.Logger.Warn("no auth tokens configured: every client token is accepted (development only)")
	}
	return &Server{cfg: cfg, registry: NewRegistry(cfg.NameHold), log: cfg.Logger}
}

// PublicURL returns the public URL for a tunnel name.
func (s *Server) PublicURL(name string) string {
	host := name + "." + s.cfg.Domain
	if s.cfg.PublicPort != 0 {
		host = net.JoinHostPort(host, strconv.Itoa(s.cfg.PublicPort))
	}
	return s.cfg.PublicScheme + "://" + host
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
