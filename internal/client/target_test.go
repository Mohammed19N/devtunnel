package client

import "testing"

func TestParseTarget(t *testing.T) {
	ok := map[string]string{
		"3000":           "http://localhost:3000",
		"localhost:8000": "http://localhost:8000",
		"https://x:9":    "https://x:9",
		"http://a.b":     "http://a.b",
	}
	for in, want := range ok {
		u, err := ParseTarget(in)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", in, err)
			continue
		}
		if u.String() != want {
			t.Errorf("ParseTarget(%q) = %s, want %s", in, u, want)
		}
	}
	for _, in := range []string{"", "abc def", "ftp://x"} {
		if u, err := ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) = %s, want error", in, u)
		}
	}
}
