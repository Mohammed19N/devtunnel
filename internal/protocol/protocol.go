package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Version is the wire protocol version. Client and server must match.
const Version = 1

// TunnelPath is the HTTP path a client uses to open its tunnel connection.
const TunnelPath = "/_tunnel"

// MaxFrameSize is the largest JSON payload, in bytes, a single frame may carry.
const MaxFrameSize = 64 << 10

// KindHTTP is the StreamHeader kind for proxied HTTP traffic.
const KindHTTP = "http"

// Error codes carried in Error.Code.
const (
	CodeVersionMismatch = "version_mismatch"
	CodeUnauthorized    = "unauthorized"
	CodeInvalidName     = "invalid_name"
	CodeNameTaken       = "name_taken"
	CodeInternal        = "internal"
	CodeLimitReached    = "limit_reached"
	CodeNameNotOwned    = "name_not_owned"
	CodeBanned          = "banned"
)

// Hello is the first frame a client sends during the handshake.
type Hello struct {
	Version       int    `json:"version"`
	Token         string `json:"token,omitempty"`
	Name          string `json:"name,omitempty"`           // explicit; must be granted or fail
	PreferredName string `json:"preferred_name,omitempty"` // best effort (reconnect)
	SessionID     string `json:"session_id"`               // random per CLI process
	Domain        string `json:"domain,omitempty"`         // verified custom domain (cloud only); excludes Name
	ClientVersion string `json:"client_version,omitempty"`
}

// Ready is returned by the server when a tunnel has been granted.
type Ready struct {
	URL       string `json:"url"`
	Name      string `json:"name"`
	Plan      string `json:"plan,omitempty"`
	RateLimit int    `json:"rate_limit,omitempty"`
	Bandwidth int64  `json:"bandwidth,omitempty"`
}

// Error is a handshake or protocol error reported by the server.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error implements the error interface as "<code>: <message>".
func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// HelloResponse is the server's reply to Hello: exactly one of Ready or Error is set.
type HelloResponse struct {
	Ready *Ready `json:"ready,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// StreamHeader opens each proxied stream and declares its kind.
type StreamHeader struct {
	Kind string `json:"kind"`
}

// WriteFrame marshals v as JSON and writes it preceded by a 4-byte big-endian length.
// It returns an error if the encoded payload exceeds MaxFrameSize.
func WriteFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("protocol: marshal frame: %w", err)
	}
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("protocol: frame size %d exceeds max %d", len(payload), MaxFrameSize)
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(payload)))
	copy(buf[4:], payload)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("protocol: write frame: %w", err)
	}
	return nil
}

// ReadFrame reads one length-prefixed JSON frame from r and unmarshals it into v.
// It rejects frames whose declared length exceeds MaxFrameSize.
func ReadFrame(r io.Reader, v any) error {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return fmt.Errorf("protocol: read frame length: %w", err)
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n > MaxFrameSize {
		return fmt.Errorf("protocol: frame size %d exceeds max %d", n, MaxFrameSize)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("protocol: read frame body: %w", err)
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("protocol: unmarshal frame: %w", err)
	}
	return nil
}

// IsFatal reports whether err is a server Error that the client must not retry.
func IsFatal(err error) bool {
	var pe *Error
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Code {
	case CodeVersionMismatch, CodeUnauthorized, CodeInvalidName, CodeNameTaken,
		CodeLimitReached, CodeNameNotOwned, CodeBanned:
		return true
	}
	return false
}

// HandshakeStream is the control stream Handshake runs over.
type HandshakeStream interface {
	io.ReadWriter
	SetReadDeadline(time.Time) error
}

// Handshake sends hello on the control stream and reads the server's reply.
//
// When the server rejects a hello it answers and closes the session at once.
// yamux can then report the hello write as failed ("session shutdown") even
// though the server received it, so a write error is not final: the reply may
// already be buffered, and it carries the real reason. Only when no reply can
// be read is the write error returned.
func Handshake(st HandshakeStream, hello Hello, timeout time.Duration) (HelloResponse, error) {
	var resp HelloResponse
	werr := WriteFrame(st, hello)
	_ = st.SetReadDeadline(time.Now().Add(timeout))
	if err := ReadFrame(st, &resp); err != nil {
		if werr != nil {
			return resp, werr
		}
		return resp, fmt.Errorf("reading handshake response: %w", err)
	}
	_ = st.SetReadDeadline(time.Time{})
	return resp, nil
}
