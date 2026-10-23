package inspector

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newTestInspector() *Inspector {
	return New(0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
}

func do(in *Inspector, method, path, host, ctype, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	in.UI().ServeHTTP(rec, req)
	return rec
}

func TestAPIGuard(t *testing.T) {
	in := newTestInspector()
	cases := []struct {
		name                      string
		method, path, host, ctype string
		hdr                       []string
		want                      int
	}{
		{"bad host", "GET", "/api/requests", "evil.com", "", nil, 403},
		{"bad host with port", "GET", "/api/requests", "evil.com:4040", "", nil, 403},
		{"replay no ctype", "POST", "/api/requests/1/replay", "127.0.0.1:4040", "", nil, 415},
		{"replay wrong ctype", "POST", "/api/requests/1/replay", "127.0.0.1:4040", "text/plain", nil, 415},
		{"delete no ctype", "DELETE", "/api/requests", "127.0.0.1:4040", "", nil, 415},
		{"delete json", "DELETE", "/api/requests", "127.0.0.1:4040", "application/json", nil, 204},
		{"origin evil", "POST", "/api/requests/1/replay", "127.0.0.1:4040", "application/json", []string{"Origin", "http://evil.com"}, 403},
		{"origin evil no ctype", "POST", "/api/requests/1/replay", "127.0.0.1:4040", "", []string{"Origin", "http://evil.com"}, 403},
		{"origin ok", "GET", "/api/requests", "127.0.0.1:4040", "", []string{"Origin", "http://127.0.0.1:4040"}, 200},
		{"origin https rejected", "GET", "/api/requests", "127.0.0.1:4040", "", []string{"Origin", "https://127.0.0.1:4040"}, 403},
		{"suffix host", "GET", "/api/requests", "localhost.evil.com", "", nil, 403},
		{"nip host", "GET", "/api/requests", "127.0.0.1.nip.io", "", nil, 403},
		{"origin null", "GET", "/api/requests", "127.0.0.1:4040", "", []string{"Origin", "null"}, 403},
		{"form ctype", "POST", "/api/requests/1/replay", "127.0.0.1:4040", "application/x-www-form-urlencoded", nil, 415},
		{"json charset", "DELETE", "/api/requests", "127.0.0.1:4040", "application/json; charset=utf-8", nil, 204},
		{"localhost", "GET", "/api/requests", "localhost:4040", "", nil, 200},
		{"ipv6", "GET", "/api/requests", "[::1]:4040", "", nil, 200},
		{"ipv6 no port", "GET", "/api/requests", "[::1]", "", nil, 200},
		{"loopback", "GET", "/api/requests", "127.0.0.1:4040", "", nil, 200},
		{"loopback /8", "GET", "/api/requests", "127.0.0.2:4040", "", []string{"Origin", "http://127.0.0.2:4040"}, 200},
		{"loopback /8 high", "GET", "/api/requests", "127.255.0.9:4040", "", nil, 200},
		{"LOCALHOST", "GET", "/api/requests", "LocalHost:4040", "", nil, 200},
		{"ipv6 long form", "GET", "/api/requests", "[0:0:0:0:0:0:0:1]:4040", "", nil, 200},
		{"not loopback", "GET", "/api/requests", "128.0.0.1:4040", "", nil, 403},
		{"unspecified", "GET", "/api/requests", "0.0.0.0:4040", "", nil, 403},
	}
	for _, c := range cases {
		rec := do(in, c.method, c.path, c.host, c.ctype, "", c.hdr...)
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d", c.name, rec.Code, c.want)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header set", c.name)
		}
	}
}

