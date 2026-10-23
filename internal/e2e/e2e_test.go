package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mohammed19n/passway/internal/client"
	"github.com/mohammed19n/passway/internal/inspector"
	"github.com/mohammed19n/passway/internal/protocol"
	"github.com/mohammed19n/passway/internal/server"
)

type opts struct {
	handler     http.Handler // local app; default 404
	rewriteHost bool
	tls         bool
	target      *url.URL // overrides the local app's URL (e.g. a closed port)

	inspector      bool // wrap the client's handler with an inspector
	rateLimit      int  // server requests/minute per tunnel; 0 = off
	browserWarning bool // server shows the interstitial to browser GETs
}

type env struct {
	port       int
	publicHost string
	local      *httptest.Server
	httpc      *http.Client
	scheme     string
	insp       *inspector.Inspector // nil unless opts.inspector
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
	srv := server.New(server.Config{Domain: "localhost", PublicScheme: scheme, PublicPort: port, Logger: discard,
		RateLimit: o.rateLimit, BrowserWarning: o.browserWarning})
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
	e := &env{port: port, scheme: scheme, publicHost: fmt.Sprintf("%s.localhost:%d", r.Name, port), local: local, insp: insp}
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

	list := e.insp.Store().List()
	if len(list) != 1 {
		t.Fatalf("store has %d exchanges, want 1", len(list))
	}
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
	if n := len(e.insp.Store().List()); n != 2 {
		t.Fatalf("store has %d exchanges after replay, want 2", n)
	}
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
