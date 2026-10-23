package inspector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/client"
)

// An app that answers before reading the body (401 on a large POST) leaves the
// proxy's transport still reading the request body after the handler returns,
// and the transport then closes it. The record must not race with that, and a
// body cut short must be marked truncated, never shown as complete.
func TestCaptureAppAnswersBeforeReadingBody(t *testing.T) {
	for _, size := range []int{512 << 10, 3 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer app.Close()
			target, _ := url.Parse(app.URL)
			in := New(0, client.NewProxy(target, false, slog.New(slog.NewTextHandler(io.Discard, nil))))
			front := httptest.NewServer(in.Handler())
			defer front.Close()

			payload := bytes.Repeat([]byte("0123456789abcdef"), size/16)
			pr, pw := io.Pipe()
			go func() {
				for off := 0; off < len(payload); off += 32 << 10 {
					if _, err := pw.Write(payload[off:min(off+32<<10, len(payload))]); err != nil {
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
				pw.Close()
			}()
			defer pr.Close()
			req, _ := http.NewRequest("POST", front.URL+"/upload", pr)
			req.ContentLength = int64(len(payload))
			if resp, err := http.DefaultClient.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("status = %d", resp.StatusCode)
				}
			}

			var e *Exchange
			for i := 0; i < 500 && e == nil; i++ {
				if e, _ = in.Store().Get(1); e == nil {
					sleepBrief()
				}
			}
			if e == nil {
				t.Fatal("exchange not recorded")
			}
			b := e.Request.Body
			if !b.Truncated {
				if b.Size != int64(len(payload)) || !bytes.Equal(b.Data, payload) {
					t.Fatalf("body marked complete but has %d of %d bytes (size %d)", len(b.Data), len(payload), b.Size)
				}
				return
			}
			if _, err := in.Replay(context.Background(), 1, nil); !errors.Is(err, ErrTruncated) {
				t.Fatalf("replay of truncated body: err = %v", err)
			}
		})
	}
}
