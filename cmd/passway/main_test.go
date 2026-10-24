package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/mohammed19n/passway/internal/client"
	"github.com/mohammed19n/passway/internal/config"
	"github.com/mohammed19n/passway/internal/protocol"
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

	// An open inspector tab (SSE stream) must not hold up Ctrl+C.
	ev, err := http.Get(insp + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Body.Close()
	start := time.Now()
	stopRun(t, cancel, codeCh, &errb)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("shutdown took %v with an event stream open", d)
	}
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
	ln, err := listenInspector(defaultInspectAddr, true)
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
	if ln2, err := listenInspector(other.Addr().String(), false); err == nil {
		ln2.Close()
		t.Fatal("explicit taken address should fail")
	}
	if ln2, err := listenInspector(defaultInspectAddr, false); err == nil {
		ln2.Close()
		t.Fatal("explicitly requested default address should not fall back")
	}
}

func TestRunHTTPExplicitInspectAddrTaken(t *testing.T) {
	hold, err := net.Listen("tcp", defaultInspectAddr)
	if err != nil {
		t.Skipf("cannot bind %s: %v", defaultInspectAddr, err)
	}
	defer hold.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // a fallback would keep running
	defer cancel()
	var out, errb syncBuf
	code := run(ctx, []string{"http", "--inspect-addr", defaultInspectAddr, "--server", "http://127.0.0.1:1", "3000"}, &out, &errb, cfgPath(t))
	if code != 1 || !strings.Contains(errb.String(), defaultInspectAddr) {
		t.Fatalf("code=%d stderr=%q, want bind error for %s", code, errb.String(), defaultInspectAddr)
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

// cloudDir is a minimal server.Directory for CLI tests: one Free account that owns one name.
type cloudDir struct{ banned bool }

func (cloudDir) Authenticate(context.Context, string) (server.Account, error) {
	return server.Account{ID: "u1", Login: "alice", Plan: server.PlanFree}, nil
}
func (cloudDir) Names(context.Context, server.Account) ([]string, error) {
	return []string{"quick-otter-42"}, nil
}
func (cloudDir) Owns(_ context.Context, _ server.Account, n string) (bool, error) {
	return n == "quick-otter-42", nil
}
func (d cloudDir) Banned(context.Context, server.Account, string) (bool, error) { return d.banned, nil }
func (cloudDir) AddUsage(context.Context, server.Account, int64) error          { return nil }
func (cloudDir) Usage(context.Context, server.Account) (int64, error)           { return 0, nil }
func (cloudDir) PlanUsage(context.Context, string) (int64, error)               { return 0, nil }
func (cloudDir) BannedLists(context.Context) ([]string, []string, error)        { return nil, nil, nil }
func (cloudDir) RevokedTokens(context.Context, []int64) ([]int64, error)        { return nil, nil }

func startCloudEnv(t *testing.T) string { return startCloudEnvDir(t, cloudDir{}) }

func startCloudEnvDir(t *testing.T, d cloudDir) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	ts.Config.Handler = server.New(server.Config{Domain: "localhost", PublicScheme: "http", PublicPort: port, Directory: d})
	ts.Start()
	t.Cleanup(ts.Close)
	return fmt.Sprintf("http://localhost:%d", port)
}

func TestRunHTTPPlanLine(t *testing.T) {
	su := startCloudEnv(t)
	local := startLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errb syncBuf
	codeCh := make(chan int, 1)
	cp := cfgPath(t)
	go func() {
		codeCh <- run(ctx, []string{"http", "--server", su, "--inspect=false", local.URL}, &out, &errb, cp)
	}()
	waitFor(t, "Plan line", func() bool {
		return strings.Contains(out.String(), "Plan         Free - 60 req/min - 1 GB/month")
	})
	stopRun(t, cancel, codeCh, &errb)
}

func TestRunHTTPFatalMessage(t *testing.T) {
	su := startCloudEnv(t)
	local := startLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out1, err1 syncBuf
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- run(ctx, []string{"http", "--server", su, "--inspect=false", local.URL}, &out1, &err1, cfgPath(t))
	}()
	waitFor(t, "first tunnel", func() bool { return strings.Contains(out1.String(), "Forwarding") })

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"http", "--server", su, "--inspect=false", local.URL}, &out, &errb, cfgPath(t))
	want := "error: tunnel limit reached: your Free plan allows 1 concurrent tunnel(s)\nmanage your account at " + su + "/dashboard\n"
	if code != 1 || errb.String() != want {
		t.Fatalf("code=%d stderr=%q, want %q", code, errb.String(), want)
	}
	stopRun(t, cancel, codeCh, &err1)
}

func TestRunHTTPFatalMessageOtherCodes(t *testing.T) {
	local := startLocal(t)
	cases := []struct {
		name string
		dir  cloudDir
		args []string
		want string
	}{
		{"name_not_owned", cloudDir{}, []string{"--name", "other-name"}, "error: you don't own \"other-name\"; claim it in the dashboard\nmanage your account at %s/dashboard\n"},
		{"banned", cloudDir{banned: true}, nil, "error: account suspended\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			su := startCloudEnvDir(t, tc.dir)
			args := append([]string{"http", "--server", su, "--inspect=false", local.URL}, tc.args...)
			var out, errb bytes.Buffer
			code := run(context.Background(), args, &out, &errb, cfgPath(t))
			want := tc.want
			if strings.Contains(want, "%s") {
				want = fmt.Sprintf(want, su)
			}
			if code != 1 || errb.String() != want {
				t.Fatalf("code=%d stderr=%q, want %q", code, errb.String(), want)
			}
		})
	}
}

func TestPlanLine(t *testing.T) {
	for _, tc := range []struct {
		r    protocol.Ready
		want string
	}{
		{protocol.Ready{Plan: "free", RateLimit: 60, Bandwidth: 1 << 30}, "Free - 60 req/min - 1 GB/month"},
		{protocol.Ready{Plan: "pro", RateLimit: 600, Bandwidth: 50 << 30}, "Pro - 600 req/min - 50 GB/month"},
		{protocol.Ready{Plan: "unlimited"}, ""},
		{protocol.Ready{}, ""},
		{protocol.Ready{Plan: "free", RateLimit: 60, Bandwidth: 3 << 29}, "Free - 60 req/min - 1.5 GB/month"},
	} {
		if got := planLine(tc.r); got != tc.want {
			t.Errorf("planLine(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

func TestRunHTTPGaveUpMessage(t *testing.T) {
	err := fmt.Errorf("%w: could not reach %s after %s: %v", client.ErrGaveUp, "https://x.example", 2*time.Minute, errors.New("dial: boom"))
	var errb bytes.Buffer
	code := reportRunError(context.Background(), &errb, "https://x.example", err)
	want := "error: could not reach https://x.example after 2m0s: dial: boom\n"
	if code != 1 || errb.String() != want {
		t.Fatalf("code=%d stderr=%q, want %q", code, errb.String(), want)
	}
}
