package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mohammed19n/passway/internal/client"
	"github.com/mohammed19n/passway/internal/inspector"
	"github.com/mohammed19n/passway/internal/protocol"
	"github.com/mohammed19n/passway/internal/server"
	"github.com/mohammed19n/passway/internal/server/billing"
	"github.com/mohammed19n/passway/internal/server/dashboard"
	"github.com/mohammed19n/passway/internal/server/store"
)

type opts struct {
	handler     http.Handler // local app; default 404
	rewriteHost bool
	tls         bool
	target      *url.URL // overrides the local app's URL (e.g. a closed port)

	inspector      bool // wrap the client's handler with an inspector
	rateLimit      int  // server requests/minute per tunnel; 0 = off
	browserWarning bool // server shows the interstitial to browser GETs
	cloud          bool // cloud mode: store, dashboard and account directory; no auto-started tunnel

	billing  *fakeStripe        // cloud only: enables billing against this fake Stripe
	resolver dashboard.Resolver // cloud only: answers the custom-domain DNS checks
}

type env struct {
	port       int
	publicHost string
	local      *httptest.Server
	httpc      *http.Client
	scheme     string
	insp       *inspector.Inspector // nil unless opts.inspector
	store      *store.Store         // cloud only
	base       string               // the base-domain URL, http://localhost:<port>
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func setup(t *testing.T, o opts) (publicGet func(path string) *http.Response, publicHost string, srv *server.Server) {
	e, srv := setupEnv(t, o)
	return func(path string) *http.Response {
		t.Helper()
		resp, err := e.do(context.Background(), "GET", path, e.publicHost, nil)
		if err != nil {
			t.Error(err)
			return &http.Response{StatusCode: 0, Body: io.NopCloser(strings.NewReader(""))}
		}
		return resp
	}, e.publicHost, srv
}

func setupEnv(t *testing.T, o opts) (*env, *server.Server) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	scheme := "http"
	if o.tls {
		scheme = "https"
	}
	cfg := server.Config{Domain: "localhost", PublicScheme: scheme, PublicPort: port, Logger: discard,
		RateLimit: o.rateLimit, BrowserWarning: o.browserWarning}
	var st *store.Store
	var srv *server.Server // assigned below; the dashboard only reads it per request
	baseURL := fmt.Sprintf("%s://localhost:%d", scheme, port)
	if o.cloud {
		var err error
		if st, err = store.Open(filepath.Join(t.TempDir(), "p.db")); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		// Mirrors the cloud wiring in cmd/passway-server.
		dcfg := dashboard.Config{Store: st, BaseURL: baseURL, DevLogin: true, Logger: discard,
			Tunnels: func(id string) []dashboard.TunnelInfo {
				var out []dashboard.TunnelInfo
				for _, tn := range srv.TunnelsFor(id) {
					out = append(out, dashboard.TunnelInfo{URL: tn.URL, ClientVersion: tn.ClientVersion, ConnectedAt: tn.ConnectedAt, Requests: tn.Requests})
				}
				return out
			},
			Disconnect:    func(id string, token int64) { srv.CloseTokenTunnels(id, token) },
			PlanChanged:   func(id int64) { srv.CloseAccountTunnels(strconv.FormatInt(id, 10)) },
			Resolver:      o.resolver,
			DomainRemoved: func(host string, id int64) { srv.CloseHostTunnelsOf(host, strconv.FormatInt(id, 10)) },
			DomainVerified: func(host string, id int64) {
				srv.CloseHostTunnelsExcept(host, strconv.FormatInt(id, 10))
			},
		}
		if o.billing != nil {
			dcfg.Billing = &billing.Client{Key: "sk_test_e2e", BaseURL: o.billing.srv.URL}
			dcfg.ProPrice = billing.Price{ID: "price_e2e", Currency: "usd", Interval: "month", UnitAmount: 700}
			dcfg.PriceID = "price_e2e"
			dcfg.WebhookSecret = o.billing.secret
		}
		dash, err := dashboard.New(dcfg)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Directory = &store.Directory{S: st}
		cfg.Dashboard = dash
		cfg.BlockWords = true
	}
	srv = server.New(cfg)
	ts.Config.Handler = srv
	if o.tls {
		ts.StartTLS()
	} else {
		ts.Start()
	}
	t.Cleanup(ts.Close)

	h := o.handler
	if h == nil {
		h = http.NotFoundHandler()
	}
	local := httptest.NewServer(h)
	t.Cleanup(local.Close)
	target, _ := url.Parse(local.URL)
	if o.target != nil {
		target = o.target
	}

	var insp *inspector.Inspector
	var handler http.Handler // nil = the client's default local proxy
	if o.inspector {
		insp = inspector.New(inspector.DefaultCapacity, client.NewProxy(target, o.rewriteHost, discard))
		handler = insp.Handler()
	}

	if o.cloud { // tests start their own clients with account tokens
		e := &env{port: port, scheme: scheme, local: local, store: st, base: baseURL}
		e.httpc = &http.Client{Timeout: 30 * time.Second, Transport: e.pin(http.DefaultTransport, false),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		t.Cleanup(e.httpc.CloseIdleConnections)
		return e, srv
	}

	ready := make(chan protocol.Ready, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = client.Run(ctx, client.Config{
			Server:      fmt.Sprintf("%s://localhost:%d", scheme, port),
			Target:      target,
			RewriteHost: o.rewriteHost,
			Insecure:    o.tls,
			Handler:     handler,
			Logger:      discard,
			OnReady:     func(r protocol.Ready) { ready <- r },
		})
	}()
	t.Cleanup(func() { cancel(); <-done })

	var r protocol.Ready
	select {
	case r = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("tunnel never became ready")
	}
	e := &env{port: port, scheme: scheme, publicHost: fmt.Sprintf("%s.localhost:%d", r.Name, port), local: local, insp: insp, base: baseURL}
	base := http.DefaultTransport
	if o.tls {
		base = ts.Client().Transport
	}
	e.httpc = &http.Client{
		Timeout:       30 * time.Second,
		Transport:     e.pin(base, o.tls),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t.Cleanup(e.httpc.CloseIdleConnections)
	return e, srv
}

// pin returns a transport that dials 127.0.0.1:<port> regardless of URL host.
func (e *env) pin(rt http.RoundTripper, isTLS bool) http.RoundTripper {
	tr := rt.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", e.port))
	}
	if isTLS {
		tr.TLSClientConfig.ServerName = "127.0.0.1"
	}
	return tr
}

func (e *env) do(ctx context.Context, method, path, host string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("%s://127.0.0.1:%d%s", e.scheme, e.port, path), body)
	if err != nil {
		return nil, err
	}
	req.Host = host
	return e.httpc.Do(req)
}

