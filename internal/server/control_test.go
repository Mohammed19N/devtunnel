package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/mohammed19n/passway/internal/protocol"
)

func newTestServer(t *testing.T, auth Authenticator) (*Server, *httptest.Server) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	srv := New(Config{
		Domain:       "localhost",
		PublicScheme: "http",
		PublicPort:   port,
		Auth:         auth,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
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
	return protocol.Hello{Version: protocol.Version, Name: name, SessionID: session}
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
