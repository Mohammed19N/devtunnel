package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/protocol"
)

// newTunnelProxy builds t's reverse proxy and transport. t.Name is read at
// request time; Registry.Claim sets it before the tunnel becomes visible.
func newTunnelProxy(t *Tunnel, maxConns int, log *slog.Logger, footer func() pageData) {
	t.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			type result struct {
				conn net.Conn
				err  error
			}
			// session.Open can block (e.g. a full accept backlog), so run it
			// off-thread and let ctx abandon it.
			ch := make(chan result, 1)
			go func() {
				st, err := t.session.Open()
				if err != nil {
					ch <- result{nil, err}
					return
				}
				// The client blocks in Accept until it reads this header.
				if err := protocol.WriteFrame(st, protocol.StreamHeader{Kind: protocol.KindHTTP}); err != nil {
					st.Close()
					ch <- result{nil, err}
					return
				}
				ch <- result{st, nil}
			}()
			select {
			case r := <-ch:
				return r.conn, r.err
			case <-ctx.Done():
				go func() {
					if r := <-ch; r.conn != nil {
						r.conn.Close()
					}
				}()
				return nil, ctx.Err()
			}
		},
		MaxConnsPerHost:     maxConns,
		MaxIdleConnsPerHost: min(16, maxConns),
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	t.proxy = &httputil.ReverseProxy{
		FlushInterval: -1,
		Transport:     t.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: t.Name})
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			stripWarnCookie(pr.Out.Header)
		},
		// The app must not be able to set the edge's warned cookie.
		ModifyResponse: func(resp *http.Response) error {
			dropWarnSetCookie(resp.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, errBandwidth) {
				renderPage(w, r, pageBandwidth, footer().withHost(r.Host))
				return
			}
			if !errors.Is(err, context.Canceled) {
				log.Warn("proxy error", "name", t.Name, "err", err)
			}
			renderPage(w, r, pageOffline, footer().withHost(r.Host))
		},
	}
}

// serveEdge routes a request whose host is not the base domain. A host under
// the base domain is looked up by its label; in cloud mode any other host is
// looked up as a custom domain by its full name. Registry keys for custom
// domains always contain a dot and labels never do, so neither lookup can
// reach the other kind of tunnel.
func (s *Server) serveEdge(w http.ResponseWriter, r *http.Request, host string) {
	label, ok := strings.CutSuffix(host, "."+s.cfg.Domain)
	if ok {
		if label == "" || strings.Contains(label, ".") {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
	} else {
		if s.cfg.Directory == nil || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
		label = host
	}
	// In cloud mode /_passway/continue is reserved on every tunnel host (Free
	// plans need it), so it never reaches a tunnel.
	if r.URL.Path == continuePath {
		if !s.cfg.BrowserWarning && s.cfg.Directory == nil {
			http.NotFound(w, r)
			return
		}
		handleContinue(w, r, s.cfg.CookieSecret, s.cfg.PublicScheme == "https")
		return
	}
	cloud := s.cfg.Directory != nil
	pd := func(t *Tunnel) pageData { return s.pageBase(t).withHost(r.Host) }
	// A banned host is refused even after its session is gone.
	if cloud && s.bans.Load().banned(Account{}, label) {
		renderPage(w, r, pageBanned, pd(nil))
		return
	}
	t, ok := s.registry.Lookup(label)
	if !ok {
		renderPage(w, r, pageOffline, pd(nil))
		return
	}
	acc := t.Account
	warn, rate := s.cfg.BrowserWarning, s.cfg.RateLimit
	if cloud {
		if s.bans.Load().banned(acc, t.Name) {
			renderPage(w, r, pageBanned, pd(t))
			return
		}
		warn = acc.Plan.Warning || (acc.Plan.Name == PlanUnlimited.Name && s.cfg.BrowserWarning)
		if acc.Plan.RateLimit > 0 {
			rate = acc.Plan.RateLimit
		}
	}
	if warn && needsWarning(r, s.cfg.CookieSecret) {
		d := pd(t)
		d.Continue = continuePath + "?to=" + url.QueryEscape(r.URL.RequestURI())
		if cloud {
			d.Owner = acc.Login
		}
		renderPage(w, r, pageWarning, d)
		return
	}
	over := func() bool {
		return s.meter.Over(acc) || (acc.Plan.Name == PlanFree.Name && s.meter.FreeCapHit())
	}
	if s.meter != nil && over() {
		renderPage(w, r, pageBandwidth, pd(t))
		return
	}
	if rate > 0 && s.limiter != nil {
		if ok, retry := s.limiter.AllowN(t.Name, rate); !ok {
			secs := int((retry + time.Second - 1) / time.Second)
			d := pd(t)
			d.RetryIn = max(secs, 1)
			renderPage(w, r, pageRateLimited, d)
			return
		}
	}
	// Fast-fail at the cap instead of queueing in the transport.
	if t.active.Add(1) > int32(s.cfg.MaxConnsPerTunnel) {
		t.active.Add(-1)
		renderPage(w, r, pageBusy, pd(t))
		return
	}
	defer t.active.Add(-1)
	t.requests.Add(1)
	if s.meter != nil {
		m := &transferMeter{add: func(n int64) { s.meter.Add(acc, n) }, over: over}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &countingBody{ReadCloser: r.Body, m: m}
		}
		w = &countingWriter{ResponseWriter: w, m: m}
	}
	t.proxy.ServeHTTP(w, r)
}

// withHost returns d with Host set.
func (d pageData) withHost(host string) pageData {
	d.Host = host
	return d
}

// pageBase returns the footer fields for a page of t (nil = unknown host).
// Self-hosted servers never show the footer; in cloud mode it shows for
// unknown hosts and for plans with Branding.
func (s *Server) pageBase(t *Tunnel) pageData {
	d := pageData{Footer: s.cfg.Directory != nil && (t == nil || t.Account.Plan.Branding)}
	if d.Footer {
		host := s.cfg.Domain
		if s.cfg.PublicPort != 0 {
			host = net.JoinHostPort(host, strconv.Itoa(s.cfg.PublicPort))
		}
		d.HomeURL = s.cfg.PublicScheme + "://" + host
	}
	return d
}
