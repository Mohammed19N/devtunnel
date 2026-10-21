package main

import (
	"bytes"
	"context"
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
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"http", "3000", "--host-header", "bogus"}, &out, &errb, cfgPath(t)); code != 2 || !strings.Contains(errb.String(), "host-header must be") {
		t.Fatalf("code=%d stderr=%q, want 2 from the bad flag value", code, errb.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"http", "3000", "extra"}, &out, &errb, cfgPath(t)); code != 2 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("code=%d stderr=%q, want usage error", code, errb.String())
	}
}
