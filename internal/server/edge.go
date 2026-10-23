package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/protocol"
)

// newTunnelProxy builds t's reverse proxy and transport. t.Name is read at
// request time; Registry.Claim sets it before the tunnel becomes visible.
func newTunnelProxy(t *Tunnel, maxConns int, log *slog.Logger) {
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
				renderPage(w, r, pageBandwidth, pageData{Host: r.Host})
				return
			}
			if !errors.Is(err, context.Canceled) {
				log.Warn("proxy error", "name", t.Name, "err", err)
			}
			renderPage(w, r, pageOffline, pageData{Host: r.Host})
		},
	}
}

// serveEdge routes a request whose host is not the base domain.
func (s *Server) serveEdge(w http.ResponseWriter, r *http.Request, host string) {
	label, ok := strings.CutSuffix(host, "."+s.cfg.Domain)
	if !ok || label == "" || strings.Contains(label, ".") {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
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
	// A banned host is refused even after its session is gone.
	if cloud && s.bans.Load().banned(Account{}, label) {
		renderPage(w, r, pageBanned, pageData{Host: r.Host})
		return
	}
	t, ok := s.registry.Lookup(label)
	if !ok {
		renderPage(w, r, pageOffline, pageData{Host: r.Host})
		return
	}
	acc := t.Account
	warn, rate := s.cfg.BrowserWarning, s.cfg.RateLimit
	if cloud {
		if s.bans.Load().banned(acc, t.Name) {
			renderPage(w, r, pageBanned, pageData{Host: r.Host})
			return
		}
		warn = acc.Plan.Warning || (acc.Plan.Name == PlanUnlimited.Name && s.cfg.BrowserWarning)
		if acc.Plan.RateLimit > 0 {
			rate = acc.Plan.RateLimit
		}
	}
	if warn && needsWarning(r, s.cfg.CookieSecret) {
		d := pageData{Host: r.Host, Continue: continuePath + "?to=" + url.QueryEscape(r.URL.RequestURI())}
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
		renderPage(w, r, pageBandwidth, pageData{Host: r.Host})
		return
	}
	if rate > 0 && s.limiter != nil {
		if ok, retry := s.limiter.AllowN(t.Name, rate); !ok {
			secs := int((retry + time.Second - 1) / time.Second)
			renderPage(w, r, pageRateLimited, pageData{Host: r.Host, RetryIn: max(secs, 1)})
			return
		}
	}
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
