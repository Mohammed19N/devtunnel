package server

import (
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
