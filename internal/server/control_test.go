package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/mohammed19n/passway/internal/protocol"
)

func newTestServer(t *testing.T, auth Authenticator) (*Server, *httptest.Server) {
	t.Helper()
	return newTestServerCfg(t, auth, nil)
}

// newTestServerCfg is newTestServer with a hook to adjust the Config.
func newTestServerCfg(t *testing.T, auth Authenticator, mutate func(*Config)) (*Server, *httptest.Server) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	cfg := Config{
		Domain:       "localhost",
		PublicScheme: "http",
		PublicPort:   port,
		Auth:         auth,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv := New(cfg)
	ts.Config.Handler = srv
	ts.Start()
	t.Cleanup(ts.Close)
	return srv, ts
}

func dialRaw(t *testing.T, ts *httptest.Server, hello protocol.Hello) (protocol.HelloResponse, *yamux.Session) {
	t.Helper()
	u, _ := url.Parse(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://localhost:"+u.Port()+protocol.TunnelPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	ycfg := yamux.DefaultConfig()
	ycfg.LogOutput = io.Discard
	sess, err := yamux.Client(conn, ycfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	st, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(st, hello); err != nil {
		t.Fatal(err)
	}
	var resp protocol.HelloResponse
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.ReadFrame(st, &resp); err != nil {
		t.Fatal(err)
	}
	return resp, sess
}

func hello(name, session string) protocol.Hello {
	return protocol.Hello{Version: protocol.Version, Name: name, SessionID: session + strings.Repeat("-", 16)}
}

func TestHandshakeReady(t *testing.T) {
	_, ts := newTestServer(t, nil)
	resp, _ := dialRaw(t, ts, hello("", "s1"))
	if resp.Ready == nil {
		t.Fatalf("expected ready, got %+v", resp.Error)
	}
	u, _ := url.Parse(ts.URL)
	want := "http://" + resp.Ready.Name + ".localhost:" + u.Port()
	if resp.Ready.URL != want {
		t.Fatalf("url = %q, want %q", resp.Ready.URL, want)
	}
}

func TestHandshakeVersionMismatch(t *testing.T) {
	_, ts := newTestServer(t, nil)
	h := hello("", "s1")
	h.Version = 999
	resp, _ := dialRaw(t, ts, h)
	if resp.Error == nil || resp.Error.Code != protocol.CodeVersionMismatch {
		t.Fatalf("got %+v", resp)
	}
}

func TestHandshakeUnauthorized(t *testing.T) {
	_, ts := newTestServer(t, StaticTokens{"good"})
	h := hello("", "s1")
	h.Token = "bad"
	resp, _ := dialRaw(t, ts, h)
	if resp.Error == nil || resp.Error.Code != protocol.CodeUnauthorized {
		t.Fatalf("got %+v", resp)
	}
	h.Token = "good"
	resp, _ = dialRaw(t, ts, h)
	if resp.Ready == nil {
		t.Fatalf("expected ready, got %+v", resp.Error)
	}
}

func TestHandshakeNameTaken(t *testing.T) {
	_, ts := newTestServer(t, nil)
	resp, _ := dialRaw(t, ts, hello("acme", "s1"))
	if resp.Ready == nil {
		t.Fatalf("first: %+v", resp.Error)
	}
	resp, _ = dialRaw(t, ts, hello("acme", "s2"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeNameTaken {
		t.Fatalf("second: %+v", resp)
	}
}

func TestPreferredNameGrantedOnFreshServer(t *testing.T) {
	_, ts := newTestServer(t, nil)
	h := hello("", "s1")
	h.PreferredName = "quick-otter-42"
	resp, _ := dialRaw(t, ts, h)
	if resp.Ready == nil || resp.Ready.Name != "quick-otter-42" {
		t.Fatalf("got %+v", resp)
	}
}

func TestDisconnectReleases(t *testing.T) {
	_, ts := newTestServer(t, nil)
	resp, sess := dialRaw(t, ts, hello("acme", "A"))
	if resp.Ready == nil {
		t.Fatalf("first: %+v", resp.Error)
	}
	sess.Close()
	deadline := time.Now().Add(time.Second)
	for {
		resp, _ = dialRaw(t, ts, hello("acme", "A"))
		if resp.Ready != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not released: %+v", resp.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReconnectSameSessionWhileActive(t *testing.T) {
	_, ts := newTestServer(t, nil)
	resp, first := dialRaw(t, ts, hello("acme", "A"))
	if resp.Ready == nil {
		t.Fatalf("first: %+v", resp.Error)
	}
	resp, _ = dialRaw(t, ts, hello("acme", "A"))
	if resp.Ready == nil || resp.Ready.Name != "acme" {
		t.Fatalf("second: %+v", resp)
	}
	select {
	case <-first.CloseChan():
	case <-time.After(2 * time.Second):
		t.Fatal("superseded session not closed")
	}
	resp, _ = dialRaw(t, ts, hello("acme", "B"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeNameTaken {
		t.Fatalf("other session: %+v", resp)
	}
}

func TestCloseTunnels(t *testing.T) {
	srv, ts := newTestServer(t, nil)
	_, sess := dialRaw(t, ts, hello("acme", "A"))
	srv.CloseTunnels()
	select {
	case <-sess.CloseChan():
	case <-time.After(2 * time.Second):
		t.Fatal("client session not closed")
	}
	resp, _ := dialRaw(t, ts, hello("other", "B"))
	if resp.Ready == nil {
		t.Fatalf("server should still accept: %+v", resp.Error)
	}
}

func TestHealthz(t *testing.T) {
	_, ts := newTestServer(t, nil)
	u, _ := url.Parse(ts.URL)
	req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
	req.Host = "localhost:" + u.Port()
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || string(b) != "ok" {
		t.Fatalf("got %d %q", r.StatusCode, b)
	}
}

func TestHandshakeShortSessionID(t *testing.T) {
	_, ts := newTestServer(t, nil)
	resp, _ := dialRaw(t, ts, protocol.Hello{Version: protocol.Version, Name: "acme", SessionID: "short"})
	if resp.Error == nil || resp.Error.Code != protocol.CodeInternal || resp.Error.Message != "invalid session id" {
		t.Fatalf("got %+v", resp)
	}
}

func TestClientOpenedStreamsRejected(t *testing.T) {
	_, ts := newTestServer(t, nil)
	resp, sess := dialRaw(t, ts, hello("acme", "A"))
	if resp.Ready == nil {
		t.Fatalf("expected ready, got %+v", resp.Error)
	}
	st, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = st.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := st.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
		t.Fatalf("read err = %v, want stream closed by server", err)
	}
}

// streamListener adapts a client-side yamux session into a net.Listener that
// consumes the per-stream header the server writes first.
type streamListener struct{ sess *yamux.Session }

func (l streamListener) Accept() (net.Conn, error) {
	for {
		st, err := l.sess.AcceptStream()
		if err != nil {
			return nil, err
		}
		var h protocol.StreamHeader
		if err := protocol.ReadFrame(st, &h); err != nil {
			st.Close()
			continue
		}
		return st, nil
	}
}
func (l streamListener) Close() error   { return l.sess.Close() }
func (l streamListener) Addr() net.Addr { return l.sess.Addr() }

func TestTunnelConnCap(t *testing.T) {
	_, ts := newTestServerCfg(t, nil, func(c *Config) { c.MaxConnsPerTunnel = 2 })
	resp, sess := dialRaw(t, ts, hello("capped", "A"))
	if resp.Ready == nil {
		t.Fatalf("ready: %+v", resp.Error)
	}
	var inflight, max atomic.Int32
	release := make(chan struct{})
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			m := max.Load()
			if n <= m || max.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		w.WriteHeader(200)
	})}
	go hs.Serve(streamListener{sess})
	t.Cleanup(func() { hs.Close() })

	u, _ := url.Parse(ts.URL)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", ts.URL+"/", nil)
			req.Host = "capped.localhost:" + u.Port()
			r, err := client.Do(req)
			if err != nil {
				return
			}
			r.Body.Close()
			codes[i] = r.StatusCode
		}()
	}
	deadline := time.Now().Add(3 * time.Second)
	for inflight.Load() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("in-flight = %d, want 2", inflight.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := max.Load(); got != 2 {
		t.Fatalf("max in-flight = %d, want 2", got)
	}
	close(release)
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Errorf("request %d: status %d, want 200", i, c)
		}
	}
}

