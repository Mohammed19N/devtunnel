package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestFooterSelfHostedHidden(t *testing.T) {
	_, ts := newTestServer(t, nil)
	u, _ := url.Parse(ts.URL)
	c, b := get(t, ts.URL, "gone.localhost:"+u.Port(), "text/html")
	if c != 502 || !strings.Contains(b, "offline") {
		t.Fatalf("got %d", c)
	}
	if strings.Contains(b, "Served by") {
		t.Fatal("self-hosted page must not have the footer")
	}
}

func TestFooterCloudFreeShown(t *testing.T) {
	d := newUsageDir()
	d.acc.Login = "u1login"
	srv, base, host, _ := cloudEdge(t, d, okBackend())
	c, b := get(t, base, host, "text/html")
	if c != 200 || !strings.Contains(b, "You are about to visit") {
		t.Fatalf("got %d", c)
	}
	_, port, _ := strings.Cut(host, ":")
	want := `href="http://localhost:` + port + `"`
	if !strings.Contains(b, "Served by") || !strings.Contains(b, want) {
		t.Fatalf("footer missing %s in %s", want, b)
	}
	if strings.Contains(b, "passway.run") {
		t.Fatal("hard-coded home URL leaked")
	}
	_ = srv
}

func TestFooterCloudProHidden(t *testing.T) {
	d := newUsageDir()
	d.acc.Plan = PlanPro
	d.acc.Login = "u1login"
	srv, base, host, _ := cloudEdge(t, d, okBackend())
	srv.bans.Store(&banSet{users: map[string]bool{"u1login": true}, hosts: map[string]bool{}, sticky: map[string]string{}})
	c, b := get(t, base, host, "text/html")
	if c != 403 {
		t.Fatalf("got %d, want 403 banned page", c)
	}
	if strings.Contains(b, "Served by") {
		t.Fatal("pro tunnel page must not have the footer")
	}
}

func TestFooterCloudUnknownHostShown(t *testing.T) {
	d := newUsageDir()
	_, base, host, _ := cloudEdge(t, d, okBackend())
	_, port, _ := strings.Cut(host, ":")
	c, b := get(t, base, "nobody.localhost:"+port, "text/html")
	if c != 502 || !strings.Contains(b, "Served by") {
		t.Fatalf("got %d, footer missing", c)
	}
}

func TestEdgeBusyFast503(t *testing.T) {
	_, ts := newTestServerCfg(t, nil, func(c *Config) { c.MaxConnsPerTunnel = 1 })
	resp, sess := dialRaw(t, ts, hello("busy", "A"))
	if resp.Ready == nil {
		t.Fatalf("ready: %+v", resp.Error)
	}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		if r.URL.Path == "/hold" {
			<-release
		}
		io.WriteString(w, "ok")
	})}
	go hs.Serve(streamListener{sess})
	t.Cleanup(func() { hs.Close() })
	u, _ := url.Parse(ts.URL)
	host := "busy.localhost:" + u.Port()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	do := func(path, accept string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = host
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}

	first := make(chan int, 1)
	go func() {
		r, _ := do("/hold", "")
		first <- r.StatusCode
	}()
	<-started

	t0 := time.Now()
	r, b := do("/", "")
	if el := time.Since(t0); el > 200*time.Millisecond {
		t.Fatalf("503 took %v", el)
	}
	if r.StatusCode != 503 || r.Header.Get("Retry-After") != "1" || !strings.Contains(b, "tunnel is busy: too many concurrent connections") {
		t.Fatalf("got %d %q retry=%q", r.StatusCode, b, r.Header.Get("Retry-After"))
	}
	r, b = do("/", "text/html")
	if r.StatusCode != 503 || !strings.Contains(b, "Tunnel busy") {
		t.Fatalf("html: %d %q", r.StatusCode, b)
	}

	close(release)
	if c := <-first; c != 200 {
		t.Fatalf("first = %d", c)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		r, _ = do("/", "")
		if r.StatusCode == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after release: %d", r.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
