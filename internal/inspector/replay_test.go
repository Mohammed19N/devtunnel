package inspector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func ptr[T any](v T) *T { return &v }

type seen struct {
	mu                  sync.Mutex
	method, uri, hdr    string
	body                string
	contentLen          int64
	contentLenHdr, host string
}

func (s *seen) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.method, s.uri, s.hdr = r.Method, r.URL.RequestURI(), r.Header.Get("X-Test")
		s.body, s.contentLen = string(b), r.ContentLength
		s.contentLenHdr, s.host = r.Header.Get("Content-Length"), r.Host
		w.WriteHeader(202)
	})
}

func capturePost(in *Inspector, body []byte) {
	req := httptest.NewRequest("POST", "/hook?a=1", bytes.NewReader(body))
	req.Host = "app.local"
	req.Header.Set("X-Test", "v")
	in.Handler().ServeHTTP(httptest.NewRecorder(), req)
}

func TestReplaySendsSameRequest(t *testing.T) {
	var s seen
	in := New(0, s.handler())
	capturePost(in, []byte(`{"a":1}`))
	id, err := in.Replay(context.Background(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Fatalf("id = %d", id)
	}
	if s.method != "POST" || s.uri != "/hook?a=1" || s.hdr != "v" || s.body != `{"a":1}` || s.host != "app.local" {
		t.Fatalf("seen = %+v", &s)
	}
	e, ok := in.Store().Get(2)
	if !ok || e.ReplayOf != 1 {
		t.Fatalf("exchange = %+v ok=%v", e, ok)
	}
}

func TestReplayWithEdit(t *testing.T) {
	var s seen
	in := New(0, s.handler())
	capturePost(in, []byte(`{"a":1}`))
	_, err := in.Replay(context.Background(), 1, &Edit{Method: ptr("PUT"), URI: ptr("/other"), Body: ptr(`{"b":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	if s.method != "PUT" || s.uri != "/other" || s.body != `{"b":2}` {
		t.Fatalf("seen = %+v", &s)
	}
	if s.contentLen != 7 || s.contentLenHdr != "7" {
		t.Fatalf("content-length = %d / %q", s.contentLen, s.contentLenHdr)
	}
}

func TestReplayBadEdit(t *testing.T) {
	var s seen
	in := New(0, s.handler())
	capturePost(in, []byte(`x`))
	for _, e := range []*Edit{{URI: ptr("nope")}, {Method: ptr("BAD METHOD")}} {
		if _, err := in.Replay(context.Background(), 1, e); !errors.Is(err, ErrBadEdit) {
			t.Fatalf("edit %+v: err = %v", e, err)
		}
	}
}

func TestReplayTruncated(t *testing.T) {
	var s seen
	in := New(0, s.handler())
	capturePost(in, bytes.Repeat([]byte("a"), 2<<20))
	if _, err := in.Replay(context.Background(), 1, nil); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v", err)
	}
	if _, err := in.Replay(context.Background(), 1, &Edit{Body: ptr("x")}); err != nil {
		t.Fatal(err)
	}
	if s.body != "x" {
		t.Fatalf("body = %q", s.body)
	}
}

func TestReplayNotFound(t *testing.T) {
	in := New(0, http.NotFoundHandler())
	if _, err := in.Replay(context.Background(), 9, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
