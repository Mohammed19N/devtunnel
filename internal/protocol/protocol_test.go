package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"testing"
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
