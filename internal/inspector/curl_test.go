package inspector

import (
	"net/http"
	"testing"
)

func TestCurlCommand(t *testing.T) {
	e := &Exchange{Request: Request{
		Method: "POST", URI: "/hook?a=1", Host: "localhost:3000",
		Header: http.Header{
			"Stripe-Signature": {"t=1,v1=abc"},
			"Host":             {"x"}, "Content-Length": {"14"}, "Connection": {"close"},
			"Accept-Encoding": {"gzip"}, "X-Forwarded-For": {"1.2.3.4"}, "X-Forwarded-Proto": {"https"},
		},
		Body: Body{Data: []byte(`{"id":"evt_1"}`), Size: 14},
	}}
	want := `curl -X 'POST' 'https://x.passway.run/hook?a=1' -H 'Stripe-Signature: t=1,v1=abc' --data-binary '{"id":"evt_1"}'`
	if got := CurlCommand(e, "https://x.passway.run"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCurlEscapesAndFallbacks(t *testing.T) {
	e := &Exchange{Request: Request{
		Method: "POST", URI: "/", Host: "localhost:3000",
		Body: Body{Data: []byte("it's"), Size: 9, Truncated: true},
	}}
	want := `curl -X 'POST' 'http://localhost:3000/' --data-binary 'it'\''s' # body truncated`
	if got := CurlCommand(e, ""); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCurlMultiValueHeader(t *testing.T) {
	e := &Exchange{Request: Request{Method: "GET", URI: "/", Host: "h",
		Header: http.Header{"X-A": {"1", "2"}}}}
	want := `curl -X 'GET' 'http://h/' -H 'X-A: 1' -H 'X-A: 2'`
	if got := CurlCommand(e, ""); got != want {
		t.Fatalf("got %s", got)
	}
}
