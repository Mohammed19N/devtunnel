package inspector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
)

// Edit overrides parts of a captured request. Nil fields keep the original.
type Edit struct {
	Method *string     `json:"method,omitempty"`
	URI    *string     `json:"uri,omitempty"`    // must start with "/"
	Header http.Header `json:"header,omitempty"` // replaces the whole header when non-nil
	Body   *string     `json:"body,omitempty"`   // UTF-8 text; replaces body
}

var (
	ErrTruncated = errors.New("inspector: captured body was truncated; edit the body to replay")
	ErrNotFound  = errors.New("inspector: no such request")
	// ErrBadEdit wraps problems with an Edit (bad method or URI).
	ErrBadEdit = errors.New("inspector: invalid edit")
)

// Inspector captures traffic passing through Handler and serves the local UI and API.
type Inspector struct {
	store     *Store
	handler   http.Handler
	publicURL atomic.Value // string
	target    atomic.Value // string
}

// New wraps next (the client's local proxy) with Capture. Handler() is what the tunnel serves.
func New(capacity int, next http.Handler) *Inspector {
	s := NewStore(capacity)
	return &Inspector{store: s, handler: Capture(s, next)}
}

// Handler is the capturing handler the tunnel should serve.
func (in *Inspector) Handler() http.Handler { return in.handler }

// Store returns the exchange store.
func (in *Inspector) Store() *Store { return in.store }

// SetPublicURL records the tunnel's public URL (used by cURL export).
func (in *Inspector) SetPublicURL(u string) { in.publicURL.Store(u) }

// SetTarget records the local address the tunnel forwards to (shown in the UI).
func (in *Inspector) SetTarget(t string) { in.target.Store(t) }

func (in *Inspector) base() string {
	u, _ := in.publicURL.Load().(string)
	return u
}

// discardWriter is a ResponseWriter that drops the response.
type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header         { return d.h }
func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardWriter) WriteHeader(int)             {}

// Replay rebuilds request id (with edit applied), runs it through Handler() in-process
// marked as a replay of id, and returns the new exchange's ID. Content-Length is recomputed.
func (in *Inspector) Replay(ctx context.Context, id int64, edit *Edit) (int64, error) {
	orig, ok := in.store.Get(id)
	if !ok {
		return 0, ErrNotFound
	}
	r := orig.Request
	if edit == nil {
		edit = &Edit{}
	}
	if edit.Body == nil && r.Body.Truncated {
		return 0, ErrTruncated
	}
	method, uri := r.Method, r.URI
	if edit.Method != nil {
		method = *edit.Method
	}
	if method == "" {
		return 0, fmt.Errorf("%w: method must not be empty", ErrBadEdit)
	}
	if edit.URI != nil {
		uri = *edit.URI
	}
	if !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") {
		return 0, fmt.Errorf("%w: uri must be a path starting with a single \"/\"", ErrBadEdit)
	}
	if u, err := url.ParseRequestURI(uri); err != nil || u.Host != "" {
		return 0, fmt.Errorf("%w: uri must be a path", ErrBadEdit)
	}
	body := r.Body.Data
	if edit.Body != nil {
		body = []byte(*edit.Body)
	}
	hdr := r.Header.Clone()
	if edit.Header != nil {
		hdr = http.Header{}
		for k, vs := range edit.Header {
			ck := http.CanonicalHeaderKey(k)
			hdr[ck] = append(hdr[ck], vs...)
		}
	}
	if hdr == nil {
		hdr = http.Header{}
	}

	rctx := WithReplayOf(ctx, id)
	var rd *bytes.Reader
	var req *http.Request
	var err error
	if len(body) > 0 {
		rd = bytes.NewReader(body)
		req, err = http.NewRequestWithContext(rctx, method, uri, rd)
	} else {
		req, err = http.NewRequestWithContext(rctx, method, uri, nil)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBadEdit, err)
	}
	req.Host = r.Host
	req.RemoteAddr = "127.0.0.1:0"
	req.Header = hdr
	req.RequestURI = ""
	if len(body) > 0 {
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	} else {
		req.ContentLength = 0
		req.Header.Del("Content-Length")
	}

	in.handler.ServeHTTP(&discardWriter{h: http.Header{}}, req)
	ri := replayInfoFrom(rctx)
	if ri == nil || ri.newID == 0 {
		return 0, errors.New("inspector: replay was not recorded")
	}
	return ri.newID, nil
}
