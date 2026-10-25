// Package proxyproto parses the HAProxy PROXY protocol header (v2 binary, or
// the v1 text form that nginx's stream proxy_protocol sends) so a server
// behind a TCP passthrough proxy sees real client addresses.
package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNoHeader is returned when a connection does not start with a valid PROXY header.
var ErrNoHeader = errors.New("proxyproto: missing or invalid PROXY header")

const maxLen = 4096

var signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// NewListener wraps ln so each accepted conn must start with a PROXY v1 or v2 header.
// The header is parsed lazily on the first Read or RemoteAddr call (in the
// connection's own goroutine, never in Accept), with a read deadline of timeout.
func NewListener(ln net.Listener, timeout time.Duration) net.Listener {
	return &listener{Listener: ln, timeout: timeout}
}

type listener struct {
	net.Listener
	timeout time.Duration
}

func (l *listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &conn{Conn: c, timeout: l.timeout}, nil
}

type conn struct {
	net.Conn
	timeout time.Duration
	once    sync.Once
	remote  net.Addr
	err     error
}

func (c *conn) init() {
	c.once.Do(func() {
		c.remote = c.Conn.RemoteAddr()
		c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
		addr, err := readHeader(c.Conn)
		if err != nil {
			c.err = ErrNoHeader
			c.Conn.Close()
			return
		}
		c.Conn.SetReadDeadline(time.Time{})
		if addr != nil {
			c.remote = addr
		}
	})
}

func (c *conn) Read(p []byte) (int, error) {
	c.init()
	if c.err != nil {
		return 0, c.err
	}
	return c.Conn.Read(p)
}

func (c *conn) RemoteAddr() net.Addr {
	c.init()
	return c.remote
}

// readHeader consumes exactly the header bytes (no buffering) and returns the
// source address, or nil to keep the socket address.
func readHeader(r io.Reader) (net.Addr, error) {
	var fixed [16]byte
	if _, err := io.ReadFull(r, fixed[:1]); err != nil {
		return nil, err
	}
	if fixed[0] == 'P' {
		return readV1(r)
	}
	if _, err := io.ReadFull(r, fixed[1:]); err != nil {
		return nil, err
	}
	if !bytes.Equal(fixed[:12], signature) || fixed[12]>>4 != 2 {
		return nil, ErrNoHeader
	}
	n := int(binary.BigEndian.Uint16(fixed[14:16]))
	if n > maxLen {
		return nil, ErrNoHeader
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	switch fixed[12] & 0x0f {
	case 0: // LOCAL
		return nil, nil
	case 1: // PROXY
	default:
		return nil, ErrNoHeader
	}
	if fixed[13]&0x0f != 1 { // not STREAM (includes UNSPEC)
		return nil, nil
	}
	switch fixed[13] >> 4 {
	case 1:
		if n < 12 {
			return nil, ErrNoHeader
		}
		ip := netip.AddrFrom4([4]byte(body[0:4]))
		return net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, binary.BigEndian.Uint16(body[8:10]))), nil
	case 2:
		if n < 36 {
			return nil, ErrNoHeader
		}
		ip := netip.AddrFrom16([16]byte(body[0:16]))
		return net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, binary.BigEndian.Uint16(body[32:34]))), nil
	}
	return nil, nil // UNSPEC or unknown family
}

// maxV1 is the longest valid v1 header, CRLF included.
const maxV1 = 107

// readV1 reads the rest of a v1 text header ("PROXY TCP4 <src> <dst> <sport>
// <dport>\r\n", after its first byte) one byte at a time, so nothing past the
// header is consumed.
func readV1(r io.Reader) (net.Addr, error) {
	line := []byte{'P'}
	var b [1]byte
	for !bytes.HasSuffix(line, []byte("\r\n")) {
		if len(line) >= maxV1 {
			return nil, ErrNoHeader
		}
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		line = append(line, b[0])
	}
	f := strings.Split(string(line[:len(line)-2]), " ")
	if len(f) < 2 || f[0] != "PROXY" {
		return nil, ErrNoHeader
	}
	if f[1] == "UNKNOWN" {
		return nil, nil // the receiver must ignore the rest of the line
	}
	if len(f) != 6 || (f[1] != "TCP4" && f[1] != "TCP6") {
		return nil, ErrNoHeader
	}
	src, err := netip.ParseAddr(f[2])
	if err != nil || src.Zone() != "" || src.Is4() != (f[1] == "TCP4") {
		return nil, ErrNoHeader
	}
	if dst, err := netip.ParseAddr(f[3]); err != nil || dst.Is4() != src.Is4() {
		return nil, ErrNoHeader
	}
	sport, ok := v1Port(f[4])
	if _, ok2 := v1Port(f[5]); !ok || !ok2 {
		return nil, ErrNoHeader
	}
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(src, sport)), nil
}

// v1Port parses a v1 port: plain decimal digits, 0-65535.
func v1Port(s string) (uint16, bool) {
	if s == "" || len(s) > 5 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > 65535 {
		return 0, false
	}
	return uint16(n), true
}
