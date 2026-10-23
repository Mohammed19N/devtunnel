package inspector

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCaptureRecordsExchange(t *testing.T) {
	s := NewStore(0)
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Test", "y")
		w.WriteHeader(201)
		w.Write(b)
	}))
	req := httptest.NewRequest("POST", "/hook?x=1", strings.NewReader(`{"a":1}`))
	req.Header.Set("X-Req", "z")
	h.ServeHTTP(httptest.NewRecorder(), req)
	e, ok := s.Get(1)
	if !ok {
		t.Fatal("no exchange")
	}
	if e.Request.Method != "POST" || e.Request.URI != "/hook?x=1" || e.Request.Header.Get("X-Req") != "z" || string(e.Request.Body.Data) != `{"a":1}` {
		t.Fatalf("request = %+v", e.Request)
	}
	if e.Response.Status != 201 || e.Response.Header.Get("X-Test") != "y" || string(e.Response.Body.Data) != `{"a":1}` {
		t.Fatalf("response = %+v", e.Response)
	}
	if e.Duration <= 0 || e.ReplayOf != 0 {
		t.Fatalf("duration=%v replay=%d", e.Duration, e.ReplayOf)
	}
}

func TestCaptureTruncatesLargeBody(t *testing.T) {
	s := NewStore(0)
	big := bytes.Repeat([]byte("a"), 3<<20)
	var read int64
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, _ = io.Copy(io.Discard, r.Body)
		w.Write(big)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", bytes.NewReader(big)))
	if read != 3<<20 {
		t.Fatalf("handler read %d", read)
	}
	e, _ := s.Get(1)
	for name, b := range map[string]Body{"req": e.Request.Body, "resp": e.Response.Body} {
		if b.Size != 3<<20 || len(b.Data) != MaxBody || !b.Truncated {
			t.Fatalf("%s body size=%d len=%d trunc=%v", name, b.Size, len(b.Data), b.Truncated)
		}
	}
}

func TestCaptureKeepsFlusher(t *testing.T) {
	s := NewStore(0)
	release := make(chan struct{})
	srv := httptest.NewServer(Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first"))
		http.NewResponseController(w).Flush()
		<-release
	})))
	defer srv.Close()
	defer close(release)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "first" {
		t.Fatalf("read %q err %v", buf, err)
	}
}

func TestCaptureHijack(t *testing.T) {
	s := NewStore(0)
	hijackErr := make(chan error, 1)
	srv := httptest.NewServer(Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := http.NewResponseController(w).Hijack()
		hijackErr <- err
		if err == nil {
			c.Close()
		}
	})))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if resp != nil {
		resp.Body.Close()
	}
	_ = err
	if err := <-hijackErr; err != nil {
		t.Fatal(err)
	}
	// the exchange is added after the handler returns
	var e *Exchange
	for i := 0; i < 200 && e == nil; i++ {
		e, _ = s.Get(1)
		if e == nil {
			sleepBrief()
		}
	}
	if e == nil || !e.Upgraded || e.Response.Status != 101 {
		t.Fatalf("exchange = %+v", e)
	}
}

func TestCaptureReplayOf(t *testing.T) {
	s := NewStore(0)
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(WithReplayOf(req.Context(), 7))
	h.ServeHTTP(httptest.NewRecorder(), req)
	e, _ := s.Get(1)
	if e.ReplayOf != 7 {
		t.Fatalf("ReplayOf = %d", e.ReplayOf)
	}
}

func TestCaptureFillsReplayNewID(t *testing.T) {
	s := NewStore(0)
	s.Add(&Exchange{}) // id 1
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	ctx := WithReplayOf(req.Context(), 1)
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	ri := replayInfoFrom(ctx)
	if ri == nil || ri.of != 1 || ri.newID != 2 {
		t.Fatalf("replayInfo = %+v", ri)
	}
	if replayInfoFrom(context.Background()) != nil {
		t.Fatal("expected nil")
	}
}

func TestCaptureSnapshotsRequestBeforeHandler(t *testing.T) {
	s := NewStore(0)
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("X-Orig")
		r.URL.Path = "/rewritten"
		r.URL.RawQuery = ""
		r.Host = "other"
	}))
	req := httptest.NewRequest("GET", "/orig?q=1", nil)
	req.Header.Set("X-Orig", "keep")
	h.ServeHTTP(httptest.NewRecorder(), req)
	e, _ := s.Get(1)
	if e.Request.URI != "/orig?q=1" || e.Request.Header.Get("X-Orig") != "keep" || e.Request.Host == "other" {
		t.Fatalf("request = %+v", e.Request)
	}
}

func TestCaptureDrainsUnreadBody(t *testing.T) {
	s := NewStore(0)
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("0123456789")))
	e, _ := s.Get(1)
	if string(e.Request.Body.Data) != "0123456789" || e.Request.Body.Size != 10 || e.Request.Body.Truncated {
		t.Fatalf("body = %+v", e.Request.Body)
	}
}

func TestCaptureDrainCapMarksTruncated(t *testing.T) {
	s := NewStore(0)
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", bytes.NewReader(make([]byte, maxDrain+100))))
	e, _ := s.Get(1)
	b := e.Request.Body
	if !b.Truncated || len(b.Data) != MaxBody || b.Size < maxDrain || b.Size > maxDrain+1 {
		t.Fatalf("len=%d size=%d trunc=%v", len(b.Data), b.Size, b.Truncated)
	}
}

func TestCapturePanicRecords500AndRepanics(t *testing.T) {
	s := NewStore(0)
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }))
	func() {
		defer func() {
			if v := recover(); v != "boom" {
				t.Fatalf("recovered %v", v)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	e, ok := s.Get(1)
	if !ok || e.Response.Status != 500 {
		t.Fatalf("exchange = %+v", e)
	}
}

func TestCapturePanicMarksUnreadBodyTruncated(t *testing.T) {
	s := NewStore(0)
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body.Read(make([]byte, 2))
		panic("boom")
	}))
	func() {
		defer func() { recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("0123456789")))
	}()
	e, _ := s.Get(1)
	if b := e.Request.Body; !b.Truncated || string(b.Data) != "01" {
		t.Fatalf("body = %+v, want the 2 bytes read, truncated", b)
	}
}

func TestCaptureClosedUnreadBodyIsTruncated(t *testing.T) {
	s := NewStore(0)
	h := Capture(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body.Read(make([]byte, 4))
		r.Body.Close()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("0123456789")))
	e, _ := s.Get(1)
	if b := e.Request.Body; !b.Truncated || string(b.Data) != "0123" || b.Size != 4 {
		t.Fatalf("body = %+v, want the 4 bytes read, truncated", b)
	}
}
