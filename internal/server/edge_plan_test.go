package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// cloudEdge starts a cloud server for d with a live tunnel "quick" whose
// backend answers 200 "ok".
func cloudEdge(t *testing.T, d *usageDir, backend http.Handler) (*Server, string, string, *yamux.Session) {
	t.Helper()
	srv, ts := newCloudServer(t, d.fakeDirectory)
	srv.cfg.Directory = d
	srv.meter = NewMeter(d, 500<<30, 0, quietLog())
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
