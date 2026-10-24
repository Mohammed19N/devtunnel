// Package proxyproto parses the HAProxy PROXY protocol v2 header so a server
// behind a TCP passthrough proxy (for example nginx stream) sees real client
// addresses.
package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// ErrNoHeader is returned when a connection does not start with a valid PROXY v2 header.
var ErrNoHeader = errors.New("proxyproto: missing or invalid PROXY v2 header")

const maxLen = 4096

var signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// NewListener wraps ln so each accepted conn must start with a PROXY v2 header.
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
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
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
