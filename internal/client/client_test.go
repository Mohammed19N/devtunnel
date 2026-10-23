package client

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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/protocol"
	"github.com/mohammed19n/passway/internal/server"
)

func newTestServer(t *testing.T, auth server.Authenticator) (*server.Server, string) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	srv := server.New(server.Config{
		Domain:       "localhost",
		PublicScheme: "http",
		PublicPort:   port,
		Auth:         auth,
		NameHold:     time.Minute,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts.Config.Handler = srv
	ts.Start()
	t.Cleanup(ts.Close)
	return srv, "http://localhost:" + strconv.Itoa(port)
}

func testConfig(serverURL string) Config {
	target, _ := url.Parse("http://127.0.0.1:1")
	return Config{
		Server:         serverURL,
		Target:         target,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		initialBackoff: 20 * time.Millisecond,
	}
}

func runAsync(ctx context.Context, cfg Config) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- Run(ctx, cfg) }()
	return ch
}

func TestRunReady(t *testing.T) {
	_, su := newTestServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan protocol.Ready, 4)
	cfg := testConfig(su)
	cfg.OnReady = func(r protocol.Ready) { ready <- r }
	done := runAsync(ctx, cfg)
	select {
	case r := <-ready:
		if !strings.Contains(r.URL, ".localhost:") {
			t.Fatalf("url %q", r.URL)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ready")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunFatalUnauthorized(t *testing.T) {
	_, su := newTestServer(t, server.StaticTokens{"good"})
	cfg := testConfig(su)
	cfg.Token = "bad"
	select {
	case err := <-runAsync(context.Background(), cfg):
		if !protocol.IsFatal(err) {
			t.Fatalf("err = %v, want fatal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunFatalNameTaken(t *testing.T) {
	_, su := newTestServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan protocol.Ready, 1)
	cfg := testConfig(su)
	cfg.Name = "acme"
	cfg.OnReady = func(r protocol.Ready) { ready <- r }
	first := runAsync(ctx, cfg)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("first not ready")
	}
	cfg2 := testConfig(su)
	cfg2.Name = "acme"
	select {
	case err := <-runAsync(ctx, cfg2):
		var pe *protocol.Error
		if !errors.As(err, &pe) || pe.Code != protocol.CodeNameTaken {
			t.Fatalf("err = %v, want name_taken", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Run did not return")
	}
	cancel()
	<-first
}

func TestRunReconnectsSameName(t *testing.T) {
	srv, su := newTestServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan protocol.Ready, 4)
	cfg := testConfig(su)
	cfg.OnReady = func(r protocol.Ready) { ready <- r }
	done := runAsync(ctx, cfg)
	var first protocol.Ready
	select {
	case first = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("no first ready")
	}
	srv.CloseTunnels()
	select {
	case second := <-ready:
		if second.Name != first.Name {
			t.Fatalf("name %q != %q", second.Name, first.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reconnect")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunNotPasswayServer(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	select {
	case err := <-runAsync(context.Background(), testConfig(ts.URL)):
		if !errors.Is(err, ErrNotPassway) {
			t.Fatalf("err = %v, want ErrNotPassway", err)
		}
		if !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("err = %q, want HTTP 404", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunCustomHandler(t *testing.T) {
	srv, su := newTestServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan protocol.Ready, 1)
	cfg := testConfig(su)
	cfg.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "custom") })
	cfg.OnReady = func(r protocol.Ready) { ready <- r }
	done := runAsync(ctx, cfg)
	var r protocol.Ready
	select {
	case r = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("no ready")
	}
	req, _ := http.NewRequest("GET", su+"/", nil)
	req.Host = strings.TrimPrefix(r.URL, "http://")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "custom" {
		t.Fatalf("body = %q", b)
	}
	_ = srv
	cancel()
	<-done
}
