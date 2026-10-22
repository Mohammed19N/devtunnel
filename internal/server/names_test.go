package server

import (
	"strings"
	"testing"

	"github.com/mohammed19n/passway/internal/protocol"
)

func TestValidateName(t *testing.T) {
	for _, n := range []string{"abc", "quick-otter-42", strings.Repeat("a", 32)} {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range []string{"ab", strings.Repeat("a", 33), "-abc", "abc-", "ABC", "a.b", "www", "admin", "docs"} {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

func TestRandomNameValid(t *testing.T) {
	for i := 0; i < 1000; i++ {
		n := RandomName()
		if err := ValidateName(n); err != nil {
			t.Fatalf("RandomName() = %q invalid: %v", n, err)
		}
	}
}

func TestValidateNameBlocklist(t *testing.T) {
	for _, n := range []string{"paypal", "my-paypal-help", "login-now", "secure-bank", "applesupport"} {
		if err := ValidateName(n); err == nil {
			t.Errorf("%q accepted", n)
		} else {
			wantCode(t, err, protocol.CodeInvalidName)
		}
	}
	if err := ValidateName("happy-otter-12"); err != nil {
		t.Fatal(err)
	}
}

func TestWordListsAvoidBlocklist(t *testing.T) {
	for _, list := range [][]string{adjectives, animals} {
		for _, w := range list {
			if b := blockedWord(w); b != "" {
				t.Errorf("%q contains blocked word %q", w, b)
			}
		}
	}
}
