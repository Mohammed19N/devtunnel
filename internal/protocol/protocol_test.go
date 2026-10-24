package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Hello{Version: 1, Token: "t", SessionID: "s"}
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var got Hello
	if err := ReadFrame(&buf, &got); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(MaxFrameSize+1))
	buf.Write(prefix[:])
	var v Hello
	if err := ReadFrame(&buf, &v); err == nil {
		t.Fatal("ReadFrame accepted a frame larger than MaxFrameSize")
	}
}

func TestFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], 10)
	buf.Write(prefix[:])
	buf.WriteString("abc")
	var v Hello
	if err := ReadFrame(&buf, &v); err == nil {
		t.Fatal("ReadFrame accepted a truncated frame")
	}
}

func TestErrorString(t *testing.T) {
	got := (&Error{Code: "name_taken", Message: "x"}).Error()
	if got != "name_taken: x" {
		t.Fatalf("Error() = %q, want %q", got, "name_taken: x")
	}
}

func TestIsFatal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unauthorized", &Error{Code: CodeUnauthorized}, true},
		{"wrapped unauthorized", fmt.Errorf("%w", &Error{Code: CodeUnauthorized}), true},
		{"internal", &Error{Code: CodeInternal}, false},
		{"eof", io.EOF, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsFatal(tc.err); got != tc.want {
				t.Fatalf("IsFatal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsFatalNewCodes(t *testing.T) {
	for _, c := range []string{CodeLimitReached, CodeNameNotOwned, CodeBanned} {
		if !IsFatal(&Error{Code: c}) {
			t.Errorf("%s should be fatal", c)
		}
	}
}

func TestReadyOmitsEmptyPlan(t *testing.T) {
	b, err := json.Marshal(Ready{URL: "u", Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"plan"`, `"rate_limit"`, `"bandwidth"`} {
		if bytes.Contains(b, []byte(k)) {
			t.Errorf("%s present in %s", k, b)
		}
	}
}

// shutdownStream mimics a yamux stream whose session shut down right after
// the server replied: the write reports ErrSessionShutdown even though the
// hello reached the server, and the reply is already buffered for reading.
type shutdownStream struct {
	reply io.Reader
}

func (s shutdownStream) Write([]byte) (int, error)     { return 0, errors.New("session shutdown") }
func (s shutdownStream) Read(p []byte) (int, error)    { return s.reply.Read(p) }
func (shutdownStream) SetReadDeadline(time.Time) error { return nil }

func TestHandshakeWriteRaceKeepsServerError(t *testing.T) {
	var reply bytes.Buffer
	want := &Error{Code: CodeInvalidName, Message: "use either --name or --domain"}
	if err := WriteFrame(&reply, HelloResponse{Error: want}); err != nil {
		t.Fatal(err)
	}
	resp, err := Handshake(shutdownStream{&reply}, Hello{Version: Version}, time.Second)
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if resp.Error == nil || resp.Error.Message != want.Message {
		t.Fatalf("got %+v", resp)
	}
}

func TestHandshakeWriteFailureNoReply(t *testing.T) {
	_, err := Handshake(shutdownStream{bytes.NewReader(nil)}, Hello{Version: Version}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "session shutdown") {
		t.Fatalf("want the write error, got %v", err)
	}
}