func TestAPIListGetReplay(t *testing.T) {
	in := newTestInspector()
	capturePost(in, []byte(`{"a":1}`))
	const h = "127.0.0.1:4040"

	var list []Summary
	rec := do(in, "GET", "/api/requests", h, "", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %v err=%v", rec.Body.String(), err)
	}
	var e Exchange
	rec = do(in, "GET", "/api/requests/1", h, "", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || string(e.Request.Body.Data) != `{"a":1}` {
		t.Fatalf("get = %v err=%v", rec.Body.String(), err)
	}
	if rec = do(in, "GET", "/api/requests/77", h, "", ""); rec.Code != 404 {
		t.Fatalf("missing = %d", rec.Code)
	}
	if rec = do(in, "GET", "/api/requests/abc", h, "", ""); rec.Code != 404 {
		t.Fatalf("bad id = %d", rec.Code)
	}
	rec = do(in, "GET", "/api/requests/1/curl", h, "", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || !strings.HasPrefix(rec.Body.String(), "curl -X 'POST' 'http://app.local/hook?a=1'") {
		t.Fatalf("curl = %d %q", rec.Code, rec.Body.String())
	}
	rec = do(in, "POST", "/api/requests/1/replay", h, "application/json", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"id":2}` {
		t.Fatalf("replay = %d %q", rec.Code, rec.Body.String())
	}
	rec = do(in, "POST", "/api/requests/1/replay", h, "application/json", `{"body":"zz","uri":"/x"}`)
	if rec.Code != 200 {
		t.Fatalf("edit replay = %d %q", rec.Code, rec.Body.String())
	}
	if rec = do(in, "POST", "/api/requests/1/replay", h, "application/json", `{"uri":"x"}`); rec.Code != 400 {
		t.Fatalf("bad edit = %d", rec.Code)
	}
	if rec = do(in, "POST", "/api/requests/1/replay", h, "application/json", `{"edit":{"body":"zz"}}`); rec.Code != 400 {
		t.Fatalf("unknown field = %d %q", rec.Code, rec.Body.String())
	}
	if rec = do(in, "POST", "/api/requests/1/replay", h, "application/json", `{"body":"zz"} {}`); rec.Code != 400 {
		t.Fatalf("trailing data = %d %q", rec.Code, rec.Body.String())
	}
	if rec = do(in, "POST", "/api/requests/1/replay", h, "application/json", `{not json`); rec.Code != 400 {
		t.Fatalf("bad json = %d", rec.Code)
	}
	if rec = do(in, "POST", "/api/requests/99/replay", h, "application/json", ""); rec.Code != 404 {
		t.Fatalf("replay missing = %d", rec.Code)
	}

	ws := newTestInspector()
	ws.Store().Add(&Exchange{Request: Request{Method: "GET", URI: "/ws"}, Upgraded: true})
	if rec = do(ws, "POST", "/api/requests/1/replay", h, "application/json", ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "WebSocket") {
		t.Fatalf("upgraded = %d %q", rec.Code, rec.Body.String())
	}

	big := newTestInspector()
	capturePost(big, make([]byte, 2<<20))
	if rec = do(big, "POST", "/api/requests/1/replay", h, "application/json", ""); rec.Code != 409 {
		t.Fatalf("truncated = %d", rec.Code)
	}
}

func TestAPIIndex(t *testing.T) {
	in := newTestInspector()
	rec := do(in, "GET", "/", "localhost:4040", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Passway Inspector") {
		t.Fatalf("index = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAPIEvents(t *testing.T) {
	in := newTestInspector()
	srv := httptest.NewServer(in.UI())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("headers = %v", resp.Header)
	}
	capturePost(in, []byte("x"))
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.After(2 * time.Second)
	var gotEvent bool
	for !gotEvent {
		select {
		case l := <-lines:
			if l == "event: exchange" {
				gotEvent = true
			}
		case <-deadline:
			t.Fatal("no event")
		}
	}
	select {
	case l := <-lines:
		var s Summary
		if !strings.HasPrefix(l, "data: ") || json.Unmarshal([]byte(l[6:]), &s) != nil || s.ID != 1 {
			t.Fatalf("data line = %q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no data")
	}
}

func TestAPIEventsPing(t *testing.T) {
	old := pingInterval
	pingInterval = 20 * time.Millisecond
	defer func() { pingInterval = old }()
	in := newTestInspector()
	srv := httptest.NewServer(in.UI())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), ": ping") {
				got <- sc.Text()
				return
			}
		}
	}()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("no ping")
	}
}

func TestAPIClear(t *testing.T) {
	in := newTestInspector()
	capturePost(in, []byte("x"))
	if rec := do(in, "DELETE", "/api/requests", "localhost:4040", "application/json", ""); rec.Code != 204 {
		t.Fatalf("delete = %d", rec.Code)
	}
	var list []Summary
	rec := do(in, "GET", "/api/requests", "localhost:4040", "", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 0 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("list = %q", rec.Body.String())
	}
}

func TestAPIHardeningHeaders(t *testing.T) {
	in := newTestInspector()
	rec := do(in, "GET", "/", "localhost:4040", "", "")
	h := rec.Header()
	const csp = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; frame-ancestors 'none'"
	if h.Get("X-Frame-Options") != "DENY" || h.Get("Content-Security-Policy") != csp {
		t.Fatalf("index headers = %v", h)
	}
	for _, rec := range []*httptest.ResponseRecorder{
		rec, do(in, "GET", "/api/requests", "localhost:4040", "", ""), do(in, "GET", "/api/requests", "evil.com", "", ""),
	} {
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("missing nosniff: %v", rec.Header())
		}
	}
}

func TestAPIStatus(t *testing.T) {
	in := newTestInspector()
	rec := do(in, "GET", "/api/status", "localhost:4040", "", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"public_url":"","target":"","capacity":500}` {
		t.Fatalf("status before = %d %q", rec.Code, rec.Body.String())
	}
	in.SetPublicURL("https://abc.passway.dev")
	in.SetTarget("http://localhost:3000")
	rec = do(in, "GET", "/api/status", "localhost:4040", "", "")
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"public_url":"https://abc.passway.dev","target":"http://localhost:3000","capacity":500}` {
		t.Fatalf("status = %q", got)
	}
}

func TestUIIndex(t *testing.T) {
	in := newTestInspector()
	rec := do(in, "GET", "/", "localhost:4040", "", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("index = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{"<title>Passway Inspector</title>", "/api/events", "/replay", "/curl", "/api/status"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	ext := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?\s*(https?:)?//`)
	if m := ext.FindString(body); m != "" {
		t.Errorf("index has external load: %q", m)
	}
	if strings.Contains(body, "@import") {
		t.Error("index has @import")
	}
}

// Element.append(null) writes the text "null", which showed up in the top bar
// of the tunnel URL on http tunnels. Direct appends must never be handed null.
func TestUIStatusBarNeverAppendsNull(t *testing.T) {
	i := strings.Index(string(indexHTML), "t.append(\n")
	if i < 0 {
		t.Fatal("status bar append not found")
	}
	end := strings.Index(string(indexHTML)[i:], "\n    );")
	if end < 0 {
		t.Fatal("end of status bar append not found")
	}
	if call := string(indexHTML)[i : i+end]; regexp.MustCompile(`:\s*null\s*,`).MatchString(call) {
		t.Fatalf("t.append(...) argument can be null:\n%s", call)
	}
}

// readEvents streams the SSE lines of resp into a channel.
func readEvents(resp *http.Response) <-chan string {
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	return lines
}

func TestAPIEventsClear(t *testing.T) {
	in := newTestInspector()
	srv := httptest.NewServer(in.UI())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := readEvents(resp)
	in.Store().Clear()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case l := <-lines:
			if l == "event: clear" {
				return
			}
		case <-deadline:
			t.Fatal("no clear event")
		}
	}
}

func TestAPIEventsEndOnCloseStreams(t *testing.T) {
	in := newTestInspector()
	srv := httptest.NewServer(in.UI())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := readEvents(resp)
	in.CloseStreams()
	select {
	case _, ok := <-lines:
		for ok {
			_, ok = <-lines
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event stream still open after CloseStreams")
	}
	// New streams after CloseStreams end at once too.
	r2, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, r2.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("new event stream not ended")
	}
}