// recvTimeout receives from ch or fails the test after d.
func recvTimeout[T any](t *testing.T, ch <-chan T, d time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatal("timed out waiting on channel")
		panic("unreachable")
	}
}

// waitStoreLen polls until the store holds exactly n exchanges.
func waitStoreLen(t *testing.T, s *inspector.Store, n int) []inspector.Summary {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := s.List()
		if len(list) == n {
			return list
		}
		if len(list) > n || time.Now().After(deadline) {
			t.Fatalf("store has %d exchanges, want %d", len(list), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestForwardGET(t *testing.T) {
	get, _, _ := setup(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "1")
		fmt.Fprintf(w, "hello %s", r.URL.Query().Get("name"))
	})})
	resp := get("/hello?name=x")
	defer resp.Body.Close()
	if resp.StatusCode != 200 || readAll(t, resp.Body) != "hello x" || resp.Header.Get("X-Test") != "1" {
		t.Fatalf("unexpected response: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestForwardLargePOST(t *testing.T) {
	e, _ := setupEnv(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, r.Body)
	})})
	payload := make([]byte, 5<<20)
	_, _ = rand.Read(payload)
	resp, err := e.do(context.Background(), "POST", "/echo", e.publicHost, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := readAll(t, resp.Body)
	if sha256.Sum256([]byte(got)) != sha256.Sum256(payload) {
		t.Fatalf("echo mismatch (got %d bytes)", len(got))
	}
}

func TestForwardStreaming(t *testing.T) {
	get, _, _ := setup(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte("b"))
	})})
	resp := get("/")
	defer resp.Body.Close()
	start := time.Now()
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || buf[0] != 'a' {
		t.Fatalf("first read: %v %q", err, buf)
	}
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Fatalf("first chunk took %v, not streamed", d)
	}
	if rest := readAll(t, resp.Body); rest != "b" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestForwardWebSocket(t *testing.T) {
	e, _ := setupEnv(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if c.Write(r.Context(), typ, b) != nil {
				return
			}
		}
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+e.publicHost+"/ws", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: e.pin(http.DefaultTransport, false)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, b, err := c.Read(ctx)
	if err != nil || string(b) != "ping" {
		t.Fatalf("got %q, %v", b, err)
	}
}

func TestForwardedHeaders(t *testing.T) {
	type seen struct{ xff, xfh, xfp, host string }
	ch := make(chan seen, 1)
	get, publicHost, _ := setup(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- seen{r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-Proto"), r.Host}
	})})
	get("/").Body.Close()
	s := recvTimeout(t, ch, 5*time.Second)
	if !strings.HasPrefix(s.xff, "127.0.0.1") || s.xfh != publicHost || s.xfp != "http" || s.host != publicHost {
		t.Fatalf("unexpected headers: %+v (want host %s)", s, publicHost)
	}
}

func TestForgedForwardedHeadersIgnored(t *testing.T) {
	ch := make(chan http.Header, 1)
	e, _ := setupEnv(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- r.Header.Clone()
	})})
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", e.port), nil)
	req.Host = e.publicHost
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := e.httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := recvTimeout(t, ch, 5*time.Second)
	if got := h.Get("X-Forwarded-For"); got != "127.0.0.1" {
		t.Errorf("X-Forwarded-For = %q", got)
	}
	if got := h.Get("X-Forwarded-Host"); got != e.publicHost {
		t.Errorf("X-Forwarded-Host = %q, want %q", got, e.publicHost)
	}
	if got := h.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q", got)
	}
}

