package server

import (
	"context"
	"crypto/subtle"
	"errors"
)

// Authenticator validates a client token and returns the owning account.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (accountID string, err error)
}

// StaticTokens is an Authenticator over a fixed token list.
// An empty list allows every token (development only).
type StaticTokens []string

// Authenticate implements Authenticator.
func (s StaticTokens) Authenticate(_ context.Context, token string) (string, error) {
	if len(s) == 0 {
		return "anonymous", nil
	}
	ok := false
	for _, t := range s {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			ok = true
		}
	}
	if !ok {
		return "", errors.New("invalid token")
	}
	return "static", nil
}
