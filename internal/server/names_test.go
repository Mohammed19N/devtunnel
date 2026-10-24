package server

import (
	"fmt"
	"strings"
	"testing"
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

func TestValidateNameSyntaxOnly(t *testing.T) {
	for _, n := range []string{"paypal", "stripe-hooks", "applesupport"} {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil (blocklist is separate)", n, err)
		}
	}
}

func TestBlockedWordLookalikes(t *testing.T) {
	for _, n := range []string{"paypal", "my-paypal-help", "login-now", "secure-bank", "applesupport",
		"stripe-hooks", "pay-pal-x", "paypa1", "g00gle-sso", "rnicrosoft", "vvellsfargo", "githubhooks"} {
		if BlockedWord(n) == "" {
			t.Errorf("%q not blocked", n)
		}
	}
}

func TestBlockedWordAllowsOrdinary(t *testing.T) {
	for _, n := range []string{"acme", "hooks-dev", "my-shop-2", "quick-otter-42"} {
		if w := BlockedWord(n); w != "" {
			t.Errorf("%q blocked by %q", n, w)
		}
	}
}

func TestRandomNamesNeverBlocked(t *testing.T) {
	for _, a := range adjectives {
		for _, an := range animals {
			for i := 0; i < 100; i++ {
				n := fmt.Sprintf("%s-%s-%d", a, an, i)
				if w := BlockedWord(n); w != "" {
					t.Fatalf("%q blocked by %q", n, w)
				}
			}
		}
	}
}

func TestWordListsAvoidBlocklist(t *testing.T) {
	for _, list := range [][]string{adjectives, animals} {
		for _, w := range list {
			if b := BlockedWord(w); b != "" {
				t.Errorf("%q contains blocked word %q", w, b)
			}
		}
	}
}