func TestRewriteHost(t *testing.T) {
	ch := make(chan string, 1)
	e, _ := setupEnv(t, opts{rewriteHost: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- r.Host
	})})
	resp, err := e.do(context.Background(), "GET", "/", e.publicHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, want := recvTimeout(t, ch, 5*time.Second), strings.TrimPrefix(e.local.URL, "http://"); got != want {
		t.Fatalf("Host = %q, want %q", got, want)
	}
}

func TestOfflineUnknown(t *testing.T) {
	e, _ := setupEnv(t, opts{})
	req, err := http.NewRequest("GET", fmt.Sprintf("%s://127.0.0.1:%d/", e.scheme, e.port), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = fmt.Sprintf("nobody.localhost:%d", e.port)
	req.Header.Set("Accept", "text/html")
	resp, err := e.httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp.Body)
	if resp.StatusCode != 502 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "offline") || !strings.Contains(body, "nobody.localhost") ||
		!strings.Contains(body, "This tunnel is offline") {
		t.Fatalf("unexpected: %d %v %s", resp.StatusCode, resp.Header, body)
	}
}

func TestMultiLabelHost(t *testing.T) {
	e, _ := setupEnv(t, opts{})
	resp, err := e.do(context.Background(), "GET", "/", fmt.Sprintf("a.b.localhost:%d", e.port), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body := strings.TrimSpace(readAll(t, resp.Body)); resp.StatusCode != 404 || body != "unknown host" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestConcurrent(t *testing.T) {
	get, _, _ := setup(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "body-", r.URL.Query().Get("i"))
	})})
	var wg sync.WaitGroup
	errs := make(chan string, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := get(fmt.Sprintf("/?i=%d", i))
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || string(b) != fmt.Sprint("body-", i) {
				errs <- fmt.Sprintf("req %d: %d %q", i, resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestTLSForwarding(t *testing.T) {
	ch := make(chan string, 1)
	e, _ := setupEnv(t, opts{tls: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- r.Header.Get("X-Forwarded-Proto")
	})})
	resp, err := e.do(context.Background(), "GET", "/", e.publicHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := recvTimeout(t, ch, 5*time.Second); got != "https" {
		t.Fatalf("X-Forwarded-Proto = %q", got)
	}
}

func TestLocalAppDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("http://" + l.Addr().String())
	l.Close()
	get, _, _ := setup(t, opts{target: u})
	start := time.Now()
	resp := get("/")
	defer resp.Body.Close()
	body := readAll(t, resp.Body)
	if resp.StatusCode != 502 || !strings.Contains(body, "couldn't reach") || strings.Contains(body, "refused") || time.Since(start) > 3*time.Second {
		t.Fatalf("got %d %q after %v", resp.StatusCode, body, time.Since(start))
	}
}

func TestCallerCancelPropagates(t *testing.T) {
	started := make(chan struct{})
	unblocked := make(chan struct{})
	e, _ := setupEnv(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(unblocked)
	})})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
		}
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if resp, err := e.do(ctx, "GET", "/", e.publicHost, nil); err == nil {
		resp.Body.Close()
		t.Fatal("expected cancellation error")
	}
	select {
	case <-unblocked:
	case <-time.After(2 * time.Second):
		t.Fatal("local handler never saw cancellation")
	}
}

func TestHostNormalization(t *testing.T) {
	e, _ := setupEnv(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})})
	resp, err := e.do(context.Background(), "GET", "/", strings.ToUpper(e.publicHost), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestInspectorCaptureAndReplay(t *testing.T) {
	fixture, err := os.ReadFile("testdata/stripe-checkout-completed.json")
	if err != nil {
		t.Fatal(err)
	}
	type hit struct {
		body []byte
		sig  string
	}
	hits := make(chan hit, 4)
	e, _ := setupEnv(t, opts{inspector: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits <- hit{b, r.Header.Get("Stripe-Signature")}
		w.WriteHeader(200)
	})})
	const sig = "t=1729612800,v1=deadbeef"
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/webhooks/stripe", e.port), bytes.NewReader(fixture))
	req.Host = e.publicHost
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", sig)
	resp, err := e.httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	first := recvTimeout(t, hits, 5*time.Second)
	if !bytes.Equal(first.body, fixture) || first.sig != sig {
		t.Fatalf("first hit: sig %q, %d bytes", first.sig, len(first.body))
	}

	// The inspector records an exchange only after the handler chain
	// returns, which can be after the client already saw the response.
	list := waitStoreLen(t, e.insp.Store(), 1)
	ex, ok := e.insp.Store().Get(list[0].ID)
	if !ok || !bytes.Equal(ex.Request.Body.Data, fixture) || ex.Response.Status != 200 {
		t.Fatalf("captured exchange: %+v", ex)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.insp.Replay(ctx, ex.ID, nil); err != nil {
		t.Fatal(err)
	}
	second := recvTimeout(t, hits, 5*time.Second)
	if !bytes.Equal(second.body, fixture) || second.sig != sig {
		t.Fatalf("replayed hit: sig %q, %d bytes", second.sig, len(second.body))
	}
	waitStoreLen(t, e.insp.Store(), 2)
}

