package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// cloudEdge starts a cloud server for d with a live tunnel "quick" whose
// backend answers 200 "ok".
func cloudEdge(t *testing.T, d *usageDir, backend http.Handler, mutate ...func(*Server)) (*Server, string, string, *yamux.Session) {
	t.Helper()
	srv, ts := newCloudServer(t, d.fakeDirectory)
	srv.cfg.Directory = d
	srv.meter = NewMeter(d, 500<<30, 0, quietLog())
	for _, m := range mutate {
		m(srv)
	}
	resp, sess := dialRaw(t, ts, hello("", "A"))
	if resp.Ready == nil {
		t.Fatalf("ready: %+v", resp.Error)
	}
	hs := &http.Server{Handler: backend}
	go hs.Serve(streamListener{sess})
	t.Cleanup(func() { hs.Close() })
	u, _ := url.Parse(ts.URL)
	return srv, ts.URL, "quick.localhost:" + u.Port(), sess
}

func okBackend() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
}

func get(t *testing.T, base, host, accept string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/", nil)
	req.Host = host
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func TestEdgeFreeRateLimit(t *testing.T) {
	d := newUsageDir()
	srv, base, host, _ := cloudEdge(t, d, okBackend())
	now := time.Unix(1_000_000, 0)
	srv.limiter.now = func() time.Time { return now }
	srv.limiter.lastSweep = now
	for i := 0; i < 60; i++ {
		if c, _ := get(t, base, host, ""); c != 200 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	if c, _ := get(t, base, host, ""); c != 429 {
		t.Fatalf("61st: %d, want 429", c)
	}
}

func TestEdgeProNoWarning(t *testing.T) {
	d := newUsageDir()
	d.acc.Plan = PlanPro
	d.acc.Login = "u1login"
	_, base, host, _ := cloudEdge(t, d, okBackend())
	if c, b := get(t, base, host, "text/html"); c != 200 || b != "ok" {
		t.Fatalf("pro: %d %q", c, b)
	}

	d2 := newUsageDir()
	d2.acc.Login = "u1login"
	_, base, host, _ = cloudEdge(t, d2, okBackend())
	c, b := get(t, base, host, "text/html")
	if c != 200 || !strings.Contains(b, "@u1login") || b == "ok" {
		t.Fatalf("free: %d %q", c, b)
	}
}

func TestEdgeBandwidth402(t *testing.T) {
	d := newUsageDir()
	d.base = PlanFree.Bandwidth
	_, base, host, _ := cloudEdge(t, d, okBackend())
	c, b := get(t, base, host, "")
	if c != 402 || !strings.Contains(b, "monthly bandwidth limit reached") {
		t.Fatalf("got %d %q", c, b)
	}
}

func TestEdgeFreeCap402(t *testing.T) {
	d := newUsageDir()
	srv, base, host, _ := cloudEdge(t, d, okBackend())
	srv.meter.freeCap = 100
	srv.meter.Add(Account{ID: "other", Plan: PlanFree}, 200)
	if c, _ := get(t, base, host, ""); c != 402 {
		t.Fatalf("free cap: %d", c)
	}
}

func TestEdgeRequestBytesCounted(t *testing.T) {
	d := newUsageDir()
	srv, base, host, _ := cloudEdge(t, d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, strings.Repeat("x", 500))
	}))
	req, _ := http.NewRequest("POST", base+"/", strings.NewReader(strings.Repeat("y", 300)))
	req.Host = host
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	srv.meter.mu.Lock()
	n := srv.meter.pending["u1"].n
	srv.meter.mu.Unlock()
	if n < 800 {
		t.Fatalf("counted %d, want >= 800", n)
	}
}

