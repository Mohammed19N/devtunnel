package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/config"
	"github.com/mohammed19n/passway/internal/server"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func cfgPath(t *testing.T) string { return filepath.Join(t.TempDir(), "config.yaml") }

func TestRunNoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), nil, &out, &errb, cfgPath(t)); code != 2 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(errb.String(), "usage") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"version"}, &out, &errb, cfgPath(t)); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if out.String() != "passway dev\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestRunLogin(t *testing.T) {
	p := cfgPath(t)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"login", "abc"}, &out, &errb, p); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, errb.String())
	}
	c, err := config.Load(p)
	if err != nil || c.Token != "abc" {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestRunHTTPBadTarget(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"http", "abc def"}, &out, &errb, cfgPath(t)); code != 2 {
		t.Fatalf("code = %d", code)
	}
}

func TestRunHTTPHostHeaderInvalid(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"http", "--host-header", "bogus", "3000"}, &out, &errb, cfgPath(t)); code != 2 {
		t.Fatalf("code = %d", code)
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunHTTPForwards(t *testing.T) {
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	srv := server.New(server.Config{Domain: "localhost", PublicScheme: "http", PublicPort: port})
	ts.Config.Handler = srv
	ts.Start()
	defer ts.Close()

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer local.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errb syncBuf
	codeCh := make(chan int, 1)
	cp := cfgPath(t)
	go func() {
		codeCh <- run(ctx, []string{"http", "--server", fmt.Sprintf("http://localhost:%d", port), local.URL}, &out, &errb, cp)
	}()

	waitFor(t, "Forwarding", func() bool { return strings.Contains(out.String(), "Forwarding") })
	m := regexp.MustCompile(`Forwarding\s+(\S+) ->`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no URL in %q", out.String())
	}
	host := strings.TrimPrefix(m[1], "http://")

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/x", port), nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	waitFor(t, "request line", func() bool { return strings.Contains(out.String(), "GET     /x") })
	cancel()
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("code = %d, stderr %q", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
}

func TestRunHTTPFlagsAfterTarget(t *testing.T) {
	t.Run("errors", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := run(context.Background(), []string{"http", "3000", "--host-header", "bogus"}, &out, &errb, cfgPath(t)); code != 2 || !strings.Contains(errb.String(), "host-header must be") {
			t.Fatalf("code=%d stderr=%q, want 2 from the bad flag value", code, errb.String())
		}
		errb.Reset()
		if code := run(context.Background(), []string{"http", "3000", "extra"}, &out, &errb, cfgPath(t)); code != 2 || !strings.Contains(errb.String(), "usage") {
			t.Fatalf("code=%d stderr=%q, want usage error", code, errb.String())
		}
	})
	t.Run("name applied", func(t *testing.T) {
		su, _ := startTunnelEnv(t, nil)
		local := startLocal(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var out, errb syncBuf
		codeCh := make(chan int, 1)
		cp := cfgPath(t)
		go func() {
			codeCh <- run(ctx, []string{"http", local.URL, "--name", "flagname", "--server", su, "--inspect=false"}, &out, &errb, cp)
		}()
		waitFor(t, "Forwarding", func() bool { return strings.Contains(out.String(), "Forwarding") })
		m := regexp.MustCompile(`Forwarding\s+(\S+) ->`).FindStringSubmatch(out.String())
		if m == nil || !strings.Contains(m[1], "flagname") {
			t.Fatalf("output %q, want URL containing flagname", out.String())
		}
		stopRun(t, cancel, codeCh, &errb)
	})
}

// startTunnelEnv starts a Passway server and a local target, returning the
// server URL and port.
func startTunnelEnv(t *testing.T, auth server.Authenticator) (string, int) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	ts.Config.Handler = server.New(server.Config{Domain: "localhost", PublicScheme: "http", PublicPort: port, Auth: auth})
	ts.Start()
	t.Cleanup(ts.Close)
	return fmt.Sprintf("http://localhost:%d", port), port
}

func startLocal(t *testing.T) *httptest.Server {
	t.Helper()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	t.Cleanup(local.Close)
	return local
}

func stopRun(t *testing.T, cancel context.CancelFunc, codeCh chan int, errb *syncBuf) {
	t.Helper()
	cancel()
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("code = %d, stderr %q", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
}

func TestRunHTTPInspector(t *testing.T) {
	su, port := startTunnelEnv(t, nil)
	local := startLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errb syncBuf
	codeCh := make(chan int, 1)
	cp := cfgPath(t)
	go func() {
		codeCh <- run(ctx, []string{"http", "--server", su, "--inspect-addr", "127.0.0.1:0", local.URL}, &out, &errb, cp)
	}()
	waitFor(t, "Inspector", func() bool { return strings.Contains(out.String(), "Inspector    http://127.0.0.1:") })
	insp := regexp.MustCompile(`Inspector\s+(http://127\.0\.0\.1:\d+)`).FindStringSubmatch(out.String())[1]
	m := regexp.MustCompile(`Forwarding\s+(\S+) ->`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no URL in %q", out.String())
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/x", port), nil)
	req.Host = strings.TrimPrefix(m[1], "http://")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	waitFor(t, "captured request", func() bool {
		r, err := http.Get(insp + "/api/requests")
		if err != nil {
			return false
		}
		defer r.Body.Close()
		var list []json.RawMessage
		return json.NewDecoder(r.Body).Decode(&list) == nil && len(list) == 1
	})
	stopRun(t, cancel, codeCh, &errb)
}

func TestRunHTTPInspectorRejectsPublicAddr(t *testing.T) {
	for _, a := range []string{"0.0.0.0:4040", ":4040", "example.com:4040"} {
		var out, errb bytes.Buffer
		code := run(context.Background(), []string{"http", "--inspect-addr", a, "3000"}, &out, &errb, cfgPath(t))
		if code != 2 || !strings.Contains(errb.String(), "inspector must listen on a loopback address") {
			t.Fatalf("%s: code=%d stderr=%q", a, code, errb.String())
		}
	}
}

func TestListenInspectorFallback(t *testing.T) {
	hold, err := net.Listen("tcp", defaultInspectAddr)
	if err != nil {
		t.Skipf("cannot bind %s: %v", defaultInspectAddr, err)
	}
	defer hold.Close()
	ln, err := listenInspector(defaultInspectAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if p := ln.Addr().(*net.TCPAddr).Port; p < 4041 || p > 4049 {
		t.Fatalf("port = %d, want 4041-4049", p)
	}
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if ln2, err := listenInspector(other.Addr().String()); err == nil {
		ln2.Close()
		t.Fatal("explicit taken address should fail")
	}
}

func TestRunHTTPInspectDisabled(t *testing.T) {
	su, _ := startTunnelEnv(t, nil)
	local := startLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errb syncBuf
	codeCh := make(chan int, 1)
	cp := cfgPath(t)
	go func() {
		codeCh <- run(ctx, []string{"http", "--server", su, "--inspect=false", local.URL}, &out, &errb, cp)
	}()
	waitFor(t, "Forwarding", func() bool { return strings.Contains(out.String(), "Forwarding") })
	if strings.Contains(out.String(), "Inspector") {
		t.Fatalf("unexpected Inspector line: %q", out.String())
	}
	stopRun(t, cancel, codeCh, &errb)
}

func TestRunHTTPTokenPrecedence(t *testing.T) {
	for _, tc := range []struct{ name, env, allowed string }{
		{"env wins over config", "envtok", "envtok"},
		{"config used without env", "", "cfgtok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			su, _ := startTunnelEnv(t, server.StaticTokens{tc.allowed})
			local := startLocal(t)
			cp := cfgPath(t)
			if err := config.Save(cp, config.Config{Token: "cfgtok"}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PASSWAY_TOKEN", tc.env)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out, errb syncBuf
			codeCh := make(chan int, 1)
			go func() {
				codeCh <- run(ctx, []string{"http", "--server", su, "--inspect=false", local.URL}, &out, &errb, cp)
			}()
			waitFor(t, "Forwarding", func() bool { return strings.Contains(out.String(), "Forwarding") })
			stopRun(t, cancel, codeCh, &errb)
		})
	}
}
