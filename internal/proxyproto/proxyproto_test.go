package proxyproto

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

var sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

func header(verCmd, famProto byte, body []byte) []byte {
	b := append([]byte{}, sig...)
	b = append(b, verCmd, famProto)
	b = binary.BigEndian.AppendUint16(b, uint16(len(body)))
	return append(b, body...)
}

func v4Body(src, dst string, sp, dp uint16) []byte {
	b := append([]byte{}, net.ParseIP(src).To4()...)
	b = append(b, net.ParseIP(dst).To4()...)
	b = binary.BigEndian.AppendUint16(b, sp)
	return binary.BigEndian.AppendUint16(b, dp)
}

func v6Body(src, dst string, sp, dp uint16) []byte {
	b := append([]byte{}, net.ParseIP(src).To16()...)
	b = append(b, net.ParseIP(dst).To16()...)
	b = binary.BigEndian.AppendUint16(b, sp)
	return binary.BigEndian.AppendUint16(b, dp)
}

// serve starts a wrapped listener, dials it, writes payload, and returns the
// server-side conn and the client conn.
func serve(t *testing.T, timeout time.Duration, payload []byte) (net.Conn, net.Conn) {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := NewListener(raw, timeout)
	t.Cleanup(func() { ln.Close() })
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	if len(payload) > 0 {
		if _, err := cli.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, cli
}

func readN(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf)
}

func TestProxyV2IPv4(t *testing.T) {
	p := append(header(0x21, 0x11, v4Body("203.0.113.7", "10.0.0.1", 4321, 443)), "hello"...)
	srv, _ := serve(t, time.Second, p)
	if got := srv.RemoteAddr().String(); got != "203.0.113.7:4321" {
		t.Fatalf("RemoteAddr = %s", got)
	}
	if got := readN(t, srv, 5); got != "hello" {
		t.Fatalf("payload = %q", got)
	}
}

func TestProxyV2IPv6(t *testing.T) {
	p := append(header(0x21, 0x21, v6Body("2001:db8::7", "2001:db8::1", 4321, 443)), "hello"...)
	srv, _ := serve(t, time.Second, p)
	if got := readN(t, srv, 5); got != "hello" {
		t.Fatalf("payload = %q", got)
	}
	if got := srv.RemoteAddr().String(); got != "[2001:db8::7]:4321" {
		t.Fatalf("RemoteAddr = %s", got)
	}
}

func TestProxyV2Local(t *testing.T) {
	p := append(header(0x20, 0x00, nil), "hi"...)
	srv, cli := serve(t, time.Second, p)
	if got, want := srv.RemoteAddr().String(), cli.LocalAddr().String(); got != want {
		t.Fatalf("RemoteAddr = %s, want %s", got, want)
	}
	if got := readN(t, srv, 2); got != "hi" {
		t.Fatalf("payload = %q", got)
	}
}

func TestProxyV2Unspec(t *testing.T) {
	p := append(header(0x21, 0x00, []byte{1, 2, 3}), "hi"...)
	srv, cli := serve(t, time.Second, p)
	if got, want := srv.RemoteAddr().String(), cli.LocalAddr().String(); got != want {
		t.Fatalf("RemoteAddr = %s, want %s", got, want)
	}
	if got := readN(t, srv, 2); got != "hi" {
		t.Fatalf("payload = %q", got)
	}
}

func TestProxyV2TLVsSkipped(t *testing.T) {
	body := append(v4Body("203.0.113.7", "10.0.0.1", 4321, 443), 0x04, 0x00, 0x03, 'a', 'b', 'c')
	p := append(header(0x21, 0x11, body), "hello"...)
	srv, _ := serve(t, time.Second, p)
	if got := readN(t, srv, 5); got != "hello" {
		t.Fatalf("payload = %q", got)
	}
	if got := srv.RemoteAddr().String(); got != "203.0.113.7:4321" {
		t.Fatalf("RemoteAddr = %s", got)
	}
}

func TestNoHeaderClosed(t *testing.T) {
	srv, cli := serve(t, time.Second, []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	if _, err := srv.Read(make([]byte, 10)); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("Read err = %v, want ErrNoHeader", err)
	}
	if got, want := srv.RemoteAddr().String(), cli.LocalAddr().String(); got != want {
		t.Fatalf("RemoteAddr = %s, want %s", got, want)
	}
	cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := cli.Read(make([]byte, 1)); err == nil {
		t.Fatal("client read succeeded, want closed conn")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("conn was not closed")
	}
}

func TestProxyV1Rejected(t *testing.T) {
	srv, _ := serve(t, time.Second, []byte("PROXY TCP4 203.0.113.7 10.0.0.1 4321 443\r\nGET / HTTP/1.1\r\n\r\n"))
	if _, err := srv.Read(make([]byte, 10)); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("Read err = %v, want ErrNoHeader", err)
	}
}

func TestOversizeLengthRejected(t *testing.T) {
	h := append([]byte{}, sig...)
	h = append(h, 0x21, 0x11, 0x20, 0x00) // length 8192
	srv, _ := serve(t, time.Second, h)
	if _, err := srv.Read(make([]byte, 10)); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("Read err = %v, want ErrNoHeader", err)
	}
}

func TestBadVersionRejected(t *testing.T) {
	srv, _ := serve(t, time.Second, header(0x11, 0x11, v4Body("1.2.3.4", "5.6.7.8", 1, 2)))
	if _, err := srv.Read(make([]byte, 10)); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("Read err = %v, want ErrNoHeader", err)
	}
}

func TestShortBodyRejected(t *testing.T) {
	srv, _ := serve(t, time.Second, header(0x21, 0x11, []byte{1, 2, 3}))
	if _, err := srv.Read(make([]byte, 10)); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("Read err = %v, want ErrNoHeader", err)
	}
}

func TestSlowPeerDoesNotBlockAccept(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := NewListener(raw, 5*time.Second)
	defer ln.Close()
	slow, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	srv1, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer srv1.Close()

	fast, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	fast.Write(append(header(0x21, 0x11, v4Body("203.0.113.9", "10.0.0.1", 99, 443)), "ok"...))
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	select {
	case srv2 := <-acc:
		defer srv2.Close()
		if got := readN(t, srv2, 2); got != "ok" {
			t.Fatalf("payload = %q", got)
		}
		if got := srv2.RemoteAddr().String(); got != "203.0.113.9:99" {
			t.Fatalf("RemoteAddr = %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept blocked behind slow peer")
	}
}

func TestHeaderTimeout(t *testing.T) {
	srv, _ := serve(t, 100*time.Millisecond, nil)
	start := time.Now()
	if _, err := srv.Read(make([]byte, 10)); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("Read err = %v, want ErrNoHeader", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestWriteAfterHeaderRoundTrip(t *testing.T) {
	srv, cli := serve(t, time.Second, header(0x21, 0x11, v4Body("203.0.113.7", "10.0.0.1", 1, 2)))
	go srv.Write([]byte("pong"))
	if got := readN(t, cli, 4); got != "pong" {
		t.Fatalf("got %q", got)
	}
}
