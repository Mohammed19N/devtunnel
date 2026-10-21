// Package client implements the Passway tunnel client: it connects to a
// Passway server, serves proxied streams and forwards them to a local target.
package client

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseTarget turns a user-supplied target such as "3000", "localhost:8000"
// or "https://host:9" into an http(s) URL.
func ParseTarget(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty target")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return nil, fmt.Errorf("invalid target %q", s)
	}
	if !strings.Contains(s, "://") {
		if !strings.Contains(s, ":") {
			if !isDigits(s) {
				return nil, fmt.Errorf("invalid target %q: expected port, host:port or URL", s)
			}
			s = "localhost:" + s
		}
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid target %q: %w", s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid target %q: scheme must be http or https", s)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("invalid target %q: missing host", s)
	}
	return u, nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