func TestInspectorStreamingStillWorks(t *testing.T) {
	e, _ := setupEnv(t, opts{inspector: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte("b"))
	})})
	resp, err := e.do(context.Background(), "GET", "/", e.publicHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || buf[0] != 'a' {
		t.Fatalf("first read: %v %q", err, buf)
	}
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Fatalf("first chunk took %v, not streamed", d)
	}
	if rest := readAll(t, resp.Body); rest != "b" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestInspectorWebSocket(t *testing.T) {
	e, _ := setupEnv(t, opts{inspector: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if c.Write(r.Context(), typ, b) != nil {
				return
			}
		}
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+e.publicHost+"/ws", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: e.pin(http.DefaultTransport, false)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, b, err := c.Read(ctx); err != nil || string(b) != "ping" {
		t.Fatalf("got %q, %v", b, err)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// The exchange is recorded when the hijacked connection ends.
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, s := range e.insp.Store().List() {
			if s.Upgraded {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no upgraded exchange recorded: %+v", e.insp.Store().List())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRateLimitE2E(t *testing.T) {
	get, _, _ := setup(t, opts{rateLimit: 3, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})})
	for i := 0; i < 3; i++ {
		resp := get("/")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i+1, resp.StatusCode)
		}
	}
	resp := get("/")
	defer resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestBrowserWarningE2E(t *testing.T) {
	type seen struct{ method, path string }
	hits := make(chan seen, 4)
	e, _ := setupEnv(t, opts{browserWarning: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- seen{r.Method, r.URL.RequestURI()}
		fmt.Fprint(w, "app page")
	})})
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{
		Jar:       jar,
		Transport: e.pin(http.DefaultTransport, false),
		Timeout:   10 * time.Second,
	}
	// The jar keys on the URL host, so request the public host and dial the pinned port.
	resp, err := browser.Do(newBrowserGET(t, "http://"+e.publicHost+"/dash?x=1"))
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(body, "/_passway/continue?to=") || strings.Contains(body, "app page") {
		t.Fatalf("expected warning page, got %d %q", resp.StatusCode, body)
	}
	select {
	case h := <-hits:
		t.Fatalf("warning page leaked a request to the app: %+v", h)
	default:
	}

	// The browser follows the continue link; the redirect lands on the original path.
	cont := "http://" + e.publicHost + "/_passway/continue?to=" + url.QueryEscape("/dash?x=1")
	resp, err = browser.Do(newBrowserGET(t, cont))
	if err != nil {
		t.Fatal(err)
	}
	body = readAll(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || body != "app page" {
		t.Fatalf("after continue: %d %q", resp.StatusCode, body)
	}
	if h := recvTimeout(t, hits, 5*time.Second); h.method != "GET" || h.path != "/dash?x=1" {
		t.Fatalf("app saw %+v", h)
	}

	// A webhook POST with no cookie is never interrupted.
	resp, err = e.do(context.Background(), "POST", "/webhooks/stripe", e.publicHost, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := recvTimeout(t, hits, 5*time.Second); resp.StatusCode != 200 || h.method != "POST" || h.path != "/webhooks/stripe" {
		t.Fatalf("webhook: %d %+v", resp.StatusCode, h)
	}
}

func newBrowserGET(t *testing.T, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	return req
}

// --- cloud flow ---

// browserSession is a cookie-jar client signed in to the dashboard.
type browserSession struct {
	t    *testing.T
	e    *env
	http *http.Client
}

func (e *env) devLogin(t *testing.T, login string) *browserSession {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	b := &browserSession{t: t, e: e, http: &http.Client{
		Jar: jar, Timeout: 10 * time.Second, Transport: e.pin(http.DefaultTransport, false),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	req, err := http.NewRequest(http.MethodPost, e.base+"/auth/dev", strings.NewReader(url.Values{"login": {login}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.base)
	resp, err := b.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dev login: %d", resp.StatusCode)
	}
	return b
}

var (
	csrfRe  = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	tokenRe = regexp.MustCompile(`pw_[A-Za-z0-9_-]+`)
)

func (b *browserSession) get(path string) (int, string) {
	b.t.Helper()
	resp, err := b.http.Get(b.e.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(b.t, resp.Body)
}

// post fetches the page at path for its CSRF token, then posts the form to it.
func (b *browserSession) post(path string, form url.Values) (int, string) {
	b.t.Helper()
	return b.postFrom(path, path, form)
}

// postFrom posts form to path with the CSRF token from the page at from.
func (b *browserSession) postFrom(from, path string, form url.Values) (int, string) {
	b.t.Helper()
	resp := b.submit(from, path, form)
	defer resp.Body.Close()
	return resp.StatusCode, readAll(b.t, resp.Body)
}

// submit is postFrom returning the whole response; the caller closes its body.
func (b *browserSession) submit(from, path string, form url.Values) *http.Response {
	b.t.Helper()
	_, page := b.get(from)
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		b.t.Fatalf("no csrf token on %s: %s", from, page)
	}
	form.Set("csrf", m[1])
	req, _ := http.NewRequest("POST", b.e.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.e.base)
	resp, err := b.http.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	return resp
}

func (b *browserSession) newToken(name string) string {
	b.t.Helper()
	code, body := b.post("/dashboard/tokens", url.Values{"name": {name}})
	tok := tokenRe.FindString(body)
	if code != 200 || tok == "" {
		b.t.Fatalf("create token: %d %s", code, body)
	}
	return tok
}

// startClient runs a tunnel client for token and returns its Ready and result.
func (e *env) startClient(t *testing.T, token, name string, handler http.Handler) (protocol.Ready, <-chan error) {
	t.Helper()
	r, res, _ := e.startStoppableClient(t, token, name, handler)
	return r, res
}

// startStoppableClient is startClient that also returns a function which
// stops the client (like Ctrl-C) and waits for it to exit.
func (e *env) startStoppableClient(t *testing.T, token, name string, handler http.Handler) (protocol.Ready, <-chan error, func()) {
	t.Helper()
	r, _, res, stop := e.runClient(t, client.Config{Token: token, Name: name, Handler: handler})
	return r, res, stop
}

// runClient runs a tunnel client with cfg (Server, Target, Logger and
// OnReady are filled in) and waits for its first Ready. ready receives the
// Ready of every later reconnect.
func (e *env) runClient(t *testing.T, cfg client.Config) (first protocol.Ready, ready <-chan protocol.Ready, result <-chan error, stop func()) {
	t.Helper()
	readyc := make(chan protocol.Ready, 4)
	res := make(chan error, 1)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cfg.Server = e.base
	cfg.Target, _ = url.Parse(e.local.URL)
	cfg.Logger = discard
	cfg.OnReady = func(r protocol.Ready) { readyc <- r }
	go func() {
		defer close(done)
		res <- client.Run(ctx, cfg)
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("client did not exit")
		}
	}
	t.Cleanup(stop)
	select {
	case r := <-readyc:
		return r, readyc, res, stop
	case err := <-res:
		t.Fatalf("client exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("tunnel never became ready")
	}
	return protocol.Ready{}, nil, nil, nil
}

func TestCloudSignupLoginClaim(t *testing.T) {
	e, _ := setupEnv(t, opts{cloud: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "app page")
	})})
	alice := e.devLogin(t, "alice")
	tok := alice.newToken("laptop")

	r1, _ := e.startClient(t, tok, "", nil)
	alice0, err := e.store.UserByLogin(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	subs, err := e.store.Subdomains(context.Background(), alice0.ID)
	if err != nil || len(subs) != 1 || !subs[0].IsDefault {
		t.Fatalf("stored subdomains: %+v, %v", subs, err)
	}
	if r1.Name != subs[0].Name || !strings.HasSuffix(r1.URL, fmt.Sprintf(".localhost:%d", e.port)) {
		t.Fatalf("ready: %+v", r1)
	}
	host := fmt.Sprintf("%s.localhost:%d", r1.Name, e.port)
	resp, err := e.do(context.Background(), "POST", "/", host, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp.Body); resp.StatusCode != 200 || body != "app page" {
		t.Fatalf("public request: %d %q", resp.StatusCode, body)
	}
	resp.Body.Close()

	u, err := e.store.UserByLogin(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetPlan(context.Background(), u.ID, "pro"); err != nil {
		t.Fatal(err)
	}
	if code, body := alice.post("/dashboard/subdomains", url.Values{"name": {"acme"}}); code != http.StatusSeeOther {
		t.Fatalf("claim: %d %s", code, body)
	}
	r2, _ := e.startClient(t, tok, "acme", nil)
	if r2.Name != "acme" {
		t.Fatalf("second tunnel: %+v", r2)
	}
	_, page := alice.get("/dashboard")
	// Coupled to the dashboard template: each active tunnel is a row whose
	// first cell is its URL.
	if n := strings.Count(page, "<td>http://"); n != 2 {
		t.Fatalf("dashboard shows %d tunnels, want 2:\n%s", n, page)
	}
}

// Restarting the CLI (a new session ID) gets the account's default name back
// at once, not name_taken for the post-disconnect hold.
func TestCloudRestartKeepsDefaultName(t *testing.T) {
	e, _ := setupEnv(t, opts{cloud: true})
	tok := e.devLogin(t, "alice").newToken("laptop")
	r1, _, stop := e.startStoppableClient(t, tok, "", nil)
	stop()
	r2, _ := e.startClient(t, tok, "", nil)
	if r2.Name != r1.Name {
		t.Fatalf("after restart got %q, want %q", r2.Name, r1.Name)
	}
}

func TestCloudFreeSecondTunnelRefused(t *testing.T) {
	e, _ := setupEnv(t, opts{cloud: true})
	tok := e.devLogin(t, "alice").newToken("laptop")
	e.startClient(t, tok, "", nil)

	target, _ := url.Parse(e.local.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := client.Run(ctx, client.Config{Server: e.base, Token: tok, Name: "", Target: target, Logger: discard})
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeLimitReached ||
		pe.Message != "tunnel limit reached: your Free plan allows 1 concurrent tunnel(s)" {
		t.Fatalf("got %v", err)
	}
}

func TestCloudFreeWarningShowsOwner(t *testing.T) {
	hits := make(chan string, 4)
	e, _ := setupEnv(t, opts{cloud: true, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- r.Method
		fmt.Fprint(w, "app page")
	})})
	tok := e.devLogin(t, "alice").newToken("laptop")
	r, _ := e.startClient(t, tok, "", nil)
	host := fmt.Sprintf("%s.localhost:%d", r.Name, e.port)

	req := newBrowserGET(t, "http://"+host+"/")
	resp, err := e.httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(body, "@alice") || strings.Contains(body, "app page") {
		t.Fatalf("warning: %d %q", resp.StatusCode, body)
	}
	resp, err = e.do(context.Background(), "POST", "/hook", host, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if m := recvTimeout(t, hits, 5*time.Second); resp.StatusCode != 200 || m != "POST" {
		t.Fatalf("webhook: %d %s", resp.StatusCode, m)
	}
}

func TestCloudBanDisconnects(t *testing.T) {
	e, srv := setupEnv(t, opts{cloud: true})
	tok := e.devLogin(t, "mallory").newToken("laptop")
	_, res := e.startClient(t, tok, "", nil)

	ctx := context.Background()
	u, err := e.store.UserByLogin(ctx, "mallory")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.BanUser(ctx, u, "abuse"); err != nil {
		t.Fatal(err)
	}
	srv.PollBansNow(ctx)
	select {
	case err := <-res:
		var pe *protocol.Error
		if !errors.As(err, &pe) || pe.Code != protocol.CodeBanned {
			t.Fatalf("client ended with %v, want banned", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("banned client was not disconnected")
	}
}

// waitUnauthorized waits for a client whose token was revoked to end with unauthorized.
func waitUnauthorized(t *testing.T, res <-chan error) {
	t.Helper()
	select {
	case err := <-res:
		var pe *protocol.Error
		if !errors.As(err, &pe) || pe.Code != protocol.CodeUnauthorized {
			t.Fatalf("client ended with %v, want unauthorized", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("client of a revoked token was not disconnected")
	}
}

func TestCloudDashboardRevokeDisconnects(t *testing.T) {
	e, _ := setupEnv(t, opts{cloud: true})
	alice := e.devLogin(t, "alice")
	tok := alice.newToken("laptop")
	_, res := e.startClient(t, tok, "", nil)
	u, err := e.store.UserByLogin(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	toks, err := e.store.ListTokens(context.Background(), u.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("tokens: %v %v", toks, err)
	}
	if code, body := alice.postFrom("/dashboard/tokens", "/dashboard/tokens/revoke", url.Values{"id": {strconv.FormatInt(toks[0].ID, 10)}}); code != http.StatusSeeOther {
		t.Fatalf("revoke: %d %s", code, body)
	}
	waitUnauthorized(t, res)
}

// A token revoked outside the server process is caught by the ban poller.
func TestCloudStoreRevokeDisconnects(t *testing.T) {
	e, srv := setupEnv(t, opts{cloud: true})
	tok := e.devLogin(t, "alice").newToken("laptop")
	_, res := e.startClient(t, tok, "", nil)
	ctx := context.Background()
	u, err := e.store.UserByLogin(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	toks, err := e.store.ListTokens(ctx, u.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("tokens: %v %v", toks, err)
	}
	if err := e.store.RevokeToken(ctx, u.ID, toks[0].ID); err != nil {
		t.Fatal(err)
	}
	srv.PollBansNow(ctx)
	waitUnauthorized(t, res)
}

func TestSelfHostedStillRandomNames(t *testing.T) {
	get, host, _ := setup(t, opts{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hi")
	})})
	resp := get("/")
	defer resp.Body.Close()
	if body := readAll(t, resp.Body); resp.StatusCode != 200 || body != "hi" || !strings.Contains(host, "-") {
		t.Fatalf("%s: %d %q", host, resp.StatusCode, body)
	}
}

// --- launch: billing, custom domains, modes ---

// fakeStripe is a Stripe API stand-in: Checkout returns a fixed session URL.
// Webhooks are signed with secret, as Stripe signs them.
type fakeStripe struct {
	srv    *httptest.Server
	secret string
	mu     sync.Mutex
	ref    string // client_reference_id of the last Checkout Session
}

const fakeCheckoutURL = "https://checkout.stripe.com/c/pay/cs_test_e2e"

func newFakeStripe(t *testing.T) *fakeStripe {
	t.Helper()
	f := &fakeStripe{secret: "whsec_e2e"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/checkout/sessions" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		f.mu.Lock()
		f.ref = r.PostForm.Get("client_reference_id")
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"url":%q}`, fakeCheckoutURL)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStripe) clientRef() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ref
}

// sign returns a Stripe-Signature header for payload, signed now.
func (f *fakeStripe) sign(payload []byte) string {
	ts := time.Now().Unix()
	m := hmac.New(sha256.New, []byte(f.secret))
	fmt.Fprintf(m, "%d.%s", ts, payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(m.Sum(nil)))
}

// sendWebhook posts a signed event to the server's Stripe webhook endpoint.
func (e *env) sendWebhook(t *testing.T, f *fakeStripe, payload string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.base+"/stripe/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", f.sign([]byte(payload)))
	resp, err := e.httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: %d %s", resp.StatusCode, body)
	}
}

// fakeResolver answers the custom-domain DNS checks from maps.
type fakeResolver struct {
	mu    sync.Mutex
	cname map[string]string
	txt   map[string][]string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{cname: map[string]string{}, txt: map[string][]string{}}
}

func (f *fakeResolver) set(host, cname, txt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cname[host] = cname
	f.txt["_passway."+host] = []string{txt}
}

func (f *fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cname[host]; ok {
		return c, nil
	}
	return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (e *env) userID(t *testing.T, login string) int64 {
	t.Helper()
	u, err := e.store.UserByLogin(context.Background(), login)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func (e *env) planOf(t *testing.T, login string) string {
	t.Helper()
	u, err := e.store.UserByLogin(context.Background(), login)
	if err != nil {
		t.Fatal(err)
	}
	return u.Plan
}

func TestCloudUpgradeViaStripe(t *testing.T) {
	stripe := newFakeStripe(t)
	e, _ := setupEnv(t, opts{cloud: true, billing: stripe})
	alice := e.devLogin(t, "alice")
	tok := alice.newToken("laptop")
	id := e.userID(t, "alice")
	_, ready1, res1, _ := e.runClient(t, client.Config{Token: tok})

	// Upgrade: the dashboard sends the browser to Stripe Checkout.
	resp := alice.submit("/dashboard", "/billing/checkout", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != fakeCheckoutURL {
		t.Fatalf("checkout: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got := stripe.clientRef(); got != strconv.FormatInt(id, 10) {
		t.Fatalf("checkout client_reference_id = %q, want %d", got, id)
	}

	// Stripe reports the paid Checkout; the account becomes Pro.
	now := time.Now().Unix()
	e.sendWebhook(t, stripe, fmt.Sprintf(`{"id":"evt_e2e_1","object":"event","type":"checkout.session.completed","created":%d,`+
		`"data":{"object":{"object":"checkout.session","mode":"subscription","payment_status":"paid","client_reference_id":"%d","customer":"cus_e2e","subscription":"sub_e2e"}}}`, now, id))
	if p := e.planOf(t, "alice"); p != "pro" {
		t.Fatalf("plan after checkout = %q, want pro", p)
	}
	if _, page := alice.get("/dashboard?upgraded=1"); !strings.Contains(page, "Payment received — your plan updates in a few seconds.") {
		t.Fatalf("no upgrade notice on the dashboard:\n%s", page)
	}
	// The plan change reconnects the live tunnel under the Pro limits.
	recvTimeout(t, ready1, 30*time.Second)

	if code, body := alice.post("/dashboard/subdomains", url.Values{"name": {"acme"}}); code != http.StatusSeeOther {
		t.Fatalf("claim: %d %s", code, body)
	}
	// No --name: the second client gets the account's next free subdomain.
	// (A --name acme client would get name_not_owned after the downgrade,
	// since Free keeps only the first subdomain usable.)
	r2, ready2, res2, _ := e.runClient(t, client.Config{Token: tok})
	if r2.Name != "acme" {
		t.Fatalf("second tunnel: %+v", r2)
	}

	// The subscription ends; both tunnels drop, and only one fits on Free.
	e.sendWebhook(t, stripe, fmt.Sprintf(`{"id":"evt_e2e_2","object":"event","type":"customer.subscription.deleted","created":%d,`+
		`"data":{"object":{"id":"sub_e2e","object":"subscription","customer":"cus_e2e","status":"canceled"}}}`, now+1))
	if p := e.planOf(t, "alice"); p != "free" {
		t.Fatalf("plan after cancellation = %q, want free", p)
	}
	var refused error
	var survivor <-chan protocol.Ready
	select {
	case refused = <-res1:
		survivor = ready2
	case refused = <-res2:
		survivor = ready1
	case <-time.After(30 * time.Second):
		t.Fatal("no tunnel was refused after the downgrade")
	}
	var pe *protocol.Error
	if !errors.As(refused, &pe) || pe.Code != protocol.CodeLimitReached ||
		pe.Message != "tunnel limit reached: your Free plan allows 1 concurrent tunnel(s)" {
		t.Fatalf("refused tunnel ended with %v, want limit_reached", refused)
	}
	recvTimeout(t, survivor, 30*time.Second) // the other one reconnected
}

func TestCloudCustomDomain(t *testing.T) {
	res := newFakeResolver()
	e, _ := setupEnv(t, opts{cloud: true, resolver: res, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "app page for %s", r.Host)
	})})
	alice := e.devLogin(t, "alice")
	tok := alice.newToken("laptop")
	ctx := context.Background()
	id := e.userID(t, "alice")
	if err := e.store.SetPlan(ctx, id, "pro"); err != nil {
		t.Fatal(err)
	}
	const host = "hooks.example.test"
	if code, body := alice.post("/dashboard/domains", url.Values{"host": {host}}); code != http.StatusSeeOther {
		t.Fatalf("add domain: %d %s", code, body)
	}

	// Not verified yet: the client is refused with the dashboard hint.
	target, _ := url.Parse(e.local.URL)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := client.Run(cctx, client.Config{Server: e.base, Token: tok, Domain: host, Target: target, Logger: discard})
	cancel()
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeNameNotOwned ||
		pe.Message != `you don't own "hooks.example.test" or it isn't verified; add it in the dashboard` {
		t.Fatalf("unverified domain: %v", err)
	}

	doms, err := e.store.Domains(ctx, id)
	if err != nil || len(doms) != 1 {
		t.Fatalf("domains: %+v %v", doms, err)
	}
	res.set(host, "edge.localhost.", "passway-verify="+doms[0].Token)
	if code, body := alice.postFrom("/dashboard/domains", "/dashboard/domains/verify", url.Values{"host": {host}}); code != http.StatusSeeOther {
		t.Fatalf("verify: %d %s", code, body)
	}

	r, _, _, _ := e.runClient(t, client.Config{Token: tok, Domain: host})
	if !strings.Contains(r.URL, host) {
		t.Fatalf("ready: %+v", r)
	}
	pub := fmt.Sprintf("%s:%d", host, e.port)
	resp, err := e.do(ctx, "POST", "/hook", pub, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || body != "app page for "+pub {
		t.Fatalf("custom domain request: %d %q", resp.StatusCode, body)
	}
}

// offlinePage fetches the HTML page for a host with no tunnel.
func (e *env) offlinePage(t *testing.T) string {
	t.Helper()
	req := newBrowserGET(t, fmt.Sprintf("http://nobody.localhost:%d/", e.port))
	resp, err := e.httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "This tunnel is offline") {
		t.Fatalf("offline page: %d %s", resp.StatusCode, body)
	}
	return body
}

// Self-hosted servers carry no Passway branding and no brand-word blocklist
// (owner rulings R1 and R3 apply to the cloud only).
func TestSelfHostedNoFooterNoBlocklist(t *testing.T) {
	e, _ := setupEnv(t, opts{})
	if body := e.offlinePage(t); strings.Contains(body, "Served by") {
		t.Fatalf("self-hosted page has the footer:\n%s", body)
	}
	r, _, _, _ := e.runClient(t, client.Config{Name: "stripe-hooks"})
	if r.Name != "stripe-hooks" {
		t.Fatalf("ready: %+v", r)
	}
}

// The cloud counterpart: the footer shows on unknown hosts and brand words
// are refused.
func TestCloudFooterAndBlocklist(t *testing.T) {
	e, _ := setupEnv(t, opts{cloud: true})
	if body := e.offlinePage(t); !strings.Contains(body, "Served by") {
		t.Fatalf("cloud page has no footer:\n%s", body)
	}
	tok := e.devLogin(t, "alice").newToken("laptop")
	target, _ := url.Parse(e.local.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := client.Run(ctx, client.Config{Server: e.base, Token: tok, Name: "stripe-hooks", Target: target, Logger: discard})
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeInvalidName || !strings.Contains(pe.Message, "blocked word") {
		t.Fatalf("got %v, want a blocked-word refusal", err)
	}
}

// A non-owner adding and removing a pending row for someone else's verified
// domain must not touch the owner's live tunnel (review I1).
func TestCloudCustomDomainNonOwnerRemoveKeepsTunnel(t *testing.T) {
	res := newFakeResolver()
	e, srv := setupEnv(t, opts{cloud: true, resolver: res})
	ctx := context.Background()
	const host = "hooks.example.test"

	alice := e.devLogin(t, "alice")
	tok := alice.newToken("laptop")
	aliceID := e.userID(t, "alice")
	if err := e.store.SetPlan(ctx, aliceID, "pro"); err != nil {
		t.Fatal(err)
	}
	if code, body := alice.post("/dashboard/domains", url.Values{"host": {host}}); code != http.StatusSeeOther {
		t.Fatalf("alice add: %d %s", code, body)
	}
	doms, _ := e.store.Domains(ctx, aliceID)
	res.set(host, "edge.localhost.", "passway-verify="+doms[0].Token)
	if code, body := alice.postFrom("/dashboard/domains", "/dashboard/domains/verify", url.Values{"host": {host}}); code != http.StatusSeeOther {
		t.Fatalf("alice verify: %d %s", code, body)
	}
	_, ready, _, _ := e.runClient(t, client.Config{Token: tok, Domain: host})
	aid := strconv.FormatInt(aliceID, 10)
	before := srv.TunnelsFor(aid)
	if len(before) != 1 {
		t.Fatalf("alice tunnels: %+v", before)
	}

	mallory := e.devLogin(t, "mallory")
	if err := e.store.SetPlan(ctx, e.userID(t, "mallory"), "pro"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if code, body := mallory.post("/dashboard/domains", url.Values{"host": {host}}); code != http.StatusSeeOther {
			t.Fatalf("mallory add: %d %s", code, body)
		}
		if code, body := mallory.postFrom("/dashboard/domains", "/dashboard/domains/remove", url.Values{"host": {host}}); code != http.StatusSeeOther {
			t.Fatalf("mallory remove: %d %s", code, body)
		}
	}
	after := srv.TunnelsFor(aid)
	if len(after) != 1 || !after[0].ConnectedAt.Equal(before[0].ConnectedAt) {
		t.Fatalf("alice's tunnel was cut: before %+v, after %+v", before, after)
	}
	select {
	case r := <-ready:
		t.Fatalf("alice's client reconnected: %+v", r)
	case <-time.After(500 * time.Millisecond):
	}
}