func TestEdgeWebSocketBytesCounted(t *testing.T) {
	d := newUsageDir()
	srv, ts, host, _ := cloudEdge(t, d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		brw.Flush()
		io.Copy(conn, io.LimitReader(brw, 1000))
	}))
	u, _ := url.Parse(ts)
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", host)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	payload := strings.Repeat("z", 1000)
	io.WriteString(c, payload)
	got := make([]byte, 1000)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.meter.mu.Lock()
		var n int64
		if e := srv.meter.pending["u1"]; e != nil {
			n = e.n
		}
		srv.meter.mu.Unlock()
		if n >= 2000 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("counted %d, want >= 2000", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBanPollerClosesTunnel(t *testing.T) {
	d := newUsageDir()
	srv, base, host, sess := cloudEdge(t, d, okBackend())
	if c, _ := get(t, base, host, ""); c != 200 {
		t.Fatalf("before ban: %d", c)
	}
	d.umu.Lock()
	d.banUser = []string{"ALICE"} // case-insensitive login match
	d.umu.Unlock()
	srv.pollBans(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for !sess.IsClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !sess.IsClosed() {
		t.Fatal("banned user's session not closed")
	}

	d.umu.Lock()
	d.banUser, d.banHost = nil, []string{"Quick"}
	d.umu.Unlock()
	srv.pollBans(context.Background())
	c, b := get(t, base, host, "")
	if c != 403 || !strings.Contains(b, "this tunnel has been disabled") {
		t.Fatalf("banned host: %d %q", c, b)
	}
}

func TestBanPollerMatchesUserID(t *testing.T) {
	d := newUsageDir()
	srv, _, _, sess := cloudEdge(t, d, okBackend())
	d.umu.Lock()
	d.banUser = []string{"ID:u1"} // a ban keyed on the account id survives a login rename
	d.umu.Unlock()
	srv.pollBans(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for !sess.IsClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !sess.IsClosed() {
		t.Fatal("session of user banned by id not closed")
	}
}

func TestRunBackgroundStopsAndFlushes(t *testing.T) {
	d := newUsageDir()
	srv, _, _, _ := cloudEdge(t, d, okBackend())
	srv.meter.Add(Account{ID: "u1", Plan: PlanFree}, 42)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.RunBackground(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunBackground did not return")
	}
	d.umu.Lock()
	defer d.umu.Unlock()
	if d.added != 42 {
		t.Fatalf("flushed %d, want 42", d.added)
	}
}

func TestEdgeUnlimitedFollowsBrowserWarning(t *testing.T) {
	for _, flag := range []bool{false, true} {
		d := newUsageDir()
		d.acc.Plan = PlanUnlimited
		_, base, host, _ := cloudEdge(t, d, okBackend(), func(s *Server) { s.cfg.BrowserWarning = flag })
		c, b := get(t, base, host, "text/html")
		warned := c == 200 && b != "ok"
		if warned != flag {
			t.Fatalf("BrowserWarning=%v: warned=%v (%d %q)", flag, warned, c, b)
		}
	}
}

func TestEdgeProNotBlockedByFreeCap(t *testing.T) {
	d := newUsageDir()
	d.acc.Plan = PlanPro
	srv, base, host, _ := cloudEdge(t, d, okBackend())
	srv.meter.freeCap = 100
	srv.meter.Add(Account{ID: "other", Plan: PlanFree}, 200)
	if !srv.meter.FreeCapHit() {
		t.Fatal("setup: cap not hit")
	}
	if c, _ := get(t, base, host, ""); c != 200 {
		t.Fatalf("pro blocked by free cap: %d", c)
	}
}

func TestUserBanHostStaysForbidden(t *testing.T) {
	d := newUsageDir()
	srv, base, host, sess := cloudEdge(t, d, okBackend())
	d.umu.Lock()
	d.banUser = []string{"alice"}
	d.umu.Unlock()
	srv.pollBans(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.registry.Lookup("quick"); !ok && sess.IsClosed() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := srv.registry.Lookup("quick"); ok {
		t.Fatal("tunnel not released")
	}
	if c, _ := get(t, base, host, ""); c != 403 {
		t.Fatalf("after user ban and release: %d, want 403", c)
	}
	srv.pollBans(context.Background()) // still banned: stays 403
	if c, _ := get(t, base, host, ""); c != 403 {
		t.Fatalf("second poll: %d, want 403", c)
	}
	d.umu.Lock()
	d.banUser = nil
	d.umu.Unlock()
	srv.pollBans(context.Background())
	if c, _ := get(t, base, host, ""); c != 502 {
		t.Fatalf("after unban: %d, want 502 offline", c)
	}
}

func TestRunBackgroundSecondCallIsNoop(t *testing.T) {
	d := newUsageDir()
	srv, _, _, _ := cloudEdge(t, d, okBackend())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.RunBackground(ctx)
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { srv.RunBackground(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second RunBackground did not return")
	}
}

// capPlan is a plan with a 1 MiB monthly bandwidth and no other limits.
var capPlan = Plan{Name: PlanPro.Name, MaxTunnels: 5, MaxSubdomains: 5, Bandwidth: 1 << 20}

func TestEdgeBandwidthCutsLargeResponse(t *testing.T) {
	d := newUsageDir()
	d.acc.Plan = capPlan
	chunk := []byte(strings.Repeat("x", 32<<10))
	_, base, host, _ := cloudEdge(t, d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 640; i++ { // 20 MiB
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	req, _ := http.NewRequest("GET", base+"/", nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if n >= 2<<20 || err == nil {
		t.Fatalf("received %d bytes, err %v; want < 2 MiB and a broken body", n, err)
	}
}

func TestEdgeBandwidthCutsUpload(t *testing.T) {
	d := newUsageDir()
	d.acc.Plan = capPlan
	got := make(chan int64, 1)
	_, base, host, _ := cloudEdge(t, d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got <- n
		io.WriteString(w, "ok")
	}))
	req, _ := http.NewRequest("POST", base+"/", bytes.NewReader(make([]byte, 20<<20)))
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		// The client may instead see the connection drop mid-upload.
		if resp.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("upload over the cap: status %d, want 402", resp.StatusCode)
		}
	}
	select {
	case n := <-got:
		if n >= 2<<20 {
			t.Fatalf("backend read %d bytes, want < 2 MiB", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend never finished reading")
	}
}

func TestEdgeBandwidthClosesWebSocket(t *testing.T) {
	d := newUsageDir()
	d.acc.Plan = capPlan
	_, ts, host, _ := cloudEdge(t, d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		brw.Flush()
		io.Copy(conn, brw)
	}))
	u, _ := url.Parse(ts)
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", host)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	go func() {
		chunk := make([]byte, 64<<10)
		for i := 0; i < 128; i++ { // 8 MiB
			if _, err := c.Write(chunk); err != nil {
				return
			}
		}
	}()
	n, err := io.Copy(io.Discard, br)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("stream not closed after %d echoed bytes (timed out)", n)
	}
	if n >= 2<<20 {
		t.Fatalf("echoed %d bytes, want < 2 MiB", n)
	}
}

// staleListsDir is a usageDir whose first BannedLists call blocks until gate
// is closed and then returns an empty (stale) list.
type staleListsDir struct {
	*usageDir
	calls   atomic.Int32
	entered chan struct{}
	gate    chan struct{}
}

func (d *staleListsDir) BannedLists(ctx context.Context) ([]string, []string, error) {
	if d.calls.Add(1) == 1 {
		close(d.entered)
		<-d.gate
		return nil, nil, nil
	}
	return d.usageDir.BannedLists(ctx)
}

// A poll that read the database before a newer poll applied its result must
// not overwrite that result (PollBansNow racing the ticker).
func TestPollBansStaleResultDiscarded(t *testing.T) {
	d := newUsageDir()
	sd := &staleListsDir{usageDir: d, entered: make(chan struct{}), gate: make(chan struct{})}
	srv, base, host, sess := cloudEdge(t, d, okBackend(), func(s *Server) { s.cfg.Directory = sd })
	d.umu.Lock()
	d.banUser = []string{"alice"}
	d.umu.Unlock()
	ctx := context.Background()
	stale := make(chan struct{})
	go func() { defer close(stale); srv.PollBansNow(ctx) }()
	<-sd.entered
	srv.PollBansNow(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.registry.Lookup("quick"); !ok && sess.IsClosed() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c, _ := get(t, base, host, ""); c != 403 {
		t.Fatalf("after ban: %d, want 403", c)
	}
	close(sd.gate)
	<-stale
	if c, _ := get(t, base, host, ""); c != 403 {
		t.Fatalf("after the stale poll finished: %d, want 403", c)
	}
}

// A token revoked from another process (the admin CLI) is noticed by the
// ban poller, which closes the tunnels using it.
func TestBanPollerClosesRevokedTokenTunnels(t *testing.T) {
	d := newUsageDir()
	d.acc.TokenID = 5
	srv, _, _, sess := cloudEdge(t, d, okBackend())
	srv.pollBans(context.Background())
	time.Sleep(50 * time.Millisecond)
	if sess.IsClosed() {
		t.Fatal("closed before the token was revoked")
	}
	d.umu.Lock()
	d.revoked = []int64{5}
	d.umu.Unlock()
	srv.pollBans(context.Background())
	waitClosed(t, sess, "revoked token")
	d.umu.Lock()
	defer d.umu.Unlock()
	if len(d.revokedAsked) != 2 || !slices.Equal(d.revokedAsked[1], []int64{5}) {
		t.Fatalf("RevokedTokens calls = %v, want one per poll with [5]", d.revokedAsked)
	}
}
