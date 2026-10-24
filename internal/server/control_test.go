package server

import (
	"bytes"
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
	"slices"
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

func TestSelfHostedBlockWordsOff(t *testing.T) {
	_, ts := newTestServer(t, nil)
	resp, _ := dialRaw(t, ts, hello("paypal-test", "s1"))
	if resp.Ready == nil || resp.Ready.Name != "paypal-test" {
		t.Fatalf("got %+v", resp)
	}
}

func TestBlockWordsOnRefuses(t *testing.T) {
	_, ts := newTestServerCfg(t, nil, func(c *Config) { c.BlockWords = true })
	resp, _ := dialRaw(t, ts, hello("paypal-test", "s1"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeInvalidName {
		t.Fatalf("got %+v", resp)
	}
	resp, _ = dialRaw(t, ts, hello("acme", "s2"))
	if resp.Ready == nil {
		t.Fatalf("ordinary name refused: %+v", resp.Error)
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
	// The cap answers a fast 503 instead of queueing: two get through.
	var ok, busy int
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 503:
			busy++
		}
	}
	if ok != 2 || busy != 3 {
		t.Errorf("statuses = %v, want two 200 and three 503", codes)
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

func (fakeDir) Names(context.Context, Account) ([]string, error)    { return nil, nil }
func (fakeDir) Owns(context.Context, Account, string) (bool, error) { return false, nil }
func (fakeDir) OwnsDomain(context.Context, Account, string) (bool, error) {
	return false, nil
}
func (fakeDir) Banned(context.Context, Account, string) (bool, error)   { return false, nil }
func (fakeDir) AddUsage(context.Context, Account, int64) error          { return nil }
func (fakeDir) Usage(context.Context, Account) (int64, error)           { return 0, nil }
func (fakeDir) PlanUsage(context.Context, string) (int64, error)        { return 0, nil }
func (fakeDir) BannedLists(context.Context) ([]string, []string, error) { return nil, nil, nil }
func (fakeDir) RevokedTokens(context.Context, []int64) ([]int64, error) { return nil, nil }

// fakeDirectory is an in-memory Directory for handshake tests.
type fakeDirectory struct {
	mu       sync.Mutex
	acc      Account
	names    []string
	domains  []string        // verified custom domains OwnsDomain reports
	banned   map[string]bool // "" = the account itself
	namesErr error
}

func newFakeDirectory(plan Plan, names ...string) *fakeDirectory {
	return &fakeDirectory{acc: Account{ID: "u1", Login: "alice", Plan: plan}, names: names, banned: map[string]bool{}}
}

func (d *fakeDirectory) Authenticate(_ context.Context, token string) (Account, error) {
	if token == "bad" {
		return Account{}, ErrInvalidToken
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.acc, nil
}
func (d *fakeDirectory) Names(context.Context, Account) ([]string, error) {
	return d.names, d.namesErr
}
func (d *fakeDirectory) Owns(_ context.Context, _ Account, name string) (bool, error) {
	for _, n := range d.names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}
func (d *fakeDirectory) OwnsDomain(_ context.Context, _ Account, host string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.domains, host), nil
}
func (d *fakeDirectory) Banned(_ context.Context, _ Account, label string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.banned[label], nil
}
func (d *fakeDirectory) AddUsage(context.Context, Account, int64) error   { return nil }
func (d *fakeDirectory) Usage(context.Context, Account) (int64, error)    { return 0, nil }
func (d *fakeDirectory) PlanUsage(context.Context, string) (int64, error) { return 0, nil }
func (d *fakeDirectory) BannedLists(context.Context) ([]string, []string, error) {
	return nil, nil, nil
}
func (d *fakeDirectory) RevokedTokens(context.Context, []int64) ([]int64, error) { return nil, nil }

func newCloudServer(t *testing.T, d *fakeDirectory) (*Server, *httptest.Server) {
	return newTestServerCfg(t, nil, func(c *Config) { c.Directory = d })
}

func TestCloudDefaultName(t *testing.T) {
	_, ts := newCloudServer(t, newFakeDirectory(PlanFree, "quick-otter-42", "acme"))
	resp, _ := dialRaw(t, ts, hello("", "s1"))
	if resp.Ready == nil || resp.Ready.Name != "quick-otter-42" {
		t.Fatalf("got %+v / %+v", resp.Ready, resp.Error)
	}
}

func TestCloudNameNotOwned(t *testing.T) {
	_, ts := newCloudServer(t, newFakeDirectory(PlanFree, "quick-otter-42", "acme"))
	resp, _ := dialRaw(t, ts, hello("other", "s1"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeNameNotOwned || resp.Error.Message != `you don't own "other"; claim it in the dashboard` {
		t.Fatalf("got %+v", resp.Error)
	}
	resp, _ = dialRaw(t, ts, hello("acme", "s1"))
	if resp.Ready == nil || resp.Ready.Name != "acme" {
		t.Fatalf("owned name: %+v", resp.Error)
	}
}

func TestCloudTunnelLimit(t *testing.T) {
	_, ts := newCloudServer(t, newFakeDirectory(PlanFree, "quick-otter-42", "acme"))
	resp, _ := dialRaw(t, ts, hello("", "s1"))
	if resp.Ready == nil {
		t.Fatalf("first: %+v", resp.Error)
	}
	resp, _ = dialRaw(t, ts, hello("acme", "s2"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeLimitReached ||
		resp.Error.Message != "tunnel limit reached: your Free plan allows 1 concurrent tunnel(s)" {
		t.Fatalf("second: %+v", resp)
	}
}

func TestCloudReconnectSameSessionNotLimited(t *testing.T) {
	_, ts := newCloudServer(t, newFakeDirectory(PlanFree, "quick-otter-42"))
	resp, _ := dialRaw(t, ts, hello("", "s1"))
	if resp.Ready == nil {
		t.Fatalf("first: %+v", resp.Error)
	}
	resp, _ = dialRaw(t, ts, hello("", "s1"))
	if resp.Ready == nil || resp.Ready.Name != "quick-otter-42" {
		t.Fatalf("takeover: %+v", resp.Error)
	}
}

func TestCloudConcurrentClaimsRespectLimit(t *testing.T) {
	names := []string{"nm0", "nm1", "nm2", "nm3", "nm4", "nm5", "nm6", "nm7", "nm8", "nm9"}
	_, ts := newCloudServer(t, newFakeDirectory(PlanFree, names...))
	var wg sync.WaitGroup
	var ready, limited atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := dialRaw(t, ts, hello("", "sess"+strconv.Itoa(i)))
			switch {
			case resp.Ready != nil:
				ready.Add(1)
			case resp.Error != nil && resp.Error.Code == protocol.CodeLimitReached:
				limited.Add(1)
			default:
				t.Errorf("unexpected %+v", resp.Error)
			}
		}()
	}
	wg.Wait()
	if ready.Load() != 1 || limited.Load() != 9 {
		t.Fatalf("ready=%d limited=%d, want 1 and 9", ready.Load(), limited.Load())
	}
}

func TestCloudAllNamesBusy(t *testing.T) {
	d := newFakeDirectory(PlanPro, "one", "two")
	_, ts := newCloudServer(t, d)
	for i, n := range []string{"one", "two"} {
		resp, _ := dialRaw(t, ts, hello(n, "s"+strconv.Itoa(i)))
		if resp.Ready == nil {
			t.Fatalf("%s: %+v", n, resp.Error)
		}
	}
	resp, _ := dialRaw(t, ts, hello("", "s9"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeNameTaken ||
		resp.Error.Message != "all your subdomains are in use; use --name or upgrade" {
		t.Fatalf("got %+v", resp.Error)
	}
}

func TestCloudBanned(t *testing.T) {
	d := newFakeDirectory(PlanFree, "bad-host", "good-host")
	d.banned[""] = true
	_, ts := newCloudServer(t, d)
	resp, _ := dialRaw(t, ts, hello("", "s1"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeBanned || resp.Error.Message != "account suspended" {
		t.Fatalf("account ban: %+v", resp)
	}
	d.banned[""] = false
	d.banned["bad-host"] = true
	resp, _ = dialRaw(t, ts, hello("bad-host", "s1"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeBanned {
		t.Fatalf("explicit banned label: %+v", resp)
	}
	resp, _ = dialRaw(t, ts, hello("", "s1"))
	if resp.Ready == nil || resp.Ready.Name != "good-host" {
		t.Fatalf("default should skip banned label: %+v", resp.Error)
	}
}

func TestCloudReadyPlan(t *testing.T) {
	_, ts := newCloudServer(t, newFakeDirectory(PlanFree, "quick-otter-42"))
	resp, _ := dialRaw(t, ts, hello("", "s1"))
	if resp.Ready == nil || resp.Ready.Plan != "free" || resp.Ready.RateLimit != 60 || resp.Ready.Bandwidth != 1<<30 {
		t.Fatalf("got %+v / %+v", resp.Ready, resp.Error)
	}
}

func TestCloudTunnelsFor(t *testing.T) {
	srv, ts := newCloudServer(t, newFakeDirectory(PlanFree, "quick-otter-42"))
	h := hello("", "s1")
	h.ClientVersion = "1.2.3"
	if resp, _ := dialRaw(t, ts, h); resp.Ready == nil {
		t.Fatalf("%+v", resp.Error)
	}
	got := srv.TunnelsFor("u1")
	if len(got) != 1 || got[0].ClientVersion != "1.2.3" || got[0].ConnectedAt.IsZero() || got[0].URL != srv.PublicURL("quick-otter-42") {
		t.Fatalf("got %+v", got)
	}
	if len(srv.TunnelsFor("other")) != 0 {
		t.Fatal("other account should see none")
	}
}

func TestSelfHostedUnchanged(t *testing.T) {
	_, ts := newTestServer(t, nil)
	for i := 0; i < 3; i++ {
		resp, _ := dialRaw(t, ts, hello("", "s"+strconv.Itoa(i)))
		if resp.Ready == nil || resp.Ready.Plan != "" || resp.Ready.RateLimit != 0 || resp.Ready.Bandwidth != 0 {
			t.Fatalf("got %+v / %+v", resp.Ready, resp.Error)
		}
	}
}

func TestCloudPreferredName(t *testing.T) {
	_, ts := newCloudServer(t, newFakeDirectory(PlanPro, "first-one", "second-one"))
	h := hello("", "s1")
	h.PreferredName = "second-one"
	resp, _ := dialRaw(t, ts, h)
	if resp.Ready == nil || resp.Ready.Name != "second-one" {
		t.Fatalf("owned preferred: %+v", resp)
	}
	h = hello("", "s2")
	h.PreferredName = "not-mine"
	resp, _ = dialRaw(t, ts, h)
	if resp.Ready == nil || resp.Ready.Name != "first-one" {
		t.Fatalf("unowned preferred: %+v", resp)
	}
}

func TestCloudDirectoryErrorIsRetryable(t *testing.T) {
	d := newFakeDirectory(PlanFree, "quick-otter-42")
	d.namesErr = errors.New("db down")
	_, ts := newCloudServer(t, d)
	resp, _ := dialRaw(t, ts, hello("", "s1"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeInternal || protocol.IsFatal(resp.Error) {
		t.Fatalf("got %+v", resp.Error)
	}
}

// The tunnel up/down log lines name the account, for takedown requests.
func TestTunnelLogsNameAccount(t *testing.T) {
	var buf bytes.Buffer
	lw := lockedWriter{w: &buf, mu: &sync.Mutex{}}
	logged := func(msg string) bool {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "msg=\""+msg+"\"") && strings.Contains(line, "account=u1") && strings.Contains(line, "login=alice") {
				return true
			}
		}
		return false
	}
	_, ts := newTestServerCfg(t, nil, func(c *Config) {
		c.Directory = newFakeDirectory(PlanFree, "quick")
		c.Logger = slog.New(slog.NewTextHandler(lw, nil))
	})
	resp, sess := dialRaw(t, ts, hello("", "s1"))
	if resp.Ready == nil {
		t.Fatalf("ready: %+v", resp.Error)
	}
	sess.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !(logged("tunnel up") && logged("tunnel down")) {
		if time.Now().After(deadline) {
			lw.mu.Lock()
			defer lw.mu.Unlock()
			t.Fatalf("tunnel up/down lines lack account and login:\n%s", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitClosed(t *testing.T, sess *yamux.Session, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !sess.IsClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !sess.IsClosed() {
		t.Fatalf("%s: session not closed", what)
	}
}

func TestCloseTokenTunnels(t *testing.T) {
	d := newFakeDirectory(PlanPro, "app-one", "app-two")
	srv, ts := newCloudServer(t, d)
	setToken := func(id int64) {
		d.mu.Lock()
		d.acc.TokenID = id
		d.mu.Unlock()
	}
	setToken(7)
	if resp, _ := dialRaw(t, ts, hello("app-one", "s1")); resp.Ready == nil {
		t.Fatalf("a1: %+v", resp.Error)
	}
	s1 := srv.registry.ByAccount("u1")[0].session
	setToken(8)
	resp, s2 := dialRaw(t, ts, hello("app-two", "s2"))
	if resp.Ready == nil {
		t.Fatalf("a2: %+v", resp.Error)
	}
	srv.CloseTokenTunnels("other", 7) // another account's token 7: no effect
	srv.CloseTokenTunnels("u1", 7)
	waitClosed(t, s1, "token 7 tunnel")
	time.Sleep(50 * time.Millisecond)
	if s2.IsClosed() {
		t.Fatal("tunnel of another token was closed")
	}
}

func domainHello(domain, session string) protocol.Hello {
	h := hello("", session)
	h.Domain = domain
	return h
}

func TestHandshakeCustomDomain(t *testing.T) {
	d := newFakeDirectory(PlanPro, "quick-otter-42")
	d.domains = []string{"hooks.example.com"}
	srv, ts := newCloudServer(t, d)
	resp, _ := dialRaw(t, ts, domainHello("Hooks.Example.com.", "s1"))
	if resp.Ready == nil {
		t.Fatalf("got %+v", resp.Error)
	}
	u, _ := url.Parse(ts.URL)
	if want := "http://hooks.example.com:" + u.Port(); resp.Ready.URL != want || resp.Ready.Name != "hooks.example.com" {
		t.Fatalf("ready = %+v, want URL %q", resp.Ready, want)
	}
	if tl := srv.TunnelsFor("u1"); len(tl) != 1 || tl[0].URL != "http://hooks.example.com:"+u.Port() {
		t.Fatalf("TunnelsFor = %+v", tl)
	}
	// A second session cannot take the same host while it is active.
	resp, _ = dialRaw(t, ts, domainHello("hooks.example.com", "s2"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeNameTaken {
		t.Fatalf("second session: %+v", resp)
	}
}

func TestHandshakeDomainNotOwned(t *testing.T) {
	d := newFakeDirectory(PlanPro, "quick-otter-42")
	d.domains = []string{"mine.example.com", "evil.localhost"}
	_, ts := newCloudServer(t, d)
	resp, _ := dialRaw(t, ts, domainHello("hooks.example.com", "s1"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeNameNotOwned ||
		resp.Error.Message != `you don't own "hooks.example.com" or it isn't verified; add it in the dashboard` {
		t.Fatalf("got %+v", resp)
	}
	// A host under the base domain is never a custom domain, whatever the directory says.
	resp, _ = dialRaw(t, ts, domainHello("evil.localhost", "s1"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeInvalidName {
		t.Fatalf("base-domain host: %+v", resp)
	}
}

func TestHandshakeDomainAndName(t *testing.T) {
	d := newFakeDirectory(PlanPro, "acme")
	d.domains = []string{"hooks.example.com"}
	_, ts := newCloudServer(t, d)
	h := domainHello("hooks.example.com", "s1")
	h.Name = "acme"
	resp, _ := dialRaw(t, ts, h)
	if resp.Error == nil || resp.Error.Code != protocol.CodeInvalidName || resp.Error.Message != "use either --name or --domain" {
		t.Fatalf("got %+v", resp)
	}
}

func TestHandshakeDomainSelfHosted(t *testing.T) {
	_, ts := newTestServer(t, nil)
	resp, _ := dialRaw(t, ts, domainHello("hooks.example.com", "s1"))
	if resp.Error == nil || resp.Error.Code != protocol.CodeInvalidName || resp.Error.Message != "custom domains need cloud mode" {
		t.Fatalf("got %+v", resp)
	}
}
