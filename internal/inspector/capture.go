package inspector

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

type replayKey struct{}

// replayInfo is the context value set by WithReplayOf. Capture copies of into
// Exchange.ReplayOf and, after Store.Add, writes the new exchange ID into newID.
// newID is written before the handler returns, so the caller reads it only after
// ServeHTTP completes.
type replayInfo struct {
	of    int64
	newID int64
}

// WithReplayOf marks requests served with the returned context as replays of exchange id.
func WithReplayOf(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, replayKey{}, &replayInfo{of: id})
}

func replayInfoFrom(ctx context.Context) *replayInfo {
	ri, _ := ctx.Value(replayKey{}).(*replayInfo)
	return ri
}

// capBuffer keeps the first MaxBody bytes written to it and counts all of them.
type capBuffer struct {
	data []byte
	size int64
}

func (c *capBuffer) add(p []byte) {
	c.size += int64(len(p))
	if room := MaxBody - len(c.data); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		c.data = append(c.data, p...)
	}
}

func (c *capBuffer) body() Body {
	return Body{Data: c.data, Size: c.size, Truncated: c.size > int64(len(c.data))}
}

type capReader struct {
	rc  io.ReadCloser
	buf *capBuffer
}

func (r *capReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.buf.add(p[:n])
	}
	return n, err
}

func (r *capReader) Close() error { return r.rc.Close() }

type capWriter struct {
	http.ResponseWriter
	status   int
	header   http.Header
	body     capBuffer
	upgraded bool
}

func (w *capWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *capWriter) WriteHeader(code int) {
	// Informational responses (other than 101) may precede the final one.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status == 0 {
		w.status = code
		w.header = w.ResponseWriter.Header().Clone()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.body.add(p[:n])
	}
	return n, err
}

func (w *capWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *capWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.upgraded = true
		w.status = http.StatusSwitchingProtocols
		if w.header == nil {
			w.header = w.ResponseWriter.Header().Clone()
		}
	}
	return c, rw, err
}

// Capture records every request served by next into s. The request body is teed (capped
// at MaxBody for the record, unbounded for next). The response writer wrapper captures
// status, header snapshot at WriteHeader, and up to MaxBody of body. It implements Unwrap
// so Flush/Hijack reach the real writer. A hijack sets Upgraded=true and Status 101. The
// exchange is added when next returns. The context value set by WithReplayOf is copied
// into ReplayOf.
func Capture(s *Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var reqBuf capBuffer
		if r.Body != nil {
			r.Body = &capReader{rc: r.Body, buf: &reqBuf}
		}
		cw := &capWriter{ResponseWriter: w}
		defer func() {
			status := cw.status
			if status == 0 {
				status = http.StatusOK
			}
			hdr := cw.header
			if hdr == nil {
				hdr = w.Header().Clone()
			}
			e := &Exchange{
				Start:    start,
				Duration: time.Since(start),
				Request: Request{
					Method: r.Method,
					URI:    r.URL.RequestURI(),
					Host:   r.Host,
					Proto:  r.Proto,
					Header: r.Header.Clone(),
					Body:   reqBuf.body(),
				},
				Response: Response{Status: status, Header: hdr, Body: cw.body.body()},
				Upgraded: cw.upgraded,
			}
			ri := replayInfoFrom(r.Context())
			if ri != nil {
				e.ReplayOf = ri.of
			}
			id := s.Add(e)
			if ri != nil {
				ri.newID = id
			}
		}()
		next.ServeHTTP(cw, r)
	})
}
