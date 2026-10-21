package client

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/mohammed19n/passway/internal/protocol"
)

const streamHeaderTimeout = 10 * time.Second

// sessionListener adapts a yamux session into a net.Listener yielding HTTP streams.
type sessionListener struct {
	sess *yamux.Session
	log  *slog.Logger
}

func (l *sessionListener) Accept() (net.Conn, error) {
	for {
		st, err := l.sess.AcceptStream()
		if err != nil {
			return nil, err
		}
		_ = st.SetReadDeadline(time.Now().Add(streamHeaderTimeout))
		var h protocol.StreamHeader
		if err := protocol.ReadFrame(st, &h); err != nil {
			l.log.Debug("reading stream header failed", "err", err)
			st.Close()
			continue
		}
		_ = st.SetReadDeadline(time.Time{})
		if h.Kind != protocol.KindHTTP {
			l.log.Debug("unknown stream kind", "kind", h.Kind)
			st.Close()
			continue
		}
		return st, nil
	}
}

func (l *sessionListener) Close() error   { return l.sess.Close() }
func (l *sessionListener) Addr() net.Addr { return l.sess.LocalAddr() }

func newHTTPServer(cfg Config, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           logRequests(newProxy(cfg, log), cfg.OnRequest),
		ReadHeaderTimeout: 30 * time.Second,
	}
}

func newProxy(cfg Config, log *slog.Logger) http.Handler {
	target := cfg.Target
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			if !cfg.RewriteHost {
				pr.Out.Host = pr.In.Host
			}
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v, ok := pr.In.Header[h]; ok {
					pr.Out.Header[h] = v
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Warn("local target unreachable", "target", target.String(), "err", err)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "Passway couldn't reach %s", target)
		},
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush/Hijack on the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logRequests(next http.Handler, on func(RequestLog)) http.Handler {
	if on == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		on(RequestLog{Method: r.Method, Path: r.URL.Path, Status: sw.status, Duration: time.Since(start)})
	})
}