// The client never accepts streams, so yamux SYNs are never ACKed. After the
// server's accept backlog (16) fills, session.Open blocks; DialContext must
// still honour the request context.
func TestDialHonoursContext(t *testing.T) {
	_, ts := newTestServerCfg(t, nil, func(c *Config) { c.MaxConnsPerTunnel = 64 })
	resp, _ := dialRaw(t, ts, hello("stuck", "A"))
	if resp.Ready == nil {
		t.Fatalf("ready: %+v", resp.Error)
	}
	u, _ := url.Parse(ts.URL)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 300 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest("GET", ts.URL+"/", nil)
				req.Host = "stuck.localhost:" + u.Port()
				if r, err := client.Do(req); err == nil {
					r.Body.Close()
				}
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("requests did not return within 2s: DialContext ignores ctx")
	}
}

// The HTTP-level test above cannot fail on its own (the client timeout fires
// regardless), so this one pins DialContext directly: with the accept backlog
// full, session.Open blocks, and DialContext must return when ctx expires.
func TestDialContextReturnsOnCancelWhenOpenBlocks(t *testing.T) {
	srv, ts := newTestServerCfg(t, nil, nil)
	resp, _ := dialRaw(t, ts, hello("stuck", "A"))
	if resp.Ready == nil {
		t.Fatalf("ready: %+v", resp.Error)
	}
	tun, ok := srv.registry.Lookup("stuck")
	if !ok {
		t.Fatal("tunnel not registered")
	}
	// Exhaust the unacknowledged-stream backlog; the client never accepts.
	go func() {
		for i := 0; i < 64; i++ {
			if _, err := tun.session.Open(); err != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c, err := tun.transport.DialContext(ctx, "tcp", "x:80")
		if c != nil {
			c.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dial succeeded unexpectedly")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DialContext did not return after ctx expired")
	}
}

func TestEdgeRateLimited(t *testing.T) {
	_, ts := newTestServerCfg(t, nil, func(c *Config) { c.RateLimit = 2 })
	resp, sess := dialRaw(t, ts, hello("limited", "A"))
	if resp.Ready == nil {
		t.Fatalf("ready: %+v", resp.Error)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})}
	go hs.Serve(streamListener{sess})
	t.Cleanup(func() { hs.Close() })

	u, _ := url.Parse(ts.URL)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
	var last *http.Response
	for i, want := range []int{200, 200, 429} {
		req, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req.Host = "limited.localhost:" + u.Port()
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("request %d: status %d, want %d", i, r.StatusCode, want)
		}
		last = r
	}
	n, err := strconv.Atoi(last.Header.Get("Retry-After"))
	if err != nil || n < 1 {
		t.Fatalf("Retry-After = %q", last.Header.Get("Retry-After"))
	}
}

