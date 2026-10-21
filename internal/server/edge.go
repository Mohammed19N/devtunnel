package server

import (
	"context"
	_ "embed"
	"html/template"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/protocol"
)

//go:embed offline.html
var offlineHTML string

var offlineTmpl = template.Must(template.New("offline").Parse(offlineHTML))

func writeOffline(w http.ResponseWriter, host string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_ = offlineTmpl.Execute(w, struct{ Host string }{host})
}

// newTunnelProxy builds t's reverse proxy and transport. t.Name is read at
// request time; Registry.Claim sets it before the tunnel becomes visible.
func newTunnelProxy(t *Tunnel) {
	t.transport = &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			st, err := t.session.Open()
			if err != nil {
				return nil, err
			}
			// The client blocks in Accept until it reads this header.
			if err := protocol.WriteFrame(st, protocol.StreamHeader{Kind: protocol.KindHTTP}); err != nil {
				st.Close()
				return nil, err
			}
			return st, nil
		},
		MaxIdleConnsPerHost: 16,
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
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			writeOffline(w, r.Host)
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
	t, ok := s.registry.Lookup(label)
	if !ok {
		writeOffline(w, r.Host)
		return
	}
	t.proxy.ServeHTTP(w, r)
}