type errAuth struct{ err error }

func (a errAuth) Authenticate(context.Context, string) (Account, error) { return Account{}, a.err }

func TestHandshakeAuthErrors(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		code, msg string
		fatal     bool
	}{
		{"banned", &protocol.Error{Code: protocol.CodeBanned, Message: "account suspended"}, protocol.CodeBanned, "account suspended", true},
		{"invalid", ErrInvalidToken, protocol.CodeUnauthorized, "invalid token", true},
		{"wrapped invalid", fmt.Errorf("lookup: %w", ErrInvalidToken), protocol.CodeUnauthorized, "invalid token", true},
		{"transient", errors.New("database is locked"), protocol.CodeInternal, "temporary server error; retrying", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newTestServer(t, errAuth{tc.err})
			resp, _ := dialRaw(t, ts, hello("", "s1"))
			if resp.Error == nil || resp.Error.Code != tc.code || resp.Error.Message != tc.msg {
				t.Fatalf("got %+v", resp)
			}
			if protocol.IsFatal(resp.Error) != tc.fatal {
				t.Fatalf("IsFatal = %v, want %v", !tc.fatal, tc.fatal)
			}
		})
	}
}

func TestNewWarnsWhenDirectoryAndAuthBothSet(t *testing.T) {
	var buf strings.Builder
	New(Config{Domain: "x", Auth: StaticTokens{"a"}, Directory: fakeDir{}, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	if !strings.Contains(buf.String(), "ignored") {
		t.Fatalf("no warning: %q", buf.String())
	}
}

type fakeDir struct{ errAuth }

func (fakeDir) Names(context.Context, Account) ([]string, error)        { return nil, nil }
func (fakeDir) Owns(context.Context, Account, string) (bool, error)     { return false, nil }
func (fakeDir) Banned(context.Context, Account, string) (bool, error)   { return false, nil }
func (fakeDir) AddUsage(context.Context, Account, int64) error          { return nil }
func (fakeDir) Usage(context.Context, Account) (int64, error)           { return 0, nil }
func (fakeDir) PlanUsage(context.Context, string) (int64, error)        { return 0, nil }
func (fakeDir) BannedLists(context.Context) ([]string, []string, error) { return nil, nil, nil }
